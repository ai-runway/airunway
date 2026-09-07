"""Configure Hermes for AI Runway and expose only its OpenAI-compatible API."""

from __future__ import annotations

import hmac
import http.client
import json
import os
import secrets
import signal
import socket
import subprocess
import sys
import threading
import time
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


STATE_DIR = Path("/tmp/airunway-hermes")
DEFAULT_INTERNAL_PORT = 8642
MAX_REQUEST_BYTES = 4 * 1024 * 1024
MAX_CONCURRENT_REQUESTS = 32
HEALTH_PROBE_CONNECTION_RESERVE = 2
REQUEST_READ_TIMEOUT_SECONDS = 30.0
HEALTH_UPSTREAM_TIMEOUT_SECONDS = 10.0
UPSTREAM_REQUEST_TIMEOUT_SECONDS = 600.0
GATEWAY_WAIT_POLL_SECONDS = 0.1
GATEWAY_SHUTDOWN_TIMEOUT_SECONDS = 30.0
GATEWAY_KILL_TIMEOUT_SECONDS = 5.0


class RequestReadTimeout(TimeoutError):
    """Raised when a client does not finish its request body in time."""


def read_exact_body(
    stream: Any,
    connection: socket.socket,
    length: int,
    timeout: float,
) -> bytes:
    """Read exactly length bytes within one absolute wall-clock deadline."""

    deadline = time.monotonic() + timeout
    previous_timeout = connection.gettimeout()
    chunks: list[bytes] = []
    remaining = length
    try:
        while remaining:
            time_left = deadline - time.monotonic()
            if time_left <= 0:
                raise RequestReadTimeout("request body read timed out")
            connection.settimeout(time_left)
            try:
                chunk = stream.read1(min(remaining, 64 * 1024))
            except TimeoutError as exc:
                raise RequestReadTimeout("request body read timed out") from exc
            if not chunk:
                raise ValueError("request body ended before Content-Length bytes arrived")
            chunks.append(chunk)
            remaining -= len(chunk)
        return b"".join(chunks)
    finally:
        connection.settimeout(previous_timeout)


class HeaderDeadlineHandler(BaseHTTPRequestHandler):
    """Base handler that enforces one absolute request-line/header deadline."""

    def setup(self) -> None:
        super().setup()
        self._header_deadline_lock = threading.Lock()
        self._header_deadline_timer: threading.Timer | None = None
        self._header_deadline_token: object | None = None
        self._header_deadline_expired = False

    def _start_header_deadline(self) -> None:
        timeout = self.server.request_timeout
        token = object()
        timer = threading.Timer(timeout, self._expire_header_deadline, args=(token,))
        timer.daemon = True
        with self._header_deadline_lock:
            self._header_deadline_expired = False
            self._header_deadline_token = token
            self._header_deadline_timer = timer
        timer.start()

    def _expire_header_deadline(self, token: object) -> None:
        with self._header_deadline_lock:
            if self._header_deadline_token is not token:
                return
            self._header_deadline_token = None
            self._header_deadline_timer = None
            self._header_deadline_expired = True
        self.close_connection = True
        try:
            self.connection.shutdown(socket.SHUT_RD)
        except OSError:
            pass

    def _finish_header_deadline(self) -> bool:
        with self._header_deadline_lock:
            timer = self._header_deadline_timer
            self._header_deadline_token = None
            self._header_deadline_timer = None
            expired = self._header_deadline_expired
        if timer is not None:
            timer.cancel()
        return expired

    def handle_one_request(self) -> None:
        self._start_header_deadline()
        try:
            super().handle_one_request()
        finally:
            self._finish_header_deadline()

    def parse_request(self) -> bool:
        parsed = super().parse_request()
        if not self._finish_header_deadline():
            return parsed
        self.close_connection = True
        if parsed:
            self.send_error(HTTPStatus.REQUEST_TIMEOUT, "Request headers timed out")
        return False


class BoundedThreadingHTTPServer(ThreadingHTTPServer):
    """Threading HTTP server with bounded work and absolute read deadlines."""

    daemon_threads = True

    def __init__(
        self,
        server_address: tuple[str, int],
        request_handler_class: type[BaseHTTPRequestHandler],
        *,
        max_workers: int = MAX_CONCURRENT_REQUESTS,
        request_timeout: float = REQUEST_READ_TIMEOUT_SECONDS,
    ) -> None:
        if max_workers < 1:
            raise ValueError("max_workers must be at least 1")
        if request_timeout <= 0:
            raise ValueError("request_timeout must be positive")
        self.request_timeout = request_timeout
        self._connection_slots = threading.BoundedSemaphore(
            max_workers + HEALTH_PROBE_CONNECTION_RESERVE
        )
        self._work_slots = threading.BoundedSemaphore(max_workers)
        self._health_slots = threading.BoundedSemaphore(
            HEALTH_PROBE_CONNECTION_RESERVE
        )
        super().__init__(server_address, request_handler_class)

    def get_request(self) -> tuple[socket.socket, Any]:
        request, client_address = super().get_request()
        request.settimeout(self.request_timeout)
        return request, client_address

    def process_request(self, request: socket.socket, client_address: Any) -> None:
        if not self._connection_slots.acquire(blocking=False):
            try:
                request.sendall(
                    b"HTTP/1.1 503 Service Unavailable\r\n"
                    b"Content-Type: application/json\r\n"
                    b"Connection: close\r\n"
                    b"Content-Length: 45\r\n\r\n"
                    b'{"error":{"message":"server is at capacity"}}'
                )
            except OSError:
                pass
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self._connection_slots.release()
            raise

    def process_request_thread(self, request: socket.socket, client_address: Any) -> None:
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._connection_slots.release()

    def acquire_work_slot(self) -> bool:
        return self._work_slots.acquire(blocking=False)

    def release_work_slot(self) -> None:
        self._work_slots.release()

    def acquire_health_slot(self) -> bool:
        return self._health_slots.acquire(blocking=False)

    def release_health_slot(self) -> None:
        self._health_slots.release()


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise ValueError(f"{name} is required")
    return value


def mounted_config() -> dict[str, Any]:
    path = Path(os.environ.get("AIRUNWAY_AGENT_CONFIG", "/etc/airunway/agent.json"))
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"{path} must contain a JSON object")
    return data


def model_config() -> tuple[str, str, str, str]:
    if os.environ.get("ANTHROPIC_MODEL"):
        return (
            "anthropic",
            required("ANTHROPIC_MODEL"),
            required("ANTHROPIC_BASE_URL"),
            "${ANTHROPIC_API_KEY}",
        )
    if os.environ.get("AZURE_OPENAI_MODEL"):
        return (
            "azure-foundry",
            required("AZURE_OPENAI_MODEL"),
            required("AZURE_OPENAI_ENDPOINT"),
            "${AZURE_OPENAI_API_KEY}",
        )
    return (
        "custom",
        required("OPENAI_MODEL"),
        required("OPENAI_BASE_URL"),
        "${OPENAI_API_KEY}",
    )


def internal_port_for(external_port: int) -> int:
    """Keep Hermes' loopback listener distinct from the public listener."""
    if external_port == DEFAULT_INTERNAL_PORT:
        return DEFAULT_INTERNAL_PORT + 1
    return DEFAULT_INTERNAL_PORT


def write_runtime_config(
    config: dict[str, Any], api_key: str, internal_port: int
) -> None:
    provider, model, base_url, model_key = model_config()
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    payload = (
        "model:\n"
        f"  provider: {json.dumps(provider)}\n"
        f"  default: {json.dumps(model)}\n"
        f"  base_url: {json.dumps(base_url)}\n"
        f"  api_key: {json.dumps(model_key)}\n"
        "tool_loop_guardrails:\n"
        "  hard_stop_enabled: true\n"
        # The public contract is chat-only. An explicit empty platform list is
        # an allowlist, not a denylist, and avoids known late tool injection
        # paths that can defeat category-level disabled_toolsets settings.
        "platform_toolsets:\n"
        "  api_server: []\n"
        "gateway:\n"
        "  api_server:\n"
        "    enabled: true\n"
        f"    port: {internal_port}\n"
        "    host: 127.0.0.1\n"
        f"    key: {json.dumps(api_key)}\n"
        "    model_name: hermes-agent\n"
    )
    (STATE_DIR / "config.yaml").write_text(payload, encoding="utf-8")
    (STATE_DIR / "config.yaml").chmod(0o600)
    system_prompt = config.get("systemPrompt")
    if isinstance(system_prompt, str) and system_prompt.strip():
        (STATE_DIR / "SOUL.md").write_text(system_prompt.strip() + "\n", encoding="utf-8")


class ProxyHandler(HeaderDeadlineHandler):
    internal_key = ""
    access_token = ""
    internal_port = DEFAULT_INTERNAL_PORT

    def _json_error(self, status: HTTPStatus, message: str) -> None:
        payload = json.dumps({"error": {"message": message}}).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        if status == HTTPStatus.UNAUTHORIZED:
            self.send_header("WWW-Authenticate", "Bearer")
        self.end_headers()
        self.wfile.write(payload)

    def _authenticated(self) -> bool:
        authorization = self.headers.get("Authorization", "")
        prefix = "Bearer "
        scheme = authorization[: len(prefix)]
        if scheme.lower() != prefix.lower() or not hmac.compare_digest(
            authorization[len(prefix) :], self.access_token
        ):
            self._json_error(
                HTTPStatus.UNAUTHORIZED,
                "valid bearer authentication is required",
            )
            return False
        return True

    def _body(self) -> bytes | None:
        if self.headers.get("Transfer-Encoding"):
            raise ValueError("chunked request bodies are not supported")
        raw_length = self.headers.get("Content-Length")
        if self.command == "GET":
            if raw_length is not None:
                raise ValueError("GET request bodies are not supported")
            return None
        if raw_length is None:
            raise ValueError("a non-empty Content-Length is required")
        try:
            length = int(raw_length)
        except ValueError as exc:
            raise ValueError("Content-Length must be an integer") from exc
        if length < 1:
            raise ValueError("a non-empty Content-Length is required")
        if length > MAX_REQUEST_BYTES:
            raise OverflowError("request body exceeds 4 MiB")
        return read_exact_body(
            self.rfile,
            self.connection,
            length,
            self.server.request_timeout,
        )

    def _proxy(self) -> None:
        allowed = {
            ("GET", "/healthz"),
            ("GET", "/readyz"),
            ("GET", "/v1/models"),
            ("POST", "/v1/chat/completions"),
        }
        if (self.command, self.path) not in allowed:
            self._json_error(HTTPStatus.NOT_FOUND, "not found")
            return
        health_request = self.path in {"/healthz", "/readyz"}
        health_slot_acquired = False
        work_slot_acquired = False
        if health_request:
            if not self.server.acquire_health_slot():
                self._json_error(
                    HTTPStatus.SERVICE_UNAVAILABLE, "server is at capacity"
                )
                return
            health_slot_acquired = True
        else:
            if not self.server.acquire_work_slot():
                self._json_error(
                    HTTPStatus.SERVICE_UNAVAILABLE, "server is at capacity"
                )
                return
            work_slot_acquired = True
        try:
            if not health_request and not self._authenticated():
                return
            upstream_path = "/health" if health_request else self.path
            try:
                body = self._body()
            except RequestReadTimeout as exc:
                self.close_connection = True
                self._json_error(HTTPStatus.REQUEST_TIMEOUT, str(exc))
                return
            except OverflowError as exc:
                self._json_error(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, str(exc))
                return
            except ValueError as exc:
                self._json_error(HTTPStatus.BAD_REQUEST, str(exc))
                return
            headers = {
                key: value
                for key, value in self.headers.items()
                if key.lower() not in {"authorization", "connection", "host"}
            }
            headers["Authorization"] = f"Bearer {self.internal_key}"
            upstream_timeout = (
                HEALTH_UPSTREAM_TIMEOUT_SECONDS
                if health_request
                else UPSTREAM_REQUEST_TIMEOUT_SECONDS
            )
            connection = http.client.HTTPConnection(
                "127.0.0.1", self.internal_port, timeout=upstream_timeout
            )
            try:
                connection.request(
                    self.command, upstream_path, body=body, headers=headers
                )
                response = connection.getresponse()
                payload = response.read()
                self.send_response(response.status)
                for key, value in response.getheaders():
                    if key.lower() not in {
                        "connection",
                        "content-length",
                        "transfer-encoding",
                    }:
                        self.send_header(key, value)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
            except OSError:
                self._json_error(
                    HTTPStatus.SERVICE_UNAVAILABLE, "gateway is not ready"
                )
            finally:
                connection.close()
        finally:
            if health_slot_acquired:
                self.server.release_health_slot()
            if work_slot_acquired:
                self.server.release_work_slot()

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self._proxy()

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self._proxy()

    def log_message(self, fmt: str, *args: Any) -> None:
        print(f"{self.address_string()} {fmt % args}", file=sys.stderr, flush=True)


def serve_until_gateway_exit(
    gateway: subprocess.Popen[Any],
    server: ThreadingHTTPServer,
    stopping: threading.Event | None = None,
    *,
    shutdown_timeout: float = GATEWAY_SHUTDOWN_TIMEOUT_SECONDS,
    kill_timeout: float = GATEWAY_KILL_TIMEOUT_SECONDS,
) -> int:
    """Serve the public proxy only for as long as the Hermes gateway lives."""
    if stopping is None:
        stopping = threading.Event()
    server_thread = threading.Thread(
        target=server.serve_forever,
        name="airunway-hermes-proxy",
        daemon=True,
    )
    server_thread.start()
    accepting_stopped = False

    def stop_accepting() -> None:
        nonlocal accepting_stopped
        if accepting_stopped:
            return
        server.shutdown()
        server.server_close()
        server_thread.join(timeout=5)
        accepting_stopped = True

    try:
        while not stopping.is_set():
            try:
                return gateway.wait(timeout=GATEWAY_WAIT_POLL_SECONDS)
            except subprocess.TimeoutExpired:
                pass
        # Refuse new public traffic before waiting for the gateway to drain.
        # Already accepted request-handler threads remain alive and can finish
        # while the gateway performs its bounded graceful shutdown.
        stop_accepting()
        if gateway.poll() is None:
            gateway.terminate()
        try:
            return gateway.wait(timeout=shutdown_timeout)
        except subprocess.TimeoutExpired:
            gateway.kill()
            return gateway.wait(timeout=kill_timeout)
    finally:
        stop_accepting()


def install_shutdown_handlers(stopping: threading.Event) -> None:
    """Translate process shutdown signals into the server's drain path."""

    def stop(_signum: int, _frame: Any) -> None:
        stopping.set()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)


def main() -> None:
    config = mounted_config()
    external_port = int(os.environ.get("AIRUNWAY_AGENT_PORT", "8080"))
    if not 1 <= external_port <= 65535:
        raise ValueError("AIRUNWAY_AGENT_PORT must be between 1 and 65535")
    internal_port = internal_port_for(external_port)
    internal_key = secrets.token_hex(32)
    write_runtime_config(config, internal_key, internal_port)
    os.environ.update(
        {
            "HOME": str(STATE_DIR),
            "HERMES_HOME": str(STATE_DIR),
            "HERMES_WRITE_SAFE_ROOT": str(STATE_DIR),
            "API_SERVER_ENABLED": "true",
            "API_SERVER_HOST": "127.0.0.1",
            "API_SERVER_PORT": str(internal_port),
            "API_SERVER_KEY": internal_key,
        }
    )

    if os.environ.get("AIRUNWAY_AGENT_MODE", "server") == "job":
        task = config.get("task") or config.get("prompt")
        if not isinstance(task, str) or not task.strip():
            raise ValueError("job lifecycle requires spec.config.task or spec.config.prompt")
        os.execvpe("hermes", ["hermes", "-z", task], os.environ)

    ProxyHandler.internal_key = internal_key
    ProxyHandler.access_token = required("AIRUNWAY_AGENT_API_KEY")
    ProxyHandler.internal_port = internal_port
    server = BoundedThreadingHTTPServer(("0.0.0.0", external_port), ProxyHandler)
    gateway = subprocess.Popen(
        ["hermes", "gateway", "run", "--no-supervise"], env=os.environ
    )
    stopping = threading.Event()
    install_shutdown_handlers(stopping)
    print(f"AI Runway Hermes proxy listening on :{external_port}", flush=True)
    try:
        return_code = serve_until_gateway_exit(gateway, server, stopping)
    finally:
        if gateway.poll() is None:
            gateway.terminate()
            try:
                gateway.wait(timeout=30)
            except subprocess.TimeoutExpired:
                gateway.kill()
                gateway.wait(timeout=5)
    if not stopping.is_set():
        print(
            f"Hermes gateway exited unexpectedly with status {return_code}",
            file=sys.stderr,
            flush=True,
        )
        raise SystemExit(return_code if return_code > 0 else 1)


if __name__ == "__main__":
    main()

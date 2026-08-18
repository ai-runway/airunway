"""Small HTTP/runtime contract shared by AI Runway's Python agent images."""

from __future__ import annotations

import asyncio
import hmac
import json
import os
import socket
import sys
import threading
import time
import uuid
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Callable, Protocol


MAX_CONCURRENT_REQUESTS = 32
HEALTH_PROBE_CONNECTION_RESERVE = 2
REQUEST_READ_TIMEOUT_SECONDS = 30.0


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
                # BufferedReader.read() may issue multiple socket reads, each
                # with a fresh idle timeout. read1() performs at most one raw
                # read, letting us enforce the absolute deadline between reads.
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
        # Keep a small connection reserve for health probes while model work is
        # saturated. Header parsing remains bounded, and non-probe handlers
        # separately consume one of the max_workers work slots.
        self._connection_slots = threading.BoundedSemaphore(
            max_workers + HEALTH_PROBE_CONNECTION_RESERVE
        )
        self._work_slots = threading.BoundedSemaphore(max_workers)
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


class AsyncLoopRunner:
    """Own one event loop and safely submit async work from HTTP worker threads."""

    def __init__(self) -> None:
        self._loop = asyncio.new_event_loop()
        self._started = threading.Event()
        self._closed = False
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()
        self._started.wait()

    def _run(self) -> None:
        asyncio.set_event_loop(self._loop)
        self._started.set()
        try:
            self._loop.run_forever()
        finally:
            self._loop.close()

    def run(self, awaitable: Any) -> Any:
        if self._closed:
            raise RuntimeError("async loop runner is closed")
        return asyncio.run_coroutine_threadsafe(awaitable, self._loop).result()

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        self._loop.call_soon_threadsafe(self._loop.stop)
        self._thread.join(timeout=5)


class AgentAdapter(Protocol):
    """Framework adapter used by the transport-neutral runtime."""

    def invoke(self, messages: list[dict[str, Any]], config: dict[str, Any]) -> str:
        """Run one agent turn and return the final text response."""


def load_config() -> dict[str, Any]:
    path = Path(os.environ.get("AIRUNWAY_AGENT_CONFIG", "/etc/airunway/agent.json"))
    if not path.exists():
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"{path} must contain a JSON object")
    return data


def configured_model() -> str:
    for key in ("OPENAI_MODEL", "ANTHROPIC_MODEL", "AZURE_OPENAI_MODEL"):
        value = os.environ.get(key)
        if value:
            return value
    raise ValueError("no model binding was injected")


def completion_response(content: str) -> dict[str, Any]:
    return {
        "id": f"chatcmpl-{uuid.uuid4().hex}",
        "object": "chat.completion",
        "created": int(time.time()),
        "model": configured_model(),
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": content},
                "finish_reason": "stop",
            }
        ],
    }


def validate_messages(value: Any) -> list[dict[str, Any]]:
    if not isinstance(value, list) or not value:
        raise ValueError("messages must be a non-empty array")
    messages: list[dict[str, Any]] = []
    for item in value:
        if not isinstance(item, dict):
            raise ValueError("each message must be an object")
        role = item.get("role")
        content = item.get("content")
        if role not in {"system", "user", "assistant"}:
            raise ValueError("each message needs a supported role")
        if not isinstance(content, str):
            raise ValueError("text message content must be a string")
        messages.append({"role": role, "content": content})
    return messages


def job_messages(config: dict[str, Any]) -> list[dict[str, str]]:
    task = config.get("task") or config.get("prompt")
    if not isinstance(task, str) or not task.strip():
        raise ValueError("job lifecycle requires spec.config.task or spec.config.prompt")
    messages: list[dict[str, str]] = []
    system_prompt = config.get("systemPrompt")
    if isinstance(system_prompt, str) and system_prompt.strip():
        messages.append({"role": "system", "content": system_prompt})
    messages.append({"role": "user", "content": task})
    return messages


def run_job(adapter: AgentAdapter, config: dict[str, Any]) -> None:
    result = adapter.invoke(job_messages(config), config)
    print(result, flush=True)


def handler_for(
    adapter: AgentAdapter,
    config: dict[str, Any],
    access_token: str,
) -> type[BaseHTTPRequestHandler]:
    class Handler(HeaderDeadlineHandler):
        server_version = "airunway-agent/1"

        def _json(self, status: HTTPStatus, body: dict[str, Any]) -> None:
            payload = json.dumps(body, separators=(",", ":")).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def _authenticated(self) -> bool:
            authorization = self.headers.get("Authorization", "")
            prefix = "Bearer "
            scheme = authorization[: len(prefix)]
            if scheme.lower() != prefix.lower() or not hmac.compare_digest(
                authorization[len(prefix) :], access_token
            ):
                self._json(
                    HTTPStatus.UNAUTHORIZED,
                    {"error": {"message": "valid bearer authentication is required"}},
                )
                return False
            return True

        def _acquire_work_slot(self) -> bool:
            if self.server.acquire_work_slot():
                return True
            self._json(
                HTTPStatus.SERVICE_UNAVAILABLE,
                {"error": {"message": "server is at capacity"}},
            )
            return False

        def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            if self.path in {"/healthz", "/readyz"}:
                self._json(HTTPStatus.OK, {"status": "ok"})
                return
            if self.path == "/v1/models":
                if not self._authenticated() or not self._acquire_work_slot():
                    return
                try:
                    model = configured_model()
                    self._json(
                        HTTPStatus.OK,
                        {
                            "object": "list",
                            "data": [{"id": model, "object": "model"}],
                        },
                    )
                finally:
                    self.server.release_work_slot()
                return
            self._json(HTTPStatus.NOT_FOUND, {"error": {"message": "not found"}})

        def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            if self.path != "/v1/chat/completions":
                self._json(HTTPStatus.NOT_FOUND, {"error": {"message": "not found"}})
                return
            if not self._authenticated() or not self._acquire_work_slot():
                return
            try:
                try:
                    if self.headers.get("Transfer-Encoding"):
                        raise ValueError("chunked request bodies are not supported")
                    length = int(self.headers.get("Content-Length", "0"))
                    if length <= 0 or length > 4 * 1024 * 1024:
                        raise ValueError("request body must be between 1 byte and 4 MiB")
                    payload = read_exact_body(
                        self.rfile,
                        self.connection,
                        length,
                        self.server.request_timeout,
                    )
                    body = json.loads(payload)
                    if not isinstance(body, dict):
                        raise ValueError("request body must be a JSON object")
                    if body.get("stream") is True:
                        raise ValueError(
                            "streaming responses are not supported by this image"
                        )
                    messages = validate_messages(body.get("messages"))
                except RequestReadTimeout as exc:
                    self.close_connection = True
                    self._json(
                        HTTPStatus.REQUEST_TIMEOUT,
                        {"error": {"message": str(exc)}},
                    )
                    return
                except (
                    json.JSONDecodeError,
                    UnicodeDecodeError,
                    ValueError,
                    RecursionError,
                ) as exc:
                    self._json(
                        HTTPStatus.BAD_REQUEST, {"error": {"message": str(exc)}}
                    )
                    return

                try:
                    result = adapter.invoke(messages, dict(config))
                    if not isinstance(result, str):
                        raise TypeError("agent adapter returned a non-string response")
                    self._json(HTTPStatus.OK, completion_response(result))
                except Exception as exc:  # framework failures become API failures
                    print(
                        f"agent invocation failed: {exc}", file=sys.stderr, flush=True
                    )
                    self._json(
                        HTTPStatus.INTERNAL_SERVER_ERROR,
                        {"error": {"message": "agent invocation failed"}},
                    )
            finally:
                self.server.release_work_slot()

        def log_message(self, fmt: str, *args: Any) -> None:
            print(f"{self.address_string()} {fmt % args}", file=sys.stderr, flush=True)

    return Handler


def main(adapter_factory: Callable[[], AgentAdapter]) -> None:
    config = load_config()
    adapter = adapter_factory()
    try:
        if os.environ.get("AIRUNWAY_AGENT_MODE", "server") == "job":
            run_job(adapter, config)
            return
        port = int(os.environ.get("AIRUNWAY_AGENT_PORT", "8080"))
        access_token = os.environ.get("AIRUNWAY_AGENT_API_KEY")
        if not access_token:
            raise ValueError("AIRUNWAY_AGENT_API_KEY is required in server mode")
        server = BoundedThreadingHTTPServer(
            ("0.0.0.0", port), handler_for(adapter, config, access_token)
        )
        print(f"AI Runway agent listening on :{port}", flush=True)
        try:
            server.serve_forever()
        finally:
            server.server_close()
    finally:
        close = getattr(adapter, "close", None)
        if callable(close):
            close()

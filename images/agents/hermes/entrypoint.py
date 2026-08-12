"""Configure Hermes for AI Runway and expose only its OpenAI-compatible API."""

from __future__ import annotations

import hmac
import http.client
import json
import os
import secrets
import signal
import subprocess
import sys
import threading
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


STATE_DIR = Path("/tmp/airunway-hermes")
DEFAULT_INTERNAL_PORT = 8642
MAX_REQUEST_BYTES = 4 * 1024 * 1024


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


class ProxyHandler(BaseHTTPRequestHandler):
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
        if not authorization.startswith(prefix) or not hmac.compare_digest(
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
        return self.rfile.read(length)

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
        if not health_request and not self._authenticated():
            return
        upstream_path = "/health" if self.path in {"/healthz", "/readyz"} else self.path
        try:
            body = self._body()
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
        try:
            connection = http.client.HTTPConnection(
                "127.0.0.1", self.internal_port, timeout=600
            )
            connection.request(self.command, upstream_path, body=body, headers=headers)
            response = connection.getresponse()
            payload = response.read()
            self.send_response(response.status)
            for key, value in response.getheaders():
                if key.lower() not in {"connection", "content-length", "transfer-encoding"}:
                    self.send_header(key, value)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
        except OSError:
            payload = b'{"error":{"message":"gateway is not ready"}}'
            self.send_response(HTTPStatus.SERVICE_UNAVAILABLE)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self._proxy()

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self._proxy()

    def log_message(self, fmt: str, *args: Any) -> None:
        print(f"{self.address_string()} {fmt % args}", file=sys.stderr, flush=True)


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

    gateway = subprocess.Popen(["hermes", "gateway", "run", "--no-supervise"], env=os.environ)
    ProxyHandler.internal_key = internal_key
    ProxyHandler.access_token = required("AIRUNWAY_AGENT_API_KEY")
    ProxyHandler.internal_port = internal_port
    server = ThreadingHTTPServer(("0.0.0.0", external_port), ProxyHandler)

    def stop(_signum: int, _frame: Any) -> None:
        gateway.terminate()
        # BaseServer.shutdown must run outside the serve_forever thread.
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    print(f"AI Runway Hermes proxy listening on :{external_port}", flush=True)
    try:
        server.serve_forever()
    finally:
        gateway.terminate()
        gateway.wait(timeout=30)


if __name__ == "__main__":
    main()

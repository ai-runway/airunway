"""Small HTTP/runtime contract shared by AI Runway's Python agent images."""

from __future__ import annotations

import hmac
import json
import os
import sys
import time
import uuid
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Callable, Protocol


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
        if role not in {"system", "user", "assistant", "tool"}:
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
    class Handler(BaseHTTPRequestHandler):
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
            if not authorization.startswith(prefix) or not hmac.compare_digest(
                authorization[len(prefix) :], access_token
            ):
                self._json(
                    HTTPStatus.UNAUTHORIZED,
                    {"error": {"message": "valid bearer authentication is required"}},
                )
                return False
            return True

        def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            if self.path in {"/healthz", "/readyz"}:
                self._json(HTTPStatus.OK, {"status": "ok"})
                return
            if self.path == "/v1/models":
                if not self._authenticated():
                    return
                model = configured_model()
                self._json(
                    HTTPStatus.OK,
                    {"object": "list", "data": [{"id": model, "object": "model"}]},
                )
                return
            self._json(HTTPStatus.NOT_FOUND, {"error": {"message": "not found"}})

        def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            if self.path != "/v1/chat/completions":
                self._json(HTTPStatus.NOT_FOUND, {"error": {"message": "not found"}})
                return
            if not self._authenticated():
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if length <= 0 or length > 4 * 1024 * 1024:
                    raise ValueError("request body must be between 1 byte and 4 MiB")
                body = json.loads(self.rfile.read(length))
                if not isinstance(body, dict):
                    raise ValueError("request body must be a JSON object")
                if body.get("stream") is True:
                    raise ValueError("streaming responses are not supported by this image")
                messages = validate_messages(body.get("messages"))
                result = adapter.invoke(messages, dict(config))
                self._json(HTTPStatus.OK, completion_response(result))
            except (json.JSONDecodeError, ValueError) as exc:
                self._json(HTTPStatus.BAD_REQUEST, {"error": {"message": str(exc)}})
            except Exception as exc:  # framework failures become API failures
                print(f"agent invocation failed: {exc}", file=sys.stderr, flush=True)
                self._json(
                    HTTPStatus.INTERNAL_SERVER_ERROR,
                    {"error": {"message": "agent invocation failed"}},
                )

        def log_message(self, fmt: str, *args: Any) -> None:
            print(f"{self.address_string()} {fmt % args}", file=sys.stderr, flush=True)

    return Handler


def main(adapter_factory: Callable[[], AgentAdapter]) -> None:
    config = load_config()
    adapter = adapter_factory()
    if os.environ.get("AIRUNWAY_AGENT_MODE", "server") == "job":
        run_job(adapter, config)
        return
    port = int(os.environ.get("AIRUNWAY_AGENT_PORT", "8080"))
    access_token = os.environ.get("AIRUNWAY_AGENT_API_KEY")
    if not access_token:
        raise ValueError("AIRUNWAY_AGENT_API_KEY is required in server mode")
    server = ThreadingHTTPServer(
        ("0.0.0.0", port), handler_for(adapter, config, access_token)
    )
    print(f"AI Runway agent listening on :{port}", flush=True)
    server.serve_forever()

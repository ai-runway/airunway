"""Minimal local OpenAI-compatible endpoint for agent image smoke tests."""

from __future__ import annotations

import hmac
import json
import os
import sys
import time
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any


class Handler(BaseHTTPRequestHandler):
    expected_api_key = ""

    def _json(
        self,
        status: HTTPStatus,
        body: dict[str, Any],
        headers: dict[str, str] | None = None,
    ) -> None:
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(payload)

    def _authenticated(self) -> bool:
        expected = f"Bearer {self.expected_api_key}"
        if hmac.compare_digest(self.headers.get("Authorization", ""), expected):
            return True
        self._json(
            HTTPStatus.UNAUTHORIZED,
            {"error": {"message": "valid model credential is required"}},
            {"WWW-Authenticate": "Bearer"},
        )
        return False

    def _sse(self, events: list[dict[str, Any]]) -> None:
        payload = b"".join(
            b"data: " + json.dumps(event).encode() + b"\n\n" for event in events
        ) + b"data: [DONE]\n\n"
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        if self.path == "/healthz":
            self._json(HTTPStatus.OK, {"status": "ok"})
            return
        if self.path == "/v1/models":
            if not self._authenticated():
                return
            self._json(
                HTTPStatus.OK,
                {"object": "list", "data": [{"id": "smoke-model", "object": "model"}]},
            )
            return
        self._json(HTTPStatus.NOT_FOUND, {"error": {"message": "not found"}})

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        if not self._authenticated():
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length)) if length else {}
        model = body.get("model", "smoke-model") if isinstance(body, dict) else "smoke-model"
        response_content = "smoke response"
        if isinstance(body, dict):
            messages = body.get("messages")
            if isinstance(messages, list):
                message_text = "\n".join(
                    str(message.get("content", ""))
                    for message in messages
                    if isinstance(message, dict)
                )
                if "airunway-isolation-second" in message_text:
                    response_content = (
                        "state leaked"
                        if "airunway-isolation-first" in message_text
                        else "state isolated"
                    )
        if self.path == "/v1/chat/completions":
            if isinstance(body, dict) and body.get("stream") is True:
                self._sse(
                    [
                        {
                            "id": "chatcmpl-smoke",
                            "object": "chat.completion.chunk",
                            "created": int(time.time()),
                            "model": model,
                            "choices": [
                                {
                                    "index": 0,
                                    "delta": {
                                        "role": "assistant",
                                        "content": response_content,
                                    },
                                    "finish_reason": None,
                                }
                            ],
                        },
                        {
                            "id": "chatcmpl-smoke",
                            "object": "chat.completion.chunk",
                            "created": int(time.time()),
                            "model": model,
                            "choices": [
                                {"index": 0, "delta": {}, "finish_reason": "stop"}
                            ],
                        },
                    ]
                )
                return
            self._json(
                HTTPStatus.OK,
                {
                    "id": "chatcmpl-smoke",
                    "object": "chat.completion",
                    "created": int(time.time()),
                    "model": model,
                    "choices": [
                        {
                            "index": 0,
                            "message": {"role": "assistant", "content": response_content},
                            "finish_reason": "stop",
                        }
                    ],
                    "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                },
            )
            return
        if self.path == "/v1/responses":
            self._json(
                HTTPStatus.OK,
                {
                    "id": "resp-smoke",
                    "object": "response",
                    "created_at": int(time.time()),
                    "model": model,
                    "status": "completed",
                    "output": [
                        {
                            "type": "message",
                            "id": "msg-smoke",
                            "role": "assistant",
                            "status": "completed",
                            "content": [{"type": "output_text", "text": "smoke response", "annotations": []}],
                        }
                    ],
                    "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
                },
            )
            return
        self._json(HTTPStatus.NOT_FOUND, {"error": {"message": "not found"}})

    def log_message(self, _fmt: str, *_args: Any) -> None:
        return


def main() -> None:
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 18081
    expected_api_key = os.environ.get("AIRUNWAY_MOCK_API_KEY")
    if not expected_api_key:
        raise ValueError("AIRUNWAY_MOCK_API_KEY is required")
    Handler.expected_api_key = expected_api_key
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()

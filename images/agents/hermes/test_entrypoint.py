from __future__ import annotations

import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

import entrypoint


class _UpstreamHandler(BaseHTTPRequestHandler):
    authorization = ""

    def _reply(self) -> None:
        type(self).authorization = self.headers.get("Authorization", "")
        payload = b'{"status":"ok"}'
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self._reply()

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self._reply()

    def log_message(self, _fmt: str, *_args: object) -> None:
        return


class HermesEntrypointTest(unittest.TestCase):
    def test_internal_port_never_collides_with_external_port(self) -> None:
        self.assertEqual(entrypoint.internal_port_for(8080), 8642)
        self.assertEqual(entrypoint.internal_port_for(8642), 8643)

    def test_runtime_config_removes_api_server_tools(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir, patch.object(
            entrypoint, "STATE_DIR", Path(tmpdir)
        ), patch.dict(
            os.environ,
            {
                "OPENAI_MODEL": "test-model",
                "OPENAI_BASE_URL": "http://models.invalid/v1",
            },
            clear=True,
        ):
            entrypoint.write_runtime_config({}, "internal-key", 8643)
            config = (Path(tmpdir) / "config.yaml").read_text(encoding="utf-8")
        self.assertIn("platform_toolsets:\n  api_server: []\n", config)
        self.assertIn("port: 8643\n", config)

    def test_proxy_authenticates_and_bounds_the_public_surface(self) -> None:
        upstream = ThreadingHTTPServer(("127.0.0.1", 0), _UpstreamHandler)
        entrypoint.ProxyHandler.internal_key = "internal-key"
        entrypoint.ProxyHandler.access_token = "external-key"
        entrypoint.ProxyHandler.internal_port = upstream.server_port
        proxy = ThreadingHTTPServer(("127.0.0.1", 0), entrypoint.ProxyHandler)
        threads = [
            threading.Thread(target=upstream.serve_forever, daemon=True),
            threading.Thread(target=proxy.serve_forever, daemon=True),
        ]
        for thread in threads:
            thread.start()
        base_url = f"http://127.0.0.1:{proxy.server_port}"
        try:
            with urllib.request.urlopen(base_url + "/healthz") as response:
                self.assertEqual(response.status, 200)

            with self.assertRaises(urllib.error.HTTPError) as raised:
                urllib.request.urlopen(base_url + "/v1/models")
            self.assertEqual(raised.exception.code, 401)
            raised.exception.close()

            authenticated = urllib.request.Request(
                base_url + "/v1/models",
                headers={"Authorization": "Bearer external-key"},
            )
            with urllib.request.urlopen(authenticated) as response:
                self.assertEqual(json.load(response), {"status": "ok"})
            self.assertEqual(_UpstreamHandler.authorization, "Bearer internal-key")

            oversized = urllib.request.Request(
                base_url + "/v1/chat/completions",
                data=b"x",
                headers={
                    "Authorization": "Bearer external-key",
                    "Content-Length": str(entrypoint.MAX_REQUEST_BYTES + 1),
                },
                method="POST",
            )
            with self.assertRaises(urllib.error.HTTPError) as raised:
                urllib.request.urlopen(oversized)
            self.assertEqual(raised.exception.code, 413)
            raised.exception.close()
        finally:
            proxy.shutdown()
            upstream.shutdown()
            proxy.server_close()
            upstream.server_close()
            for thread in threads:
                thread.join(timeout=5)


if __name__ == "__main__":
    unittest.main()

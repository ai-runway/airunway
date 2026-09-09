from __future__ import annotations

import json
import os
import socket
import subprocess
import tempfile
import threading
import time
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


class _ExitedGateway:
    def __init__(self, return_code: int) -> None:
        self.return_code = return_code

    def wait(self, timeout: float | None = None) -> int:
        # Give the proxy thread time to enter serve_forever before simulating
        # an immediately failing child process.
        time.sleep(0.05)
        return self.return_code


class _StubbornGateway:
    def __init__(self) -> None:
        self.killed = False

    def wait(self, timeout: float | None = None) -> int:
        if self.killed:
            return -9
        raise subprocess.TimeoutExpired("hermes", timeout)

    def poll(self) -> int | None:
        return -9 if self.killed else None

    def terminate(self) -> None:
        return

    def kill(self) -> None:
        self.killed = True


class HermesEntrypointTest(unittest.TestCase):
    def test_body_deadline(self) -> None:
        class DripReader:
            def read1(self, _length: int) -> bytes:
                time.sleep(0.04)
                return b"x"

        class FakeConnection:
            timeout = None

            def gettimeout(self):
                return self.timeout

            def settimeout(self, value):
                self.timeout = value

        started = time.monotonic()
        with self.assertRaisesRegex(
            entrypoint.RequestReadTimeout, "request body read timed out"
        ):
            entrypoint.read_exact_body(
                DripReader(), FakeConnection(), length=10, timeout=0.1
            )
        self.assertLess(time.monotonic() - started, 0.2)

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
        proxy = entrypoint.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0), entrypoint.ProxyHandler
        )
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

    def test_bearer_scheme_is_case_insensitive_but_token_is_exact(self) -> None:
        upstream = ThreadingHTTPServer(("127.0.0.1", 0), _UpstreamHandler)
        entrypoint.ProxyHandler.internal_key = "internal-key"
        entrypoint.ProxyHandler.access_token = "access-token"
        entrypoint.ProxyHandler.internal_port = upstream.server_port
        proxy = entrypoint.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0), entrypoint.ProxyHandler
        )
        threads = [
            threading.Thread(target=upstream.serve_forever, daemon=True),
            threading.Thread(target=proxy.serve_forever, daemon=True),
        ]
        for thread in threads:
            thread.start()
        url = f"http://127.0.0.1:{proxy.server_port}/v1/models"
        try:
            for authorization in (
                "bearer access-token",
                "bEaReR access-token",
            ):
                request = urllib.request.Request(
                    url, headers={"Authorization": authorization}
                )
                with urllib.request.urlopen(request) as response:
                    self.assertEqual(response.status, HTTPStatus.OK)

            for authorization in (
                "Basic access-token",
                "Bearer wrong-token",
            ):
                request = urllib.request.Request(
                    url, headers={"Authorization": authorization}
                )
                with self.assertRaises(urllib.error.HTTPError) as raised:
                    urllib.request.urlopen(request)
                self.assertEqual(raised.exception.code, HTTPStatus.UNAUTHORIZED)
                raised.exception.close()
        finally:
            proxy.shutdown()
            upstream.shutdown()
            proxy.server_close()
            upstream.server_close()
            for thread in threads:
                thread.join(timeout=5)

    def test_proxy_keeps_health_available_at_work_capacity(self) -> None:
        upstream = ThreadingHTTPServer(("127.0.0.1", 0), _UpstreamHandler)
        entrypoint.ProxyHandler.internal_key = "internal-key"
        entrypoint.ProxyHandler.access_token = "external-key"
        entrypoint.ProxyHandler.internal_port = upstream.server_port
        proxy = entrypoint.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0), entrypoint.ProxyHandler, max_workers=1
        )
        self.assertTrue(proxy._work_slots.acquire(blocking=False))
        threads = [
            threading.Thread(target=upstream.serve_forever, daemon=True),
            threading.Thread(target=proxy.serve_forever, daemon=True),
        ]
        for thread in threads:
            thread.start()
        try:
            with urllib.request.urlopen(
                f"http://127.0.0.1:{proxy.server_port}/healthz"
            ) as response:
                self.assertEqual(response.status, HTTPStatus.OK)

            request = urllib.request.Request(
                f"http://127.0.0.1:{proxy.server_port}/v1/models",
                headers={"Authorization": "Bearer external-key"},
            )
            with self.assertRaises(urllib.error.HTTPError) as raised:
                urllib.request.urlopen(request)
            self.assertEqual(raised.exception.code, HTTPStatus.SERVICE_UNAVAILABLE)
            raised.exception.close()
        finally:
            proxy._work_slots.release()
            proxy.shutdown()
            upstream.shutdown()
            proxy.server_close()
            upstream.server_close()
            for thread in threads:
                thread.join(timeout=5)

    def test_proxy_bounds_health_requests_without_starving_work(self) -> None:
        health_count = 0
        health_count_lock = threading.Lock()
        health_capacity_reached = threading.Event()
        release_health = threading.Event()

        class BlockingHealthHandler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
                nonlocal health_count
                if self.path == "/health":
                    with health_count_lock:
                        health_count += 1
                        if health_count == entrypoint.HEALTH_PROBE_CONNECTION_RESERVE:
                            health_capacity_reached.set()
                    release_health.wait(timeout=2)
                payload = b'{"status":"ok"}'
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, _fmt: str, *_args: object) -> None:
                return

        upstream = ThreadingHTTPServer(("127.0.0.1", 0), BlockingHealthHandler)
        entrypoint.ProxyHandler.internal_key = "internal-key"
        entrypoint.ProxyHandler.access_token = "external-key"
        entrypoint.ProxyHandler.internal_port = upstream.server_port
        proxy = entrypoint.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0), entrypoint.ProxyHandler, max_workers=1
        )
        server_threads = [
            threading.Thread(target=upstream.serve_forever, daemon=True),
            threading.Thread(target=proxy.serve_forever, daemon=True),
        ]
        for thread in server_threads:
            thread.start()

        health_statuses: list[int] = []
        health_errors: list[BaseException] = []

        def call_health() -> None:
            try:
                with urllib.request.urlopen(
                    f"http://127.0.0.1:{proxy.server_port}/healthz", timeout=2
                ) as response:
                    health_statuses.append(response.status)
            except BaseException as exc:
                health_errors.append(exc)

        health_threads = [
            threading.Thread(target=call_health, daemon=True)
            for _ in range(entrypoint.HEALTH_PROBE_CONNECTION_RESERVE)
        ]
        for thread in health_threads:
            thread.start()
        try:
            self.assertTrue(health_capacity_reached.wait(timeout=1))

            with self.assertRaises(urllib.error.HTTPError) as raised:
                urllib.request.urlopen(
                    f"http://127.0.0.1:{proxy.server_port}/readyz", timeout=1
                )
            self.assertEqual(raised.exception.code, HTTPStatus.SERVICE_UNAVAILABLE)
            raised.exception.close()

            work_request = urllib.request.Request(
                f"http://127.0.0.1:{proxy.server_port}/v1/models",
                headers={"Authorization": "Bearer external-key"},
            )
            with urllib.request.urlopen(work_request, timeout=1) as response:
                self.assertEqual(response.status, HTTPStatus.OK)

            release_health.set()
            for thread in health_threads:
                thread.join(timeout=2)
            self.assertTrue(all(not thread.is_alive() for thread in health_threads))
            self.assertEqual(health_errors, [])
            self.assertEqual(
                health_statuses,
                [HTTPStatus.OK] * entrypoint.HEALTH_PROBE_CONNECTION_RESERVE,
            )
        finally:
            release_health.set()
            for thread in health_threads:
                thread.join(timeout=2)
            proxy.shutdown()
            upstream.shutdown()
            proxy.server_close()
            upstream.server_close()
            for thread in server_threads:
                thread.join(timeout=5)

    def test_proxy_times_out_stalled_health_upstream(self) -> None:
        release_health = threading.Event()

        class StalledHealthHandler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
                release_health.wait(timeout=2)

            def log_message(self, _fmt: str, *_args: object) -> None:
                return

        upstream = ThreadingHTTPServer(("127.0.0.1", 0), StalledHealthHandler)
        entrypoint.ProxyHandler.internal_key = "internal-key"
        entrypoint.ProxyHandler.access_token = "external-key"
        entrypoint.ProxyHandler.internal_port = upstream.server_port
        proxy = entrypoint.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0), entrypoint.ProxyHandler
        )
        threads = [
            threading.Thread(target=upstream.serve_forever, daemon=True),
            threading.Thread(target=proxy.serve_forever, daemon=True),
        ]
        for thread in threads:
            thread.start()
        started = time.monotonic()
        try:
            with patch.object(entrypoint, "HEALTH_UPSTREAM_TIMEOUT_SECONDS", 0.1):
                with self.assertRaises(urllib.error.HTTPError) as raised:
                    urllib.request.urlopen(
                        f"http://127.0.0.1:{proxy.server_port}/healthz", timeout=1
                    )
            self.assertEqual(raised.exception.code, HTTPStatus.SERVICE_UNAVAILABLE)
            raised.exception.close()
            self.assertLess(time.monotonic() - started, 1)
        finally:
            release_health.set()
            proxy.shutdown()
            upstream.shutdown()
            proxy.server_close()
            upstream.server_close()
            for thread in threads:
                thread.join(timeout=5)

    def test_proxy_times_out_incomplete_request_bodies(self) -> None:
        entrypoint.ProxyHandler.internal_key = "internal-key"
        entrypoint.ProxyHandler.access_token = "external-key"
        proxy = entrypoint.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            entrypoint.ProxyHandler,
            request_timeout=0.1,
        )
        thread = threading.Thread(target=proxy.serve_forever, daemon=True)
        thread.start()
        try:
            with socket.create_connection(
                ("127.0.0.1", proxy.server_port), timeout=2
            ) as client:
                client.sendall(
                    b"POST /v1/chat/completions HTTP/1.1\r\n"
                    b"Host: localhost\r\n"
                    b"Authorization: Bearer external-key\r\n"
                    b"Content-Length: 10\r\n\r\n"
                    b"x"
                )
                chunks = []
                while chunk := client.recv(4096):
                    chunks.append(chunk)
            response = b"".join(chunks)
            self.assertIn(b" 408 ", response)
            self.assertIn(b"request body read timed out", response)
        finally:
            proxy.shutdown()
            proxy.server_close()
            thread.join(timeout=5)

    def test_proxy_releases_slow_header_clients_at_deadline(self) -> None:
        proxy = entrypoint.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            entrypoint.ProxyHandler,
            max_workers=1,
            request_timeout=0.15,
        )
        server_thread = threading.Thread(target=proxy.serve_forever, daemon=True)
        server_thread.start()
        client = socket.create_connection(("127.0.0.1", proxy.server_port), timeout=2)
        stop_dripping = threading.Event()
        held_reserve_slots = []
        for _ in range(entrypoint.HEALTH_PROBE_CONNECTION_RESERVE):
            self.assertTrue(proxy._connection_slots.acquire(blocking=False))
            held_reserve_slots.append(True)

        def drip_header() -> None:
            client.sendall(b"GET /healthz HTTP/1.1\r\nX-Slow: ")
            while not stop_dripping.wait(0.03):
                try:
                    client.sendall(b"x")
                except OSError:
                    return

        drip_thread = threading.Thread(target=drip_header, daemon=True)
        drip_thread.start()
        acquired = False
        try:
            occupied = False
            deadline = time.monotonic() + 0.5
            while time.monotonic() < deadline:
                if proxy._connection_slots.acquire(blocking=False):
                    proxy._connection_slots.release()
                    time.sleep(0.01)
                    continue
                occupied = True
                break
            self.assertTrue(occupied, "slow header client never occupied a worker slot")

            deadline = time.monotonic() + 0.75
            while time.monotonic() < deadline:
                acquired = proxy._connection_slots.acquire(blocking=False)
                if acquired:
                    break
                time.sleep(0.01)
            self.assertTrue(acquired, "slow header client retained the only worker slot")
        finally:
            if acquired:
                proxy._connection_slots.release()
            for _ in held_reserve_slots:
                proxy._connection_slots.release()
            stop_dripping.set()
            client.close()
            drip_thread.join(timeout=2)
            proxy.shutdown()
            proxy.server_close()
            server_thread.join(timeout=5)

    def test_proxy_stops_when_gateway_child_exits(self) -> None:
        proxy = ThreadingHTTPServer(("127.0.0.1", 0), _UpstreamHandler)
        return_code = entrypoint.serve_until_gateway_exit(_ExitedGateway(7), proxy)
        self.assertEqual(return_code, 7)
        self.assertEqual(proxy.socket.fileno(), -1)

    def test_signal_refuses_new_traffic_while_active_request_drains(self) -> None:
        request_started = threading.Event()
        release_request = threading.Event()
        gateway_waiting = threading.Event()
        gateway_terminated = threading.Event()
        release_gateway = threading.Event()
        stopping = threading.Event()
        registered_handlers = {}

        def register_handler(signum: int, handler: object) -> None:
            registered_handlers[signum] = handler

        with patch.object(
            entrypoint.signal, "signal", side_effect=register_handler
        ):
            entrypoint.install_shutdown_handlers(stopping)

        class DrainingHandler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
                request_started.set()
                release_request.wait(timeout=2)
                payload = b"ok"
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, _fmt: str, *_args: object) -> None:
                return

        class GracefulGateway:
            def poll(self) -> int | None:
                return None

            def terminate(self) -> None:
                self_outer.assertEqual(proxy.socket.fileno(), -1)
                gateway_terminated.set()

            def wait(self, timeout: float | None = None) -> int:
                if not stopping.is_set():
                    time.sleep(min(timeout or 0.01, 0.01))
                    raise subprocess.TimeoutExpired("hermes", timeout)
                gateway_waiting.set()
                if not release_gateway.wait(timeout=2):
                    raise subprocess.TimeoutExpired("hermes", timeout)
                return 0

            def kill(self) -> None:
                release_gateway.set()

        proxy = entrypoint.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0), DrainingHandler
        )
        self_outer = self
        proxy_port = proxy.server_port
        serve_result: list[int] = []
        active_status: list[int] = []
        active_errors: list[BaseException] = []
        serve_thread = threading.Thread(
            target=lambda: serve_result.append(
                entrypoint.serve_until_gateway_exit(
                    GracefulGateway(), proxy, stopping, shutdown_timeout=1
                )
            ),
            daemon=True,
        )

        def call_active_request() -> None:
            try:
                with urllib.request.urlopen(
                    f"http://127.0.0.1:{proxy_port}/active", timeout=2
                ) as response:
                    active_status.append(response.status)
            except BaseException as exc:
                active_errors.append(exc)

        active_thread = threading.Thread(target=call_active_request, daemon=True)
        serve_thread.start()
        active_thread.start()
        try:
            self.assertTrue(request_started.wait(timeout=1))
            registered_handlers[entrypoint.signal.SIGTERM](
                entrypoint.signal.SIGTERM, None
            )
            self.assertTrue(gateway_terminated.wait(timeout=2))
            self.assertTrue(gateway_waiting.wait(timeout=2))

            self.assertEqual(proxy.socket.fileno(), -1)
            with self.assertRaises(OSError):
                socket.create_connection(("127.0.0.1", proxy_port), timeout=0.2)

            release_request.set()
            active_thread.join(timeout=2)
            self.assertFalse(active_thread.is_alive())
            self.assertEqual(active_errors, [])
            self.assertEqual(active_status, [HTTPStatus.OK])

            release_gateway.set()
            serve_thread.join(timeout=2)
            self.assertFalse(serve_thread.is_alive())
            self.assertEqual(serve_result, [0])
        finally:
            release_request.set()
            release_gateway.set()
            stopping.set()
            active_thread.join(timeout=2)
            serve_thread.join(timeout=2)
            proxy.server_close()

    def test_proxy_kills_gateway_that_ignores_shutdown(self) -> None:
        proxy = ThreadingHTTPServer(("127.0.0.1", 0), _UpstreamHandler)
        gateway = _StubbornGateway()
        stopping = threading.Event()
        stopping.set()
        return_code = entrypoint.serve_until_gateway_exit(
            gateway,
            proxy,
            stopping,
            shutdown_timeout=0.01,
            kill_timeout=0.01,
        )
        self.assertEqual(return_code, -9)
        self.assertTrue(gateway.killed)
        self.assertEqual(proxy.socket.fileno(), -1)


if __name__ == "__main__":
    unittest.main()

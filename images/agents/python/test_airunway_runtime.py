from __future__ import annotations

import asyncio
import json
import os
import socket
import threading
import time
import unittest
import urllib.error
import urllib.request
from http import HTTPStatus
from unittest.mock import patch

import airunway_runtime


class RuntimeContractTest(unittest.TestCase):
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
            airunway_runtime.RequestReadTimeout, "request body read timed out"
        ):
            airunway_runtime.read_exact_body(
                DripReader(), FakeConnection(), length=10, timeout=0.1
            )
        self.assertLess(time.monotonic() - started, 0.2)

    def test_model_precedence_and_completion_shape(self) -> None:
        with patch.dict(os.environ, {"OPENAI_MODEL": "demo-model"}, clear=True):
            response = airunway_runtime.completion_response("hello")
        self.assertEqual(response["model"], "demo-model")
        self.assertEqual(response["choices"][0]["message"], {"role": "assistant", "content": "hello"})

    def test_messages_are_strict_text_chat_messages(self) -> None:
        messages = [{"role": "user", "content": "hello"}]
        self.assertEqual(airunway_runtime.validate_messages(messages), messages)
        with self.assertRaisesRegex(ValueError, "non-empty"):
            airunway_runtime.validate_messages([])
        with self.assertRaisesRegex(ValueError, "supported role"):
            airunway_runtime.validate_messages([{"role": "root", "content": "hello"}])
        with self.assertRaisesRegex(ValueError, "supported role"):
            airunway_runtime.validate_messages([{"role": "tool", "content": "output"}])
        with self.assertRaisesRegex(ValueError, "must be a string"):
            airunway_runtime.validate_messages([{"role": "user", "content": []}])

    def test_job_requires_task_and_includes_system_prompt(self) -> None:
        self.assertEqual(
            airunway_runtime.job_messages({"systemPrompt": "be concise", "task": "summarise"}),
            [
                {"role": "system", "content": "be concise"},
                {"role": "user", "content": "summarise"},
            ],
        )
        with self.assertRaisesRegex(ValueError, "requires spec.config.task"):
            airunway_runtime.job_messages({"systemPrompt": "not a task"})

    def test_http_contract_routes_and_invokes_adapter(self) -> None:
        class EchoAdapter:
            def invoke(self, messages, config):
                self.messages = messages
                self.config = config
                return messages[-1]["content"]

        adapter = EchoAdapter()
        access_token = "test-access-token"
        server = airunway_runtime.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            airunway_runtime.handler_for(
                adapter, {"systemPrompt": "test"}, access_token
            ),
        )
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        base_url = f"http://127.0.0.1:{server.server_port}"
        try:
            with patch.dict(os.environ, {"OPENAI_MODEL": "demo-model"}, clear=True):
                with urllib.request.urlopen(base_url + "/healthz") as response:
                    self.assertEqual(json.load(response), {"status": "ok"})

                with self.assertRaises(urllib.error.HTTPError) as raised:
                    urllib.request.urlopen(base_url + "/v1/models")
                self.assertEqual(raised.exception.code, 401)
                raised.exception.close()

                request = urllib.request.Request(
                    base_url + "/v1/chat/completions",
                    data=json.dumps(
                        {
                            "messages": [{"role": "user", "content": "hello"}],
                        }
                    ).encode(),
                    headers={
                        "Authorization": f"Bearer {access_token}",
                        "Content-Type": "application/json",
                    },
                    method="POST",
                )
                with urllib.request.urlopen(request) as response:
                    completion = json.load(response)
                self.assertEqual(completion["choices"][0]["message"]["content"], "hello")
                self.assertEqual(adapter.config, {"systemPrompt": "test"})

                unauthenticated = urllib.request.Request(
                    base_url + "/v1/chat/completions",
                    data=b'{}',
                    headers={"Content-Type": "application/json"},
                    method="POST",
                )
                with self.assertRaises(urllib.error.HTTPError) as raised:
                    urllib.request.urlopen(unauthenticated)
                self.assertEqual(raised.exception.code, 401)
                raised.exception.close()

                streaming = urllib.request.Request(
                    base_url + "/v1/chat/completions",
                    data=json.dumps(
                        {"messages": [{"role": "user", "content": "hello"}], "stream": True}
                    ).encode(),
                    headers={
                        "Authorization": f"Bearer {access_token}",
                        "Content-Type": "application/json",
                    },
                    method="POST",
                )
                with self.assertRaises(urllib.error.HTTPError) as raised:
                    urllib.request.urlopen(streaming)
                self.assertEqual(raised.exception.code, 400)
                raised.exception.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_bearer_scheme_is_case_insensitive_but_token_is_exact(self) -> None:
        class UnusedAdapter:
            def invoke(self, _messages, _config):
                return "unused"

        server = airunway_runtime.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            airunway_runtime.handler_for(UnusedAdapter(), {}, "access-token"),
        )
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        url = f"http://127.0.0.1:{server.server_port}/v1/models"
        try:
            with patch.dict(os.environ, {"OPENAI_MODEL": "demo-model"}, clear=True):
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
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_adapter_value_error_is_an_internal_failure(self) -> None:
        class FailingAdapter:
            def invoke(self, _messages, _config):
                raise ValueError("internal adapter detail")

        access_token = "test-access-token"
        server = airunway_runtime.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            airunway_runtime.handler_for(FailingAdapter(), {}, access_token),
        )
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            request = urllib.request.Request(
                f"http://127.0.0.1:{server.server_port}/v1/chat/completions",
                data=json.dumps(
                    {"messages": [{"role": "user", "content": "hello"}]}
                ).encode(),
                headers={
                    "Authorization": f"Bearer {access_token}",
                    "Content-Type": "application/json",
                },
                method="POST",
            )
            with self.assertRaises(urllib.error.HTTPError) as raised:
                urllib.request.urlopen(request)
            self.assertEqual(raised.exception.code, HTTPStatus.INTERNAL_SERVER_ERROR)
            self.assertEqual(
                json.load(raised.exception),
                {"error": {"message": "agent invocation failed"}},
            )
            raised.exception.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_recursive_json_is_a_bad_request(self) -> None:
        class UnusedAdapter:
            def invoke(self, _messages, _config):
                return "unused"

        access_token = "test-access-token"
        server = airunway_runtime.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            airunway_runtime.handler_for(UnusedAdapter(), {}, access_token),
        )
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            request = urllib.request.Request(
                f"http://127.0.0.1:{server.server_port}/v1/chat/completions",
                data=b'{"messages":[]}',
                headers={
                    "Authorization": f"Bearer {access_token}",
                    "Content-Type": "application/json",
                },
                method="POST",
            )
            with patch.object(
                airunway_runtime.json,
                "loads",
                side_effect=RecursionError("maximum JSON nesting exceeded"),
            ):
                with self.assertRaises(urllib.error.HTTPError) as raised:
                    urllib.request.urlopen(request)
            self.assertEqual(raised.exception.code, HTTPStatus.BAD_REQUEST)
            self.assertEqual(
                json.load(raised.exception),
                {"error": {"message": "maximum JSON nesting exceeded"}},
            )
            raised.exception.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_bounded_server_keeps_health_available_at_work_capacity(self) -> None:
        class UnusedAdapter:
            def invoke(self, _messages, _config):
                return "unused"

        server = airunway_runtime.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            airunway_runtime.handler_for(UnusedAdapter(), {}, "token"),
            max_workers=1,
        )
        self.assertTrue(server._work_slots.acquire(blocking=False))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with urllib.request.urlopen(
                f"http://127.0.0.1:{server.server_port}/healthz"
            ) as response:
                self.assertEqual(json.load(response), {"status": "ok"})

            request = urllib.request.Request(
                f"http://127.0.0.1:{server.server_port}/v1/models",
                headers={"Authorization": "Bearer token"},
            )
            with self.assertRaises(urllib.error.HTTPError) as raised:
                urllib.request.urlopen(request)
            self.assertEqual(raised.exception.code, HTTPStatus.SERVICE_UNAVAILABLE)
            raised.exception.close()
        finally:
            server._work_slots.release()
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_bounded_server_times_out_incomplete_request_bodies(self) -> None:
        class UnusedAdapter:
            def invoke(self, _messages, _config):
                return "unused"

        server = airunway_runtime.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            airunway_runtime.handler_for(UnusedAdapter(), {}, "token"),
            request_timeout=0.1,
        )
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with socket.create_connection(
                ("127.0.0.1", server.server_port), timeout=2
            ) as client:
                client.sendall(
                    b"POST /v1/chat/completions HTTP/1.1\r\n"
                    b"Host: localhost\r\n"
                    b"Authorization: Bearer token\r\n"
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
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_bounded_server_releases_slow_header_clients_at_deadline(self) -> None:
        class UnusedAdapter:
            def invoke(self, _messages, _config):
                return "unused"

        server = airunway_runtime.BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            airunway_runtime.handler_for(UnusedAdapter(), {}, "token"),
            max_workers=1,
            request_timeout=0.15,
        )
        server_thread = threading.Thread(target=server.serve_forever, daemon=True)
        server_thread.start()
        client = socket.create_connection(("127.0.0.1", server.server_port), timeout=2)
        stop_dripping = threading.Event()
        held_reserve_slots = []
        for _ in range(airunway_runtime.HEALTH_PROBE_CONNECTION_RESERVE):
            self.assertTrue(server._connection_slots.acquire(blocking=False))
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
                if server._connection_slots.acquire(blocking=False):
                    server._connection_slots.release()
                    time.sleep(0.01)
                    continue
                occupied = True
                break
            self.assertTrue(occupied, "slow header client never occupied a worker slot")

            deadline = time.monotonic() + 0.75
            while time.monotonic() < deadline:
                acquired = server._connection_slots.acquire(blocking=False)
                if acquired:
                    break
                time.sleep(0.01)
            self.assertTrue(acquired, "slow header client retained the only worker slot")
        finally:
            if acquired:
                server._connection_slots.release()
            for _ in held_reserve_slots:
                server._connection_slots.release()
            stop_dripping.set()
            client.close()
            drip_thread.join(timeout=2)
            server.shutdown()
            server.server_close()
            server_thread.join(timeout=5)

    def test_async_loop_runner_reuses_one_loop(self) -> None:
        runner = airunway_runtime.AsyncLoopRunner()

        async def loop_identity() -> int:
            return id(asyncio.get_running_loop())

        try:
            self.assertEqual(runner.run(loop_identity()), runner.run(loop_identity()))
        finally:
            runner.close()

    def test_async_loop_runner_does_not_close_a_running_loop(self) -> None:
        runner = airunway_runtime.AsyncLoopRunner()
        callback_started = threading.Event()
        release_callback = threading.Event()

        def blocking_callback() -> None:
            callback_started.set()
            release_callback.wait()

        runner._loop.call_soon_threadsafe(blocking_callback)
        self.assertTrue(callback_started.wait(timeout=1))
        try:
            with patch.object(runner._thread, "join", return_value=None):
                runner.close()
            self.assertTrue(runner._thread.is_alive())
            self.assertFalse(runner._loop.is_closed())
        finally:
            release_callback.set()
            runner._thread.join(timeout=2)
        self.assertFalse(runner._thread.is_alive())
        self.assertTrue(runner._loop.is_closed())


if __name__ == "__main__":
    unittest.main()

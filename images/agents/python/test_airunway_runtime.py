from __future__ import annotations

import json
import os
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from unittest.mock import patch

import airunway_runtime


class RuntimeContractTest(unittest.TestCase):
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
        server = ThreadingHTTPServer(
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


if __name__ == "__main__":
    unittest.main()

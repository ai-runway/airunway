from __future__ import annotations

import importlib.util
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import patch


class CrewAIAdapterTest(unittest.TestCase):
    def test_system_prompt_reaches_explicit_agents_with_backstories(self) -> None:
        descriptions: list[str] = []

        class FakeAgent:
            def __init__(self, **_kwargs):
                pass

        class FakeTask:
            def __init__(self, **kwargs):
                descriptions.append(kwargs["description"])

        class FakeCrew:
            def __init__(self, **_kwargs):
                pass

            def kickoff(self):
                return "done"

        fake_crewai = types.ModuleType("crewai")
        fake_crewai.Agent = FakeAgent
        fake_crewai.Crew = FakeCrew
        fake_crewai.LLM = object
        fake_crewai.Process = types.SimpleNamespace(sequential=object())
        fake_crewai.Task = FakeTask

        adapter_path = Path(__file__).parents[1] / "crewai" / "adapter.py"
        spec = importlib.util.spec_from_file_location("crewai_adapter_under_test", adapter_path)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        module = importlib.util.module_from_spec(spec)
        with patch.dict(sys.modules, {"crewai": fake_crewai}):
            spec.loader.exec_module(module)

        module._llm = lambda: object()
        result = module.CrewAIAdapter().invoke(
            [{"role": "user", "content": "ordinary user request"}],
            {
                "systemPrompt": "MATRIX-SYSTEM-PROMPT-SENTINEL",
                "agents": [
                    {
                        "role": "analyst",
                        "goal": "analyze",
                        "backstory": "explicit backstory",
                    }
                ],
            },
        )

        self.assertEqual(result, "done")
        self.assertEqual(len(descriptions), 1)
        self.assertIn("SYSTEM: MATRIX-SYSTEM-PROMPT-SENTINEL", descriptions[0])
        self.assertIn("USER: ordinary user request", descriptions[0])


if __name__ == "__main__":
    unittest.main()

"""CrewAI implementation of the AI Runway agent runtime contract."""

from __future__ import annotations

import os
from typing import Any

from airunway_runtime import main
from crewai import Agent, Crew, LLM, Process, Task


def _llm() -> LLM:
    if os.environ.get("ANTHROPIC_MODEL"):
        return LLM(
            model=f"anthropic/{os.environ['ANTHROPIC_MODEL']}",
            base_url=os.environ.get("ANTHROPIC_BASE_URL"),
            api_key=os.environ.get("ANTHROPIC_API_KEY"),
            max_tokens=4096,
        )
    if os.environ.get("AZURE_OPENAI_MODEL"):
        return LLM(
            model=f"azure/{os.environ['AZURE_OPENAI_MODEL']}",
            endpoint=os.environ.get("AZURE_OPENAI_ENDPOINT"),
            api_key=os.environ.get("AZURE_OPENAI_API_KEY"),
            api_version=os.environ.get("AZURE_API_VERSION", "2024-06-01"),
        )
    model = os.environ.get("OPENAI_MODEL")
    if not model:
        raise ValueError("no supported model binding was injected")
    return LLM(
        model=f"openai/{model}",
        base_url=os.environ.get("OPENAI_BASE_URL"),
        api_key=os.environ.get("OPENAI_API_KEY"),
    )


def _conversation(messages: list[dict[str, Any]]) -> str:
    return "\n\n".join(f"{message['role'].upper()}: {message['content']}" for message in messages)


class CrewAIAdapter:
    def invoke(self, messages: list[dict[str, Any]], config: dict[str, Any]) -> str:
        llm = _llm()
        definitions = config.get("agents")
        if not isinstance(definitions, list) or not definitions:
            definitions = [
                {
                    "role": "AI assistant",
                    "goal": "Answer the user's request accurately and completely",
                    "backstory": config.get("systemPrompt", "You are a helpful assistant."),
                }
            ]

        agents: list[Agent] = []
        for index, definition in enumerate(definitions):
            if not isinstance(definition, dict):
                raise ValueError(f"agents[{index}] must be an object")
            agents.append(
                Agent(
                    role=str(definition.get("role") or f"Specialist {index + 1}"),
                    goal=str(definition.get("goal") or "Help complete the user's request"),
                    backstory=str(definition.get("backstory") or config.get("systemPrompt") or ""),
                    llm=llm,
                    verbose=False,
                    allow_delegation=False,
                )
            )

        request = _conversation(messages)
        tasks: list[Task] = []
        for index, agent in enumerate(agents):
            final = index == len(agents) - 1
            description = (
                "Produce the final response to this request, using the prior crew's work as context:\n\n"
                if final
                else "Analyze this request from your specialist role and produce notes for the next crew member:\n\n"
            )
            tasks.append(
                Task(
                    description=description + request,
                    expected_output="A direct final answer." if final else "Useful analysis and evidence.",
                    agent=agent,
                    context=tasks.copy(),
                )
            )

        result = Crew(
            agents=agents,
            tasks=tasks,
            process=Process.sequential,
            verbose=False,
        ).kickoff()
        return str(result)


if __name__ == "__main__":
    main(CrewAIAdapter)

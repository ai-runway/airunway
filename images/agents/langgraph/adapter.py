"""LangGraph implementation of the AI Runway agent runtime contract."""

from __future__ import annotations

import asyncio
import os
from typing import Any

from airunway_runtime import main
from langchain_anthropic import ChatAnthropic
from langchain_core.messages import BaseMessage
from langchain_openai import AzureChatOpenAI, ChatOpenAI
from langgraph.graph import END, START, MessagesState, StateGraph


def _model() -> Any:
    if os.environ.get("ANTHROPIC_MODEL"):
        return ChatAnthropic(
            model=os.environ["ANTHROPIC_MODEL"],
            base_url=os.environ.get("ANTHROPIC_BASE_URL"),
            api_key=os.environ.get("ANTHROPIC_API_KEY"),
        )
    if os.environ.get("AZURE_OPENAI_MODEL"):
        return AzureChatOpenAI(
            azure_deployment=os.environ["AZURE_OPENAI_MODEL"],
            azure_endpoint=os.environ.get("AZURE_OPENAI_ENDPOINT"),
            api_key=os.environ.get("AZURE_OPENAI_API_KEY"),
            api_version=os.environ.get("AZURE_API_VERSION", "2024-06-01"),
        )
    model = os.environ.get("OPENAI_MODEL")
    if not model:
        raise ValueError("no supported model binding was injected")
    return ChatOpenAI(
        model=model,
        base_url=os.environ.get("OPENAI_BASE_URL"),
        api_key=os.environ.get("OPENAI_API_KEY"),
    )


def _content(message: BaseMessage) -> str:
    if isinstance(message.content, str):
        return message.content
    return "".join(
        str(block.get("text", "")) if isinstance(block, dict) else str(block)
        for block in message.content
    )


class LangGraphAdapter:
    def __init__(self) -> None:
        model = _model()

        async def call_model(state: MessagesState) -> dict[str, list[BaseMessage]]:
            return {"messages": [await model.ainvoke(state["messages"])]}

        graph = StateGraph(MessagesState)
        graph.add_node("model", call_model)
        graph.add_edge(START, "model")
        graph.add_edge("model", END)
        # Each HTTP request already carries its complete message history. A
        # process-global checkpointer would merge unrelated callers into the
        # same conversation whenever they omit or reuse a thread identifier.
        self.graph = graph.compile()

    def invoke(self, messages: list[dict[str, Any]], config: dict[str, Any]) -> str:
        configured_system = config.get("systemPrompt")
        if isinstance(configured_system, str) and configured_system.strip():
            if not messages or messages[0].get("role") != "system":
                messages = [{"role": "system", "content": configured_system}, *messages]
        result = asyncio.run(self.graph.ainvoke({"messages": messages}))
        return _content(result["messages"][-1])


if __name__ == "__main__":
    main(LangGraphAdapter)

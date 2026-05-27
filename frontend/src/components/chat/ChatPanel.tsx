import { type CSSProperties, type FormEvent, useEffect, useRef, useState } from 'react'
import { Bot, Loader2, Send, Trash2, User } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { deploymentsApi, type ChatMessage } from '@/lib/api'

interface ChatUiMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
}

interface ChatPanelProps {
  deploymentName: string
  namespace: string
  className?: string
  style?: CSSProperties
}

function getErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : 'An unexpected error occurred'
}

function getMessageFromErrorPayload(payload: unknown): string | undefined {
  if (!payload || typeof payload !== 'object') return undefined

  const body = payload as {
    error?: { message?: unknown; details?: unknown }
    message?: unknown
  }

  if (typeof body.error?.message === 'string' && body.error.message.trim()) {
    return body.error.message
  }

  if (typeof body.message === 'string' && body.message.trim()) {
    return body.message
  }

  const details = body.error?.details
  if (typeof details === 'string' && details.trim()) {
    try {
      const parsedDetails = JSON.parse(details) as { message?: unknown }
      if (typeof parsedDetails.message === 'string' && parsedDetails.message.trim()) {
        return parsedDetails.message
      }
    } catch {
      return details
    }
  }

  return undefined
}

async function getChatResponseError(response: Response): Promise<string> {
  const fallback = `Chat request failed with status ${response.status}`
  const responseText = await response.text()

  if (!responseText) return fallback

  try {
    const parsed = JSON.parse(responseText)
    return getMessageFromErrorPayload(parsed) ?? fallback
  } catch {
    return responseText
  }
}

type ChatStreamEvent =
  | { kind: 'content'; content: string }
  | { kind: 'error'; message: string }
  | { kind: 'empty' }

function parseChatStreamPayload(payload: string): ChatStreamEvent {
  let parsed: unknown
  try {
    parsed = JSON.parse(payload)
  } catch {
    return { kind: 'empty' }
  }

  if (!parsed || typeof parsed !== 'object') {
    return { kind: 'empty' }
  }

  const errorMessage = getMessageFromErrorPayload(parsed)
  if (errorMessage) {
    return { kind: 'error', message: errorMessage }
  }

  const choices = (parsed as {
    choices?: Array<{
      delta?: { content?: unknown }
      finish_reason?: unknown
    }>
  }).choices
  const firstChoice = choices?.[0]

  if (firstChoice?.finish_reason === 'error') {
    return { kind: 'error', message: 'Upstream model server returned an error' }
  }

  const content = firstChoice?.delta?.content
  if (typeof content === 'string' && content) {
    return { kind: 'content', content }
  }

  return { kind: 'empty' }
}

export function ChatPanel({ deploymentName, namespace, className, style }: ChatPanelProps) {
  const [messages, setMessages] = useState<ChatUiMessage[]>([])
  const [input, setInput] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [isStreaming, setIsStreaming] = useState(false)
  const mountedRef = useRef(false)
  const messagesContainerRef = useRef<HTMLDivElement | null>(null)
  const shouldAutoScrollRef = useRef(true)
  const abortControllerRef = useRef<AbortController | null>(null)

  useEffect(() => {
    mountedRef.current = true

    return () => {
      mountedRef.current = false
      abortControllerRef.current?.abort()
      abortControllerRef.current = null
    }
  }, [])

  useEffect(() => {
    abortControllerRef.current?.abort()
    abortControllerRef.current = null
    shouldAutoScrollRef.current = true
    setMessages([])
    setInput('')
    setError(null)
    setIsStreaming(false)
  }, [deploymentName, namespace])

  useEffect(() => {
    if (!shouldAutoScrollRef.current) return

    const container = messagesContainerRef.current
    if (!container) return

    container.scrollTop = container.scrollHeight
  }, [messages, isStreaming])

  const handleMessagesScroll = () => {
    const container = messagesContainerRef.current
    if (!container) return

    const distanceFromBottom = container.scrollHeight - container.scrollTop - container.clientHeight
    shouldAutoScrollRef.current = distanceFromBottom < 80
  }

  const clearConversation = () => {
    if (isStreaming) return
    shouldAutoScrollRef.current = true
    setMessages([])
    setInput('')
    setError(null)
  }

  const handleSend = async (event?: FormEvent) => {
    event?.preventDefault()

    const prompt = input.trim()
    if (!prompt || isStreaming) return

    const userMessage: ChatUiMessage = {
      id: `user-${Date.now()}`,
      role: 'user',
      content: prompt,
    }
    const assistantMessage: ChatUiMessage = {
      id: `assistant-${Date.now()}`,
      role: 'assistant',
      content: '',
    }
    const outgoingMessages: ChatMessage[] = [...messages, userMessage].map(({ role, content }) => ({
      role,
      content,
    }))

    abortControllerRef.current?.abort()
    const abortController = new AbortController()
    abortControllerRef.current = abortController

    shouldAutoScrollRef.current = true
    setInput('')
    setError(null)
    setIsStreaming(true)
    setMessages((currentMessages) => [...currentMessages, userMessage, assistantMessage])

    let assistantHasContent = false

    const appendAssistantContent = (content: string) => {
      if (!content || !mountedRef.current || abortController.signal.aborted) return
      assistantHasContent = true
      setMessages((currentMessages) =>
        currentMessages.map((message) =>
          message.id === assistantMessage.id
            ? { ...message, content: message.content + content }
            : message
        )
      )
    }

    const processSseLine = (line: string): boolean => {
      const trimmed = line.trim()
      if (!trimmed.startsWith('data:')) return false

      const data = trimmed.slice('data:'.length).trim()
      if (data === '[DONE]') return true

      const event = parseChatStreamPayload(data)
      if (event.kind === 'error') {
        throw new Error(event.message)
      }
      if (event.kind === 'content') {
        appendAssistantContent(event.content)
      }
      return false
    }

    try {
      const response = await deploymentsApi.chat(
        deploymentName,
        { messages: outgoingMessages },
        namespace,
        { signal: abortController.signal }
      )

      if (!response.ok) {
        throw new Error(await getChatResponseError(response))
      }

      if (!response.body) {
        throw new Error('Chat response did not include a stream')
      }

      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      let streamDone = false

      while (!streamDone) {
        const { value, done } = await reader.read()
        if (!mountedRef.current || abortController.signal.aborted) return
        const chunk = value
          ? decoder.decode(value, { stream: !done })
          : done
            ? decoder.decode()
            : ''

        if (chunk) {
          buffer += chunk
        }

        const lines = buffer.split(/\r?\n/)
        buffer = lines.pop() ?? ''

        for (const line of lines) {
          if (processSseLine(line)) {
            streamDone = true
            break
          }
        }

        if (done) {
          if (!streamDone && buffer) {
            streamDone = processSseLine(buffer)
          }
          break
        }
      }
    } catch (chatError) {
      if (chatError instanceof Error && chatError.name === 'AbortError') return
      if (!mountedRef.current || abortController.signal.aborted) return

      setError(getErrorMessage(chatError))
      setMessages((currentMessages) =>
        assistantHasContent
          ? currentMessages
          : currentMessages.filter((message) => message.id !== assistantMessage.id)
      )
    } finally {
      if (abortControllerRef.current === abortController) {
        abortControllerRef.current = null
      }
      if (mountedRef.current && !abortController.signal.aborted) {
        setIsStreaming(false)
      }
    }
  }

  const hasMessages = messages.length > 0
  const transcriptClassName = `mb-4 max-h-80 overflow-y-auto rounded-xl ${
    hasMessages
      ? 'space-y-3 border border-white/10 bg-black/20 p-3'
      : 'px-3 py-6'
  }`

  return (
    <div className={`glass-panel ${className ?? ''}`} style={style}>
      <div className="mb-1 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-center gap-2">
          <Bot className="h-5 w-5" />
          <h2 className="text-lg font-heading">Chat with model</h2>
        </div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={clearConversation}
          disabled={!hasMessages || isStreaming}
        >
          <Trash2 className="mr-2 h-4 w-4" />
          Clear conversation
        </Button>
      </div>
      <p className="mb-4 text-sm text-muted-foreground">
        Send a message directly to this running model and see its reply here.
      </p>

      <div
        ref={messagesContainerRef}
        aria-live="polite"
        data-testid="chat-transcript"
        className={transcriptClassName}
        onScroll={handleMessagesScroll}
      >
        {!hasMessages ? (
          <div className="flex flex-col items-center justify-center gap-2 text-center text-sm text-muted-foreground">
            <Bot className="h-5 w-5 opacity-70" />
            <p>Start a conversation with this model.</p>
          </div>
        ) : (
          messages.map((message) => (
            <div
              key={message.id}
              className={`rounded-lg p-3 ${message.role === 'user' ? 'bg-primary/10' : 'bg-white/[0.03]'}`}
            >
              <div className="mb-1 flex items-center gap-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
                {message.role === 'user' ? (
                  <User className="h-3.5 w-3.5" />
                ) : (
                  <Bot className="h-3.5 w-3.5" />
                )}
                {message.role === 'user' ? 'You' : 'Assistant'}
              </div>
              <div className="whitespace-pre-wrap text-sm">
                {message.content || (message.role === 'assistant' && isStreaming ? 'Thinking…' : '')}
              </div>
            </div>
          ))
        )}
      </div>

      {error && (
        <p role="alert" className="mb-3 text-sm text-destructive">
          {error}
        </p>
      )}

      <form onSubmit={handleSend} className="flex flex-col gap-3 sm:flex-row sm:items-end">
        <textarea
          aria-label="Message"
          className="min-h-24 flex-1 rounded-xl border border-input bg-background px-3 py-2 text-sm outline-none transition-colors placeholder:text-muted-foreground focus:border-ring focus:ring-2 focus:ring-ring/20 disabled:cursor-not-allowed disabled:opacity-50"
          disabled={isStreaming}
          onChange={(event) => setInput(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              void handleSend()
            }
          }}
          placeholder="Ask the model a question..."
          rows={3}
          value={input}
        />
        <Button type="submit" disabled={!input.trim() || isStreaming}>
          {isStreaming ? (
            <Loader2 className="mr-2 h-4 w-4 animate-spin" />
          ) : (
            <Send className="mr-2 h-4 w-4" />
          )}
          Send
        </Button>
      </form>
    </div>
  )
}

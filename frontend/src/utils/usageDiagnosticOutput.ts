type JsonObject = Record<string, unknown>
type TextPart = { outputIndex: number; contentIndex: number; delta: string; done?: string }

function object(value: unknown): JsonObject | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as JsonObject : null
}

function objects(value: unknown): JsonObject[] {
  return Array.isArray(value) ? value.map(object).filter((item): item is JsonObject => item !== null) : []
}

function parseJson(content: string): unknown {
  try {
    return JSON.parse(content)
  } catch {
    return undefined
  }
}

function joinText(parts: (string | null)[]): string | null {
  const text = parts.filter((part): part is string => part !== null)
  return text.length ? text.join('') : null
}

function textBlocks(content: unknown, type: string): string | null {
  return joinText(objects(content).map(part => part.type === type && typeof part.text === 'string' ? part.text : null))
}

function responsesOutput(response: JsonObject | null): string | null {
  return joinText(objects(response?.output).map(item => {
    if (item.type !== 'message' || (item.role != null && item.role !== 'assistant')) return null
    return textBlocks(item.content, 'output_text')
  }))
}

function chatText(message: JsonObject | null): string | null {
  if (!message || (message.role != null && message.role !== 'assistant')) return null
  return typeof message.content === 'string' ? message.content : textBlocks(message.content, 'text')
}

function index(value: unknown, fallback = 0): number | null {
  if (value == null) return fallback
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : null
}

function addText(parts: Map<string, TextPart>, key: string, outputIndex: number, contentIndex: number, text: string, done = false) {
  const part = parts.get(key) ?? { outputIndex, contentIndex, delta: '' }
  if (done) part.done = text
  else part.delta += text
  parts.set(key, part)
}

function joinedParts(parts: Map<string, TextPart>): string | null {
  if (!parts.size) return null
  return [...parts.values()]
    .sort((a, b) => a.outputIndex - b.outputIndex || a.contentIndex - b.contentIndex)
    .map(part => part.done ?? part.delta).join('')
}

function collectChat(data: JsonObject, parts: Map<string, TextPart>) {
  objects(data.choices).forEach((choice, position) => {
    const outputIndex = index(choice.index, position)
    if (outputIndex === null) return
    const message = chatText(object(choice.message))
    const delta = chatText(object(choice.delta))
    if (message !== null) addText(parts, `chat:${outputIndex}`, outputIndex, 0, message, true)
    else if (delta !== null) addText(parts, `chat:${outputIndex}`, outputIndex, 0, delta)
  })
}

function collectGemini(data: JsonObject, parts: Map<string, TextPart>) {
  objects(data.candidates).forEach((candidate, position) => {
    const outputIndex = index(candidate.index, position)
    const content = object(candidate.content)
    if (outputIndex === null || (content?.role != null && content.role !== 'model')) return
    const text = joinText(objects(content?.parts).map(part =>
      part.thought !== true && typeof part.text === 'string' ? part.text : null
    ))
    if (text !== null) addText(parts, `gemini:${outputIndex}`, outputIndex, 0, text)
  })
}

function jsonOutput(value: unknown): string | null {
  const parts = new Map<string, TextPart>()
  if (Array.isArray(value)) {
    value.forEach(chunk => {
      const data = object(chunk)
      if (data) collectGemini(data, parts)
    })
    return joinedParts(parts)
  }
  const data = object(value)
  if (!data) return null
  const output = responsesOutput(data)
  if (output !== null) return output
  if (data.type === 'message' && (data.role == null || data.role === 'assistant')) {
    return textBlocks(data.content, 'text')
  }
  collectChat(data, parts)
  collectGemini(data, parts)
  return joinedParts(parts)
}

function* sseEvents(content: string): Generator<{ type: string; data: JsonObject }> {
  let event = ''
  let lines: string[] = []
  const frame = () => {
    const data = object(parseJson(lines.join('\n')))
    const type = typeof data?.type === 'string' ? data.type : event
    event = ''
    lines = []
    return data ? { type, data } : null
  }
  for (const line of content.replace(/^\uFEFF/, '').split(/\r\n|\n|\r/)) {
    if (!line) {
      const parsed = frame()
      if (parsed) yield parsed
      continue
    }
    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'event') event = value
    else if (field === 'data') lines.push(value)
  }
  // A capped snapshot may end after a complete JSON frame without the SSE delimiter.
  const parsed = frame()
  if (parsed) yield parsed
}

function streamOutput(content: string): string | null {
  const parts = new Map<string, TextPart>()
  let completed: string | null = null
  for (const { type, data } of sseEvents(content)) {
    if (type === 'response.completed' || type === 'response.done') {
      const output = responsesOutput(object(data.response))
      if (output !== null) completed = output
    } else if (type === 'response.output_text.delta' || type === 'response.output_text.done') {
      const outputIndex = index(data.output_index)
      const contentIndex = index(data.content_index)
      const done = type === 'response.output_text.done'
      const text = done ? data.text : data.delta
      if (outputIndex !== null && contentIndex !== null && typeof text === 'string') {
        const item = data.output_index ?? (typeof data.item_id === 'string' ? data.item_id : 0)
        addText(parts, `responses:${item}:${contentIndex}`, outputIndex, contentIndex, text, done)
      }
    } else if (type === 'content_block_start' || type === 'content_block_delta') {
      const outputIndex = index(data.index)
      const block = object(data.content_block)
      const delta = object(data.delta)
      const text = type === 'content_block_start' && block?.type === 'text' ? block.text
        : type === 'content_block_delta' && delta?.type === 'text_delta' ? delta.text : null
      if (outputIndex !== null && typeof text === 'string') {
        addText(parts, `anthropic:${outputIndex}`, outputIndex, 0, text)
      }
    }
    collectChat(data, parts)
    collectGemini(data, parts)
  }
  // Terminal output replaces streamed fragments; done text replaces each part's deltas.
  return completed ?? joinedParts(parts)
}

export function extractUsageDiagnosticOutput(content: string, contentType?: string): string | null {
  const value = parseJson(content)
  if (value !== undefined) return jsonOutput(value)
  const eventStream = contentType?.split(';')[0].trim().toLowerCase() === 'text/event-stream'
  if (eventStream || /(?:^|[\r\n])data:/.test(content.replace(/^\uFEFF/, ''))) return streamOutput(content)
  return null
}

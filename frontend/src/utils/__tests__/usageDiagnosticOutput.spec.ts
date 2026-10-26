import { Buffer } from 'node:buffer'
import { createHash } from 'node:crypto'
import { describe, expect, it } from 'vitest'
import { extractUsageDiagnosticOutput } from '@/utils/usageDiagnosticOutput'

function sse(...events: unknown[]): string {
  return events.map(event => `data: ${JSON.stringify(event)}\n\n`).join('')
}

const largeProtocols = ['Responses JSON', 'Responses SSE', 'Chat JSON', 'Chat SSE', 'Anthropic JSON', 'Anthropic SSE', 'Gemini JSON', 'Gemini SSE'] as const
type LargeProtocol = typeof largeProtocols[number]

function largeFixture(protocol: LargeProtocol) {
  const row = 'UTF8 \u6a21\u578b\u8f93\u51fa \u03bb \uD83D\uDE80 <literal> ' + '0123456789abcdef'.repeat(12) + '\n'
  const tail = `UNIQUE_DIAGNOSTIC_TAIL_${protocol.replace(/ /g, '_')}_20261027_\u7ec8\u70b9`
  const text = ' \r\n' + row.repeat(Math.ceil((3.6 * 1024 * 1024 + 4096) / Buffer.byteLength(row, 'utf8'))) + tail + '\r\n  '
  const chunks: string[] = []
  for (let offset = 0; offset < text.length; offset += 65521) chunks.push(text.slice(offset, offset + 65521))
  const message = { type: 'message', role: 'assistant', content: [{ type: 'output_text', text }] }
  let content: string
  switch (protocol) {
    case 'Responses JSON':
      content = JSON.stringify({ output: [{ type: 'reasoning', summary: [{ text: 'hidden reasoning' }] }, message] })
      break
    case 'Responses SSE':
      content = sse(...chunks.map(delta => ({ type: 'response.output_text.delta', output_index: 0, content_index: 0, delta })),
        { type: 'response.output_text.done', output_index: 0, content_index: 0, text },
        { type: 'response.completed', response: { output: [message] } })
      break
    case 'Chat JSON':
      content = JSON.stringify({ choices: [{ index: 0, message: { role: 'assistant', content: text, reasoning_content: 'hidden reasoning' } }] })
      break
    case 'Chat SSE':
      content = sse(...chunks.map(content => ({ choices: [{ index: 0, delta: { content } }] })), { choices: [], usage: { completion_tokens: 10 } }) + 'data: [DONE]\n\n'
      break
    case 'Anthropic JSON':
      content = JSON.stringify({ type: 'message', role: 'assistant', content: [{ type: 'thinking', thinking: 'hidden reasoning' }, { type: 'text', text }] })
      break
    case 'Anthropic SSE':
      content = sse({ type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } },
        ...chunks.map(text => ({ type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text } })),
        { type: 'content_block_stop', index: 0 }, { type: 'message_stop' })
      break
    case 'Gemini JSON':
      content = JSON.stringify({ candidates: [{ index: 0, content: { role: 'model', parts: [{ thought: true, text: 'hidden reasoning' }, { text }] } }] })
      break
    case 'Gemini SSE':
      content = sse(...chunks.map(text => ({ candidates: [{ index: 0, content: { role: 'model', parts: [{ thought: true, text: 'hidden reasoning' }, { text }] } }] })))
      break
  }
  return { content, text, tail, contentType: protocol.endsWith('SSE') ? 'text/event-stream; charset=utf-8' : 'application/json' }
}

function sha256(text: string) {
  return createHash('sha256').update(text, 'utf8').digest('hex')
}

describe('usage diagnostic model output extraction', () => {
  it.each(largeProtocols)('preserves more than 3.6 MiB of UTF8 output and its unique tail from %s', (protocol) => {
    const fixture = largeFixture(protocol)
    expect(Buffer.byteLength(fixture.text, 'utf8')).toBeGreaterThan(3.6 * 1024 * 1024)
    expect(Buffer.byteLength(fixture.content, 'utf8')).toBeGreaterThan(3.6 * 1024 * 1024)
    const actual = extractUsageDiagnosticOutput(fixture.content, fixture.contentType)

    expect(actual?.length).toBe(fixture.text.length)
    expect(Buffer.byteLength(actual!, 'utf8')).toBe(Buffer.byteLength(fixture.text, 'utf8'))
    expect(sha256(actual!)).toBe(sha256(fixture.text))
    expect(actual!.endsWith(fixture.tail + '\r\n  ')).toBe(true)
    expect(actual!.split(fixture.tail)).toHaveLength(2)
    expect(actual === fixture.text).toBe(true)
  })

  it('deduplicates repeated large Responses done frames without a completed snapshot', () => {
    const fixture = largeFixture('Responses JSON')
    const done = { type: 'response.output_text.done', output_index: 0, content_index: 0, text: fixture.text }
    const content = sse({ type: 'response.output_text.delta', output_index: 0, content_index: 0, delta: 'stale delta' }, done, done)
    const actual = extractUsageDiagnosticOutput(content, 'text/event-stream')

    expect(actual?.length).toBe(fixture.text.length)
    expect(sha256(actual!)).toBe(sha256(fixture.text))
    expect(actual === fixture.text).toBe(true)
  })

  it('exposes the missing tail after the old 1 MiB capture cap instead of inventing the rest', () => {
    const fixture = largeFixture('Responses SSE')
    const capped = Buffer.from(fixture.content, 'utf8').subarray(0, 1024 * 1024).toString('utf8')
    const actual = extractUsageDiagnosticOutput(capped, fixture.contentType)

    expect(actual).not.toBeNull()
    expect(actual!.length).toBeLessThan(fixture.text.length)
    expect(sha256(actual!)).not.toBe(sha256(fixture.text))
    expect(actual!.includes(fixture.tail)).toBe(false)
  })

  it('extracts only Responses message output_text and preserves text verbatim', () => {
    const content = JSON.stringify({ output: [
      { type: 'reasoning', summary: [{ type: 'summary_text', text: 'reasoning only' }] },
      { type: 'compaction', encrypted_content: 'opaque marker' },
      { type: 'message', role: 'user', content: [{ type: 'output_text', text: 'input only' }] },
      { type: 'message', role: 'assistant', content: [
        { type: 'output_text', text: '  <summary>raw</summary>\n' },
        { type: 'refusal', refusal: 'not text' },
        { type: 'output_text', text: '[REDACTED] {"value":1}' }
      ] }
    ], usage: { output_tokens: 12 } })

    expect(extractUsageDiagnosticOutput(content)).toBe('  <summary>raw</summary>\n[REDACTED] {"value":1}')
  })

  it('prefers completed Responses output over both deltas and done text', () => {
    const content = sse(
      { type: 'response.output_text.delta', output_index: 0, content_index: 0, delta: 'outdated delta' },
      { type: 'response.output_text.done', output_index: 0, content_index: 0, text: 'outdated done' },
      { type: 'response.completed', response: { output: [
        { type: 'message', content: [{ type: 'output_text', text: 'final answer' }] }
      ] } }
    )

    expect(extractUsageDiagnosticOutput(content, 'text/event-stream; charset=utf-8')).toBe('final answer')
  })

  it('replaces deltas with done text per part and orders parts without duplication', () => {
    const content = sse(
      { type: 'response.output_text.delta', output_index: 1, content_index: 0, delta: 'B' },
      { type: 'response.output_text.delta', output_index: 0, content_index: 1, delta: 'part' },
      { type: 'response.output_text.delta', output_index: 0, content_index: 0, delta: 'sta' },
      { type: 'response.output_text.delta', output_index: 0, content_index: 0, delta: 'le' },
      { type: 'response.output_text.done', output_index: 0, content_index: 0, text: 'A' },
      { type: 'response.output_text.done', output_index: 0, content_index: 0, text: 'A' },
      { type: 'response.output_text.done', output_index: 0, content_index: 1, text: ' ' },
      { type: 'response.completed', response: { output: [] } }
    )

    expect(extractUsageDiagnosticOutput(content)).toBe('A B')
  })

  it('handles completed-only streams and empty final text without reviving stale deltas', () => {
    expect(extractUsageDiagnosticOutput(sse({ type: 'response.completed', response: { output: [
      { type: 'message', content: [{ type: 'output_text', text: 'terminal only' }] }
    ] } }))).toBe('terminal only')
    expect(extractUsageDiagnosticOutput(sse(
      { type: 'response.output_text.delta', delta: 'stale' },
      { type: 'response.output_text.done', text: '' }
    ))).toBe('')
  })

  it('uses SSE event names when the JSON type is absent and supports BOM, CRLF and multiline data', () => {
    const content = '\uFEFF: heartbeat\r\nevent: response.output_text.delta\r\ndata: {"delta":\r\ndata: "hello "}\r\n\r\n' +
      'event: response.output_text.done\r\nid: 7\r\ndata: {"text":"hello world"}\r\n\r\n'

    expect(extractUsageDiagnosticOutput(content, 'text/event-stream')).toBe('hello world')
  })

  it('keeps completed fragments from an interrupted SSE and ignores a malformed trailing event', () => {
    const content = sse({ type: 'response.output_text.delta', delta: 'partial answer' }) +
      'data: {"type":"response.output_text.delta","delta":"unfinished'

    expect(extractUsageDiagnosticOutput(content)).toBe('partial answer')
    expect(extractUsageDiagnosticOutput('data: {"type":"response.output_text.done","text":"last frame"}')).toBe('last frame')
  })

  it('does not promote response.created, reasoning deltas or usage into model text', () => {
    const content = sse(
      { type: 'response.created', response: { output: [{ type: 'message', content: [{ type: 'output_text', text: 'not completed' }] }] } },
      { type: 'response.reasoning_summary_text.delta', delta: 'reasoning only' },
      { type: 'response.completed', response: { output: [{ type: 'reasoning', summary: [{ text: 'reasoning only' }] }], usage: { output_tokens: 5 } } }
    )

    expect(extractUsageDiagnosticOutput(content)).toBeNull()
  })

  it('extracts Anthropic JSON text blocks without thinking or tool inputs', () => {
    const content = JSON.stringify({ type: 'message', role: 'assistant', content: [
      { type: 'thinking', thinking: 'hidden' },
      { type: 'text', text: 'Hello ' },
      { type: 'tool_use', input: { text: 'tool argument' } },
      { type: 'text', text: 'world\n' }
    ] })

    expect(extractUsageDiagnosticOutput(content)).toBe('Hello world\n')
  })

  it('joins only Anthropic text_delta events and initial text blocks by their index', () => {
    const content = sse(
      { type: 'content_block_start', index: 1, content_block: { type: 'text', text: 'world' } },
      { type: 'content_block_start', index: 0, content_block: { type: 'text', text: 'Hello' } },
      { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: ' ' } },
      { type: 'content_block_delta', index: 0, delta: { type: 'thinking_delta', thinking: 'hidden', text: 'hidden' } },
      { type: 'content_block_delta', index: 2, delta: { type: 'input_json_delta', partial_json: '{"text":"hidden"}' } },
      { type: 'content_block_delta', index: -1, delta: { type: 'text_delta', text: 'invalid index' } },
      { type: 'message_delta', delta: { stop_reason: 'end_turn' }, usage: { output_tokens: 9 } }
    )

    expect(extractUsageDiagnosticOutput(content)).toBe('Hello world')
  })

  it.each([
    ['{"choices":[{"message":{"role":"assistant","content":"  literal\\ntext  ","reasoning_content":"hidden"}}]}', '  literal\ntext  '],
    ['{"choices":[{"message":{"content":[{"type":"text","text":"one"},{"type":"image_url","image_url":{"url":"hidden"}},{"type":"text","text":"two"}]}}]}', 'onetwo'],
    ['{"choices":[{"delta":{"content":"single chunk"}}]}', 'single chunk']
  ])('extracts Chat content without reformatting it', (content, expected) => {
    expect(extractUsageDiagnosticOutput(content)).toBe(expected)
  })

  it('keeps Chat choices separate while accumulating delta.content and ignoring usage', () => {
    const content = sse(
      { choices: [{ index: 1, delta: { content: 'B' } }, { index: 0, delta: { content: 'A' } }] },
      { choices: [{ index: 0, delta: { reasoning_content: 'hidden' } }, { index: 0, delta: { content: '1' } }] },
      { choices: [{ index: 1, delta: { content: '2' }, finish_reason: 'stop' }] },
      { choices: [], usage: { completion_tokens: 4 } }
    ) + 'data: [DONE]\n\n'

    expect(extractUsageDiagnosticOutput(content)).toBe('A1B2')
  })

  it('prefers a final Chat message over previous delta fragments', () => {
    const content = sse(
      { choices: [{ index: 0, delta: { content: 'partial' } }] },
      { choices: [{ index: 0, message: { role: 'assistant', content: 'full answer' } }] }
    )

    expect(extractUsageDiagnosticOutput(content)).toBe('full answer')
  })

  it('extracts Gemini parts while excluding thought and function call parts', () => {
    const content = JSON.stringify({ candidates: [{ content: { role: 'model', parts: [
      { thought: true, text: 'thinking' },
      { text: 'Hello ' },
      { functionCall: { name: 'tool', args: { text: 'hidden' } } },
      { text: 'world' }
    ] } }], usageMetadata: { candidatesTokenCount: 2 } })

    expect(extractUsageDiagnosticOutput(content)).toBe('Hello world')
  })

  it('handles both Gemini SSE and a JSON array of stream chunks', () => {
    const first = { candidates: [{ index: 0, content: { parts: [{ text: 'Hello ' }] } }] }
    const second = { candidates: [{ index: 0, content: { parts: [{ thought: true, text: 'hidden' }, { text: 'world' }] } }] }

    expect(extractUsageDiagnosticOutput(sse(first, second))).toBe('Hello world')
    expect(extractUsageDiagnosticOutput(JSON.stringify([first, second]))).toBe('Hello world')
  })

  it.each([
    '', 'raw opaque text', '"a JSON string"', '{"text":"arbitrary field","summary":"not output","usage":{"output_tokens":3}}',
    '{"output":[{"type":"compaction","encrypted_content":"opaque"}]}',
    '{"choices":[{"message":{"reasoning_content":"hidden","tool_calls":[{"function":{"arguments":"hidden"}}]}}]}',
    '{"type":"message","role":"user","content":[{"type":"text","text":"input"}]}',
    'data: not-json\n\ndata: [DONE]\n\n',
    '{"output":[{"type":"message","content":[{"type":"output_text","text":'
  ])('returns no invented output for absent, unrelated or malformed text: %s', (content) => {
    expect(extractUsageDiagnosticOutput(content)).toBeNull()
  })
})

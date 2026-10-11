import { describe, expect, it } from 'vitest'
import { extractUsageDiagnosticOutput } from '@/utils/usageDiagnosticOutput'

function sse(...events: unknown[]): string {
  return events.map(event => `data: ${JSON.stringify(event)}\n\n`).join('')
}

describe('usage diagnostic model output extraction', () => {
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

import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { defineComponent, ref } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AdminUsageLog } from '@/types'
import type { UsageDiagnosticPayload, UsageDiagnosticResponse, UsageDiagnosticStatus } from '@/api/admin/usageDiagnostic'
import UsageDiagnosticModal from '../UsageDiagnosticModal.vue'

const { getDiagnostic, clipboard } = vi.hoisted(() => ({ getDiagnostic: vi.fn(), clipboard: vi.fn() }))
const testLocale = ref('en')
vi.mock('@/api/admin/usageDiagnostic', () => ({ getUsageDiagnostic: getDiagnostic }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ locale: testLocale }) }))
vi.mock('@/utils/format', () => ({
  formatDateTime: (value: string) => value,
  formatReasoningEffort: (value?: string | null) => value || '-'
}))

const BaseDialogStub = defineComponent({
  props: ['show', 'title'],
  emits: ['close'],
  template: '<div v-if="show"><h3>{{ title }}</h3><slot /><slot name="footer" /></div>'
})
const global = { stubs: { BaseDialog: BaseDialogStub, Icon: true } }

enableAutoUnmount(afterEach)
beforeEach(() => {
  testLocale.value = 'en'
  getDiagnostic.mockReset()
  clipboard.mockReset().mockResolvedValue(undefined)
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: clipboard } })
})
afterEach(() => vi.useRealTimers())

function usage(id: number): AdminUsageLog {
  return {
    id,
    user_id: 8,
    api_key_id: 4,
    account_id: 9,
    request_id: `usage-request-${id}`,
    upstream_request_id: `usage-upstream-${id}`,
    model: 'requested-model',
    upstream_model: 'forwarded-model',
    upstream_response_model: 'response-model',
    upstream_model_mismatch: true,
    reasoning_effort: 'xhigh',
    upstream_reasoning_effort: 'high',
    inbound_endpoint: '/v1/responses',
    upstream_endpoint: '/v1/chat/completions',
    request_type: 'stream',
    input_tokens: 110,
    output_tokens: 20,
    cache_read_tokens: 30,
    cache_creation_tokens: 40,
    cache_creation_5m_tokens: 35,
    cache_creation_1h_tokens: 5,
    duration_ms: 3000,
    first_token_ms: 120,
    created_at: '2026-10-03T09:10:11Z',
    user_agent: 'test-client/1.0'
  } as AdminUsageLog
}
function payload(content: string, overrides: Partial<UsageDiagnosticPayload> = {}): UsageDiagnosticPayload {
  return { content, original_bytes: content.length, stored_bytes: content.length, redacted: false, truncated: false, ...overrides }
}
function response(id = 1, content = `saved-body-${id}`): UsageDiagnosticResponse {
  return {
    usage: usage(id),
    capture_enabled: true,
    retention_hours: 24,
    diagnostic: {
      status: 'captured',
      captured_at: '2026-10-03T09:10:12Z',
      expires_at: '2026-10-04T09:10:12Z',
      payload: {
        schema_version: 1,
        binding: { api_key_id: 4, usage_request_id: `usage-request-${id}`, usage_created_at: '2026-10-03T09:10:11Z' },
        inbound: {
          request_id: `inbound-request-${id}`,
          client_request_id: `client-request-${id}`,
          endpoint: '/v1/responses',
          method: 'POST',
          user_agent: 'test-client/1.0',
          started_at: '2026-10-03T09:10:08Z',
          body: payload(content)
        },
        status: 200,
        captured_at: '2026-10-03T09:10:12Z',
        capture_status: 'complete',
        stored_bytes: 1024,
        dropped_attempts: 0,
        attempts: [
          {
            id: 1,
            account_id: 9,
            platform: 'openai',
            endpoint: '/v1/chat/completions',
            method: 'POST',
            model: 'forwarded-model',
            started_at: '2026-10-03T09:10:09Z',
            duration_ms: 2000,
            status: 200,
            upstream_request_id: `upstream-attempt-${id}`,
            body: payload(`final-body-${id}`),
            response_summary: payload(`response-summary-${id}`)
          }
        ]
      }
    }
  }
}
function responseWithBody(content: string, id = 1, overrides: Partial<UsageDiagnosticPayload> = {}, contentType = 'application/json'): UsageDiagnosticResponse {
  const result = response(id)
  const attempt = result.diagnostic.payload!.attempts[0]
  attempt.response_body = payload(content, overrides)
  attempt.response_content_type = contentType
  return result
}
function deferred<T = UsageDiagnosticResponse>() {
  let resolve!: (value: T) => void
  let reject!: (error: Error) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}
async function open() {
  const wrapper = mount(UsageDiagnosticModal, { props: { show: false, usageId: 1, usage: usage(1) }, global })
  expect(getDiagnostic).not.toHaveBeenCalled()
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

describe('admin usage diagnostic modal', () => {
  it('loads on open only and displays usage, endpoints, timing, tokens and IDs', async () => {
    getDiagnostic.mockResolvedValueOnce(response())
    const wrapper = await open()

    expect(getDiagnostic).toHaveBeenCalledWith(1, { signal: expect.any(AbortSignal) })
    expect(wrapper.text()).toContain('requested-model')
    expect(wrapper.text()).toContain('forwarded-model')
    expect(wrapper.text()).toContain('response-model')
    expect(wrapper.text()).toContain('xhigh')
    expect(wrapper.text()).toContain('high')
    expect(wrapper.text()).toContain('/v1/responses')
    expect(wrapper.text()).toContain('/v1/chat/completions')
    expect(wrapper.text()).toContain('3,000 ms')
    expect(wrapper.text()).toContain('120 ms')
    expect(wrapper.text()).toContain('Input tokens')
    expect(wrapper.text()).toContain('110')
    expect(wrapper.text()).toContain('Output tokens')
    expect(wrapper.text()).toContain('20')
    expect(wrapper.text()).toContain('usage-request-1')
    expect(wrapper.text()).toContain('client-request-1')
    expect(wrapper.text()).toContain('upstream-attempt-1')
    expect(wrapper.text()).toContain('2026-10-04T09:10:12Z')
    expect(wrapper.findAll('pre')).toHaveLength(0)
    expect(wrapper.text()).not.toContain('saved-body-1')
    expect(wrapper.text()).toContain('not the full response body or full SSE stream')
    expect(clipboard).not.toHaveBeenCalled()
  })

  it('reveals untrusted content only as plain text and copies the stored text on click', async () => {
    const content = '<script>alert("prompt")</script><img src="https://example.invalid/leak" onerror="alert(1)">'
    getDiagnostic.mockResolvedValueOnce(response(1, content))
    const wrapper = await open()
    const toggle = wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]')

    expect(toggle.attributes('aria-expanded')).toBe('false')
    await toggle.trigger('click')
    expect(toggle.attributes('aria-expanded')).toBe('true')
    expect(wrapper.get('pre code').text()).toBe(content)
    expect(wrapper.find('script').exists()).toBe(false)
    expect(wrapper.find('img').exists()).toBe(false)
    expect(clipboard).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(content)
    expect(wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').text()).toContain('Copied')
    await toggle.trigger('click')
    expect(wrapper.findAll('pre')).toHaveLength(0)
  })

  it('pretty-prints JSON only for display and copies the exact saved content', async () => {
    const content = JSON.stringify({ message: '<strong>plain</strong>', nested: { ok: true } })
    const formatted = JSON.stringify(JSON.parse(content), null, 2)
    getDiagnostic.mockResolvedValueOnce(response(1, content))
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')

    expect(wrapper.get('pre code').text()).toBe(formatted)
    expect(wrapper.find('strong').exists()).toBe(false)

    await wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(content)
  })

  it('preserves exact numeric tokens, duplicate keys, and empty containers', async () => {
    const content = '{"large":9007199254740993,"precise":0.123456789012345678901,"exponent":1.2300e+23,"emptyObject":{},"emptyArray":[],"large":-0}'
    const formatted = [
      '{',
      '  "large": 9007199254740993,',
      '  "precise": 0.123456789012345678901,',
      '  "exponent": 1.2300e+23,',
      '  "emptyObject": {},',
      '  "emptyArray": [],',
      '  "large": -0',
      '}'
    ].join('\n')
    getDiagnostic.mockResolvedValueOnce(response(1, content))
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')

    expect(wrapper.get('pre code').text()).toBe(formatted)
    await wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(content)
  })

  it('leaves truncated JSON with doubled escape characters unchanged', async () => {
    const content = String.raw`{"path":"C:\\temp\\new","next":`
    getDiagnostic.mockResolvedValueOnce(response(1, content))
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')

    expect(wrapper.get('pre code').text()).toBe(content)
    await wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(content)
  })

  it('preserves escape tokens and structural characters inside JSON strings', async () => {
    const content = String.raw`{"escaped":"line\ncolumn\tquote\"slash\\solidus\/unicode\u0061","symbols":"{}[],:\""}`
    const formatted = [
      '{',
      String.raw`  "escaped": "line\ncolumn\tquote\"slash\\solidus\/unicode\u0061",`,
      String.raw`  "symbols": "{}[],:\""`,
      '}'
    ].join('\n')
    getDiagnostic.mockResolvedValueOnce(response(1, content))
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')

    expect(wrapper.get('pre code').text()).toBe(formatted)
    await wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(content)
  })

  it('leaves JSON nested deeper than 64 levels unchanged', async () => {
    const content = '['.repeat(65) + '0' + ']'.repeat(65)
    getDiagnostic.mockResolvedValueOnce(response(1, content))
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')

    expect(wrapper.get('pre code').text()).toBe(content)
    await wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(content)
  })

  it.each<UsageDiagnosticStatus>(['not_captured', 'expired', 'evicted', 'unavailable'])('shows %s without inventing historical content', async (status) => {
    const result = response()
    result.diagnostic = { status, payload: null }
    getDiagnostic.mockResolvedValueOnce(result)
    const wrapper = await open()

    expect(wrapper.get('[data-testid="diagnostic-status"]').text()).toContain(status)
    expect(wrapper.text()).toContain('usage-request-1')
    expect(wrapper.find('[data-testid="diagnostic-toggle-inbound-body"]').exists()).toBe(false)
    expect(wrapper.find('pre').exists()).toBe(false)
    if (status !== 'unavailable') expect(wrapper.text()).toContain('cannot be reconstructed')
    expect(wrapper.text().toLowerCase()).not.toContain('replay')
  })

  it('keeps truncation, omissions, byte counts and incomplete attempts without redaction notices', async () => {
    const result = response()
    result.diagnostic.payload!.inbound.body = payload('', {
      original_bytes: 2048,
      stored_bytes: 0,
      redacted: true,
      truncated: true,
      omitted_reason: 'body_limit'
    })
    result.diagnostic.payload!.dropped_attempts = 2
    getDiagnostic.mockResolvedValueOnce(result)
    const wrapper = await open()
    const block = wrapper.get('[data-testid="diagnostic-payload-inbound-body"]')

    expect(block.text()).toContain('2,048 B')
    expect(block.text()).toContain('0 B')
    expect(block.text()).not.toContain('Partially redacted')
    expect(block.text()).toContain('Truncated')
    expect(block.text()).toContain('body_limit')
    expect(wrapper.get('[data-testid="diagnostic-dropped-attempts"]').text()).toContain('2')
    expect(block.get('button[data-testid="diagnostic-copy-inbound-body"]').attributes()).toHaveProperty('disabled')
    await block.get('button[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')
    expect(block.text()).toContain('No saved content.')
    expect(wrapper.text()).not.toContain('Redaction is not full anonymization')
    expect(wrapper.text()).not.toContain('sensitive prompts')
  })

  it.each([
    ['Responses JSON', '{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"model answer"}]}]}', 'application/json'],
    ['Responses SSE', 'data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"stale delta"}\n\ndata: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"stale done"}\n\ndata: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"model answer"}]}]}}\n\n', 'text/event-stream'],
    ['Anthropic JSON', '{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"private reasoning"},{"type":"text","text":"model answer"}]}', 'application/json'],
    ['Anthropic SSE', 'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"model "}}\n\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}\n\n', 'text/event-stream'],
    ['Chat JSON', '{"choices":[{"index":0,"message":{"role":"assistant","content":"model answer","reasoning_content":"private reasoning"}}]}', 'application/json'],
    ['Chat SSE', 'data: {"choices":[{"index":0,"delta":{"content":"model "}}]}\n\ndata: {"choices":[{"index":0,"delta":{"content":"answer"}}]}\n\ndata: [DONE]\n\n', 'text/event-stream'],
    ['Gemini JSON', '{"candidates":[{"content":{"parts":[{"thought":true,"text":"private reasoning"},{"text":"model answer"}]}}]}', 'application/json']
  ])('opens extracted model text from %s and keeps the raw response collapsed', async (_name, content, contentType) => {
    getDiagnostic.mockResolvedValueOnce(responseWithBody(content, 1, {}, contentType))
    const wrapper = await open()
    const output = wrapper.get('[data-testid="diagnostic-payload-attempt-1-output"]')

    expect(output.get('pre code').text()).toBe('model answer')
    expect(output.get('button[data-testid="diagnostic-toggle-attempt-1-output"]').attributes('aria-expanded')).toBe('true')
    expect(output.text()).not.toContain('private reasoning')
    expect(wrapper.get('[data-testid="diagnostic-toggle-attempt-1-response"]').attributes('aria-expanded')).toBe('false')
    expect(wrapper.text()).toContain('Upstream response body')
    expect(wrapper.text()).not.toContain('Limited upstream response summary')
    expect(wrapper.text()).not.toContain('response-summary-1')
    expect(clipboard).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="diagnostic-copy-attempt-1-output"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenLastCalledWith('model answer')
    await wrapper.get('[data-testid="diagnostic-toggle-attempt-1-response"]').trigger('click')
    await wrapper.get('[data-testid="diagnostic-copy-attempt-1-response"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenLastCalledWith(content)
  })

  it('keeps leading and trailing whitespace in model text and all payload copies', async () => {
    const modelText = '  {"summary":"literal"}\r\n  '
    const raw = ' ' + JSON.stringify({ choices: [{ message: { content: modelText } }] }) + '\r\n'
    const result = responseWithBody(raw)
    result.diagnostic.payload!.inbound.body = payload(' {"input":1} \r\n')
    result.diagnostic.payload!.attempts[0].body = payload('\r\n {"prompt":2} ')
    getDiagnostic.mockResolvedValueOnce(result)
    const wrapper = await open()

    expect(wrapper.get('[data-testid="diagnostic-payload-attempt-1-output"] pre code').element.textContent).toBe(modelText)
    for (const [key, expected] of [
      ['inbound-body', ' {"input":1} \r\n'],
      ['attempt-1-body', '\r\n {"prompt":2} '],
      ['attempt-1-output', modelText],
      ['attempt-1-response', raw]
    ]) {
      await wrapper.get(`[data-testid="diagnostic-copy-${key}"]`).trigger('click')
      await flushPromises()
      expect(clipboard).toHaveBeenLastCalledWith(expected)
    }
  })

  it('shows response truncation and omission metadata alongside extracted partial text', async () => {
    const content = 'data: {"type":"response.output_text.delta","delta":"partial output"}\n\ndata: {"type":"response.output_text.delta","delta":"unfinished'
    getDiagnostic.mockResolvedValueOnce(responseWithBody(content, 1, {
      truncated: true,
      original_bytes: 4096,
      stored_bytes: 200,
      omitted_reason: 'response_read_error'
    }, 'text/event-stream'))
    const wrapper = await open()

    expect(wrapper.get('[data-testid="diagnostic-payload-attempt-1-output"] pre code').text()).toBe('partial output')
    const raw = wrapper.get('[data-testid="diagnostic-payload-attempt-1-response"]')
    expect(raw.text()).toContain('Truncated')
    expect(raw.text()).toContain('response_read_error')
    expect(raw.text()).toContain('4,096 B')
    expect(raw.text()).toContain('200 B')
    await raw.get('button[data-testid="diagnostic-copy-attempt-1-response"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(content)
  })

  it('keeps model text, compression markers and untrusted HTML literal', async () => {
    const modelText = '{"summary":"<img src=x onerror=alert(1)>","marker":"[REDACTED]","path":"C:\\temp"}'
    const content = '  ' + JSON.stringify({ output: [
      { type: 'compaction', encrypted_content: 'opaque-compaction-marker' },
      { type: 'message', content: [{ type: 'output_text', text: modelText }] }
    ] }) + '\r\n'
    getDiagnostic.mockResolvedValueOnce(responseWithBody(content))
    const wrapper = await open()
    const output = wrapper.get('[data-testid="diagnostic-payload-attempt-1-output"]')

    expect(output.get('pre code').text()).toBe(modelText)
    expect(wrapper.find('img').exists()).toBe(false)
    await wrapper.get('[data-testid="diagnostic-toggle-attempt-1-response"]').trigger('click')
    expect(wrapper.get('[data-testid="diagnostic-payload-attempt-1-response"]').text()).toContain('opaque-compaction-marker')
    await wrapper.get('[data-testid="diagnostic-copy-attempt-1-response"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(content)
  })

  it.each([
    'not a JSON response',
    '{"output":[{"type":"compaction","encrypted_content":"opaque"},{"type":"reasoning","summary":[{"type":"summary_text","text":"reasoning only"}]}],"usage":{"output_tokens":10}}'
  ])('keeps an unextractable response available without fabricating model text', async (content) => {
    getDiagnostic.mockResolvedValueOnce(responseWithBody(content, 1, { truncated: true, original_bytes: 8192 }))
    const wrapper = await open()
    const output = wrapper.get('[data-testid="diagnostic-payload-attempt-1-output"]')
    const raw = wrapper.get('[data-testid="diagnostic-payload-attempt-1-response"]')

    expect(output.find('pre').exists()).toBe(false)
    expect(output.get('button[data-testid="diagnostic-copy-attempt-1-output"]').attributes()).toHaveProperty('disabled')
    expect(output.text()).toContain('No model text could be extracted')
    expect(raw.text()).toContain('8,192 B')
    expect(raw.text()).toContain('Truncated')
    await raw.get('button[data-testid="diagnostic-toggle-attempt-1-response"]').trigger('click')
    expect(raw.get('pre code').text()).toContain(content.startsWith('{') ? 'opaque' : content)
    await raw.get('button[data-testid="diagnostic-copy-attempt-1-response"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(content)
  })

  it.each(['null', 'omitted'] as const)('does not substitute an old summary for an explicitly %s response body', async (state) => {
    const result = responseWithBody('', 1, { original_bytes: 10000, omitted_reason: 'snapshot_limit' })
    const attempt = result.diagnostic.payload!.attempts[0]
    attempt.response_summary = payload('{"choices":[{"message":{"content":"summary must not become original"}}]}')
    if (state === 'null') attempt.response_body = null
    getDiagnostic.mockResolvedValueOnce(result)
    const wrapper = await open()
    const raw = wrapper.get('[data-testid="diagnostic-payload-attempt-1-response"]')

    expect(wrapper.find('pre').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('summary must not become original')
    expect(wrapper.text()).not.toContain('Limited upstream response summary')
    expect(raw.get('button[data-testid="diagnostic-copy-attempt-1-response"]').attributes()).toHaveProperty('disabled')
    if (state === 'omitted') {
      expect(raw.text()).toContain('snapshot_limit')
      expect(raw.text()).toContain('10,000 B')
    }
    await raw.get('button[data-testid="diagnostic-toggle-attempt-1-response"]').trigger('click')
    expect(raw.find('pre').exists()).toBe(false)
    expect(clipboard).not.toHaveBeenCalled()
  })

  it('uses a clearly limited summary only for snapshots without the response body field', async () => {
    const result = response()
    const summary = ' {"model":"legacy","usage":{"output_tokens":10}}\n'
    result.diagnostic.payload!.attempts[0].response_summary = payload(summary)
    getDiagnostic.mockResolvedValueOnce(result)
    const wrapper = await open()

    expect(wrapper.text()).toContain('Limited upstream response summary')
    expect(wrapper.text()).toContain('not the full response body or full SSE stream')
    expect(wrapper.get('[data-testid="diagnostic-payload-attempt-1-output"]').find('pre').exists()).toBe(false)
    await wrapper.get('[data-testid="diagnostic-copy-attempt-1-response"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(summary)
  })

  it('uses Chinese output labels without sensitive-content or redaction notices', async () => {
    testLocale.value = 'zh-CN'
    getDiagnostic.mockResolvedValueOnce(responseWithBody('{"choices":[{"message":{"content":"answer"}}]}', 1, { redacted: true }))
    const wrapper = await open()

    expect(wrapper.text()).toContain('模型输出')
    expect(wrapper.text()).toContain('上游响应原文')
    expect(wrapper.text()).not.toContain('敏感内容')
    expect(wrapper.text()).not.toContain('脱敏')
  })

  it('displays all recorded attempts, including errors and a limited response summary', async () => {
    const result = response()
    result.diagnostic.payload!.attempts.unshift({
      id: 5,
      account_id: 10,
      platform: 'openai',
      endpoint: '/v1/chat/completions',
      method: 'POST',
      started_at: '2026-10-03T09:10:08Z',
      duration_ms: 500,
      status: 429,
      error: 'quota exhausted',
      body: payload('first-final-body'),
      response_summary: null
    })
    getDiagnostic.mockResolvedValueOnce(result)
    const wrapper = await open()
    expect(wrapper.findAll('[data-testid^="diagnostic-section-attempt-"]')).toHaveLength(2)
    expect(wrapper.text()).toContain('quota exhausted')
    expect(wrapper.text()).toContain('429')
    expect(wrapper.text()).toContain('No upstream response body was captured.')
    await wrapper.get('[data-testid="diagnostic-toggle-attempt-1-body"]').trigger('click')
    await wrapper.get('[data-testid="diagnostic-toggle-attempt-1-response"]').trigger('click')
    expect(wrapper.text()).toContain('final-body-1')
    expect(wrapper.text()).toContain('response-summary-1')
  })

  it('aborts the previous selection and ignores its late successful response', async () => {
    const old = deferred()
    getDiagnostic.mockReturnValueOnce(old.promise).mockResolvedValueOnce(response(2, 'current-body'))
    const wrapper = await open()
    const previousSignal = getDiagnostic.mock.calls[0][1].signal as AbortSignal
    await wrapper.setProps({ usageId: 2, usage: usage(2) })
    await flushPromises()
    expect(previousSignal.aborted).toBe(true)
    old.resolve(response(1, 'old-body'))
    await flushPromises()

    expect(wrapper.text()).toContain('usage-request-2')
    expect(wrapper.text()).not.toContain('usage-request-1')
    await wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')
    expect(wrapper.get('pre code').text()).toBe('current-body')
    expect(wrapper.text()).not.toContain('old-body')
  })

  it('does not let a stale failure stop the current request loading', async () => {
    const old = deferred()
    const current = deferred()
    getDiagnostic.mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise)
    const wrapper = await open()
    await wrapper.setProps({ usageId: 2, usage: usage(2) })
    old.reject(new Error('obsolete'))
    await flushPromises()
    expect(wrapper.find('[data-testid="diagnostic-loading"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="diagnostic-load-error"]').exists()).toBe(false)
    current.resolve(response(2))
    await flushPromises()
    expect(wrapper.text()).toContain('usage-request-2')
    expect(wrapper.find('[data-testid="diagnostic-loading"]').exists()).toBe(false)
  })

  it('clears old payload and collapsed state before another record loads', async () => {
    getDiagnostic.mockResolvedValueOnce(response(1)).mockResolvedValueOnce(response(2))
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')
    expect(wrapper.text()).toContain('saved-body-1')
    await wrapper.setProps({ usageId: 2, usage: usage(2) })
    await flushPromises()
    expect(wrapper.text()).not.toContain('saved-body-1')
    expect(wrapper.find('pre').exists()).toBe(false)
    expect(wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').attributes('aria-expanded')).toBe('false')
  })

  it('clears payload caches and timers on close and refetches on reopening', async () => {
    vi.useFakeTimers()
    getDiagnostic.mockResolvedValueOnce(response()).mockResolvedValueOnce(response(1, 'new-body'))
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')
    await wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').trigger('click')
    await flushPromises()
    expect(vi.getTimerCount()).toBe(1)
    const internal = wrapper.vm as unknown as { detail: UsageDiagnosticResponse | null; snapshot: unknown; captureSections: unknown[] }
    expect(internal.snapshot).not.toBeNull()
    expect(internal.captureSections).not.toHaveLength(0)
    await wrapper.get('[data-testid="diagnostic-close"]').trigger('click')
    expect(wrapper.emitted('update:show')).toEqual([[false]])
    expect(wrapper.emitted('close')).toHaveLength(1)
    expect(internal.detail).toBeNull()
    expect(internal.snapshot).toBeNull()
    expect(internal.captureSections).toEqual([])
    expect(vi.getTimerCount()).toBe(0)
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(getDiagnostic).toHaveBeenCalledTimes(2)
    expect(wrapper.find('pre').exists()).toBe(false)
  })

  it('aborts close and unmount requests and ignores late responses', async () => {
    const first = deferred()
    getDiagnostic.mockReturnValueOnce(first.promise)
    const wrapper = await open()
    const signal = getDiagnostic.mock.calls[0][1].signal as AbortSignal
    await wrapper.setProps({ show: false })
    expect(signal.aborted).toBe(true)
    first.resolve(response())
    await flushPromises()
    expect((wrapper.vm as unknown as { detail: unknown }).detail).toBeNull()

    const second = deferred()
    getDiagnostic.mockReturnValueOnce(second.promise)
    await wrapper.setProps({ show: true })
    const secondSignal = getDiagnostic.mock.calls[1][1].signal as AbortSignal
    wrapper.unmount()
    expect(secondSignal.aborted).toBe(true)
    second.resolve(response())
    await flushPromises()
  })

  it('shows a generic load failure and supports retry without logging sensitive errors', async () => {
    const logger = vi.spyOn(console, 'error')
    getDiagnostic.mockRejectedValueOnce(new Error('secret-in-error')).mockResolvedValueOnce(response())
    const wrapper = await open()
    expect(wrapper.find('[data-testid="diagnostic-load-error"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('secret-in-error')
    expect(logger).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="diagnostic-retry"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="diagnostic-load-error"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('Snapshot captured')
    logger.mockRestore()
  })

  it('reports clipboard failures only after an explicit copy action', async () => {
    clipboard.mockRejectedValueOnce(new Error('blocked'))
    getDiagnostic.mockResolvedValueOnce(response())
    const wrapper = await open()
    expect(wrapper.text()).not.toContain('Copy failed.')
    await wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Copy failed.')
  })

  it('clears extracted output, expansion and copy timers while switching records', async () => {
    vi.useFakeTimers()
    const current = deferred()
    getDiagnostic.mockResolvedValueOnce(responseWithBody('{"choices":[{"message":{"content":"old output"}}]}')).mockReturnValueOnce(current.promise)
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-toggle-attempt-1-response"]').trigger('click')
    await wrapper.get('[data-testid="diagnostic-copy-attempt-1-output"]').trigger('click')
    await flushPromises()
    expect(vi.getTimerCount()).toBe(1)

    await wrapper.setProps({ usageId: 2, usage: usage(2) })
    expect(wrapper.text()).not.toContain('old output')
    expect(wrapper.findAll('pre')).toHaveLength(0)
    expect(vi.getTimerCount()).toBe(0)
    current.resolve(responseWithBody('{"choices":[{"message":{"content":"current output"}}]}', 2))
    await flushPromises()
    expect(wrapper.get('[data-testid="diagnostic-payload-attempt-1-output"] pre code').text()).toBe('current output')
    expect(wrapper.get('[data-testid="diagnostic-toggle-attempt-1-response"]').attributes('aria-expanded')).toBe('false')
    expect(wrapper.get('[data-testid="diagnostic-copy-attempt-1-output"]').text()).not.toContain('Copied')

    await wrapper.get('[data-testid="diagnostic-close"]').trigger('click')
    expect((wrapper.vm as unknown as { captureSections: unknown[] }).captureSections).toEqual([])
    expect(wrapper.findAll('pre')).toHaveLength(0)
  })

  it.each(['resolve', 'reject'] as const)('ignores a late clipboard %s after switching records', async (settle) => {
    vi.useFakeTimers()
    const pending = deferred<void>()
    clipboard.mockReturnValueOnce(pending.promise)
    getDiagnostic.mockResolvedValueOnce(responseWithBody('{"choices":[{"message":{"content":"old output"}}]}'))
      .mockResolvedValueOnce(responseWithBody('{"choices":[{"message":{"content":"current output"}}]}', 2))
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-copy-attempt-1-output"]').trigger('click')
    await wrapper.setProps({ usageId: 2, usage: usage(2) })
    await flushPromises()
    if (settle === 'resolve') pending.resolve()
    else pending.reject(new Error('obsolete clipboard failure'))
    await flushPromises()

    expect(wrapper.text()).toContain('current output')
    expect(wrapper.text()).not.toContain('Copy failed.')
    expect(wrapper.get('[data-testid="diagnostic-copy-attempt-1-output"]').text()).not.toContain('Copied')
    expect(vi.getTimerCount()).toBe(0)
  })
})

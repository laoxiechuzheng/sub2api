import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { defineComponent, ref } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AdminUsageLog } from '@/types'
import type { UsageDiagnosticPayload, UsageDiagnosticResponse, UsageDiagnosticStatus } from '@/api/admin/usageDiagnostic'
import UsageDiagnosticModal from '../UsageDiagnosticModal.vue'

const { getDiagnostic, clipboard } = vi.hoisted(() => ({ getDiagnostic: vi.fn(), clipboard: vi.fn() }))
vi.mock('@/api/admin/usageDiagnostic', () => ({ getUsageDiagnostic: getDiagnostic }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ locale: ref('en') }) }))
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
function deferred() {
  let resolve!: (value: UsageDiagnosticResponse) => void
  let reject!: (error: Error) => void
  const promise = new Promise<UsageDiagnosticResponse>((res, rej) => { resolve = res; reject = rej })
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

  it('pretty-prints a synthetic JSON payload for display and copy without rendering HTML', async () => {
    const content = JSON.stringify({ message: '<strong>plain</strong>', nested: { ok: true } })
    const formatted = JSON.stringify(JSON.parse(content), null, 2)
    getDiagnostic.mockResolvedValueOnce(response(1, content))
    const wrapper = await open()
    await wrapper.get('[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')

    expect(wrapper.get('pre code').text()).toBe(formatted)
    expect(wrapper.find('strong').exists()).toBe(false)

    await wrapper.get('[data-testid="diagnostic-copy-inbound-body"]').trigger('click')
    await flushPromises()
    expect(clipboard).toHaveBeenCalledWith(formatted)
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
    expect(clipboard).toHaveBeenCalledWith(formatted)
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
    expect(clipboard).toHaveBeenCalledWith(formatted)
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

  it('shows redaction, truncation, omissions, byte counts and incomplete attempts', async () => {
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
    expect(block.text()).toContain('Partially redacted')
    expect(block.text()).toContain('Truncated')
    expect(block.text()).toContain('body_limit')
    expect(wrapper.get('[data-testid="diagnostic-dropped-attempts"]').text()).toContain('2')
    expect(block.get('button[data-testid="diagnostic-copy-inbound-body"]').attributes()).toHaveProperty('disabled')
    await block.get('button[data-testid="diagnostic-toggle-inbound-body"]').trigger('click')
    expect(block.text()).toContain('No saved content.')
    expect(wrapper.text()).toContain('Redaction is not full anonymization')
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
    expect(wrapper.text()).toContain('No upstream response summary was captured.')
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
})

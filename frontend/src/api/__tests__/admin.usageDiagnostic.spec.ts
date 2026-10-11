import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get } }))

import { getUsageDiagnostic } from '@/api/admin/usageDiagnostic'

describe('admin usage diagnostic API', () => {
  beforeEach(() => get.mockReset())

  it('requests only the selected admin diagnostic and passes cancellation through', async () => {
    const response = {
      usage: { id: 42 },
      capture_enabled: true,
      retention_hours: 24,
      diagnostic: { status: 'not_captured', payload: null }
    }
    const controller = new AbortController()
    get.mockResolvedValueOnce({ data: response })

    await expect(getUsageDiagnostic(42, { signal: controller.signal })).resolves.toEqual(response)
    expect(get).toHaveBeenCalledOnce()
    expect(get).toHaveBeenCalledWith('/admin/usage/42/diagnostic', { signal: controller.signal })
  })

  it('preserves unavailable and expired statuses without inventing content', async () => {
    const response = { diagnostic: { status: 'expired' }, capture_enabled: false, retention_hours: 24 }
    get.mockResolvedValueOnce({ data: response })
    await expect(getUsageDiagnostic(3)).resolves.toBe(response)
    expect(get).toHaveBeenCalledWith('/admin/usage/3/diagnostic', { signal: undefined })
  })

  it('lets authorization and network failures reach the modal', async () => {
    const error = new Error('Forbidden')
    get.mockRejectedValueOnce(error)
    await expect(getUsageDiagnostic(7)).rejects.toBe(error)
  })

  it.each([undefined, null, {
    content: '  data: {"choices":[{"delta":{"content":"raw [REDACTED]"}}]}\r\n\r\n',
    original_bytes: 2048,
    stored_bytes: 75,
    redacted: false,
    truncated: true,
    omitted_reason: 'snapshot_limit'
  }])('preserves raw response bodies, content types and legacy field absence', async (body) => {
    const attempt = {
      id: 1,
      response_content_type: 'text/event-stream; charset=utf-8',
      response_summary: { content: 'legacy summary' },
      ...(body === undefined ? {} : { response_body: body })
    }
    const response = { diagnostic: { status: 'captured', payload: { attempts: [attempt] } } }
    get.mockResolvedValueOnce({ data: response })

    const result = await getUsageDiagnostic(42)

    expect(result).toBe(response)
    expect(result.diagnostic.payload!.attempts[0]).toBe(attempt)
    expect(result.diagnostic.payload!.attempts[0].response_body).toBe(body)
    expect(result.diagnostic.payload!.attempts[0].response_content_type).toBe('text/event-stream; charset=utf-8')
    if (body === undefined) expect(result.diagnostic.payload!.attempts[0]).not.toHaveProperty('response_body')
  })
})

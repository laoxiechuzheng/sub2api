import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { nextTick, reactive } from 'vue'
import { getUserBreakdown, type UserBreakdownResponse } from '@/api/admin/dashboard'
import UserBreakdownSubTable from '../UserBreakdownSubTable.vue'
import EndpointDistributionChart from '../EndpointDistributionChart.vue'

enableAutoUnmount(afterEach)
vi.mock('@/api/admin/dashboard', () => ({ getUserBreakdown: vi.fn() }))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key === 'admin.dashboard.noDataAvailable' ? 'No data available' : key,
    }),
  }
})

vi.mock('vue-chartjs', () => ({
  Doughnut: { template: '<div />' },
}))

describe('EndpointDistributionChart', () => {
  const endpointStats = [
    { endpoint: '/v1/responses', requests: 8, total_tokens: 1000, cost: 1.5, actual_cost: 0.2 },
    { endpoint: '/v1/messages', requests: 3, total_tokens: 500, cost: 0.5, actual_cost: 1.4 },
  ]

  describe('breakdown query isolation', () => {
    beforeEach(() => {
      vi.mocked(getUserBreakdown).mockReset()
    })

    const response = (email: string): UserBreakdownResponse => ({
      users: [{
        user_id: 7,
        email,
        requests: 2,
        input_tokens: 100,
        output_tokens: 50,
        cache_tokens: 0,
        total_tokens: 150,
        cost: 0.2,
        actual_cost: 0.1,
        account_cost: 0.05,
      }],
      start_date: '2026-10-25',
      end_date: '2026-10-26',
    })

    const deferred = () => {
      let resolve!: (value: UserBreakdownResponse) => void
      let reject!: (reason: Error) => void
      const promise = new Promise<UserBreakdownResponse>((resolvePromise, rejectPromise) => {
        resolve = resolvePromise
        reject = rejectPromise
      })
      return { promise, resolve, reject }
    }

    const mountBreakdown = (filters: Record<string, unknown> = {}) => mount(EndpointDistributionChart, {
      props: {
        endpointStats,
        upstreamEndpointStats: endpointStats,
        endpointPathStats: endpointStats,
        startDate: '2026-10-25',
        endDate: '2026-10-26',
        filters,
      },
      global: { stubs: { LoadingSpinner: true } },
    })

    type Wrapper = ReturnType<typeof mountBreakdown>
    const clickRow = (wrapper: Wrapper, label = '/v1/responses') =>
      wrapper.get('td[title="' + label + '"]').trigger('click')

    const expectPending = (wrapper: Wrapper) => {
      const breakdown = wrapper.getComponent(UserBreakdownSubTable)
      expect(breakdown.props('loading')).toBe(true)
      expect(breakdown.props('items')).toEqual([])
      expect(breakdown.find('loading-spinner-stub').exists()).toBe(true)
      expect(wrapper.text()).not.toContain('old@example.com')
    }

    const replacePendingRequest = async (
      wrapper: Wrapper,
      filters: Record<string, unknown>,
      action: 'selection' | 'reopen' | 'filters',
    ) => {
      if (action === 'selection') {
        await clickRow(wrapper, '/v1/messages')
      } else {
        if (action === 'filters') {
          filters.composite_route = 'fallback'
          await nextTick()
        } else {
          await clickRow(wrapper)
        }
        expect(wrapper.findComponent(UserBreakdownSubTable).exists()).toBe(false)
        await clickRow(wrapper)
      }
    }

    it.each([
      ['composite route routed to fallback', { composite_route: 'fallback' }],
      ['requested model', { requested_model: 'requested-b' }],
      ['billing mode', { billing_mode: 'per_request' }],
    ] as const)('clears loaded users on in-place %s changes with the same row still present', async (_, changed) => {
      const filters = reactive<Record<string, unknown>>({
        composite_route: 'routed',
        requested_model: 'requested-a',
        billing_mode: 'token',
      })
      vi.mocked(getUserBreakdown).mockResolvedValueOnce(response('old@example.com'))
      const wrapper = mountBreakdown(filters)
      await clickRow(wrapper)
      await flushPromises()
      expect(wrapper.text()).toContain('old@example.com')

      Object.assign(filters, changed)
      await nextTick()
      expect(wrapper.find('td[title="/v1/responses"]').exists()).toBe(true)
      expect(wrapper.findComponent(UserBreakdownSubTable).exists()).toBe(false)
      expect(wrapper.text()).not.toContain('old@example.com')

      vi.mocked(getUserBreakdown).mockResolvedValueOnce(response('new@example.com'))
      await clickRow(wrapper)
      await flushPromises()
      expect(getUserBreakdown).toHaveBeenLastCalledWith(expect.objectContaining({
        ...filters,
        start_date: '2026-10-25',
        end_date: '2026-10-26',
        endpoint: '/v1/responses',
        endpoint_type: 'inbound',
      }))
      expect(wrapper.text()).toContain('new@example.com')
    })

    it.each([
      ['start date', { startDate: '2026-10-24' }],
      ['end date', { endDate: '2026-10-27' }],
      ['disabled breakdown', { enableBreakdown: false }],
      ['upstream endpoint source', { source: 'upstream' }],
      ['path endpoint source', { source: 'path' }],
    ] as const)('clears loaded users when %s changes with the same row still present', async (_, props) => {
      vi.mocked(getUserBreakdown).mockResolvedValueOnce(response('old@example.com'))
      const wrapper = mountBreakdown()
      await clickRow(wrapper)
      await flushPromises()
      expect(wrapper.text()).toContain('old@example.com')

      await wrapper.setProps(props)
      expect(wrapper.find('td[title="/v1/responses"]').exists()).toBe(true)
      expect(wrapper.findComponent(UserBreakdownSubTable).exists()).toBe(false)
      expect(wrapper.text()).not.toContain('old@example.com')
    })

    describe.each(['selection', 'reopen', 'filters'] as const)('after %s', (action) => {
      it.each(['success', 'error'] as const)('keeps new users when the old request returns a late %s', async (outcome) => {
        const oldRequest = deferred()
        vi.mocked(getUserBreakdown)
          .mockReturnValueOnce(oldRequest.promise)
          .mockResolvedValueOnce(response('new@example.com'))
        const filters = reactive({ composite_route: 'routed' })
        const wrapper = mountBreakdown(filters)
        await clickRow(wrapper)
        expectPending(wrapper)
        await replacePendingRequest(wrapper, filters, action)
        await flushPromises()
        expect(wrapper.text()).toContain('new@example.com')

        if (outcome === 'success') oldRequest.resolve(response('old@example.com'))
        else oldRequest.reject(new Error('stale request failed'))
        await flushPromises()
        expect(wrapper.text()).toContain('new@example.com')
        expect(wrapper.text()).not.toContain('old@example.com')
        expect(wrapper.getComponent(UserBreakdownSubTable).props('loading')).toBe(false)
      })

      it.each(['success', 'error'] as const)('keeps new loading when the old request finishes with %s', async (outcome) => {
        const oldRequest = deferred()
        const newRequest = deferred()
        vi.mocked(getUserBreakdown)
          .mockReturnValueOnce(oldRequest.promise)
          .mockReturnValueOnce(newRequest.promise)
        const filters = reactive({ composite_route: 'routed' })
        const wrapper = mountBreakdown(filters)
        await clickRow(wrapper)
        await replacePendingRequest(wrapper, filters, action)
        expectPending(wrapper)

        if (outcome === 'success') oldRequest.resolve(response('old@example.com'))
        else oldRequest.reject(new Error('stale request failed'))
        await flushPromises()
        expectPending(wrapper)

        newRequest.resolve(response('new@example.com'))
        await flushPromises()
        expect(wrapper.text()).toContain('new@example.com')
        expect(wrapper.getComponent(UserBreakdownSubTable).props('loading')).toBe(false)
      })
    })

    it('does not restore closed users when a pending request completes', async () => {
      const pending = deferred()
      vi.mocked(getUserBreakdown).mockReturnValueOnce(pending.promise)
      const wrapper = mountBreakdown()
      await clickRow(wrapper)
      await clickRow(wrapper)
      expect(wrapper.findComponent(UserBreakdownSubTable).exists()).toBe(false)
      pending.resolve(response('old@example.com'))
      await flushPromises()
      expect(wrapper.findComponent(UserBreakdownSubTable).exists()).toBe(false)
      expect(wrapper.text()).not.toContain('old@example.com')
    })

    it('ends loading and shows no data when the current request fails', async () => {
      vi.mocked(getUserBreakdown).mockRejectedValueOnce(new Error('current request failed'))
      const wrapper = mountBreakdown()
      await clickRow(wrapper)
      await flushPromises()
      const breakdown = wrapper.getComponent(UserBreakdownSubTable)
      expect(breakdown.props('loading')).toBe(false)
      expect(breakdown.props('items')).toEqual([])
      expect(breakdown.text()).toContain('No data available')
    })
  })

})

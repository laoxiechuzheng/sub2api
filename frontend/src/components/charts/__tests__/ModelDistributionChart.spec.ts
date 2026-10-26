import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { nextTick, reactive } from 'vue'
import { getUserBreakdown, type UserBreakdownResponse } from '@/api/admin/dashboard'
import UserBreakdownSubTable from '../UserBreakdownSubTable.vue'

import ModelDistributionChart from '../ModelDistributionChart.vue'

enableAutoUnmount(afterEach)
vi.mock('@/api/admin/dashboard', () => ({ getUserBreakdown: vi.fn() }))

const messages: Record<string, string> = {
  'admin.dashboard.modelDistribution': 'Model Distribution',
  'admin.dashboard.spendingRankingTitle': 'User Spending Ranking',
  'admin.dashboard.viewModelDistribution': 'Model Distribution',
  'admin.dashboard.viewSpendingRanking': 'User Spending Ranking',
  'admin.dashboard.spendingRankingUser': 'User',
  'admin.dashboard.spendingRankingRequests': 'Requests',
  'admin.dashboard.spendingRankingTokens': 'Tokens',
  'admin.dashboard.spendingRankingSpend': 'Spend',
  'admin.dashboard.spendingRankingOther': 'Others',
  'admin.dashboard.model': 'Model',
  'admin.dashboard.requests': 'Requests',
  'admin.dashboard.tokens': 'Tokens',
  'admin.dashboard.actual': 'Actual',
  'admin.dashboard.accountCost': 'Account Cost',
  'admin.dashboard.standard': 'Standard',
  'admin.dashboard.metricTokens': 'By Tokens',
  'admin.dashboard.metricActualCost': 'By Actual Cost',
  'admin.dashboard.noDataAvailable': 'No data available',
  'admin.redeem.userPrefix': 'User #{id}',
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        const message = messages[key] ?? key
        return params
          ? message.replace(/\{(\w+)\}/g, (_, name: string) => String(params[name]))
          : message
      },
    }),
  }
})

vi.mock('vue-chartjs', () => ({
  Doughnut: {
    props: ['data'],
    template: '<div class="chart-data">{{ JSON.stringify(data) }}</div>',
  },
}))

describe('ModelDistributionChart', () => {
  const modelStats = [
    {
      model: 'model-a',
      requests: 8,
      input_tokens: 100,
      output_tokens: 50,
      cache_creation_tokens: 0,
      cache_read_tokens: 0,
      total_tokens: 1000,
      cost: 1.5,
      actual_cost: 0.2,
    },
    {
      model: 'model-b',
      requests: 3,
      input_tokens: 40,
      output_tokens: 20,
      cache_creation_tokens: 0,
      cache_read_tokens: 0,
      total_tokens: 500,
      cost: 0.5,
      actual_cost: 1.4,
    },
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

    const mountBreakdown = (filters: Record<string, unknown> = {}) => mount(ModelDistributionChart, {
      props: {
        modelStats,
        upstreamModelStats: modelStats,
        mappingModelStats: modelStats,
        startDate: '2026-10-25',
        endDate: '2026-10-26',
        filters,
      },
      global: { stubs: { LoadingSpinner: true } },
    })

    type Wrapper = ReturnType<typeof mountBreakdown>
    const clickRow = (wrapper: Wrapper, label = 'model-a') =>
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
        await clickRow(wrapper, 'model-b')
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
      expect(wrapper.find('td[title="model-a"]').exists()).toBe(true)
      expect(wrapper.findComponent(UserBreakdownSubTable).exists()).toBe(false)
      expect(wrapper.text()).not.toContain('old@example.com')

      vi.mocked(getUserBreakdown).mockResolvedValueOnce(response('new@example.com'))
      await clickRow(wrapper)
      await flushPromises()
      expect(getUserBreakdown).toHaveBeenLastCalledWith(expect.objectContaining({
        ...filters,
        start_date: '2026-10-25',
        end_date: '2026-10-26',
        model: 'model-a',
        model_source: 'requested',
      }))
      expect(wrapper.text()).toContain('new@example.com')
    })

    it.each([
      ['start date', { startDate: '2026-10-24' }],
      ['end date', { endDate: '2026-10-27' }],
      ['disabled breakdown', { enableBreakdown: false }],
      ['upstream model source', { source: 'upstream' }],
      ['mapping model source', { source: 'mapping' }],
    ] as const)('clears loaded users when %s changes with the same row still present', async (_, props) => {
      vi.mocked(getUserBreakdown).mockResolvedValueOnce(response('old@example.com'))
      const wrapper = mountBreakdown()
      await clickRow(wrapper)
      await flushPromises()
      expect(wrapper.text()).toContain('old@example.com')

      await wrapper.setProps(props)
      expect(wrapper.find('td[title="model-a"]').exists()).toBe(true)
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

  it('uses total_tokens and token ordering by default', () => {
    const wrapper = mount(ModelDistributionChart, {
      props: {
        modelStats,
      },
      global: {
        stubs: {
          LoadingSpinner: true,
        },
      },
    })

    const chartData = JSON.parse(wrapper.find('.chart-data').text())
    expect(chartData.labels).toEqual(['model-a', 'model-b'])
    expect(chartData.datasets[0].data).toEqual([1000, 500])

    const rows = wrapper.findAll('tbody tr')
    expect(rows[0].text()).toContain('model-a')
    expect(rows[1].text()).toContain('model-b')

    const options = (wrapper.vm as any).$?.setupState.doughnutOptions
    const label = options.plugins.tooltip.callbacks.label({
      label: 'model-a',
      raw: 1000,
      dataset: { data: [1000, 500] },
    })
    expect(label).toBe('model-a: 1.00K (66.7%)')
  })

  it('uses actual_cost and reorders rows in actual cost mode', () => {
    const wrapper = mount(ModelDistributionChart, {
      props: {
        modelStats,
        metric: 'actual_cost',
      },
      global: {
        stubs: {
          LoadingSpinner: true,
        },
      },
    })

    const chartData = JSON.parse(wrapper.find('.chart-data').text())
    expect(chartData.labels).toEqual(['model-b', 'model-a'])
    expect(chartData.datasets[0].data).toEqual([1.4, 0.2])

    const rows = wrapper.findAll('tbody tr')
    expect(rows[0].text()).toContain('model-b')
    expect(rows[1].text()).toContain('model-a')

    const options = (wrapper.vm as any).$?.setupState.doughnutOptions
    const label = options.plugins.tooltip.callbacks.label({
      label: 'model-b',
      raw: 1.4,
      dataset: { data: [1.4, 0.2] },
    })
    expect(label).toBe('model-b: $1.40 (87.5%)')
  })

  it('can hide account cost for user usage stats without account_cost', () => {
    const wrapper = mount(ModelDistributionChart, {
      props: {
        modelStats,
        showAccountCost: false,
      },
      global: {
        stubs: {
          LoadingSpinner: true,
        },
      },
    })

    expect(wrapper.text()).not.toContain('Account Cost')
    expect(wrapper.findAll('thead th')).toHaveLength(5)
    expect(wrapper.findAll('tbody tr')[0].findAll('td')).toHaveLength(5)
  })

  it('uses the dashboard user label policy and renders Others with a dedicated chart color', async () => {
    const wrapper = mount(ModelDistributionChart, {
      props: {
        modelStats: [],
        enableRankingView: true,
        rankingItems: [
          { user_id: 1, email: 'alpha@example.com', username: 'alpha', actual_cost: 12, requests: 10, tokens: 1000 },
          { user_id: 2, email: 'beta@example.com', username: '   ', actual_cost: 8, requests: 6, tokens: 600 },
          { user_id: 3, email: '   ', username: '', actual_cost: 0, requests: 0, tokens: 0 },
        ],
        rankingTotalActualCost: 30,
        rankingTotalRequests: 20,
        rankingTotalTokens: 2000,
      },
      global: {
        stubs: {
          LoadingSpinner: true,
        },
      },
    })

    const rankingButton = wrapper.findAll('button').find((button) => button.text() === 'User Spending Ranking')
    expect(rankingButton).toBeTruthy()
    await rankingButton!.trigger('click')

    const chartData = JSON.parse(wrapper.find('.chart-data').text())
    expect(chartData.labels).toEqual([
      '#1 alpha',
      '#2 beta@example.com',
      '#3 User #3',
      'Others',
    ])
    expect(chartData.datasets[0].data).toEqual([12, 8, 0, 10])
    expect(chartData.datasets[0].backgroundColor[0]).toBe('#3b82f6')
    expect(chartData.datasets[0].backgroundColor[3]).toBe('#94a3b8')
    expect(chartData.datasets[0].backgroundColor[3]).not.toBe(chartData.datasets[0].backgroundColor[0])

    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(4)
    expect(rows[0].text()).toContain('alpha')
    expect(rows[0].text()).not.toContain('alpha@example.com')
    expect(rows[1].text()).toContain('beta@example.com')
    expect(rows[2].text()).toContain('User #3')
    expect(rows[3].text()).toContain('Others')
    expect(rows[3].text()).toContain('4')
    expect(rows[3].text()).toContain('400')
    expect(rows[3].text()).toContain('$10.00')
  })
})

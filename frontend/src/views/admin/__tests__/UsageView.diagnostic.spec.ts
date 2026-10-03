import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { ref } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import UsageView from '../UsageView.vue'
import type { AdminUsageLog } from '@/types'

const { list, getStats, getModelStats, getSnapshotV2, getById, getDiagnostic } = vi.hoisted(() => ({
  list: vi.fn(), getStats: vi.fn(), getModelStats: vi.fn(), getSnapshotV2: vi.fn(), getById: vi.fn(), getDiagnostic: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: {
  usage: { list, getStats }, dashboard: { getModelStats, getSnapshotV2 }, users: { getById }
} }))
vi.mock('@/api/admin/usageDiagnostic', () => ({ getUsageDiagnostic: getDiagnostic }))
vi.mock('@/api/admin/ops', () => ({ listErrorLogs: vi.fn() }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('vue-router', () => ({ useRoute: () => ({ query: {} }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key, locale: ref('en') })
}))

const UsageTableStub = {
  props: ['data', 'showDiagnostic'],
  emits: ['diagnosticClick', 'userClick', 'sort'],
  template: '<div data-testid="usage-table-stub"><button data-testid="open-diagnostic" @click="$emit(\'diagnosticClick\', data[0])">diagnostic</button></div>'
}
const BaseDialogStub = { props: ['show', 'title'], template: '<div v-if="show"><h3>{{ title }}</h3><slot /><slot name="footer" /></div>' }
const global = { stubs: {
  AppLayout: { template: '<div><slot /></div>' }, UsageStatsCards: true, UsageFilters: true,
  UsageTable: UsageTableStub, UsageExportProgress: true, UsageCleanupDialog: true,
  UserBalanceHistoryModal: true, Pagination: true, Select: true, DateRangePicker: true, Icon: true,
  TokenUsageTrend: true, ModelDistributionChart: true, GroupDistributionChart: true,
  EndpointDistributionChart: true, UserTokenRanking: true, OpsErrorLogTable: true, OpsErrorDetailModal: true,
  BaseDialog: BaseDialogStub
} }
const row = { id: 44, user_id: 3, api_key_id: 2, request_id: 'usage-44', model: 'test-model', created_at: '2026-10-03T09:00:00Z' } as AdminUsageLog

enableAutoUnmount(afterEach)
beforeEach(() => {
  vi.useFakeTimers()
  list.mockReset().mockResolvedValue({ items: [row], total: 1, pages: 1 })
  getStats.mockReset().mockResolvedValue({})
  getModelStats.mockReset().mockResolvedValue({ models: [] })
  getSnapshotV2.mockReset().mockResolvedValue({ trend: [], groups: [] })
  getById.mockReset()
  getDiagnostic.mockReset().mockResolvedValue({ usage: row, capture_enabled: true, retention_hours: 24, diagnostic: { status: 'not_captured', payload: null } })
})
afterEach(() => vi.useRealTimers())

describe('admin UsageView diagnostic wiring', () => {
  it('enables the admin action and requests a diagnostic only when a row is selected', async () => {
    const wrapper = mount(UsageView, { global })
    await flushPromises()
    expect(wrapper.findComponent(UsageTableStub).props('showDiagnostic')).toBe(true)
    expect(getDiagnostic).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="open-diagnostic"]').trigger('click')
    await flushPromises()
    expect(getDiagnostic).toHaveBeenCalledWith(44, { signal: expect.any(AbortSignal) })
    expect(wrapper.text()).toContain('Not captured')
    expect(wrapper.text()).toContain('Historical content cannot be reconstructed')
    expect(getById).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="diagnostic-close"]').trigger('click')
    expect(wrapper.find('[data-testid="diagnostic-status"]').exists()).toBe(false)
    const internal = wrapper.vm as unknown as { selectedDiagnosticUsage: unknown; showUsageDiagnosticModal: boolean }
    expect(internal.selectedDiagnosticUsage).toBeNull()
    expect(internal.showUsageDiagnosticModal).toBe(false)
  })
})

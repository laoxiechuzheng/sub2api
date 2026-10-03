import { enableAutoUnmount, mount } from '@vue/test-utils'
import { ref } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'
import UsageTable from '../UsageTable.vue'
import type { AdminUsageLog } from '@/types'
import type { Column } from '@/components/common/types'

vi.mock('@/utils/ipGeoLookup', () => ({ getEntry: vi.fn(() => ({ status: 'idle' })), fetchBatch: vi.fn() }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key, locale: ref('en') })
}))

const DataTableStub = {
  props: ['data', 'columns', 'serverSideSort', 'defaultSortKey', 'defaultSortOrder'],
  emits: ['sort'],
  template: `
    <div>
      <button data-testid="sort-model" @click="$emit('sort', 'model', 'asc')">sort</button>
      <div v-for="row in data" :key="row.id">
        <slot v-for="column in columns" :name="'cell-' + column.key" :row="row" :value="row[column.key]" />
      </div>
    </div>
  `
}
const global = { stubs: { DataTable: DataTableStub, Icon: true, EmptyState: true, Teleport: true } }
const columns: Column[] = [{ key: 'user', label: 'User' }, { key: 'model', label: 'Model', sortable: true }]
const rows = [
  { id: 1, user_id: 7, user: { email: 'user@example.com' }, model: 'model-a', request_id: 'new-request' },
  { id: 2, user_id: 7, user: { email: 'user@example.com' }, model: 'model-b', request_id: '' }
] as AdminUsageLog[]

enableAutoUnmount(afterEach)

describe('UsageTable admin-only diagnostic action', () => {
  it('does not add or display diagnostics by default for user page reuse', () => {
    const wrapper = mount(UsageTable, { props: { data: rows, columns }, global })
    expect(wrapper.find('[data-testid="usage-diagnostic-button"]').exists()).toBe(false)
    expect(wrapper.findComponent(DataTableStub).props('columns')).toEqual(columns)
  })

  it('adds an unsortable diagnostic action for every row, including older records', async () => {
    const wrapper = mount(UsageTable, { props: { data: rows, columns, showDiagnostic: true }, global })
    expect(wrapper.findComponent(DataTableStub).props('columns')).toEqual([
      ...columns, { key: 'actions', label: 'View diagnostic', sortable: false }
    ])
    expect(columns).toHaveLength(2)
    const buttons = wrapper.findAll('[data-testid="usage-diagnostic-button"]')
    expect(buttons).toHaveLength(2)
    expect(buttons[1].attributes('aria-label')).toBe('View diagnostic #2')
    await buttons[1].trigger('click')
    expect(wrapper.emitted('diagnosticClick')).toEqual([[rows[1]]])
    expect(wrapper.emitted('userClick')).toBeUndefined()
  })

  it('preserves user balance click and sorting behavior', async () => {
    const wrapper = mount(UsageTable, {
      props: { data: rows, columns, showDiagnostic: true, serverSideSort: true, defaultSortKey: 'created_at', defaultSortOrder: 'desc' },
      global
    })
    await wrapper.findAll('button').find((button) => button.text() === 'user@example.com')!.trigger('click')
    expect(wrapper.emitted('userClick')).toEqual([[7, 'user@example.com']])
    expect(wrapper.emitted('diagnosticClick')).toBeUndefined()
    await wrapper.get('[data-testid="sort-model"]').trigger('click')
    expect(wrapper.emitted('sort')).toEqual([['model', 'asc']])
    const table = wrapper.findComponent(DataTableStub)
    expect(table.props('serverSideSort')).toBe(true)
    expect(table.props('defaultSortKey')).toBe('created_at')
    expect(table.props('defaultSortOrder')).toBe('desc')
  })

  it('removes the action when diagnostic visibility is disabled', async () => {
    const wrapper = mount(UsageTable, { props: { data: rows, columns, showDiagnostic: true }, global })
    await wrapper.setProps({ showDiagnostic: false })
    expect(wrapper.find('[data-testid="usage-diagnostic-button"]').exists()).toBe(false)
    expect(wrapper.findComponent(DataTableStub).props('columns')).toEqual(columns)
  })

  it('does not duplicate an existing action column', () => {
    const withActions = [...columns, { key: 'actions', label: 'Actions', sortable: false }]
    const wrapper = mount(UsageTable, { props: { data: rows, columns: withActions, showDiagnostic: true }, global })
    expect(wrapper.findComponent(DataTableStub).props('columns')).toEqual(withActions)
  })
})

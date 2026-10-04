import { defineComponent } from 'vue'
import { DOMWrapper, flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { baseCompile } from '@intlify/message-compiler'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'
import type { AdminGroup, CompositeModelRoute, CompositeRouteDecision } from '@/types'
import GroupsView from '@/views/admin/GroupsView.vue'
import { adminAPI } from '@/api/admin'

const {
  listGroups, listCompositeRoutes, createCompositeRoute, updateCompositeRoute,
  deleteCompositeRoute, previewCompositeRoute, showSuccess, showError
} = vi.hoisted(() => ({
  listGroups: vi.fn(), listCompositeRoutes: vi.fn(), createCompositeRoute: vi.fn(),
  updateCompositeRoute: vi.fn(), deleteCompositeRoute: vi.fn(), previewCompositeRoute: vi.fn(),
  showSuccess: vi.fn(), showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    groups: {
      list: listGroups,
      getAll: vi.fn().mockResolvedValue([]),
      getModelAllowlistCandidates: vi.fn().mockResolvedValue([]),
      getUsageSummary: vi.fn().mockResolvedValue([]),
      getCapacitySummary: vi.fn().mockResolvedValue([]),
      getLiveCapability: vi.fn().mockResolvedValue({ supported: false }),
      listCompositeRoutes, createCompositeRoute, updateCompositeRoute,
      deleteCompositeRoute, previewCompositeRoute
    },
    accounts: { list: vi.fn(), getById: vi.fn() }
  }
}))

vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess, showError }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isSimpleMode: false }) }))
vi.mock('@/stores/onboarding', () => ({
  useOnboardingStore: () => ({ isCurrentStep: () => false, nextStep: vi.fn() })
}))

const compositeGroup: AdminGroup = {
  id: 42, name: 'Composite', description: null, platform: 'composite',
  rate_multiplier: 1, rpm_limit: 0, is_exclusive: false, status: 'active',
  subscription_type: 'standard', daily_limit_usd: null, weekly_limit_usd: null, monthly_limit_usd: null,
  long_context_pricing_enabled: true, force_openai_fast: false, free_openai_fast: false,
  model_pricing: [], profit_control_enabled: false, profit_min_margin: 0, profit_safety_buffer: 0,
  allow_image_generation: false, allow_batch_image_generation: false,
  image_rate_independent: false, image_rate_multiplier: 1,
  batch_image_discount_multiplier: 0.5, batch_image_hold_multiplier: 0.6,
  image_price_1k: null, image_price_2k: null, image_price_4k: null,
  video_rate_independent: false, video_rate_multiplier: 1,
  video_price_480p: null, video_price_720p: null, video_price_1080p: null,
  web_search_price_per_call: null, search_price_per_1k: null,
  audio_realtime_price_per_min: null, audio_tts_price_per_million_chars: null, audio_stt_price_per_hour: null,
  peak_rate_enabled: false, peak_start: '', peak_end: '', peak_rate_multiplier: 1,
  claude_code_only: false, fallback_group_id: null, fallback_group_id_on_invalid_request: null,
  allow_messages_dispatch: false, allow_live: false, require_oauth_only: false, require_privacy_set: false,
  created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z',
  model_routing: null, model_routing_enabled: false, mcp_xml_inject: true,
  supported_model_scopes: [], account_count: 1, active_account_count: 1,
  rate_limited_account_count: 0, sort_order: 10
}

// Old responses intentionally omit all precise-routing fields.
const legacyRoute: CompositeModelRoute = {
  id: 7, group_id: 42, public_model: 'public-model', match_type: 'prefix',
  target_platform: 'deepseek', upstream_model: 'upstream-model', endpoint: 'responses',
  user_agent_contains: 'codex', body_contains: 'checkpoint', priority: 3,
  enabled: true, notes: 'existing route'
}

const legacyDecision: CompositeRouteDecision = {
  matched: true, source: 'route', group_id: 42, public_model: 'public-model',
  target_platform: 'deepseek', upstream_model: 'upstream-model', endpoint: 'responses', route: legacyRoute
}

const AppLayoutStub = defineComponent({ template: '<main><slot /></main>' })
const TablePageLayoutStub = defineComponent({
  template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>'
})
const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-actions" :row="row" /></div></div>'
})
const BaseDialogStub = defineComponent({
  props: { show: Boolean },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

let wrapper: VueWrapper

async function openRoutes(locale = 'en') {
  wrapper = mount(GroupsView, {
    attachTo: document.body,
    global: {
      plugins: [createI18n({
        legacy: false, locale, messages: { en, zh }, warnHtmlMessage: false,
        // The runtime-only i18n build needs compiled, trusted locale fixtures in tests.
        messageCompiler: (message) => {
          if (typeof message !== 'string') throw new Error('Expected a locale string')
          return new Function(`return ${baseCompile(message).code}`)()
        }
      })],
      stubs: {
        AppLayout: AppLayoutStub, TablePageLayout: TablePageLayoutStub, DataTable: DataTableStub,
        BaseDialog: BaseDialogStub, Pagination: true, ConfirmDialog: true, EmptyState: true,
        PlatformIcon: true, Icon: true, GroupCapacityBadge: true, GroupRateMultipliersModal: true,
        GroupRPMOverridesModal: true, ReasoningEffortPolicyFields: true, CodexManifestAccountsField: true,
        PricingEntryCard: true, VueDraggable: true
      }
    }
  })
  await flushPromises()
  await wrapper.get('[data-testid="group-composite-routes"]').trigger('click')
  await flushPromises()
  return wrapper
}

async function choose(selector: string, label: string) {
  await wrapper.get(selector).trigger('click')
  await flushPromises()
  const option = Array.from(document.querySelectorAll<HTMLElement>('[role="option"]'))
    .find((item) => item.textContent?.trim() === label)
  expect(option, `Missing option ${label}`).toBeTruthy()
  await new DOMWrapper(option!).trigger('click')
  await flushPromises()
}

async function editFirstRoute() {
  await wrapper.get('tbody button[title="Edit"]').trigger('click')
}

async function submitRoute() {
  await wrapper.get('form').trigger('submit')
  await flushPromises()
}

async function runPreview() {
  await wrapper.get('[data-testid="composite-preview-model"]').setValue(' public-model ')
  await wrapper.get('[data-testid="composite-preview-run"]').trigger('click')
  await flushPromises()
}

describe('GroupsView Composite route options', () => {
  it('offers Kimi, Zhipu GLM, and DeepSeek as route targets', () => {
    expect(CONCRETE_PLATFORM_OPTIONS.map((option) => option.value)).toEqual(
      expect.arrayContaining(['kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'])
    )
  })
})

describe('GroupsView precise Composite routes', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.clearAllMocks()
    vi.mocked(adminAPI.groups.getAll).mockResolvedValue([])
    vi.mocked(adminAPI.groups.getModelAllowlistCandidates).mockResolvedValue([])
    vi.mocked(adminAPI.groups.getUsageSummary).mockResolvedValue([])
    vi.mocked(adminAPI.groups.getCapacitySummary).mockResolvedValue([])
    vi.mocked(adminAPI.groups.getLiveCapability).mockResolvedValue({ supported: false } as never)
    listGroups.mockResolvedValue({ items: [compositeGroup], total: 1, page: 1, page_size: 20, pages: 1 })
    listCompositeRoutes.mockResolvedValue([legacyRoute])
    createCompositeRoute.mockResolvedValue(legacyRoute)
    updateCompositeRoute.mockResolvedValue(legacyRoute)
    deleteCompositeRoute.mockResolvedValue({ message: 'deleted' })
    previewCompositeRoute.mockResolvedValue(legacyDecision)
  })

  afterEach(() => {
    wrapper?.unmount()
    vi.restoreAllMocks()
  })

  it('submits complete precise fields for a new route without requiring a preset', async () => {
    await openRoutes()
    await wrapper.get('form input[placeholder="openrouter/gpt-5"]').setValue('new-model')
    await submitRoute()

    expect(createCompositeRoute).toHaveBeenCalledWith(42, {
      public_model: 'new-model', match_type: 'exact', target_platform: 'openai',
      upstream_model: '', endpoint: 'any', user_agent_contains: '', body_contains: '',
      request_kind: 'any', body_match_scope: 'current_turn', body_match_mode: 'any',
      body_not_contains: '', priority: 100, enabled: true, notes: ''
    })
  })

  it.each([
    { name: 'missing', fields: {} },
    { name: 'empty', fields: { request_kind: '', body_match_scope: '', body_match_mode: '', body_not_contains: '' } },
    { name: 'null', fields: { request_kind: null, body_match_scope: null, body_match_mode: null, body_not_contains: null } }
  ])('preserves full-body semantics when editing a $name legacy response', async ({ fields }) => {
    listCompositeRoutes.mockResolvedValue([{ ...legacyRoute, ...fields }])
    await openRoutes()
    await editFirstRoute()
    await submitRoute()

    expect(updateCompositeRoute).toHaveBeenCalledWith(42, 7, {
      public_model: 'public-model', match_type: 'prefix', target_platform: 'deepseek',
      upstream_model: 'upstream-model', endpoint: 'responses', user_agent_contains: 'codex',
      body_contains: 'checkpoint', request_kind: 'any', body_match_scope: 'full_body',
      body_match_mode: 'any', body_not_contains: '', priority: 3, enabled: true, notes: 'existing route'
    })
  })

  it('changes real Select controls and retains multiline exclusions in the saved payload', async () => {
    await openRoutes()
    await editFirstRoute()
    await choose('#composite-request-kind', 'Conversation')
    await choose('#composite-body-scope', 'Current terminal user message')
    await choose('#composite-body-mode', 'All signatures')
    await wrapper.get('[data-testid="composite-body-contains"]').setValue(' first\nsecond ')
    await wrapper.get('[data-testid="composite-body-not-contains"]').setValue(' quoted\nexample ')
    await submitRoute()

    expect(updateCompositeRoute).toHaveBeenCalledWith(42, 7, expect.objectContaining({
      request_kind: 'conversation', body_match_scope: 'last_message', body_match_mode: 'all',
      body_contains: 'first\nsecond', body_not_contains: 'quoted\nexample'
    }))
  })

  it('restores saved precise values and supports instructions with prefix matching', async () => {
    listCompositeRoutes.mockResolvedValue([{
      ...legacyRoute, request_kind: 'compaction', body_match_scope: 'current_turn',
      body_match_mode: 'prefix', body_not_contains: 'not a real compaction'
    }])
    await openRoutes()
    await editFirstRoute()
    expect(wrapper.get('#composite-request-kind').text()).toContain('Compaction')
    expect(wrapper.get('#composite-body-scope').text()).toContain('Current turn')
    expect(wrapper.get('#composite-body-mode').text()).toContain('Signature prefix')
    expect(wrapper.get<HTMLTextAreaElement>('[data-testid="composite-body-not-contains"]').element.value)
      .toBe('not a real compaction')
    await choose('#composite-body-scope', 'Instructions')
    await submitRoute()
    expect(updateCompositeRoute).toHaveBeenCalledWith(42, 7, expect.objectContaining({
      request_kind: 'compaction', body_match_scope: 'instructions', body_match_mode: 'prefix',
      body_not_contains: 'not a real compaction'
    }))
  })

  it('applies the compaction preset without changing model, endpoint, UA or upstream', async () => {
    await openRoutes()
    await editFirstRoute()
    await choose('#composite-body-mode', 'All signatures')
    await wrapper.get('[data-testid="composite-body-not-contains"]').setValue('example')
    await wrapper.get('[data-testid="composite-compaction-preset"]').trigger('click')
    await submitRoute()

    expect(updateCompositeRoute).toHaveBeenCalledWith(42, 7, {
      public_model: 'public-model', match_type: 'prefix', target_platform: 'deepseek',
      upstream_model: 'upstream-model', endpoint: 'responses', user_agent_contains: 'codex',
      body_contains: '', request_kind: 'compaction', body_match_scope: 'current_turn',
      body_match_mode: 'any', body_not_contains: '', priority: 3, enabled: true, notes: 'existing route'
    })
  })

  it('resets precise fields on cancel and successful save to new-route defaults', async () => {
    await openRoutes()
    await editFirstRoute()
    await wrapper.get('[data-testid="composite-compaction-preset"]').trigger('click')
    await wrapper.get('[data-testid="composite-body-not-contains"]').setValue('exclude')
    await wrapper.get('[data-testid="composite-route-cancel"]').trigger('click')

    expect(wrapper.get('#composite-request-kind').text()).toContain('Any request')
    expect(wrapper.get('#composite-body-scope').text()).toContain('Current turn')
    expect(wrapper.get('#composite-body-mode').text()).toContain('Any signature')
    expect(wrapper.get<HTMLTextAreaElement>('[data-testid="composite-body-not-contains"]').element.value).toBe('')
    await wrapper.get('form input[placeholder="openrouter/gpt-5"]').setValue('fresh-model')
    await choose('#composite-request-kind', 'Compaction')
    await submitRoute()
    expect(updateCompositeRoute).not.toHaveBeenCalled()
    expect(createCompositeRoute).toHaveBeenCalledWith(42, expect.objectContaining({ public_model: 'fresh-model' }))
    expect(wrapper.get('#composite-request-kind').text()).toContain('Any request')
    expect(wrapper.get('#composite-body-scope').text()).toContain('Current turn')
  })

  it('keeps the edited precise draft when saving fails', async () => {
    updateCompositeRoute.mockRejectedValueOnce({ response: { data: { message: 'save failed' } } })
    vi.spyOn(console, 'error').mockImplementation(() => {})
    await openRoutes()
    await editFirstRoute()
    await wrapper.get('[data-testid="composite-compaction-preset"]').trigger('click')
    await wrapper.get('[data-testid="composite-body-not-contains"]').setValue('preserve this')
    await submitRoute()

    expect(showError).toHaveBeenCalledWith('save failed')
    expect(wrapper.get('#composite-request-kind').text()).toContain('Compaction')
    expect(wrapper.get<HTMLTextAreaElement>('[data-testid="composite-body-not-contains"]').element.value)
      .toBe('preserve this')
    expect(wrapper.get('form button[type="submit"]').attributes('disabled')).toBeUndefined()
  })

  it('marks kind-only and exclusion-only routes conditional while leaving plain routes unmarked', async () => {
    listCompositeRoutes.mockResolvedValue([
      { ...legacyRoute, id: 1, user_agent_contains: '', body_contains: '', request_kind: 'compaction' },
      { ...legacyRoute, id: 2, user_agent_contains: '', body_contains: '', body_not_contains: 'example' },
      { ...legacyRoute, id: 3, user_agent_contains: '', body_contains: '', request_kind: 'any', body_not_contains: '' }
    ])
    await openRoutes()
    const rows = wrapper.findAll('tbody tr')
    expect(rows[0].text()).toContain('Conditional')
    expect(rows[1].text()).toContain('Conditional')
    expect(rows[2].text()).not.toContain('Conditional')
  })

  it('warns about short full-body signatures without blocking save', async () => {
    await openRoutes()
    await editFirstRoute()
    await wrapper.get('[data-testid="composite-body-contains"]').setValue('checkpoint\n ab\n')
    expect(wrapper.get('[role="alert"]').text()).toContain('1-2 letters')
    await submitRoute()
    expect(updateCompositeRoute).toHaveBeenCalledWith(42, 7, expect.objectContaining({ body_contains: 'checkpoint\n ab' }))
  })

  it('warns about historical quotes in full-body scope and clears the risk on scope change', async () => {
    await openRoutes()
    await editFirstRoute()
    expect(wrapper.get('[role="alert"]').text()).toContain('Historical quotes')
    await choose('#composite-body-scope', 'Current turn')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    await choose('#composite-body-scope', 'Full body (legacy)')
    await wrapper.get('[data-testid="composite-body-contains"]').setValue('')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it('sends native endpoint simulation with preview inputs and still renders legacy decisions', async () => {
    await openRoutes()
    await wrapper.get('[data-testid="composite-preview-user-agent"]').setValue(' codex ')
    await wrapper.get('[data-testid="composite-preview-body"]').setValue('{"input":"checkpoint"}')
    await choose('#composite-preview-endpoint', 'Responses')
    await wrapper.get('[data-testid="composite-preview-native-compaction"]').setValue(true)
    await runPreview()

    expect(previewCompositeRoute).toHaveBeenCalledWith(42, {
      model: 'public-model', endpoint: 'responses', user_agent: 'codex',
      body: '{"input":"checkpoint"}', native_compaction: true
    })
    expect(wrapper.get('[data-testid="composite-preview-result"]').text()).toContain('upstream-model')
    expect(wrapper.find('[data-testid="composite-request-classification"]').exists()).toBe(false)
  })

  it('keeps the Claude Code hint optional and omits it by default, including for Messages', async () => {
    await openRoutes()
    const hint = wrapper.find<HTMLButtonElement>('#composite-preview-claude-compaction-hint')
    expect(hint.exists()).toBe(true)
    expect(hint.element.disabled).toBe(true)
    expect(hint.text()).toContain('None')
    await runPreview()
    expect(previewCompositeRoute.mock.lastCall?.[1]).not.toHaveProperty('claude_compaction_hint')

    await choose('#composite-preview-endpoint', 'Messages')
    expect(hint.element.disabled).toBe(false)
    await runPreview()
    expect(previewCompositeRoute).toHaveBeenLastCalledWith(42, {
      model: 'public-model', endpoint: 'messages', user_agent: '', body: '', native_compaction: false
    })
  })

  it.each([
    { label: 'Manual', value: 'manual' },
    { label: 'Auto', value: 'auto' },
    { label: 'Reactive', value: 'reactive' },
    { label: 'Compaction class only', value: 'compaction' }
  ])('sends the selected Claude Code $value hint only in preview inputs', async ({ label, value }) => {
    const body = '{"messages":[{"role":"user","content":"private-preview-body"}]}'
    await openRoutes()
    await choose('#composite-preview-endpoint', 'Messages')
    expect(wrapper.find('#composite-preview-claude-compaction-hint').exists()).toBe(true)
    await choose('#composite-preview-claude-compaction-hint', label)
    await wrapper.get('[data-testid="composite-preview-user-agent"]').setValue(' claude-cli/2.0 ')
    await wrapper.get('[data-testid="composite-preview-body"]').setValue(body)
    await runPreview()

    expect(previewCompositeRoute).toHaveBeenLastCalledWith(42, {
      model: 'public-model', endpoint: 'messages', user_agent: 'claude-cli/2.0',
      body, native_compaction: false, claude_compaction_hint: value
    })
    await wrapper.get('form input[placeholder="openrouter/gpt-5"]').setValue('new-model')
    await submitRoute()
    expect(createCompositeRoute.mock.lastCall?.[1]).not.toHaveProperty('claude_compaction_hint')
  })

  it.each(['Any', 'Count Tokens', 'Responses', 'Chat Completions', 'Embeddings', 'Images', 'Gemini Native'])(
    'clears the Claude Code hint when switching to %s and does not restore it on return to Messages', async (endpointLabel) => {
      await openRoutes()
      await choose('#composite-preview-endpoint', 'Messages')
      expect(wrapper.find('#composite-preview-claude-compaction-hint').exists()).toBe(true)
      await choose('#composite-preview-claude-compaction-hint', 'Manual')
      await choose('#composite-preview-endpoint', endpointLabel)

      const hint = wrapper.get<HTMLButtonElement>('#composite-preview-claude-compaction-hint')
      expect(hint.element.disabled).toBe(true)
      expect(hint.text()).toContain('None')
      await runPreview()
      expect(previewCompositeRoute.mock.lastCall?.[1]).not.toHaveProperty('claude_compaction_hint')
      await choose('#composite-preview-endpoint', 'Messages')
      expect(hint.element.disabled).toBe(false)
      expect(hint.text()).toContain('None')
      await runPreview()
      expect(previewCompositeRoute.mock.lastCall?.[1]).not.toHaveProperty('claude_compaction_hint')
    }
  )

  it('omits the Claude Code hint after explicitly choosing the empty option', async () => {
    await openRoutes()
    await choose('#composite-preview-endpoint', 'Messages')
    expect(wrapper.find('#composite-preview-claude-compaction-hint').exists()).toBe(true)
    await choose('#composite-preview-claude-compaction-hint', 'Auto')
    await choose('#composite-preview-claude-compaction-hint', 'None')
    await runPreview()

    expect(previewCompositeRoute.mock.lastCall?.[1]).not.toHaveProperty('claude_compaction_hint')
  })

  it('clears the Claude Code hint when closing and reopening the routes dialog', async () => {
    await openRoutes()
    await choose('#composite-preview-endpoint', 'Messages')
    expect(wrapper.find('#composite-preview-claude-compaction-hint').exists()).toBe(true)
    await choose('#composite-preview-claude-compaction-hint', 'Reactive')
    await wrapper.get('[data-testid="composite-routes-close"]').trigger('click')
    await wrapper.get('[data-testid="group-composite-routes"]').trigger('click')
    await flushPromises()
    await choose('#composite-preview-endpoint', 'Messages')
    expect(wrapper.get('#composite-preview-claude-compaction-hint').text()).toContain('None')
    await runPreview()

    expect(previewCompositeRoute.mock.lastCall?.[1]).not.toHaveProperty('claude_compaction_hint')
  })

  it.each([
    { locale: 'en', source: 'claude_request_header', sourceLabel: 'Claude Code request header', reasonLabel: 'Claude Code request header identifies compaction' },
    { locale: 'zh', source: 'claude_request_header', sourceLabel: 'Claude Code \u8bf7\u6c42\u5934', reasonLabel: 'Claude Code \u8bf7\u6c42\u5934\u8bc6\u522b\u4e3a\u538b\u7f29' }
  ])('localizes $locale $source diagnostics without echoing request text', async ({ locale, source, sourceLabel, reasonLabel }) => {
    const body = '{"system":"private-preview-instructions","messages":[{"role":"user","content":"private-preview-user"}]}'
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision, matched: false, endpoint: 'messages', route: undefined, reason: source,
      request_classification: { kind: 'compaction', source, reason: source },
      condition_evaluations: [{ route_id: 7, matched: false, selected: false, body_match_scope: 'current_turn', checks: [
        { field: 'request_kind', matched: true, reason: source }
      ] }]
    })
    await openRoutes(locale)
    await choose('#composite-preview-endpoint', 'Messages')
    await wrapper.get('[data-testid="composite-preview-user-agent"]').setValue('claude-code/2.0')
    await wrapper.get('[data-testid="composite-preview-body"]').setValue(body)
    await runPreview()

    expect(wrapper.get('[data-testid="composite-request-classification"]').text()).toContain(sourceLabel)
    expect(wrapper.get('[data-testid="composite-condition-evaluation-7"]').text()).toContain(reasonLabel)
    const result = wrapper.get('[data-testid="composite-preview-result"]')
    expect(result.text()).toContain(reasonLabel)
    expect(result.text()).not.toContain(source)
    expect(result.html()).not.toContain(body)
    expect(result.html()).not.toContain('private-preview-instructions')
    expect(result.html()).not.toContain('private-preview-user')
  })

  it.each([
    { locale: 'en', label: 'Current terminal user message' },
    { locale: 'zh', label: '\u5f53\u524d\u672b\u6761\u7528\u6237\u6d88\u606f' }
  ])('uses the $locale terminal-user scope label while preserving the last_message API value', async ({ locale, label }) => {
    await openRoutes(locale)
    await wrapper.get('form input[placeholder="openrouter/gpt-5"]').setValue('new-model')
    await choose('#composite-body-scope', label)
    await submitRoute()

    expect(createCompositeRoute).toHaveBeenLastCalledWith(42, expect.objectContaining({
      body_match_scope: 'last_message'
    }))
  })

  it('renders classification and selected/matched/rejected checks with translated reasons', async () => {
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision,
      request_classification: { kind: 'compaction', source: 'codex_terminal_prompt', reason: 'codex_terminal_prompt' },
      condition_evaluations: [
        { route_id: 7, matched: true, selected: true, body_match_scope: 'current_turn', checks: [
          { field: 'request_kind', matched: true, reason: 'request_kind_match' },
          { field: 'body_not_contains', matched: true, reason: 'body_not_contains_clear' }
        ] },
        { route_id: 8, matched: false, selected: false, body_match_scope: 'last_message', checks: [
          { field: 'request_kind', matched: false, reason: 'request_kind_mismatch' },
          { field: 'body_not_contains', matched: false, reason: 'body_not_contains_excluded' }
        ] },
        { route_id: 9, matched: true, selected: false, body_match_scope: 'full_body', checks: [
          { field: 'user_agent_contains', matched: true, reason: 'user_agent_match' }
        ] }
      ]
    })
    await openRoutes()
    await runPreview()
    const classification = wrapper.get('[data-testid="composite-request-classification"]')
    expect(classification.text()).toContain('Compaction')
    expect(classification.text()).toContain('Codex terminal prompt')
    const selected = wrapper.get('[data-testid="composite-condition-evaluation-7"]')
    expect(selected.text()).toContain('Selected')
    expect(selected.text()).toContain('Current turn')
    expect(selected.text()).toContain('Request kind matches')
    expect(selected.text()).toContain('No excluded signature found')
    const rejected = wrapper.get('[data-testid="composite-condition-evaluation-8"]')
    expect(rejected.text()).toContain('Rejected')
    expect(rejected.text()).toContain('Request kind does not match')
    expect(rejected.text()).toContain('Excluded signature found')
    const matched = wrapper.get('[data-testid="composite-condition-evaluation-9"]')
    expect(matched.text()).toContain('Matched')
    expect(matched.text()).not.toContain('Selected')
  })

  it('enables native compaction only for Responses and clears it when switching endpoints', async () => {
    await openRoutes()
    const checkbox = wrapper.get<HTMLInputElement>('[data-testid="composite-preview-native-compaction"]')
    expect(checkbox.element.disabled).toBe(true)
    await choose('#composite-preview-endpoint', 'Responses')
    expect(checkbox.element.disabled).toBe(false)
    await checkbox.setValue(true)
    await runPreview()
    expect(previewCompositeRoute).toHaveBeenLastCalledWith(42, expect.objectContaining({
      endpoint: 'responses', native_compaction: true
    }))

    await choose('#composite-preview-endpoint', 'Messages')
    expect(checkbox.element.disabled).toBe(true)
    expect(checkbox.element.checked).toBe(false)
    await runPreview()
    expect(previewCompositeRoute).toHaveBeenLastCalledWith(42, expect.objectContaining({
      endpoint: 'messages', native_compaction: false
    }))
    await choose('#composite-preview-endpoint', 'Responses')
    expect(checkbox.element.checked).toBe(false)
    await runPreview()
    expect(previewCompositeRoute).toHaveBeenLastCalledWith(42, expect.objectContaining({
      endpoint: 'responses', native_compaction: false
    }))
  })

  it('translates model and endpoint mismatch checks from the routing engine', async () => {
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision, matched: false, condition_evaluations: [{
        route_id: 7, matched: false, selected: false, body_match_scope: 'current_turn', checks: [
          { field: 'model', matched: false, reason: 'model_mismatch' },
          { field: 'endpoint', matched: false, reason: 'endpoint_mismatch' },
          { field: 'body_not_contains', matched: false, reason: 'body_not_contains_excluded' }
        ]
      }]
    })
    await openRoutes()
    await runPreview()
    const checks = wrapper.get('[data-testid="composite-condition-evaluation-7"]').text()
    expect(checks).toContain('Model does not match')
    expect(checks).toContain('Endpoint does not match')
    expect(checks).toContain('Excluded signature found')
  })

  it('translates invalid-JSON classification sources with an unknown request kind', async () => {
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision, matched: false,
      request_classification: { kind: 'unknown', source: 'invalid_json', reason: 'invalid_json' }
    })
    await openRoutes()
    await runPreview()
    const classification = wrapper.get('[data-testid="composite-request-classification"]').text()
    expect(classification).toContain('Unknown request')
    expect(classification).toContain('Request body is not valid JSON')
  })

  it.each([
    { locale: 'en', endpointLabel: 'Any', unknownLabel: 'Unknown request', label: 'Unspecified or unsupported request endpoint' },
    { locale: 'zh', endpointLabel: '\u4efb\u610f', unknownLabel: '\u672a\u77e5\u8bf7\u6c42', label: '\u672a\u6307\u5b9a\u6216\u4e0d\u652f\u6301\u7684\u8bf7\u6c42\u63a5\u53e3' }
  ])('localizes $locale unsupported-endpoint classification and reasons while keeping preview defaults', async ({ locale, endpointLabel, unknownLabel, label }) => {
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision, matched: false, endpoint: 'any', route: undefined, reason: 'unsupported_endpoint',
      request_classification: { kind: 'unknown', source: 'unsupported_endpoint', reason: 'unsupported_endpoint' }
    })
    await openRoutes(locale)
    expect(wrapper.get('#composite-preview-endpoint').text()).toContain(endpointLabel)
    const checkbox = wrapper.get<HTMLInputElement>('[data-testid="composite-preview-native-compaction"]')
    expect(checkbox.element.disabled).toBe(true)
    expect(checkbox.element.checked).toBe(false)
    await runPreview()

    expect(previewCompositeRoute).toHaveBeenLastCalledWith(42, expect.objectContaining({
      endpoint: 'any', native_compaction: false
    }))
    const classification = wrapper.get('[data-testid="composite-request-classification"]').text()
    expect(classification).toContain(unknownLabel)
    expect(classification).toContain(label)
    const result = wrapper.get('[data-testid="composite-preview-result"]').text()
    expect(result.split(label).length - 1).toBe(2)
    expect(result).not.toContain('unsupported_endpoint')
    expect(result).not.toContain('unsupported endpoint')
  })

  it('still displays an engine-selected legacy full-body route when the Any endpoint classification is unknown', async () => {
    const route: CompositeModelRoute = {
      ...legacyRoute, endpoint: 'any', request_kind: 'any', body_match_scope: 'full_body', body_match_mode: 'any'
    }
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision, endpoint: 'any', route,
      request_classification: { kind: 'unknown', source: 'unsupported_endpoint', reason: 'unsupported_endpoint' },
      condition_evaluations: [{ route_id: 7, matched: true, selected: true, body_match_scope: 'full_body', checks: [
        { field: 'body_contains', matched: true, reason: 'body_contains_match' }
      ] }]
    })
    await openRoutes()
    await wrapper.get('[data-testid="composite-preview-body"]').setValue('{"input":"checkpoint"}')
    await runPreview()

    expect(wrapper.get('[data-testid="composite-request-classification"]').text()).toContain('Unknown request')
    const result = wrapper.get('[data-testid="composite-preview-result"]').text()
    expect(result).toContain('Matched')
    expect(result).toContain('upstream-model')
    const selected = wrapper.get('[data-testid="composite-condition-evaluation-7"]').text()
    expect(selected).toContain('Selected')
    expect(selected).toContain('Full body (legacy)')
    expect(selected).toContain('Body signatures match')
  })

  it.each([
    { locale: 'en', source: 'ambiguous_json', label: 'Duplicate JSON fields; precise matching rejected' },
    { locale: 'zh', source: 'ambiguous_json', label: '\u91cd\u590d JSON \u5b57\u6bb5\uff0c\u62d2\u7edd\u7cbe\u786e\u5339\u914d' },
    { locale: 'en', source: 'invalid_json', label: 'Duplicate JSON fields; precise matching rejected' },
    { locale: 'zh', source: 'invalid_json', label: '\u91cd\u590d JSON \u5b57\u6bb5\uff0c\u62d2\u7edd\u7cbe\u786e\u5339\u914d' }
  ])('renders $locale ambiguous-JSON diagnostics with source $source without leaking body content', async ({ locale, source, label }) => {
    const body = '{"instructions":"private-first-instruction","instructions":"private-last-instruction"}'
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision, matched: false, reason: 'ambiguous_json',
      request_classification: { kind: 'unknown', source, reason: 'ambiguous_json' },
      condition_evaluations: [{ route_id: 7, matched: false, selected: false, body_match_scope: 'current_turn', checks: [
        { field: 'conditions', matched: false, reason: 'ambiguous_json' }
      ] }]
    })
    await openRoutes(locale)
    await wrapper.get('[data-testid="composite-preview-body"]').setValue(body)
    await runPreview()

    expect(wrapper.get('[data-testid="composite-request-classification"]').text()).toContain(label)
    const checks = wrapper.get('[data-testid="composite-condition-evaluation-7"]').text()
    expect(checks).toContain(label)
    const result = wrapper.get('[data-testid="composite-preview-result"]')
    expect(result.text().split(label).length - 1).toBe(3)
    expect(result.text()).not.toContain('ambiguous_json')
    expect(result.text()).not.toContain('ambiguous json')
    expect(result.html()).not.toContain(body)
    expect(result.html()).not.toContain('private-first-instruction')
    expect(result.html()).not.toContain('private-last-instruction')
  })

  it.each([
    { locale: 'en', label: 'Native compaction request' },
    { locale: 'zh', label: '\u539f\u751f\u538b\u7f29\u8bf7\u6c42' }
  ])('describes $locale native compaction sources and reasons as requests rather than dedicated endpoints', async ({ locale, label }) => {
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision, matched: false, reason: 'native_endpoint',
      request_classification: { kind: 'compaction', source: 'native_endpoint', reason: 'native_endpoint' }
    })
    await openRoutes(locale)
    await runPreview()

    const classification = wrapper.get('[data-testid="composite-request-classification"]').text()
    expect(classification).toContain(label)
    const result = wrapper.get('[data-testid="composite-preview-result"]').text()
    expect(result.split(label).length - 1).toBe(2)
    expect(result).not.toContain('Native compaction endpoint')
    expect(result).not.toContain('\u539f\u751f\u538b\u7f29\u7aef\u70b9')
  })

  it('provides readable fallback diagnostics without rendering HTML', async () => {
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision, matched: false, reason: 'scope_empty',
      request_classification: { kind: 'future_kind', source: 'future_client', reason: 'no_compaction_signature' },
      condition_evaluations: [{ route_id: 7, matched: false, selected: false, body_match_scope: 'future_scope', checks: [
        { field: 'future_field', matched: false, reason: 'future_reason_code' },
        { field: 'body_contains', matched: false, reason: '<img src=x onerror=alert(1)>' }
      ] }]
    })
    await openRoutes()
    await runPreview()
    const result = wrapper.get('[data-testid="composite-preview-result"]')
    for (const text of ['Selected body scope is empty', 'future kind', 'future client', 'future scope',
      'future field', 'future reason code', '<img src=x onerror=alert(1)>']) {
      expect(result.text()).toContain(text)
    }
    expect(result.find('img').exists()).toBe(false)
    expect(result.text()).not.toContain('admin.groups.compositeRoutes.')
  })

  it('localizes diagnostics and compaction source names in Chinese', async () => {
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision,
      request_classification: { kind: 'compaction', source: 'native_endpoint', reason: 'native_endpoint' },
      condition_evaluations: [{ route_id: 7, matched: false, selected: false, body_match_scope: 'instructions', checks: [
        { field: 'body_contains', matched: false, reason: 'invalid_json' }
      ] }]
    })
    await openRoutes('zh')
    await runPreview()
    expect(wrapper.get('[data-testid="composite-request-classification"]').text()).toContain('\u539f\u751f\u538b\u7f29\u8bf7\u6c42')
    expect(wrapper.get('[data-testid="composite-condition-evaluation-7"]').text()).toContain('\u8bf7\u6c42\u6b63\u6587\u4e0d\u662f\u6709\u6548 JSON')
  })

  it('clears native simulation, diagnostics, and edited fields when closing and reopening', async () => {
    previewCompositeRoute.mockResolvedValue({
      ...legacyDecision,
      request_classification: { kind: 'compaction', source: 'native_endpoint', reason: 'native_endpoint' },
      condition_evaluations: []
    })
    await openRoutes()
    await choose('#composite-preview-endpoint', 'Responses')
    await wrapper.get('[data-testid="composite-preview-native-compaction"]').setValue(true)
    await runPreview()
    await editFirstRoute()
    await wrapper.get('[data-testid="composite-compaction-preset"]').trigger('click')
    await wrapper.get('[data-testid="composite-routes-close"]').trigger('click')
    await wrapper.get('[data-testid="group-composite-routes"]').trigger('click')
    await flushPromises()

    expect(wrapper.get<HTMLInputElement>('[data-testid="composite-preview-native-compaction"]').element.checked).toBe(false)
    expect(wrapper.get<HTMLInputElement>('[data-testid="composite-preview-model"]').element.value).toBe('')
    expect(wrapper.find('[data-testid="composite-preview-result"]').exists()).toBe(false)
    expect(wrapper.get('#composite-request-kind').text()).toContain('Any request')
    expect(wrapper.get('#composite-body-scope').text()).toContain('Current turn')
  })

  it('discards a pending preview result after closing and reopening the routes dialog', async () => {
    let resolvePreview!: (value: CompositeRouteDecision) => void
    previewCompositeRoute.mockImplementationOnce(() => new Promise<CompositeRouteDecision>((resolve) => {
      resolvePreview = resolve
    }))
    await openRoutes()
    await runPreview()
    await wrapper.get('[data-testid="composite-routes-close"]').trigger('click')
    await wrapper.get('[data-testid="group-composite-routes"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="composite-preview-model"]').setValue('new-model')
    const disabledAfterReopen = wrapper.get('[data-testid="composite-preview-run"]').attributes('disabled')
    resolvePreview(legacyDecision)
    await flushPromises()

    expect(disabledAfterReopen).toBeUndefined()
    expect(wrapper.find('[data-testid="composite-preview-result"]').exists()).toBe(false)
    await wrapper.get('[data-testid="composite-preview-run"]').trigger('click')
    await flushPromises()
    expect(previewCompositeRoute).toHaveBeenLastCalledWith(42, expect.objectContaining({
      model: 'new-model', native_compaction: false
    }))
  })

  it('does not show an error from a preview belonging to a closed dialog', async () => {
    let rejectPreview!: (error: Error) => void
    previewCompositeRoute.mockImplementationOnce(() => new Promise((_resolve, reject) => {
      rejectPreview = reject
    }))
    vi.spyOn(console, 'error').mockImplementation(() => {})
    await openRoutes()
    await runPreview()
    await wrapper.get('[data-testid="composite-routes-close"]').trigger('click')
    rejectPreview(new Error('stale preview failed'))
    await flushPromises()

    expect(showError).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="composite-preview-result"]').exists()).toBe(false)
  })
})

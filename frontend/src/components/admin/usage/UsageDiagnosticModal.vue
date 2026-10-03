<template>
  <BaseDialog
    :show="show"
    :title="`${text.title} #${usageId ?? '—'}`"
    width="full"
    :close-on-click-outside="true"
    @close="close"
  >
    <div v-if="show" class="space-y-5">
      <p class="rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-sm leading-relaxed text-amber-900 dark:border-amber-800 dark:bg-amber-900/10 dark:text-amber-200">
        {{ text.sensitiveNotice }}
      </p>

      <div v-if="loading" role="status" class="flex items-center justify-center gap-3 py-12 text-sm text-gray-500 dark:text-gray-400" data-testid="diagnostic-loading">
        <span class="h-6 w-6 animate-spin rounded-full border-2 border-gray-200 border-t-primary-600 motion-reduce:animate-none dark:border-dark-700 dark:border-t-primary-400" aria-hidden="true"></span>
        {{ text.loading }}
      </div>
      <div v-else-if="loadFailed" role="alert" class="rounded-lg border border-red-200 bg-red-50 p-4 dark:border-red-900 dark:bg-red-900/10" data-testid="diagnostic-load-error">
        <p class="text-sm text-red-800 dark:text-red-300">{{ text.loadFailed }}</p>
        <button type="button" class="btn btn-secondary mt-3" data-testid="diagnostic-retry" @click="loadDiagnostic">
          {{ text.retry }}
        </button>
      </div>

      <template v-if="usage">
        <section aria-labelledby="diagnostic-usage-heading">
          <h4 id="diagnostic-usage-heading" class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ text.usageSummary }}</h4>
          <dl class="grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-2 xl:grid-cols-3">
            <div v-for="field in usageFields" :key="field.label" class="min-w-0">
              <dt class="text-xs text-gray-500 dark:text-gray-400">{{ field.label }}</dt>
              <dd class="mt-0.5 whitespace-pre-wrap break-all text-gray-900 dark:text-gray-100">{{ field.value }}</dd>
            </div>
          </dl>
        </section>
      </template>

      <template v-if="detail && !loading && !loadFailed">
        <section
          class="rounded-lg border p-4 text-sm"
          :class="snapshot ? 'border-primary-200 bg-primary-50/50 dark:border-primary-900 dark:bg-primary-900/10' : 'border-gray-200 bg-gray-50 dark:border-dark-700 dark:bg-dark-900'"
          data-testid="diagnostic-status"
        >
          <div class="flex flex-wrap items-center gap-2">
            <h4 class="font-semibold text-gray-900 dark:text-gray-100">{{ statusMessage.title }}</h4>
            <span class="rounded bg-white/70 px-1.5 py-0.5 font-mono text-xs text-gray-600 dark:bg-dark-800 dark:text-gray-400">{{ detail.diagnostic.status }}</span>
          </div>
          <p class="mt-1 leading-relaxed text-gray-600 dark:text-gray-300">{{ statusMessage.description }}</p>
          <dl class="mt-3 grid grid-cols-1 gap-x-6 gap-y-2 text-xs text-gray-700 dark:text-gray-300 sm:grid-cols-2">
            <div><dt class="inline text-gray-500 dark:text-gray-400">{{ text.captureEnabled }}: </dt><dd class="inline">{{ detail.capture_enabled ? text.enabled : text.disabled }}</dd></div>
            <div><dt class="inline text-gray-500 dark:text-gray-400">{{ text.retention }}: </dt><dd class="inline">{{ detail.retention_hours }} h</dd></div>
            <div><dt class="inline text-gray-500 dark:text-gray-400">{{ text.capturedAt }}: </dt><dd class="inline">{{ dateTime(detail.diagnostic.captured_at || snapshot?.captured_at) }}</dd></div>
            <div><dt class="inline text-gray-500 dark:text-gray-400">{{ text.expiresAt }}: </dt><dd class="inline">{{ dateTime(detail.diagnostic.expires_at) }}</dd></div>
          </dl>
        </section>

        <template v-if="snapshot">
          <section class="border-t border-gray-200 pt-4 dark:border-dark-700" aria-labelledby="diagnostic-snapshot-heading">
            <h4 id="diagnostic-snapshot-heading" class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ text.snapshotMetadata }}</h4>
            <dl class="grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-2 xl:grid-cols-3">
              <div v-for="field in snapshotFields" :key="field.label" class="min-w-0">
                <dt class="text-xs text-gray-500 dark:text-gray-400">{{ field.label }}</dt>
                <dd class="mt-0.5 break-all text-gray-900 dark:text-gray-100">{{ field.value }}</dd>
              </div>
            </dl>
            <p v-if="snapshot.dropped_attempts > 0" class="mt-3 text-sm text-amber-700 dark:text-amber-300" data-testid="diagnostic-dropped-attempts">
              {{ text.droppedAttempts }}: {{ snapshot.dropped_attempts }}. {{ text.incompleteAttempts }}
            </p>
          </section>

          <article
            v-for="section in captureSections"
            :key="section.key"
            class="overflow-hidden rounded-lg border border-gray-200 dark:border-dark-700"
            :data-testid="`diagnostic-section-${section.key}`"
          >
            <div class="bg-gray-50 px-4 py-3 dark:bg-dark-900">
              <h4 class="text-sm font-semibold text-gray-900 dark:text-white">{{ section.title }}</h4>
              <dl class="mt-3 grid grid-cols-1 gap-x-6 gap-y-2 text-xs sm:grid-cols-2 lg:grid-cols-3">
                <div v-for="field in section.fields" :key="field.label" class="min-w-0">
                  <dt class="text-gray-500 dark:text-gray-400">{{ field.label }}</dt>
                  <dd class="mt-0.5 whitespace-pre-wrap break-all text-gray-900 dark:text-gray-100">{{ field.value }}</dd>
                </div>
              </dl>
            </div>
            <div class="space-y-4 p-4">
              <div v-for="block in section.payloads" :key="block.key" class="space-y-2" :data-testid="`diagnostic-payload-${block.key}`">
                <div class="flex flex-wrap items-center justify-between gap-2">
                  <button
                    type="button"
                    class="inline-flex items-center gap-1.5 rounded text-sm font-medium text-primary-700 hover:text-primary-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 dark:text-primary-300 dark:hover:text-primary-200"
                    :aria-expanded="expandedPayloads.has(block.key)"
                    :aria-controls="`diagnostic-content-${block.key}`"
                    :data-testid="`diagnostic-toggle-${block.key}`"
                    @click="togglePayload(block.key)"
                  >
                    <Icon :name="expandedPayloads.has(block.key) ? 'chevronDown' : 'chevronRight'" size="sm" />
                    {{ block.title }}
                    <span class="text-xs font-normal text-gray-500 dark:text-gray-400">{{ expandedPayloads.has(block.key) ? text.collapse : text.expand }}</span>
                  </button>
                  <button
                    type="button"
                    class="inline-flex items-center gap-1.5 rounded px-2 py-1 text-xs font-medium text-gray-600 hover:bg-gray-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 disabled:cursor-not-allowed disabled:opacity-50 dark:text-gray-300 dark:hover:bg-dark-700"
                    :disabled="!block.payload.content"
                    :data-testid="`diagnostic-copy-${block.key}`"
                    @click="copyPayload(block.key, block.payload.content)"
                  >
                    <Icon :name="copiedPayload === block.key ? 'check' : 'copy'" size="xs" />
                    {{ copiedPayload === block.key ? text.copied : text.copy }}
                  </button>
                </div>
                <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-gray-500 dark:text-gray-400">
                  <span>{{ text.originalBytes }}: {{ bytes(block.payload.original_bytes) }}</span>
                  <span>{{ text.storedBytes }}: {{ bytes(block.payload.stored_bytes) }}</span>
                  <span v-if="block.payload.redacted" class="rounded bg-amber-100 px-1.5 py-0.5 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300">{{ text.redacted }}</span>
                  <span v-if="block.payload.truncated" class="rounded bg-amber-100 px-1.5 py-0.5 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300">{{ text.truncated }}</span>
                </div>
                <p v-if="block.payload.omitted_reason" class="break-all text-xs text-amber-700 dark:text-amber-300">{{ text.omittedReason }}: {{ block.payload.omitted_reason }}</p>
                <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ block.summary ? text.limitedSummary : text.bodyNote }}</p>
                <div v-if="expandedPayloads.has(block.key)" :id="`diagnostic-content-${block.key}`">
                  <!-- 请求内容只做文本插值，不能渲染 HTML 或 Markdown。 -->
                  <pre v-if="block.payload.content" class="max-h-[480px] overflow-auto rounded-lg border border-gray-200 bg-gray-50 p-3 font-mono text-xs leading-relaxed text-gray-800 dark:border-dark-700 dark:bg-dark-900 dark:text-gray-100"><code>{{ block.payload.content }}</code></pre>
                  <p v-else class="py-3 text-sm text-gray-500 dark:text-gray-400">{{ text.emptyPayload }}</p>
                </div>
              </div>
              <p v-if="section.noResponseSummary" class="text-xs text-gray-500 dark:text-gray-400">{{ text.noResponseSummary }}</p>
            </div>
          </article>
          <p v-if="snapshot.attempts.length === 0" class="text-sm text-gray-500 dark:text-gray-400">{{ text.noAttempts }}</p>
          <p v-if="copyFailed" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ text.copyFailed }}</p>
        </template>
      </template>
    </div>
    <template #footer>
      <button type="button" class="btn btn-secondary" data-testid="diagnostic-close" @click="close">{{ text.close }}</button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { getUsageDiagnostic } from '@/api/admin/usageDiagnostic'
import type { UsageDiagnosticPayload, UsageDiagnosticResponse } from '@/api/admin/usageDiagnostic'
import type { AdminUsageLog } from '@/types'
import { formatDateTime, formatReasoningEffort } from '@/utils/format'
import { resolveUsageRequestType } from '@/utils/usageRequestType'

const props = withDefaults(defineProps<{
  show: boolean
  usageId: number | null
  usage?: AdminUsageLog | null
}>(), { usage: null })
const emit = defineEmits<{
  'update:show': [show: boolean]
  close: []
}>()
const { locale } = useI18n()

// 本地双语文案，避免与共享翻译文件并行写入。
const messages = {
  en: {
    title: 'Usage request diagnostic',
    sensitiveNotice: 'Short-lived diagnostic snapshots may still contain sensitive prompts and personal data. Redaction is not full anonymization. Open or copy content only when needed for troubleshooting; copying uses your local clipboard.',
    loading: 'Loading diagnostic…',
    loadFailed: 'Could not load this diagnostic. Check your access and connection, then retry.',
    retry: 'Retry',
    close: 'Close',
    usageSummary: 'Usage record',
    recordId: 'Usage ID',
    time: 'Usage time',
    user: 'User',
    apiKeyId: 'API key ID',
    account: 'Account',
    requestedModel: 'Requested model',
    upstreamModel: 'Model sent upstream',
    responseModel: 'Model reported by upstream',
    modelMismatch: 'Upstream model mismatch',
    mappingChain: 'Model mapping chain',
    requestedEffort: 'Requested reasoning effort',
    upstreamEffort: 'Forwarded reasoning effort',
    requestType: 'Request type',
    inboundEndpoint: 'Inbound endpoint',
    upstreamEndpoint: 'Upstream endpoint',
    duration: 'Total duration',
    firstToken: 'Time to first token',
    inputTokens: 'Input tokens',
    outputTokens: 'Output tokens',
    cacheReadTokens: 'Cache read tokens',
    cacheCreationTokens: 'Cache creation tokens',
    cache5mTokens: 'Cache creation tokens (5m)',
    cache1hTokens: 'Cache creation tokens (1h)',
    imageInputTokens: 'Image input tokens',
    imageOutputTokens: 'Image output tokens',
    imageCount: 'Image count',
    requestId: 'Request ID',
    upstreamRequestId: 'Upstream request ID',
    userAgent: 'User-Agent',
    yes: 'Yes',
    no: 'No',
    captureEnabled: 'Capture enabled now',
    enabled: 'Enabled',
    disabled: 'Disabled',
    retention: 'Retention window',
    capturedAt: 'Captured at',
    expiresAt: 'Expires at',
    captured: 'Snapshot captured',
    capturedDescription: 'Inspect the saved inbound request and final request bodies for the recorded upstream attempts below.',
    notCaptured: 'Not captured',
    notCapturedDescription: 'No request snapshot was captured for this record (including older records or requests made while capture was disabled). Historical content cannot be reconstructed.',
    expired: 'Snapshot expired',
    expiredDescription: 'The short retention window has elapsed. Only usage metadata remains; the request content cannot be reconstructed.',
    evicted: 'Snapshot evicted',
    evictedDescription: 'The snapshot was removed to stay within storage limits. Only usage metadata remains; the request content cannot be reconstructed.',
    unavailable: 'Snapshot unavailable',
    unavailableDescription: 'The snapshot is currently unavailable. Usage metadata is still shown, but no request content can be displayed.',
    snapshotMetadata: 'Snapshot metadata',
    schemaVersion: 'Schema version',
    captureStatus: 'Capture status',
    responseStatus: 'HTTP response status',
    boundRequestId: 'Bound usage request ID',
    boundCreatedAt: 'Bound usage time',
    droppedAttempts: 'Omitted upstream attempts',
    incompleteAttempts: 'The attempt list is incomplete.',
    inbound: 'Inbound request',
    attempt: 'Upstream attempt',
    attemptId: 'Attempt ID',
    clientRequestId: 'Client request ID',
    endpoint: 'Endpoint',
    method: 'Method',
    startedAt: 'Started at',
    platform: 'Platform',
    error: 'Error',
    inboundBody: 'Saved inbound body',
    upstreamBody: 'Final upstream request body',
    responseSummary: 'Limited upstream response summary',
    expand: 'Show',
    collapse: 'Hide',
    copy: 'Copy saved text',
    copied: 'Copied',
    copyFailed: 'Copy failed. Your browser may not allow clipboard access.',
    originalBytes: 'Original bytes',
    storedBytes: 'Stored bytes',
    redacted: 'Partially redacted',
    truncated: 'Truncated',
    omittedReason: 'Content omitted',
    bodyNote: 'This is the stored text, which may be redacted, truncated, or omitted—not a guaranteed complete original request.',
    limitedSummary: 'Only a limited response summary is saved. This is not the full response body or full SSE stream.',
    emptyPayload: 'No saved content.',
    noAttempts: 'No upstream attempts were captured.',
    noResponseSummary: 'No upstream response summary was captured.'
  },
  zh: {
    title: '用量请求诊断',
    sensitiveNotice: '诊断快照仅短期保留，但 prompt 和个人信息仍可能包含敏感内容；脱敏不等于完全匿名。请仅在排查需要时查看或复制，复制只写入本机剪贴板。',
    loading: '正在加载诊断详情…',
    loadFailed: '无法加载这条记录的诊断详情，请检查权限与网络后重试。',
    retry: '重试',
    close: '关闭',
    usageSummary: '用量记录',
    recordId: '用量 ID',
    time: '用量时间',
    user: '用户',
    apiKeyId: 'API Key ID',
    account: '账号',
    requestedModel: '请求模型',
    upstreamModel: '发往上游的模型',
    responseModel: '上游响应声明的模型',
    modelMismatch: '上游模型不一致',
    mappingChain: '模型映射链',
    requestedEffort: '请求推理强度',
    upstreamEffort: '实际转发推理强度',
    requestType: '请求类型',
    inboundEndpoint: '入站端点',
    upstreamEndpoint: '上游端点',
    duration: '总耗时',
    firstToken: '首字耗时',
    inputTokens: '输入 Token',
    outputTokens: '输出 Token',
    cacheReadTokens: '缓存读取 Token',
    cacheCreationTokens: '缓存创建 Token',
    cache5mTokens: '缓存创建 Token（5m）',
    cache1hTokens: '缓存创建 Token（1h）',
    imageInputTokens: '图片输入 Token',
    imageOutputTokens: '图片输出 Token',
    imageCount: '图片数量',
    requestId: '请求 ID',
    upstreamRequestId: '上游请求 ID',
    userAgent: 'User-Agent',
    yes: '是',
    no: '否',
    captureEnabled: '当前捕获开关',
    enabled: '已开启',
    disabled: '已关闭',
    retention: '保留期限',
    capturedAt: '捕获时间',
    expiresAt: '过期时间',
    captured: '已捕获快照',
    capturedDescription: '下方可查看已保存的入站内容，以及每次已记录上游尝试实际发送的最终请求体。',
    notCaptured: '未捕获',
    notCapturedDescription: '这条记录没有捕获请求快照（包括旧记录或当时未开启捕获的请求），历史内容无法事后还原。',
    expired: '快照已过期',
    expiredDescription: '已超过短期保留期限，仅保留用量元数据，请求内容无法还原。',
    evicted: '快照已移除',
    evictedDescription: '快照已因存储容量上限移除，仅保留用量元数据，请求内容无法还原。',
    unavailable: '快照不可用',
    unavailableDescription: '快照目前不可用，仍可查看用量元数据，但无法显示请求内容。',
    snapshotMetadata: '快照元数据',
    schemaVersion: '格式版本',
    captureStatus: '捕获状态',
    responseStatus: 'HTTP 响应状态',
    boundRequestId: '关联用量请求 ID',
    boundCreatedAt: '关联用量时间',
    droppedAttempts: '未保存的上游尝试数',
    incompleteAttempts: '上游尝试列表并不完整。',
    inbound: '入站请求',
    attempt: '上游尝试',
    attemptId: '尝试 ID',
    clientRequestId: '客户端请求 ID',
    endpoint: '端点',
    method: '方法',
    startedAt: '开始时间',
    platform: '平台',
    error: '错误',
    inboundBody: '已保存的入站请求体',
    upstreamBody: '最终上游请求体',
    responseSummary: '上游有限响应摘要',
    expand: '展开',
    collapse: '收起',
    copy: '复制已保存文本',
    copied: '已复制',
    copyFailed: '复制失败，浏览器可能未允许剪贴板访问。',
    originalBytes: '原始字节数',
    storedBytes: '保存字节数',
    redacted: '已部分脱敏',
    truncated: '已截断',
    omittedReason: '内容省略原因',
    bodyNote: '展示的是保存的文本，可能已脱敏、截断或省略，并不保证是完整原始请求。',
    limitedSummary: '仅保存有限响应摘要，不是完整响应体，也不是完整 SSE 流。',
    emptyPayload: '没有已保存的内容。',
    noAttempts: '没有捕获到上游尝试。',
    noResponseSummary: '未捕获上游响应摘要。'
  }
}
const text = computed(() => messages[String(locale?.value ?? 'en').toLowerCase().startsWith('zh') ? 'zh' : 'en'])

const detail = shallowRef<UsageDiagnosticResponse | null>(null)
const loading = ref(false)
const loadFailed = ref(false)
const expandedPayloads = ref(new Set<string>())
const copiedPayload = ref<string | null>(null)
const copyFailed = ref(false)
let controller: AbortController | null = null
let requestRevision = 0
let copyTimer: ReturnType<typeof setTimeout> | null = null

const usage = computed(() => props.show ? detail.value?.usage ?? props.usage : null)
const snapshot = computed(() => detail.value?.diagnostic.status === 'captured' ? detail.value.diagnostic.payload ?? null : null)
const dateTime = (value?: string | null): string => value ? formatDateTime(value) : '—'
const display = (value: string | number | null | undefined): string => value == null || value === '' ? '—' : String(value)
const tokens = (value?: number): string => (value ?? 0).toLocaleString()
const bytes = (value: number): string => `${value.toLocaleString()} B`
const duration = (value?: number | null): string => value == null ? '—' : `${value.toLocaleString()} ms`
type Field = { label: string; value: string }
const field = (label: string, value: string | number | null | undefined): Field => ({ label, value: display(value) })

const usageFields = computed<Field[]>(() => {
  const row = usage.value
  if (!row) return []
  const m = text.value
  const fields = [
    field(m.recordId, row.id),
    field(m.time, dateTime(row.created_at)),
    field(m.user, row.user?.email ? `${row.user.email} #${row.user_id}` : row.user_id),
    field(m.apiKeyId, row.api_key_id),
    field(m.account, row.account?.name ? `${row.account.name} #${row.account_id}` : row.account_id),
    field(m.requestedModel, row.model),
    field(m.upstreamModel, row.upstream_model || row.model),
    field(m.responseModel, row.upstream_response_model),
    field(m.modelMismatch, row.upstream_model_mismatch == null ? '—' : row.upstream_model_mismatch ? m.yes : m.no),
    field(m.requestedEffort, formatReasoningEffort(row.reasoning_effort)),
    field(m.upstreamEffort, formatReasoningEffort(row.upstream_reasoning_effort)),
    field(m.requestType, resolveUsageRequestType(row)),
    field(m.inboundEndpoint, row.inbound_endpoint),
    field(m.upstreamEndpoint, row.upstream_endpoint),
    field(m.duration, duration(row.duration_ms)),
    field(m.firstToken, duration(row.first_token_ms)),
    field(m.inputTokens, tokens(row.input_tokens)),
    field(m.outputTokens, tokens(row.output_tokens)),
    field(m.cacheReadTokens, tokens(row.cache_read_tokens)),
    field(m.cacheCreationTokens, tokens(row.cache_creation_tokens)),
    field(m.cache5mTokens, tokens(row.cache_creation_5m_tokens)),
    field(m.cache1hTokens, tokens(row.cache_creation_1h_tokens)),
    field(m.requestId, row.request_id),
    field(m.upstreamRequestId, row.upstream_request_id),
    field(m.userAgent, row.user_agent)
  ]
  if (row.model_mapping_chain) fields.push(field(m.mappingChain, row.model_mapping_chain))
  if (row.image_input_tokens > 0) fields.push(field(m.imageInputTokens, tokens(row.image_input_tokens)))
  if (row.image_output_tokens > 0) fields.push(field(m.imageOutputTokens, tokens(row.image_output_tokens)))
  if (row.image_count > 0) fields.push(field(m.imageCount, row.image_count))
  return fields
})

const statusMessage = computed(() => {
  const m = text.value
  switch (detail.value?.diagnostic.status) {
    case 'captured': return snapshot.value
      ? { title: m.captured, description: m.capturedDescription }
      : { title: m.unavailable, description: m.unavailableDescription }
    case 'not_captured': return { title: m.notCaptured, description: m.notCapturedDescription }
    case 'expired': return { title: m.expired, description: m.expiredDescription }
    case 'evicted': return { title: m.evicted, description: m.evictedDescription }
    default: return { title: m.unavailable, description: m.unavailableDescription }
  }
})

const snapshotFields = computed<Field[]>(() => {
  const payload = snapshot.value
  if (!payload) return []
  const m = text.value
  return [
    field(m.schemaVersion, payload.schema_version),
    field(m.captureStatus, payload.capture_status),
    field(m.responseStatus, payload.status || '—'),
    field(m.storedBytes, bytes(payload.stored_bytes)),
    field(m.apiKeyId, payload.binding.api_key_id),
    field(m.boundRequestId, payload.binding.usage_request_id),
    field(m.boundCreatedAt, dateTime(payload.binding.usage_created_at))
  ]
})

type PayloadBlock = { key: string; title: string; payload: UsageDiagnosticPayload; summary?: boolean }
type CaptureSection = { key: string; title: string; fields: Field[]; payloads: PayloadBlock[]; noResponseSummary?: boolean }
const captureSections = computed<CaptureSection[]>(() => {
  const payload = snapshot.value
  if (!payload) return []
  const m = text.value
  const inbound = payload.inbound
  return [
    {
      key: 'inbound',
      title: m.inbound,
      fields: [
        field(m.requestId, inbound.request_id),
        field(m.clientRequestId, inbound.client_request_id),
        field(m.endpoint, inbound.endpoint),
        field(m.method, inbound.method),
        field(m.startedAt, dateTime(inbound.started_at)),
        field(m.userAgent, inbound.user_agent)
      ],
      payloads: [{ key: 'inbound-body', title: m.inboundBody, payload: inbound.body }]
    },
    ...payload.attempts.map((attempt, index): CaptureSection => ({
      key: `attempt-${attempt.id}`,
      title: `${m.attempt} #${index + 1}`,
      fields: [
        field(m.attemptId, attempt.id),
        field(m.account, attempt.account_id),
        field(m.platform, attempt.platform),
        field(m.upstreamModel, attempt.model),
        field(m.endpoint, attempt.endpoint),
        field(m.method, attempt.method),
        field(m.startedAt, dateTime(attempt.started_at)),
        field(m.duration, duration(attempt.duration_ms)),
        field(m.responseStatus, attempt.status || '—'),
        field(m.upstreamRequestId, attempt.upstream_request_id),
        field(m.error, attempt.error)
      ],
      payloads: [
        { key: `attempt-${attempt.id}-body`, title: m.upstreamBody, payload: attempt.body },
        ...(attempt.response_summary ? [{ key: `attempt-${attempt.id}-response`, title: m.responseSummary, payload: attempt.response_summary, summary: true }] : [])
      ],
      noResponseSummary: !attempt.response_summary
    }))
  ]
})

// 切换、关闭、卸载时同步清除大 payload，并使迟到的响应失效。
function clearDiagnostic() {
  requestRevision += 1
  controller?.abort()
  controller = null
  detail.value = null
  // computed 是惰性求值，这里立即重算，释放缓存的快照和正文引用。
  void snapshot.value
  void captureSections.value
  loading.value = false
  loadFailed.value = false
  expandedPayloads.value = new Set()
  copiedPayload.value = null
  copyFailed.value = false
  if (copyTimer) clearTimeout(copyTimer)
  copyTimer = null
}

async function loadDiagnostic() {
  clearDiagnostic()
  if (!props.show || props.usageId == null) return
  const revision = requestRevision
  const current = new AbortController()
  controller = current
  loading.value = true
  try {
    const response = await getUsageDiagnostic(props.usageId, { signal: current.signal })
    if (revision !== requestRevision || current.signal.aborted) return
    detail.value = response
  } catch {
    if (revision !== requestRevision || current.signal.aborted) return
    loadFailed.value = true
  } finally {
    if (revision === requestRevision && !current.signal.aborted) {
      controller = null
      loading.value = false
    }
  }
}

function togglePayload(key: string) {
  if (expandedPayloads.value.has(key)) expandedPayloads.value.delete(key)
  else expandedPayloads.value.add(key)
}

// 只在管理员明确点击复制时写入本机剪贴板，不做外发或持久化。
async function copyPayload(key: string, content: string) {
  if (!content) return
  const revision = requestRevision
  copyFailed.value = false
  try {
    await navigator.clipboard.writeText(content)
    if (revision !== requestRevision) return
    copiedPayload.value = key
    if (copyTimer) clearTimeout(copyTimer)
    copyTimer = setTimeout(() => { copiedPayload.value = null; copyTimer = null }, 2000)
  } catch {
    if (revision === requestRevision) copyFailed.value = true
  }
}

function close() {
  clearDiagnostic()
  emit('update:show', false)
  emit('close')
}

// 先同步作废旧请求，再合并同一轮 props 变化，避免重开时请求旧 ID。
watch(() => [props.show, props.usageId] as const, clearDiagnostic, { flush: 'sync' })
watch(() => [props.show, props.usageId] as const, loadDiagnostic, { immediate: true })
onBeforeUnmount(clearDiagnostic)
</script>

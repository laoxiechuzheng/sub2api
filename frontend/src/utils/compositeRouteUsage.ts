import type { AdminUsageLog } from '@/types'

type Translate = (key: string, params?: Record<string, unknown>) => string

// Composite 显式路由命中的使用记录：路由编号 + 请求类型 + 备用模型链。
export function isCompositeRouted(log: Pick<AdminUsageLog, 'composite_route_id'>): boolean {
  return typeof log.composite_route_id === 'number' && log.composite_route_id > 0
}

export function compositeRouteBadgeLabel(log: AdminUsageLog, t: Translate): string {
  return log.composite_route_kind === 'compaction'
    ? t('usage.compositeRouteCompaction')
    : t('usage.compositeRouteRouted')
}

export function compositeRouteTitle(log: AdminUsageLog, t: Translate): string {
  if (!isCompositeRouted(log)) return ''
  const lines = [`${t('usage.compositeRouteId')}: #${log.composite_route_id}`]
  if (log.composite_route_kind) {
    lines.push(`${t('usage.compositeRouteKind')}: ${compositeRouteKindLabel(log.composite_route_kind, t)}`)
  }
  if (log.composite_fallback_chain) {
    lines.push(`${t('usage.compositeFallbackChain')}: ${log.composite_fallback_chain}`)
  }
  return lines.join('\n')
}

// 导出用的单元格文本，未命中路由时为空。
export function compositeRouteExportText(log: AdminUsageLog, t: Translate): string {
  if (!isCompositeRouted(log)) return ''
  const parts = [`#${log.composite_route_id} ${compositeRouteBadgeLabel(log, t)}`]
  if (log.composite_fallback_chain) parts.push(log.composite_fallback_chain)
  return parts.join(' · ')
}

function compositeRouteKindLabel(kind: string, t: Translate): string {
  if (kind === 'compaction') return t('usage.compositeRouteKindCompaction')
  if (kind === 'conversation') return t('usage.compositeRouteKindConversation')
  return kind
}

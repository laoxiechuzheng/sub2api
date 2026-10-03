import { apiClient } from '../client'
import type { AdminUsageLog } from '@/types'

export type UsageDiagnosticStatus = 'captured' | 'not_captured' | 'expired' | 'evicted' | 'unavailable'

export interface UsageDiagnosticPayload {
  content: string
  original_bytes: number
  stored_bytes: number
  redacted: boolean
  truncated: boolean
  omitted_reason?: string
}

export interface UsageDiagnosticAttempt {
  id: number
  account_id: number
  platform?: string
  endpoint: string
  method: string
  model?: string
  started_at: string
  duration_ms?: number
  status: number
  error?: string
  upstream_request_id?: string
  body: UsageDiagnosticPayload
  response_summary?: UsageDiagnosticPayload | null
}

export interface UsageDiagnosticSnapshot {
  schema_version: number
  binding: {
    api_key_id: number
    usage_request_id: string
    usage_created_at: string
  }
  inbound: {
    request_id: string
    client_request_id: string
    endpoint: string
    method: string
    user_agent?: string
    started_at: string
    body: UsageDiagnosticPayload
  }
  status: number
  captured_at: string
  capture_status: string
  dropped_attempts: number
  stored_bytes: number
  attempts: UsageDiagnosticAttempt[]
}

export interface UsageDiagnosticResponse {
  usage: AdminUsageLog
  capture_enabled: boolean
  retention_hours: number
  diagnostic: {
    status: UsageDiagnosticStatus
    captured_at?: string
    expires_at?: string
    payload?: UsageDiagnosticSnapshot | null
  }
}

// 管理员主动打开详情时才读取大请求体，不混入列表或普通用户接口。
export async function getUsageDiagnostic(
  usageId: number,
  options?: { signal?: AbortSignal }
): Promise<UsageDiagnosticResponse> {
  const { data } = await apiClient.get<UsageDiagnosticResponse>(`/admin/usage/${usageId}/diagnostic`, {
    signal: options?.signal
  })
  return data
}

import { apiClient } from '../client'
import type { AccountPlatform } from '@/types'

export interface AITarget {
  target_group_id: number | null
  target_account_id: number | null
}
export interface AICandidate extends AITarget {
  id: string
  platform: AccountPlatform
  model: string
  description: string
  cost_tier?: string
  max_input_tokens?: number
  max_output_tokens?: number
  disable_tools?: boolean
  disable_images?: boolean
  enabled: boolean
}
export interface AIPolicy {
  group_id: number
  enabled: boolean
  version: number
  engine: 'generic' | 'jev'
  decision_platform: AccountPlatform
  decision_model: string
  decision_account_id: number | null
  decision_group_id: number | null
  context_mode: 'metadata' | 'task_excerpt'
  prompt: string
  timeout_ms: number
  fallback_id: string
  candidates: AICandidate[]
}
export interface AIPolicyState {
  draft: AIPolicy
  published: AIPolicy | null
  revision: number
}
export interface AIPreview {
  eligible: string[]
  excluded: {id: string; reason: string}[]
  external_call: false
}
export interface AITestDecision {
  candidate_id: string
  model: string
  protocol: string
  input_tokens: number
  output_tokens: number
  elapsed_ms: number
  external_call: boolean
  reason?: string
  failure_reason?: string
}
export interface AIDecisionRecord {
  request_id: string
  client_request_id?: string
  usage_request_id?: string
  at: string
  public_model: string
  candidate_id: string
  execution_model: string
  decision_model: string
  decision_account_id?: number
  input_tokens: number
  output_tokens: number
  latency_ms: number
  source: string
  failure_reason?: string
  policy_version: number
  decision_cost_usd?: number | null
}
const url = (id: number) => `/admin/groups/${id}/ai-routing`
export const aiRouting = {
  async load(id: number) { return (await apiClient.get<AIPolicyState>(url(id))).data },
  async save(id: number, revision: number, policy: AIPolicy) {
    return (await apiClient.put<AIPolicyState>(url(id), {revision, policy})).data
  },
  async publish(id: number, revision: number) {
    return (await apiClient.post<AIPolicyState>(url(id) + '/publish', {revision})).data
  },
  async disable(id: number, revision: number) {
    return (await apiClient.post<AIPolicyState>(url(id) + '/disable', {revision})).data
  },
  async check(id: number, policy: AIPolicy, sample: string) {
    return (await apiClient.post<AIPreview>(url(id) + '/preview', {policy, sample})).data
  },
  async test(id: number, policy: AIPolicy, sample: string) {
    return (await apiClient.post<AITestDecision>(url(id) + '/test-decision', {policy, sample}, {timeout: policy.timeout_ms + 10000})).data
  },
  async decisions(id: number) {
    return (await apiClient.get<{items: AIDecisionRecord[]}>(url(id) + '/decisions')).data.items || []
  },
  async models(id: number, platform: string, target: AITarget, signal?: AbortSignal) {
    return (await apiClient.get<{models: string[]}>(url(id) + '/models', {params: {platform, ...target}, signal})).data.models
  }
}

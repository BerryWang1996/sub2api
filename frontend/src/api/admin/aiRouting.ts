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
  context_window_tokens?: number
  supports_tools?: boolean | null
  supports_images?: boolean | null
  disable_tools?: boolean
  disable_images?: boolean
  enabled: boolean
}
export interface AIContextBudget {
  max_history_bytes?: number
  max_history_turns?: number
  max_segments?: number
  max_decision_calls?: number
  max_parallel_calls?: number
  max_input_bytes?: number
  max_total_input_bytes?: number
  max_output_bytes?: number
  max_total_output_bytes?: number
  max_refinement_passes?: number
}
export interface AIContextDiagnostic {
  projection_version?: string
  source?: string
  mode?: string
  coverage?: string
  history_turns?: number
  history_bytes?: number
  total_spans?: number
  scanned_spans?: number
  selected_span_ids?: string[]
  segments?: number
  scanned_segments?: number
  classifier_calls?: number
  input_bytes?: number
  redacted_spans?: number
  excluded_blocks?: number
  failure_reason?: string
}
export type AIClientEndpoint = 'messages' | 'responses' | 'chat_completions' | 'responses_ws' | 'gemini'
export interface AIPolicy {
  group_id: number
  schema_version?: number
  enabled_endpoints?: AIClientEndpoint[]
  enabled: boolean
  version: number
  engine: 'generic' | 'jev'
  decision_platform: AccountPlatform
  decision_model: string
  decision_account_id: number | null
  decision_group_id: number | null
  context_mode: 'metadata' | 'task_excerpt' | 'bounded_history'
  context_budget?: AIContextBudget
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
export interface AICapacityAccountCheck {
  account_id: number
  allowed: boolean
  reason?: string
  capacity_verified: boolean
  final_payload_checked?: boolean
  capacity_method?: string
  context_window_tokens?: number
  input_bytes?: number
  output_reserve_tokens?: number
  metadata_source?: string
  metadata_synced_at?: string
  warnings?: string[]
}
export interface AICandidateCapacityCheck {
  candidate_id: string
  model: string
  reason?: string
  accounts: AICapacityAccountCheck[]
}
export interface AIPreview {
  sample_endpoint?: AIClientEndpoint
  eligible: string[]
  excluded: {id: string; reason: string}[]
  external_call: false
  context?: AIContextDiagnostic
  needs_decision?: boolean
  sample_spans?: {id: string; role: string; text: string}[]
  capacity_checks?: AICandidateCapacityCheck[]
}
export interface AIDecisionStage {
  stage: string
  model: string
  reported_model?: string
  protocol: string
  account_id: number
  input_tokens: number
  output_tokens: number
  input_bytes: number
  output_bytes?: number
  usage_known: boolean
  attempted?: boolean
  failure_reason?: string
}
export interface AITestDecision {
  sample_endpoint?: AIClientEndpoint
  candidate_id: string
  model: string
  protocol: string
  input_tokens: number
  output_tokens: number
  elapsed_ms: number
  external_call: boolean
  reason?: string
  failure_reason?: string
  context?: AIContextDiagnostic
  decision_calls?: number
  stages?: AIDecisionStage[]
}
export interface AIDecisionRecord {
  endpoint?: string
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
  context?: AIContextDiagnostic
  decision_calls?: number
  stages?: AIDecisionStage[]
}
export interface AIBackgroundJob {
  id: string
  request_id?: string
  usage_request_id: string
  status: string
  failure_reason?: string
  public_model: string
  execution_model: string
  account_id: number
  subscription_id?: number
  hold_amount: number
  actual_cost: number | null
  usage_known: boolean
  input_tokens: number
  output_tokens: number
  cache_sync_pending: boolean
  created_at: string
  updated_at: string
}
const url = (id: number) => `/admin/groups/${id}/ai-routing`
export function aiPolicyClientEndpoints(policy?: AIPolicy | null): AIClientEndpoint[] {
  if (!policy || ![0, 1, 2].includes(policy.schema_version ?? 0)) return []
  if (policy.enabled_endpoints == null) return ['messages']
  if (policy.enabled_endpoints.some(endpoint => !['messages', 'responses', 'chat_completions', 'responses_ws', 'gemini'].includes(endpoint))) return []
  if (new Set(policy.enabled_endpoints).size !== policy.enabled_endpoints.length) return []
  if ((policy.schema_version ?? 0) < 2 && policy.enabled_endpoints.some(endpoint => endpoint !== 'messages')) return []
  return [...policy.enabled_endpoints]
}
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
  async check(id: number, policy: AIPolicy, sample: string, sampleEndpoint: AIClientEndpoint = 'messages') {
    return (await apiClient.post<AIPreview>(url(id) + '/preview', {policy, sample, sample_endpoint: sampleEndpoint})).data
  },
  async test(id: number, policy: AIPolicy, sample: string, sampleEndpoint: AIClientEndpoint = 'messages') {
    return (await apiClient.post<AITestDecision>(url(id) + '/test-decision', {policy, sample, sample_endpoint: sampleEndpoint}, {timeout: policy.timeout_ms + 10000})).data
  },
  async decisions(id: number) {
    return (await apiClient.get<{items: AIDecisionRecord[]}>(url(id) + '/decisions')).data.items || []
  },
  async backgroundJobs(id: number, status?: string) {
    return (await apiClient.get<{items: AIBackgroundJob[]}>(url(id) + '/background-jobs', {params: {status}})).data.items || []
  },
  async models(id: number, platform: string, target: AITarget, signal?: AbortSignal) {
    return (await apiClient.get<{models: string[]}>(url(id) + '/models', {params: {platform, ...target}, signal})).data.models
  }
}

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { AIPolicy, AIPreview } from '@/api/admin/aiRouting'
const { post } = vi.hoisted(() => ({post:vi.fn()}))
vi.mock('@/api/client', () => ({apiClient:{post}}))
import { aiRouting, aiPolicyClientEndpoints } from '@/api/admin/aiRouting'

describe('admin AI routing offline preview API', () => {
  beforeEach(()=>post.mockReset())
  it('retains per-account checks independently and sends only the preview request', async () => {
    const policy: AIPolicy = {group_id:3,enabled:false,version:0,engine:'generic',decision_platform:'openai',decision_model:'decider',decision_account_id:10,decision_group_id:null,context_mode:'bounded_history',prompt:'Use the configured pool',timeout_ms:2000,fallback_id:'',candidates:[]}
    const preview: AIPreview = {eligible:['pool'],excluded:[],external_call:false,capacity_checks:[{candidate_id:'pool',model:'execution-alias',accounts:[
      {account_id:11,allowed:true,capacity_verified:false,final_payload_checked:false,capacity_method:'request_bytes_plus_output',context_window_tokens:8192,input_bytes:1000,output_reserve_tokens:512,metadata_source:'saved-list',metadata_synced_at:'2026-11-06T01:00:00Z',warnings:['input_tokens_estimated']},
      {account_id:12,allowed:false,capacity_verified:false,reason:'execution_context_window_unknown'}
    ]}]}
    post.mockResolvedValue({data:preview})
    const response = await aiRouting.check(3,policy,'Synthetic sample')
    expect(response).toEqual(preview)
    expect(response.capacity_checks?.[0].accounts[1].metadata_source).toBeUndefined()
    expect(post).toHaveBeenCalledTimes(1)
    expect(post).toHaveBeenCalledWith('/admin/groups/3/ai-routing/preview',{policy,sample:'Synthetic sample',sample_endpoint:'messages'})
  })
  it.each(['responses', 'chat_completions', 'responses_ws', 'gemini'] as const)('keeps the %s sample endpoint separate from the decision protocol', async (endpoint) => {
    const policy = {schema_version:2,enabled_endpoints:[endpoint],timeout_ms:2500} as AIPolicy
    const response = {sample_endpoint:endpoint,protocol:'anthropic_messages',external_call:false}
    const sample = endpoint === 'responses' ? '{"input":"Continue"}' : '{"messages":[{"role":"user","content":"Continue"}]}'
    post.mockResolvedValue({data:response})
    await expect(aiRouting.check(3,policy,sample,endpoint)).resolves.toEqual(response)
    await expect(aiRouting.test(3,policy,sample,endpoint)).resolves.toEqual(response)
    expect(post).toHaveBeenNthCalledWith(1,'/admin/groups/3/ai-routing/preview',{policy,sample,sample_endpoint:endpoint})
    expect(post).toHaveBeenNthCalledWith(2,'/admin/groups/3/ai-routing/test-decision',{policy,sample,sample_endpoint:endpoint},{timeout:12500})
  })
  it('defaults only missing endpoint configuration to Messages and fails closed for unsupported schemas', () => {
    expect(aiPolicyClientEndpoints({} as AIPolicy)).toEqual(['messages'])
    expect(aiPolicyClientEndpoints({schema_version:1} as AIPolicy)).toEqual(['messages'])
    expect(aiPolicyClientEndpoints({schema_version:2,enabled_endpoints:[]} as unknown as AIPolicy)).toEqual([])
    expect(aiPolicyClientEndpoints({schema_version:3} as AIPolicy)).toEqual([])
    expect(aiPolicyClientEndpoints({schema_version:0,enabled_endpoints:['responses']} as AIPolicy)).toEqual([])
    expect(aiPolicyClientEndpoints({schema_version:2,enabled_endpoints:['responses']} as AIPolicy)).toEqual(['responses'])
    expect(aiPolicyClientEndpoints({schema_version:2,enabled_endpoints:['messages','responses','chat_completions']} as AIPolicy)).toEqual(['messages','responses','chat_completions'])
    expect(aiPolicyClientEndpoints({schema_version:1,enabled_endpoints:['chat_completions']} as AIPolicy)).toEqual([])
    expect(aiPolicyClientEndpoints({schema_version:2,enabled_endpoints:['gemini','responses_ws']} as AIPolicy)).toEqual(['gemini','responses_ws'])
    expect(aiPolicyClientEndpoints({schema_version:1,enabled_endpoints:['gemini','responses_ws']} as AIPolicy)).toEqual([])
    expect(aiPolicyClientEndpoints({schema_version:2,enabled_endpoints:['responses_websocket']} as unknown as AIPolicy)).toEqual([])
  })
})

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AIDecisionHistory from '../AIDecisionHistory.vue'
import { aiRouting } from '@/api/admin/aiRouting'

vi.mock('@/api/admin/aiRouting', () => ({ aiRouting: { decisions: vi.fn() } }))
const { copyToClipboard } = vi.hoisted(() => ({ copyToClipboard: vi.fn() }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

describe('AI routing decision history', () => {
  beforeEach(() => vi.clearAllMocks())
  it('shows Responses and Chat separately and treats records without an endpoint as legacy Messages', async () => {
    const record = {request_id:'legacy',at:'2026-11-06T00:00:00Z',public_model:'alias',candidate_id:'c1',execution_model:'model',decision_model:'decider',input_tokens:0,output_tokens:0,latency_ms:1,source:'single',policy_version:2}
    vi.mocked(aiRouting.decisions).mockResolvedValue([record,{...record,request_id:'responses',endpoint:'responses'},{...record,request_id:'chat',endpoint:'chat_completions'}])
    const wrapper = mount(AIDecisionHistory,{props:{groupId:3}})
    wrapper.get('details').element.open = true
    await wrapper.get('details').trigger('toggle')
    await flushPromises()
    expect(wrapper.findAll('[data-testid="ai-history-endpoint"]').map(row=>row.text())).toEqual([
      'admin.groups.aiRouting.clientEndpoint · admin.groups.aiRouting.endpointMessages',
      'admin.groups.aiRouting.clientEndpoint · admin.groups.aiRouting.endpointResponses',
      'admin.groups.aiRouting.clientEndpoint · admin.groups.aiRouting.endpointChat'
    ])
    wrapper.unmount()
  })
  it.each(['continuation', 'cancelled', 'empty', 'failed'])('labels the %s execution outcome', async (source) => {
    vi.mocked(aiRouting.decisions).mockResolvedValue([{
      request_id: 'outcome-request', at: '2026-11-05T00:00:00Z', public_model: 'public-alias',
      candidate_id: '', execution_model: '', decision_model: '', input_tokens: 0,
      output_tokens: 0, latency_ms: 0, source, policy_version: 2
    }])
    const wrapper = mount(AIDecisionHistory, { props: { groupId: 3 } })
    wrapper.get('details').element.open = true
    await wrapper.get('details').trigger('toggle')
    await flushPromises()
    expect(wrapper.text()).toContain('admin.groups.aiRouting.reason_' + source)
    wrapper.unmount()
  })
  it('loads on expansion and labels fallback failures and estimated cost', async () => {
    vi.mocked(aiRouting.decisions).mockResolvedValue([{
      request_id: 'local-request', at: '2026-11-05T00:00:00Z', public_model: 'public-alias',
      candidate_id: 'c2', execution_model: 'private-model', decision_model: 'custom-decider',
      input_tokens: 10, output_tokens: 1, latency_ms: 30, source: 'fallback',
      failure_reason: 'timeout', policy_version: 2, decision_cost_usd: 0.001
    }])
    const wrapper = mount(AIDecisionHistory, { props: { groupId: 3 } })
    expect(aiRouting.decisions).not.toHaveBeenCalled()
    wrapper.get('details').element.open = true
    await wrapper.get('details').trigger('toggle')
    await flushPromises()
    expect(aiRouting.decisions).toHaveBeenCalledWith(3)
    expect(wrapper.text()).toContain('public-alias')
    expect(wrapper.text()).toContain('private-model')
    expect(wrapper.text()).toContain('timeout')
    expect(wrapper.text()).toContain('admin.groups.aiRouting.estimatedCost')
    expect(wrapper.find('button[title]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('keeps the gateway request ID and copies the exact usage ID for reconciliation', async () => {
    const usageID = 'client:ui-r3-request-with-a-long-public-correlation-id'
    vi.mocked(aiRouting.decisions).mockResolvedValue([{
      request_id: 'gateway-request', client_request_id: 'ui-r3-request-with-a-long-public-correlation-id',
      usage_request_id: usageID, at: '2026-11-05T00:00:00Z', public_model: 'public-alias',
      candidate_id: 'c2', execution_model: 'private-model', decision_model: 'custom-decider',
      input_tokens: 10, output_tokens: 1, latency_ms: 30, source: 'model', policy_version: 2
    }])
    const wrapper = mount(AIDecisionHistory, { props: { groupId: 3 } })
    wrapper.get('details').element.open = true
    await wrapper.get('details').trigger('toggle')
    await flushPromises()
    expect(wrapper.get('code').text()).toBe('gateway-request')
    const button = wrapper.get('button[title]')
    expect(button.text()).toBe(usageID)
    expect(button.attributes('title')).toBe(usageID)
    expect(button.attributes('aria-label')).toContain('admin.groups.aiRouting.copyUsageRequest')
    await button.trigger('click')
    expect(copyToClipboard).toHaveBeenCalledWith(usageID)
    wrapper.unmount()
  })
  it('shows bounded history coverage and stage usage without treating unreported usage as zero', async () => {
    vi.mocked(aiRouting.decisions).mockResolvedValue([{
      request_id:'history-request', at:'2026-11-06T00:00:00Z', public_model:'alias', candidate_id:'c2',
      execution_model:'execution', decision_model:'jev-1.14.0', input_tokens:10, output_tokens:0,
      latency_ms:30, source:'fallback', policy_version:4, decision_calls:2,
      context:{source:'request',mode:'evidence',coverage:'partial',history_turns:8,history_bytes:36000,total_spans:8,scanned_spans:4,selected_span_ids:['turn-2-span-1'],segments:3,scanned_segments:1,projection_version:'history-v1'},
      stages:[
        {stage:'evidence:s1',model:'jev-1.14.0',reported_model:'jev-1.14.0',protocol:'jev',account_id:7,input_tokens:10,output_tokens:0,input_bytes:1000,usage_known:true,attempted:true},
        {stage:'evidence:s2',model:'jev-1.14.0',protocol:'jev',account_id:7,input_tokens:0,output_tokens:0,input_bytes:1000,usage_known:false,attempted:true,failure_reason:'timeout'}
      ]
    }])
    const wrapper = mount(AIDecisionHistory, {props:{groupId:3}})
    wrapper.get('details').element.open = true
    await wrapper.get('details').trigger('toggle')
    await flushPromises()
    const summary = wrapper.get('[data-testid="ai-context-summary"]')
    expect(summary.text()).toContain('turn-2-span-1')
    expect(summary.text()).toContain('4/8')
    expect(summary.text()).toContain('admin.groups.aiRouting.decisionCalls: 2')
    expect(summary.text()).toContain('10 / 0')
    expect(summary.text()).toContain('admin.groups.aiRouting.usageUnknown')
    expect(wrapper.text()).toContain('admin.groups.aiRouting.partialUsage')
    expect(wrapper.get('[data-testid="ai-history-usage"]').text()).toContain('admin.groups.aiRouting.usageUnknown')
    expect(wrapper.get('[data-testid="ai-history-usage"]').text()).not.toContain('10 / 0')
    expect(summary.get('details').findAll('div')[1].text()).not.toContain('0 / 0')
    wrapper.unmount()
  })
})

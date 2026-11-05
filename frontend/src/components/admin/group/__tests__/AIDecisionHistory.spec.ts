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
    expect(wrapper.text()).toContain('admin.groups.aiRouting.failure_timeout')
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
})

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AIBackgroundHistory from '../AIBackgroundHistory.vue'
import { aiRouting, type AIBackgroundJob } from '@/api/admin/aiRouting'

vi.mock('@/api/admin/aiRouting', () => ({ aiRouting: { backgroundJobs: vi.fn() } }))
const { copyToClipboard } = vi.hoisted(() => ({ copyToClipboard: vi.fn() }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

const job: AIBackgroundJob = { id: 'local-job', request_id: 'decision-request', usage_request_id: 'responses-background:local-job', status: 'submission_unknown', public_model: 'auto', execution_model: 'chosen', account_id: 12, hold_amount: 3, actual_cost: null, usage_known: false, input_tokens: 0, output_tokens: 0, cache_sync_pending: true, created_at: '2026-11-06', updated_at: '2026-11-06' }

describe('AI background settlement diagnostics', () => {
  beforeEach(() => vi.clearAllMocks())
  it('loads only on expansion, shows unknown cost as unknown, and copies the durable billing ID', async () => {
    vi.mocked(aiRouting.backgroundJobs).mockResolvedValue([job])
    const wrapper = mount(AIBackgroundHistory, { props: { groupId: 8 } })
    expect(aiRouting.backgroundJobs).not.toHaveBeenCalled()
    wrapper.get('details').element.open = true
    await wrapper.get('details').trigger('toggle')
    await flushPromises()
    expect(aiRouting.backgroundJobs).toHaveBeenCalledWith(8)
    expect(wrapper.text()).toContain('backgroundStatus_submission_unknown')
    expect(wrapper.text()).toContain('backgroundHold · USD 3')
    expect(wrapper.text()).toContain('backgroundActual · admin.groups.aiRouting.usageUnknown')
    expect(wrapper.text()).not.toContain('USD 0')
    expect(wrapper.text()).toContain('backgroundSyncPending')
    await wrapper.get('button[aria-label]').trigger('click')
    expect(copyToClipboard).toHaveBeenCalledWith(job.usage_request_id)
    wrapper.unmount()
  })
  it('reports settled zero usage and subscription billing without retaining the old hold', async () => {
    vi.mocked(aiRouting.backgroundJobs).mockResolvedValue([{ ...job, subscription_id: 4, status: 'settled', actual_cost: 0, usage_known: true, cache_sync_pending: false }])
    const wrapper = mount(AIBackgroundHistory, { props: { groupId: 8 } })
    wrapper.get('details').element.open = true
    await wrapper.get('details').trigger('toggle')
    await flushPromises()
    expect(wrapper.text()).toContain('backgroundSubscription')
    expect(wrapper.text()).toContain('backgroundActual · USD 0')
    expect(wrapper.text()).toContain('backgroundHoldReleased')
    expect(wrapper.text()).not.toContain('USD 3')
    expect(wrapper.text()).not.toContain('backgroundSyncPending')
    wrapper.unmount()
  })
})

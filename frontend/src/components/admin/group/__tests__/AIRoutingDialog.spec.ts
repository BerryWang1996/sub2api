import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { defineComponent } from 'vue'
import AIRoutingDialog from '../AIRoutingDialog.vue'
import AIRoutingTarget from '../AIRoutingTarget.vue'
import { aiRouting, type AIPolicy, type AIPolicyState } from '@/api/admin/aiRouting'
import type { AdminGroup } from '@/types'
vi.mock('@/api/admin/aiRouting', () => ({ aiRouting: {
  load:vi.fn(),save:vi.fn(),publish:vi.fn(),disable:vi.fn(),check:vi.fn(),test:vi.fn(),models:vi.fn(),decisions:vi.fn()
}}))
vi.mock('vue-i18n', async () => ({...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),useI18n:() => ({t:(k:string) => k})}))
const group = { id: 3, name: 'AI', platform:'composite', ai_routing:true } as AdminGroup
const policy: AIPolicy = {group_id:3,enabled:false,version:0,engine:'generic',decision_platform:'openai',decision_group_id:2,decision_account_id:null,decision_model:'private-alias',context_mode:'metadata',prompt:'Pick based on task',timeout_ms:2000,fallback_id:'c1',candidates:[{id:'c1',enabled:true,platform:'anthropic',model:'my-alias',description:'tools',target_group_id:null,target_account_id:11}]}
const saved: AIPolicyState = {draft:policy,published:null,revision:1}
function mountDialog() {
  return mount(AIRoutingDialog, {props:{group},global:{stubs:{
    BaseDialog:defineComponent({template:'<section><slot/><slot name="footer"/></section>'}),
    RouterLink:true, AIRoutingTarget:true, AIDecisionHistory:true
  }}})
}
function button(w: ReturnType<typeof mountDialog>, key:string) {return w.findAll('button').find(b=>b.text()==='admin.groups.aiRouting.'+key)!}
beforeEach(()=>{vi.resetAllMocks(); vi.mocked(aiRouting.load).mockResolvedValue(saved)})
describe('AI routing editor', () => {
  it('renders a generic policy without Jev and keeps ordinary inspection local', async ()=>{
    const w=mountDialog()
    await flushPromises()
    expect(w.get('[data-testid="ai-routing-scope"]').text()).toBe('admin.groups.aiRouting.messagesScope')
    const decision = w.findAllComponents(AIRoutingTarget)[0]
    expect(decision.props('model')).toBe('private-alias')
    expect(decision.props('target')).toEqual({target_account_id:null,target_group_id:2})
    vi.mocked(aiRouting.check).mockResolvedValue({eligible:['c1'],excluded:[],external_call:false})
    await button(w,'check').trigger('click')
    await flushPromises()
    expect(aiRouting.check).toHaveBeenCalledWith(3,expect.objectContaining({engine:'generic'}),'')
    expect(aiRouting.test).not.toHaveBeenCalled()
    await w.findAll('textarea')[1].setValue('summarize a log')
    vi.mocked(aiRouting.test).mockResolvedValue({candidate_id:'c1',model:'custom',protocol:'chat',reason:'fallback',failure_reason:'timeout',elapsed_ms:4,input_tokens:8,output_tokens:1,external_call:true})
    await button(w,'test').trigger('click')
    await flushPromises()
    expect(aiRouting.test).toHaveBeenCalledTimes(1)
    expect(w.text()).toContain('custom')
    expect(w.text()).toContain('admin.groups.aiRouting.selected · c1 · my-alias')
    expect(w.text()).toContain('admin.groups.aiRouting.reason_fallback')
    expect(w.text()).toContain('admin.groups.aiRouting.failure_timeout')
    w.unmount()
  })
  it('saves the draft without enabling it, then publishes and disables using successive revisions',async()=>{
    const w = mountDialog()
    await flushPromises()
    await w.findAll('textarea')[0].setValue('Prefer high quality for complex tasks')
    const draft = {...policy, prompt:'Prefer high quality for complex tasks'}
    vi.mocked(aiRouting.save).mockResolvedValue({draft,published:null,revision:2})
    await button(w,'saveDraft').trigger('click')
    await flushPromises()
    expect(aiRouting.publish).not.toHaveBeenCalled()
    const published = {...draft, enabled:true, version:1}
    vi.mocked(aiRouting.publish).mockResolvedValue({draft,published,revision:3})
    await button(w,'publish').trigger('click')
    await flushPromises()
    expect(aiRouting.publish).toHaveBeenCalledWith(3,2)
    expect(w.emitted('changed')).toHaveLength(1)
    vi.mocked(aiRouting.disable).mockResolvedValue({draft,published:{...published,enabled:false},revision:4})
    await button(w,'disable').trigger('click')
    await flushPromises()
    expect(aiRouting.disable).toHaveBeenCalledWith(3,3)
    expect(w.emitted('changed')).toHaveLength(2)
    expect(aiRouting.test).not.toHaveBeenCalled()
    w.unmount()
  })
  it('clears decision source on engine changes, saves before publish and retains a conflicted draft',async()=>{
    const w = mountDialog()
    await flushPromises()
    await w.get('[data-testid=ai-engine]').setValue('jev')
    expect(w.findAllComponents(AIRoutingTarget)[0].props()).toMatchObject({target:{target_group_id:null,target_account_id:null},model:'jev-latest',platform:'typesafe'})
    expect(button(w,'publish').attributes('disabled')).toBeDefined()
    vi.mocked(aiRouting.save).mockRejectedValue(new Error('revision conflict'))
    await button(w,'saveDraft').trigger('click')
    await flushPromises()
    expect(aiRouting.save).toHaveBeenCalledWith(3,1,expect.objectContaining({engine:'jev',decision_model:'jev-latest',decision_group_id:null}))
    expect(w.find('[role=alert]').exists()).toBe(true)
    expect(w.get('[data-testid=ai-engine]').element.value).toBe('jev')
    expect(aiRouting.publish).not.toHaveBeenCalled()
    expect(aiRouting.test).not.toHaveBeenCalled()
    w.unmount()
  })
})

import { describe, it, expect, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import Select from '@/components/common/Select.vue'
import AIRoutingTarget from '../AIRoutingTarget.vue'
import CompositeRouteDestinationField from '../CompositeRouteDestinationField.vue'
import { aiRouting } from '@/api/admin/aiRouting'
import { supportsAIDecisionCredential } from '../aiRoutingOptions'
vi.mock('@/api/admin/aiRouting',()=>({aiRouting:{models:vi.fn()}}))
vi.mock('vue-i18n', async()=>({...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),useI18n:()=>({t:(key:string)=>key})}))
describe('AI target picker',()=>{
  it('loads only local model mappings and clears incompatible references when the platform changes', async()=>{
    vi.mocked(aiRouting.models).mockResolvedValue(['custom-public-model'])
    const wrapper=mount(AIRoutingTarget,{props:{groupId:3,platform:'openai',target:{target_group_id:null,target_account_id:17},model:'custom-public-model',engine:'generic'},global:{stubs:{CompositeRouteDestinationField:true}}})
    await flushPromises()
    expect(aiRouting.models).toHaveBeenCalledWith(3,'openai',{target_group_id:null,target_account_id:17},expect.any(AbortSignal))
    expect(wrapper.get('datalist').text()).toBe('')
    expect(wrapper.get('datalist option').attributes('value')).toBe('custom-public-model')
    expect(wrapper.getComponent(CompositeRouteDestinationField).props('explicitOnly')).toBe(true)
    wrapper.getComponent(Select).vm.$emit('update:modelValue', 'anthropic')
    await flushPromises()
    expect(wrapper.emitted('update:target')?.[0]).toEqual([{target_group_id:null,target_account_id:null}])
    expect(wrapper.emitted('update:model')?.[0]).toEqual([''])
    wrapper.unmount()
  })
  it('accepts a pinned Jev model and prevents target changes while a request is in flight', async () => {
    vi.mocked(aiRouting.models).mockResolvedValue(['jev-latest'])
    const wrapper = mount(AIRoutingTarget, {props:{groupId:3,platform:'typesafe',target:{target_group_id:null,target_account_id:17},model:'jev-1.14.0',engine:'jev',disabled:true}, global:{stubs:{CompositeRouteDestinationField:true}}})
    await flushPromises()
    expect(wrapper.get('input').element.value).toBe('jev-1.14.0')
    expect(wrapper.getComponent(Select).props('disabled')).toBe(true)
    expect(wrapper.getComponent(CompositeRouteDestinationField).props('disabled')).toBe(true)
    wrapper.getComponent(Select).vm.$emit('update:modelValue', 'openai')
    wrapper.getComponent(CompositeRouteDestinationField).vm.$emit('update:modelValue', {mode:'group',target_group_id:22,target_account_id:null})
    await flushPromises()
    expect(wrapper.emitted('update:platform')).toBeUndefined()
    expect(wrapper.emitted('update:target')).toBeUndefined()
    wrapper.unmount()
  })
  it('limits Jev to API keys while generic selection accepts supported OAuth credentials',()=>{
    expect(supportsAIDecisionCredential('jev','typesafe','apikey')).toBe(true)
    expect(supportsAIDecisionCredential('jev','typesafe','oauth')).toBe(false)
    expect(supportsAIDecisionCredential('generic','openai','oauth')).toBe(true)
    expect(supportsAIDecisionCredential('generic','gemini','oauth')).toBe(false)
    expect(supportsAIDecisionCredential('generic','typesafe','apikey')).toBe(false)
  })
})

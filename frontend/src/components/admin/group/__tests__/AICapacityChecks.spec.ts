import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AICapacityChecks from '../AICapacityChecks.vue'
import type { AICandidateCapacityCheck } from '@/api/admin/aiRouting'
import zh from '@/i18n/locales/zh/admin/overview'
import en from '@/i18n/locales/en/admin/overview'

// The test config uses runtime-only vue-i18n. Resolve the real locale entries
// here so these assertions cover the disclosure text without a compiler stub.
let currentLocale = 'zh'
function message(key: string): unknown {
  const messages = {admin:currentLocale === 'zh' ? zh : en}
  return key.split('.').reduce<unknown>((value, part) => value && typeof value === 'object' ? (value as Record<string, unknown>)[part] : undefined, messages)
}
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({t:(key:string) => message(key) ?? key, te:(key:string) => typeof message(key) === 'string'})
}))
function render(checks: AICandidateCapacityCheck[], locale = 'zh') {
  currentLocale = locale
  return mount(AICapacityChecks, {props:{checks}})
}
describe('AI routing account capacity disclosures', () => {
  it('keeps different account outcomes and metadata separate without implying precision from allowance', () => {
    const w = render([{candidate_id:'pool',model:'model-alias',accounts:[
      {account_id:11,allowed:true,capacity_verified:false,final_payload_checked:false,context_window_tokens:8000,input_bytes:1200,output_reserve_tokens:512,capacity_method:'request_bytes_plus_output',metadata_source:'local-snapshot',metadata_synced_at:'2026-11-06T01:00:00Z'},
      {account_id:12,allowed:false,capacity_verified:false,reason:'execution_context_window_unknown',context_window_tokens:0,warnings:['account_model_metadata_unknown']}
    ]}])
    const known = w.get('[data-testid="ai-capacity-account-pool-11"]')
    const unknown = w.get('[data-testid="ai-capacity-account-pool-12"]')
    expect(known.text()).toContain('此项能力检查通过')
    expect(known.text()).toContain('估算，未精确验证')
    expect(known.text()).toContain('未检查最终请求体')
    expect(known.text()).toContain('原请求字节估算 + 输出预留')
    expect(known.text()).toContain('8000')
    expect(known.get('time').attributes('datetime')).toBe('2026-11-06T01:00:00Z')
    expect(unknown.text()).toContain('此项能力检查未通过')
    expect(unknown.text()).toContain('上下文窗口未知')
    expect(unknown.text()).toContain('检查未通过原因')
    expect(w.text()).not.toContain('决策失败原因')
    expect(unknown.text()).toContain('账号已保存的元数据来源: 未知')
    expect(unknown.text()).toContain('未知 / 未记录有效时间')
    expect(unknown.text()).not.toContain('local-snapshot')
    expect(unknown.find('time').exists()).toBe(false)
    expect(w.text()).toContain('每个账号单独检查')
    expect(w.text()).not.toContain('容量计量已验证（仅此项）')
    expect(w.findAll('details').every(d=>d.attributes('open')===undefined)).toBe(true)
    w.unmount()
  })
  it('does not treat a final-payload check as exact capacity verification and safely retains unknown codes', () => {
    const code = '<img src=x onerror="alert(1)">'
    const w = render([{candidate_id:'other',model:'same-model',accounts:[{
      account_id:12,allowed:true,capacity_verified:false,final_payload_checked:true,capacity_method:'final_payload_bytes_plus_output',metadata_source:code,metadata_synced_at:'invalid timestamp',warnings:[code,'future_warning'],reason:'future_reason'
    }]}])
    expect(w.text()).toContain('已检查最终请求体；不等于容量已精确验证')
    expect(w.text()).toContain('最终请求体字节估算 + 输出预留')
    expect(w.text()).toContain('估算，未精确验证')
    expect(w.text()).toContain('未知 / 未记录有效时间')
    expect(w.text()).toContain(code)
    expect(w.text()).toContain('future_warning')
    expect(w.text()).toContain('future_reason')
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('time').exists()).toBe(false)
    w.unmount()
  })
  it('shows candidate-level failures without inventing account checks, and hides an empty result', () => {
    const w = render([{candidate_id:'empty',model:'same-model',reason:'destination_unavailable',accounts:[]}, {candidate_id:'disabled',model:'same-model',reason:'candidate_disabled',accounts:[]}])
    expect(w.text()).toContain('执行目标不可用，未获得账号检查结果')
    expect(w.text()).toContain('此候选已停用')
    expect(w.text()).toContain('没有可展示的账号检查结果')
    expect(w.findAll('[data-testid^="ai-capacity-account-"]')).toHaveLength(0)
    w.unmount()
    const empty = render([])
    expect(empty.find('[data-testid="ai-capacity-checks"]').exists()).toBe(false)
    empty.unmount()
  })
  it.each(['zh','en'])('translates existing capability warnings and reasons in %s while leaving future codes intact', locale => {
    const reasons = ['execution_model_unavailable','execution_capacity_unknown','execution_tools_unsupported','execution_tools_unknown','execution_modalities_unknown','execution_modality_unsupported','execution_images_unsupported','execution_images_unknown','execution_modality_unknown','execution_context_window_unknown','execution_input_limit_exceeded','execution_output_limit_exceeded','execution_context_window_exceeded','execution_native_responses_required','execution_stored_responses_required','execution_output_reserve_unknown']
    const warnings = ['protocol_overhead_unverified','input_tokens_estimated','account_model_metadata_unknown','context_window_administrator_declared','tools_administrator_declared','images_administrator_declared','attachment_token_usage_unverified','output_limit_unknown','output_reserve_from_metadata_maximum']
    const w = render([{candidate_id:'coverage',model:'model',accounts:reasons.map((reason,index)=>({account_id:index+1,allowed:false,capacity_verified:false,reason,warnings:index===0?warnings:[]}))}], locale)
    for (const code of [...reasons,...warnings]) expect(w.text()).not.toContain(code)
    expect(w.findAll('li')).toHaveLength(warnings.length)
    w.unmount()
  })
})

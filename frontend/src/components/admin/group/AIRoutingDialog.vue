<template>
  <BaseDialog :show="true" width="wide" :title="t('admin.groups.aiRouting.titleWithGroup', { name: group.name })" @close="close">
    <p class="mb-3 text-sm text-gray-500 dark:text-gray-400" data-testid="ai-routing-scope">{{ tr('clientEndpointScope') }}</p>
    <div v-if="error" ref="errorPanel" role="alert" class="mb-3 rounded-lg border border-red-300 p-3 text-sm text-red-700 dark:text-red-300">
      {{ error }} <button type="button" class="underline" :disabled="busy" @click="reload">{{ tr('reload') }}</button>
    </div>
    <p v-if="!draft && busy" class="text-sm">{{ t('common.loading') }}</p>
    <fieldset v-if="draft && state" :disabled="busy" class="min-w-0 space-y-5">
      <div class="flex items-center justify-between gap-3">
        <p class="text-sm text-gray-600 dark:text-gray-400" role="status">
          {{ tr('draft') }} #{{ state.revision }} · {{ state.published?.enabled ? tr('published') + ' #' + state.published.version : tr('disabled') }}
          <span v-if="dirty"> · {{ tr('unsaved') }}</span>
        </p>
        <button type="button" class="btn btn-secondary btn-sm" :title="tr('reload')" :aria-label="tr('reload')" :disabled="busy" @click="reload"><Icon name="refresh" size="sm" :class="busy && 'animate-spin'" /></button>
      </div>
      <details v-if="state.published" class="rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600">
        <summary class="cursor-pointer">{{ tr('publishedPolicy') }}</summary>
        <div class="mt-2 space-y-1 text-gray-600 dark:text-gray-400">
          <p>{{ state.published.decision_platform }} / {{ state.published.decision_model }}</p>
          <p>{{ tr('context') }} · {{ contextLabel(state.published.context_mode) }}</p>
          <p>{{ tr('clientEndpoints') }} · {{ aiPolicyClientEndpoints(state.published).map(endpointLabel).join(', ') || tr('noClientEndpoints') }}</p>
          <p class="whitespace-pre-wrap">{{ state.published.prompt }}</p>
          <ul><li v-for="c in state.published.candidates" :key="c.id">{{ c.id }} · {{ c.platform }}/{{ c.model }} · {{ targetLabel(c) }}</li></ul>
        </div>
      </details>

      <section class="space-y-3" data-testid="ai-decision-module">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">1. {{ tr('decider') }}</h3>
        <div class="grid gap-4 sm:grid-cols-2">
          <div class="space-y-3">
            <label class="block"><span class="input-label">{{ tr('engine') }}</span>
              <Select :model-value="draft.engine" :options="engineOptions" :disabled="busy" :aria-label="tr('engine')" data-testid="ai-engine" @update:model-value="changeEngine(String($event))" />
            </label>
            <p class="input-hint">{{ tr('decisionHint') }}</p>
            <RouterLink to="/admin/accounts" target="_blank" rel="noopener" class="text-sm text-primary-600 hover:text-primary-700 dark:text-primary-400">{{ tr('openAccounts') }}</RouterLink>
          </div>
          <AIRoutingTarget :key="draft.engine" :group-id="group.id" :engine="draft.engine" :disabled="busy"
            v-model:platform="draft.decision_platform" v-model:model="draft.decision_model"
            :target="{ target_group_id: draft.decision_group_id, target_account_id: draft.decision_account_id }" @update:target="setDeciderTarget" />
        </div>
      </section>

      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-600" data-testid="ai-rules-module">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">2. {{ tr('policy') }}</h3>
        <div>
          <p class="input-label">{{ tr('clientEndpoints') }}</p>
          <div class="flex flex-wrap gap-4">
            <label v-for="endpoint in clientEndpointOptions" :key="endpoint.value" class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
              <input type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-700" :checked="enabledEndpoints.includes(endpoint.value)" :disabled="busy" :data-testid="'ai-endpoint-' + endpoint.value" @change="setEndpoint(endpoint.value, ($event.target as HTMLInputElement).checked)" />{{ endpoint.label }}
            </label>
          </div>
          <p class="input-hint">{{ tr('clientEndpointsHint') }}</p>
          <p v-if="!enabledEndpoints.length" class="mt-1 text-xs text-amber-700 dark:text-amber-400">{{ tr('noClientEndpoints') }}</p>
        </div>
        <label class="block"><span class="input-label">{{ tr('prompt') }}</span>
          <textarea v-model="draft.prompt" class="input" rows="4" maxlength="16384" data-testid="ai-prompt" />
        </label>
        <p class="input-hint">{{ tr('rulesHint') }}</p>
        <div class="grid gap-3 sm:grid-cols-2">
          <label><span class="input-label">{{ tr('context') }}</span>
            <Select v-model="draft.context_mode" :options="contextOptions" :disabled="busy" :aria-label="tr('context')" data-testid="ai-context-mode" />
          </label>
          <label><span class="input-label">{{ tr('timeout') }}</span><input v-model.number="draft.timeout_ms" class="input" type="number" min="100" max="60000" /></label>
        </div>
        <p class="input-hint" data-testid="ai-context-scope">{{ contextHint }}</p>
        <label class="block"><span class="input-label">{{ tr('fallback') }}</span>
          <Select v-model="draft.fallback_id" :options="fallbackOptions" :disabled="busy" :aria-label="tr('fallback')" />
        </label>
        <details v-if="draft.context_mode === 'bounded_history'">
          <summary class="cursor-pointer text-sm text-gray-700 dark:text-gray-300">{{ tr('contextBudget') }}</summary>
          <p class="mt-2 input-hint">{{ tr('contextBudgetHint') }}</p>
          <div class="mt-3 grid gap-3 sm:grid-cols-2">
            <label v-for="field in budgetFields" :key="field.key"><span class="input-label">{{ tr(field.label) }}</span>
              <input :value="draft.context_budget?.[field.key] || ''" type="number" min="0" step="1" class="input" :placeholder="String(field.default)" :data-testid="'ai-budget-' + field.key" @input="setBudget(field.key, $event)" />
            </label>
          </div>
        </details>
      </section>

      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-600" data-testid="ai-pool-module">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">3. {{ tr('pool') }}</h3>
        <AIRoutingCandidatePool :key="editorEpoch" :group-id="group.id" :model-value="draft.candidates" :disabled="busy" @update:model-value="setCandidates" @pending-change="candidatePending = $event" />
      </section>

      <section class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">{{ tr('sample') }}</h3>
        <label class="block"><span class="input-label">{{ tr('sampleEndpoint') }}</span><Select v-model="sampleEndpoint" :options="sampleEndpointOptions" :disabled="busy || !enabledEndpoints.length" :aria-label="tr('sampleEndpoint')" data-testid="ai-sample-endpoint" /></label>
        <p class="input-hint">{{ tr('sampleEndpointHint') }}</p>
        <textarea v-model="sample" class="input" rows="3" :placeholder="sampleHint" data-testid="ai-sample" />
        <p class="input-hint">{{ trialHint }}</p>
        <p class="input-hint">{{ tr('sampleLimit') }}</p>
        <p v-if="candidatePending" class="text-xs text-amber-700 dark:text-amber-400">{{ tr('pendingCandidate') }}</p>
        <div class="flex flex-wrap gap-2">
          <button type="button" class="btn btn-secondary" :disabled="candidatePending || !sampleEndpoint" @click="runCheck(false)">{{ tr('check') }}</button>
          <button type="button" class="btn btn-secondary" :disabled="candidatePending || !sampleEndpoint || !sample.trim()" @click="runCheck(true)"><Icon name="play" size="sm" />{{ tr('test') }}</button>
        </div>
        <div v-if="preview" class="space-y-1 rounded-lg border border-gray-200 bg-gray-50 p-3 text-sm dark:border-dark-600 dark:bg-dark-800" role="status">
          <p data-testid="ai-preview-endpoint">{{ tr('sampleEndpoint') }} · {{ endpointLabel(preview.sample_endpoint || 'messages') }}</p>
          <p>{{ tr('eligible') }} · {{ preview.eligible?.join(', ') || '—' }}</p>
          <p v-for="e in preview.excluded" :key="e.id">{{ e.id }} · {{ e.reason }}</p>
          <p v-if="preview.needs_decision" class="text-amber-700 dark:text-amber-400">{{ tr('needDecision') }}</p>
          <AIContextSummary v-if="preview.context" :context="preview.context" />
          <AICapacityChecks v-if="preview.capacity_checks?.length" :checks="preview.capacity_checks" />
          <details v-if="preview.sample_spans?.length" class="pt-2" data-testid="ai-sample-projection">
            <summary class="cursor-pointer">{{ tr('sampleProjection') }}</summary>
            <p class="input-hint mt-2">{{ tr('sampleProjectionHint') }}</p>
            <div class="mt-2 max-h-64 space-y-2 overflow-y-auto">
              <div v-for="span in preview.sample_spans" :key="span.id">
                <p class="text-xs text-gray-500 dark:text-gray-400">{{ span.id }} · {{ span.role }}</p>
                <p class="whitespace-pre-wrap break-words">{{ span.text }}</p>
              </div>
            </div>
          </details>
        </div>
        <div v-if="trial" class="space-y-1 rounded-lg border border-gray-200 bg-gray-50 p-3 text-sm dark:border-dark-600 dark:bg-dark-800" role="status">
          <p data-testid="ai-trial-endpoint">{{ tr('sampleEndpoint') }} · {{ endpointLabel(trial.sample_endpoint || 'messages') }}</p>
          <p>{{ tr('selected') }} · {{ trial.candidate_id || '—' }} · {{ draft.candidates.find(c => c.id === trial?.candidate_id)?.model || '—' }}</p>
          <p v-if="trial.external_call">{{ tr('decider') }} · {{ trial.model || '—' }} · {{ trial.protocol }} · {{ trial.elapsed_ms }} ms</p>
          <p data-testid="ai-trial-usage">{{ tr('externalCall') }} · {{ trial.external_call ? tr('yes') : tr('no') }} · {{ tr('usage') }} {{ trial.stages?.some(stage => stage.attempted !== false && !stage.usage_known) ? tr('usageUnknown') : `${trial.input_tokens} / ${trial.output_tokens}` }}</p>
          <p v-if="trial.stages?.some(stage => stage.attempted !== false && !stage.usage_known)" class="text-amber-700 dark:text-amber-400">{{ tr('partialUsage') }}</p>
          <p v-if="trial.reason">{{ tr('reason') }} · {{ reasonLabel(trial.reason) }}</p>
          <p v-if="trial.failure_reason">{{ tr('failureReason') }} · {{ failureLabel(trial.failure_reason) }}</p>
          <AIContextSummary v-if="trial.context || trial.stages?.length || trial.decision_calls != null" :context="trial.context" :decision-calls="trial.decision_calls" :stages="trial.stages" />
        </div>
      </section>
    </fieldset>
    <AIDecisionHistory :group-id="group.id" class="mt-5" />
    <template #footer>
      <div class="flex flex-wrap items-center justify-end gap-2">
        <span v-if="busy" role="status" class="text-sm">{{ t('common.loading') }}</span>
        <span v-else-if="candidatePending" class="mr-auto text-xs text-amber-700 dark:text-amber-400">{{ tr('pendingCandidate') }}</span>
        <button class="btn btn-secondary" type="button" :disabled="busy" @click="close">{{ t('common.close') }}</button>
        <button class="btn btn-secondary" type="button" :disabled="busy || !draft || !policyDirty || candidatePending || !enabledEndpoints.length" @click="write('save')">{{ tr('saveDraft') }}</button>
        <button class="btn btn-secondary" type="button" :disabled="busy || dirty || !state?.published?.enabled" @click="write('disable')">{{ tr('disable') }}</button>
        <button class="btn btn-primary" type="button" :disabled="busy || dirty || !state?.revision" @click="write('publish')">{{ tr('publish') }}</button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch, onMounted, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import AIRoutingTarget from './AIRoutingTarget.vue'
import AIRoutingCandidatePool from './AIRoutingCandidatePool.vue'
import AIDecisionHistory from './AIDecisionHistory.vue'
import AIContextSummary from './AIContextSummary.vue'
import AICapacityChecks from './AICapacityChecks.vue'
import { aiRouting, aiPolicyClientEndpoints, type AIClientEndpoint, type AIPolicy, type AIPolicyState, type AIPreview, type AITestDecision, type AITarget, type AICandidate, type AIContextBudget } from '@/api/admin/aiRouting'
import type { AdminGroup } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ group: AdminGroup }>()
const emit = defineEmits<{ close: []; changed: [] }>()
const { t } = useI18n()
const tr = (key: string) => t('admin.groups.aiRouting.' + key)
const reasonLabel = (reason: string) => ['model', 'fallback', 'single', 'rejected', 'continuation', 'cancelled', 'empty', 'failed'].includes(reason) ? tr('reason_' + reason) : reason
const failureLabel = (reason: string) => {
  const key = 'admin.groups.aiRouting.failure_' + reason
  const translated = t(key)
  return translated === key ? reason : translated
}
const state = ref<AIPolicyState | null>(null)
const draft = ref<AIPolicy | null>(null)
const clean = ref('')
const busy = ref(false)
const error = ref('')
const errorPanel = ref<HTMLElement | null>(null)
const sample = ref('')
const sampleEndpoint = ref<AIClientEndpoint | ''>('messages')
const preview = ref<AIPreview | null>(null)
const trial = ref<AITestDecision | null>(null)
const editorEpoch = ref(0)
const candidatePending = ref(false)
const policyDirty = computed(() => !!draft.value && JSON.stringify(draft.value) !== clean.value)
const dirty = computed(() => policyDirty.value || candidatePending.value)
const targetLabel = (target: AITarget) => target.target_group_id ? tr('group') + ' #' + target.target_group_id : tr('account') + ' #' + target.target_account_id
const engineOptions = computed(() => [{ value: 'generic', label: tr('generic') }, { value: 'jev', label: 'TypeSafe / Jev' }])
const clientEndpointOptions = computed<{value:AIClientEndpoint;label:string}[]>(() => [{value:'messages',label:tr('endpointMessages')},{value:'responses',label:tr('endpointResponses')},{value:'chat_completions',label:tr('endpointChat')},{value:'responses_ws',label:tr('endpointWS')},{value:'gemini',label:tr('endpointGemini')}])
const endpointLabel = (endpoint: string) => clientEndpointOptions.value.find(option=>option.value===endpoint)?.label || endpoint
const enabledEndpoints = computed(() => aiPolicyClientEndpoints(draft.value))
const sampleEndpointOptions = computed(() => clientEndpointOptions.value.filter(option=>enabledEndpoints.value.includes(option.value)))
const sampleHint = computed(() => sampleEndpoint.value === 'responses' ? tr('responsesSampleHint') : sampleEndpoint.value === 'chat_completions' ? tr('chatSampleHint') : sampleEndpoint.value === 'responses_ws' ? tr('wsSampleHint') : sampleEndpoint.value === 'gemini' ? tr('geminiSampleHint') : draft.value?.context_mode === 'bounded_history' ? tr('historySampleHint') : tr('sampleHint'))
function setEndpoint(endpoint: AIClientEndpoint, checked: boolean) {
  if (!draft.value || busy.value || ![0,1,2].includes(draft.value.schema_version ?? 0)) return
  const next = checked ? [...enabledEndpoints.value, endpoint] : enabledEndpoints.value.filter(value=>value!==endpoint)
  draft.value.schema_version = 2
  draft.value.enabled_endpoints = clientEndpointOptions.value.map(option=>option.value).filter(value=>next.includes(value))
}
const contextOptions = computed(() => [
  { value: 'metadata', label: tr('metadata') },
  { value: 'task_excerpt', label: tr('excerpt') },
  { value: 'bounded_history', label: tr('boundedHistory') }
])
const contextLabel = (mode: string) => contextOptions.value.find(option => option.value === mode)?.label || mode
const contextHint = computed(() => tr(draft.value?.context_mode === 'metadata' ? 'metadataHint' : draft.value?.context_mode === 'bounded_history' ? 'boundedHistoryHint' : 'excerptHint'))
const trialHint = computed(() => tr(draft.value?.context_mode === 'metadata' ? 'testMetadata' : draft.value?.context_mode === 'bounded_history' ? 'testHistory' : 'testExcerpt'))
const fallbackOptions = computed(() => [{ value: '', label: tr('fail') }, ...(draft.value?.candidates.filter(c => c.enabled).map(c => ({ value: c.id, label: c.id + ' · ' + c.model })) || [])])
const budgetFields: { key: keyof AIContextBudget; label: string; default: number }[] = [
  { key: 'max_history_bytes', label: 'maxHistoryBytes', default: 131072 },
  { key: 'max_history_turns', label: 'maxHistoryTurns', default: 256 },
  { key: 'max_segments', label: 'maxSegments', default: 32 },
  { key: 'max_decision_calls', label: 'maxDecisionCalls', default: 33 },
  { key: 'max_input_bytes', label: 'maxDecisionInputBytes', default: 24576 },
  { key: 'max_total_input_bytes', label: 'maxTotalDecisionInputBytes', default: 524288 },
  { key: 'max_output_bytes', label: 'maxDecisionOutputBytes', default: 65536 },
  { key: 'max_total_output_bytes', label: 'maxTotalDecisionOutputBytes', default: 2097152 }
]
function setBudget(key: keyof AIContextBudget, event: Event) {
  if (!draft.value || busy.value) return
  draft.value.context_budget = { ...draft.value.context_budget, [key]: (event.target as HTMLInputElement).valueAsNumber || 0 }
}
function apply(value: AIPolicyState) {
  state.value = value
  draft.value = { ...value.draft, decision_platform: value.draft.decision_platform || (value.draft.engine === 'jev' ? 'typesafe' : 'openai'), ...(value.draft.enabled_endpoints ? {enabled_endpoints:[...value.draft.enabled_endpoints]} : {}), ...(value.draft.context_budget ? { context_budget: { ...value.draft.context_budget } } : {}), candidates: (value.draft.candidates || []).map(x => ({ ...x })) }
  clean.value = JSON.stringify(draft.value)
  candidatePending.value = false
  editorEpoch.value++
}
async function action(fn: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try { await fn() }
  catch (e) {
    error.value = extractApiErrorMessage(e, tr('error'))
    await nextTick()
    errorPanel.value?.scrollIntoView?.({ block: 'nearest' })
  } finally { busy.value = false }
}
function reload() {
  if (busy.value || (dirty.value && !window.confirm(tr('discard')))) return
  return action(async () => apply(await aiRouting.load(props.group.id)))
}
function close() { if (!busy.value && (!dirty.value || window.confirm(tr('discard')))) emit('close') }
function setDeciderTarget(target: AITarget) {
  if (!draft.value || busy.value) return
  draft.value.decision_account_id = target.target_account_id
  draft.value.decision_group_id = target.target_group_id
}
function changeEngine(engine: string) {
  if (!draft.value || busy.value || draft.value.engine === engine || (engine !== 'generic' && engine !== 'jev')) return
  draft.value.engine = engine
  draft.value.decision_platform = engine === 'jev' ? 'typesafe' : 'openai'
  draft.value.decision_model = engine === 'jev' ? 'jev-latest' : ''
  setDeciderTarget({ target_account_id: null, target_group_id: null })
}
function setCandidates(candidates: AICandidate[]) {
  if (!draft.value || busy.value) return
  draft.value.candidates = candidates
  if (!candidates.some(c => c.id === draft.value?.fallback_id)) draft.value.fallback_id = ''
}
function runCheck(live: boolean) {
  const policy = draft.value
  const endpoint = sampleEndpoint.value
  if (!policy || !endpoint || candidatePending.value) return
  preview.value = null
  trial.value = null
  return action(async () => {
    if (live) trial.value = await aiRouting.test(props.group.id, policy, sample.value, endpoint)
    else preview.value = await aiRouting.check(props.group.id, policy, sample.value, endpoint)
  })
}
function write(op: 'save' | 'publish' | 'disable') {
  const policy = draft.value
  const revision = state.value?.revision
  if (!policy || revision == null || candidatePending.value || (op !== 'save' && dirty.value)) return
  return action(async () => {
    apply(op === 'save' ? await aiRouting.save(props.group.id, revision, policy) : await aiRouting[op](props.group.id, revision))
    if (op !== 'save') emit('changed')
  })
}
watch(enabledEndpoints, endpoints => {
  if (!sampleEndpoint.value || !endpoints.includes(sampleEndpoint.value)) sampleEndpoint.value = endpoints[0] || ''
})
watch([draft, sample, sampleEndpoint, candidatePending], () => { preview.value = null; trial.value = null }, { deep: true })
onMounted(reload)
</script>

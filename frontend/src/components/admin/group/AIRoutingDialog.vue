<template>
  <BaseDialog :show="true" width="wide" :title="group.name + ' · ' + tr('title')" @close="close">
    <p class="mb-3 rounded border border-blue-200 bg-blue-50 p-3 text-sm text-blue-700 dark:border-blue-800 dark:bg-blue-900/20 dark:text-blue-300" data-testid="ai-routing-scope">{{ tr('messagesScope') }}</p>
    <div v-if="error" ref="errorPanel" role="alert" class="mb-3 rounded border border-red-300 p-3 text-sm text-red-700 dark:text-red-300">
      {{ error }} <button type="button" class="underline" :disabled="busy" @click="reload">{{ tr('reload') }}</button>
    </div>
    <p v-if="!draft && busy" class="text-sm">{{ t('common.loading') }}</p>
    <fieldset v-if="draft && state" :disabled="busy" class="space-y-5">
      <p class="text-sm" role="status">
        {{ tr('draft') }} #{{ state.revision }} ·
        {{ state.published?.enabled ? tr('published') + ' #' + state.published.version : tr('disabled') }}
        <span v-if="dirty"> · {{ tr('unsaved') }}</span>
      </p>
      <details v-if="state.published" class="rounded border p-2 text-sm dark:border-dark-600">
        <summary>{{ tr('publishedPolicy') }}</summary>
        <p>{{ state.published.decision_platform }} / {{ state.published.decision_model }}</p>
        <p class="whitespace-pre-wrap">{{ state.published.prompt }}</p>
        <ul><li v-for="c in state.published.candidates" :key="c.id">{{ c.id }} · {{ c.platform }}/{{ c.model }} · {{ targetLabel(c) }}</li></ul>
      </details>
      <section class="space-y-3">
        <h4 class="font-semibold">{{ tr('decider') }}</h4>
        <label class="block"><span class="input-label">{{ tr('engine') }}</span>
          <select :value="draft.engine" class="input" data-testid="ai-engine" @change="changeEngine(($event.target as HTMLSelectElement).value)">
            <option value="generic">{{ tr('generic') }}</option><option value="jev">TypeSafe / Jev</option>
          </select>
        </label>
        <AIRoutingTarget :key="draft.engine" :group-id="group.id" :engine="draft.engine"
          v-model:platform="draft.decision_platform" v-model:model="draft.decision_model"
          :target="{target_group_id:draft.decision_group_id,target_account_id:draft.decision_account_id}"
          @update:target="setDeciderTarget" />
        <RouterLink to="/admin/accounts" target="_blank" rel="noopener" class="text-sm text-primary-600 underline">{{ tr('openAccounts') }}</RouterLink>
      </section>
      <section class="space-y-2">
        <h4 class="font-semibold">{{ tr('pool') }}</h4>
        <article v-for="candidate in draft.candidates" :key="candidate.id" class="space-y-3 rounded border p-3 dark:border-dark-600">
          <div class="flex items-center justify-between">
            <span class="font-semibold">{{ candidate.id }}</span>
            <label><input v-model="candidate.enabled" type="checkbox" /> {{ tr('include') }}</label>
            <button type="button" class="btn btn-secondary" @click="remove(candidate.id)">{{ tr('remove') }}</button>
          </div>
          <AIRoutingTarget :group-id="group.id" v-model:platform="candidate.platform" v-model:model="candidate.model"
            :target="candidate" @update:target="Object.assign(candidate, $event)" />
          <label class="block"><span class="input-label">{{ tr('capability') }}</span>
            <input v-model="candidate.description" class="input" maxlength="2048" />
          </label>
          <label class="block"><span class="input-label">{{ tr('cost') }}</span>
            <input v-model="candidate.cost_tier" class="input" maxlength="64" />
          </label>
          <details><summary class="text-sm">{{ tr('limits') }}</summary>
            <p class="input-hint">{{ tr('inputEstimateHint') }}</p>
            <div class="mt-2 grid gap-2 sm:grid-cols-2">
              <label>{{ tr('maxInput') }}<input :value="candidate.max_input_tokens || ''" class="input" type="number" min="0" step="1" @input="candidate.max_input_tokens = ($event.target as HTMLInputElement).valueAsNumber || 0" /></label>
              <label>{{ tr('maxOutput') }}<input :value="candidate.max_output_tokens || ''" class="input" type="number" min="0" step="1" @input="candidate.max_output_tokens = ($event.target as HTMLInputElement).valueAsNumber || 0" /></label>
              <label><input v-model="candidate.disable_tools" type="checkbox" /> {{ tr('excludeTools') }}</label>
              <label><input v-model="candidate.disable_images" type="checkbox" /> {{ tr('excludeImages') }}</label>
            </div>
          </details>
        </article>
        <button type="button" class="btn btn-secondary" :disabled="draft.candidates.length >= 64" @click="addCandidate">{{ tr('add') }}</button>
      </section>
      <section class="space-y-3">
        <h4 class="font-semibold">{{ tr('policy') }}</h4>
        <label class="block"><span class="input-label">{{ tr('prompt') }}</span>
          <textarea v-model="draft.prompt" class="input" rows="3" maxlength="16384" />
        </label>
        <div class="grid gap-2 sm:grid-cols-2">
          <label><span class="input-label">{{ tr('context') }}</span>
            <select v-model="draft.context_mode" class="input"><option value="metadata">{{ tr('metadata') }}</option><option value="task_excerpt">{{ tr('excerpt') }}</option></select>
          </label>
          <label><span class="input-label">{{ tr('timeout') }}</span>
            <input v-model.number="draft.timeout_ms" class="input" type="number" min="100" max="60000" />
          </label>
        </div>
        <label class="block"><span class="input-label">{{ tr('fallback') }}</span>
          <select v-model="draft.fallback_id" class="input"><option value="">{{ tr('fail') }}</option>
            <option v-for="c in draft.candidates.filter(item => item.enabled)" :key="c.id" :value="c.id">{{ c.id }} · {{ c.model }}</option>
          </select>
        </label>
      </section>
      <section class="space-y-2">
        <h4 class="font-semibold">{{ tr('sample') }}</h4>
        <textarea v-model="sample" class="input" rows="2" maxlength="4000" :placeholder="tr('sampleHint')" />
        <p class="input-hint">{{ draft.context_mode === 'metadata' ? tr('testMetadata') : tr('testExcerpt') }}</p>
        <div class="flex flex-wrap gap-2">
          <button type="button" class="btn btn-secondary" @click="runCheck(false)">{{ tr('check') }}</button>
          <button type="button" class="btn btn-secondary" :disabled="!sample.trim()" @click="runCheck(true)">{{ tr('test') }}</button>
        </div>
        <div v-if="preview" class="rounded border p-2 text-sm dark:border-dark-600" role="status">
          <p>{{ tr('eligible') }} · {{ preview.eligible?.join(', ') || '—' }}</p>
          <p v-for="e in preview.excluded" :key="e.id">{{ e.id }} · {{ e.reason }}</p>
        </div>
        <div v-if="trial" class="rounded border p-2 text-sm dark:border-dark-600" role="status">
          <p>{{ tr('selected') }} · {{ trial.candidate_id || '—' }} · {{ draft?.candidates.find(c => c.id === trial?.candidate_id)?.model || '—' }}</p>
          <p v-if="trial.external_call">{{ tr('decider') }} · {{ trial.model || '—' }} · {{ trial.protocol }} · {{ trial.elapsed_ms }} ms</p>
          <p>{{ tr('externalCall') }} · {{ trial.external_call ? tr('yes') : tr('no') }} · {{ tr('usage') }} {{ trial.input_tokens }} / {{ trial.output_tokens }}</p>
          <p v-if="trial.reason">{{ tr('reason') }} · {{ reasonLabel(trial.reason) }}</p>
          <p v-if="trial.failure_reason">{{ tr('failureReason') }} · {{ tr('failure_' + trial.failure_reason) }}</p>
        </div>
      </section>
    </fieldset>
    <AIDecisionHistory :group-id="group.id" class="mt-4" />
    <template #footer>
      <div class="flex flex-wrap justify-end gap-2">
        <span v-if="busy" role="status" class="self-center text-sm">{{ t('common.loading') }}</span>
        <button class="btn btn-secondary" type="button" :disabled="busy" @click="close">{{ t('common.close') }}</button>
        <button class="btn btn-secondary" type="button" :disabled="busy || !draft || !dirty" @click="write('save')">{{ tr('saveDraft') }}</button>
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
import AIRoutingTarget from './AIRoutingTarget.vue'
import AIDecisionHistory from './AIDecisionHistory.vue'
import { aiRouting, type AIPolicy, type AIPolicyState, type AIPreview, type AITestDecision, type AITarget } from '@/api/admin/aiRouting'
import type { AdminGroup } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'
const props = defineProps<{group: AdminGroup}>()
const emit = defineEmits<{close: []; changed: []}>()
const { t } = useI18n()
const tr = (key: string) => t('admin.groups.aiRouting.' + key)
const reasonLabel = (reason: string) => ['model', 'fallback', 'single'].includes(reason) ? tr('reason_' + reason) : reason
const state = ref<AIPolicyState | null>(null)
const draft = ref<AIPolicy | null>(null)
const clean = ref('')
const busy = ref(false)
const error = ref('')
const errorPanel = ref<HTMLElement | null>(null)
const sample = ref('')
const preview = ref<AIPreview | null>(null)
const trial = ref<AITestDecision | null>(null)
const dirty = computed(() => !!draft.value && JSON.stringify(draft.value) !== clean.value)
const targetLabel = (target: AITarget) => target.target_group_id ? tr('group') + ' #' + target.target_group_id : tr('account') + ' #' + target.target_account_id
function apply(value: AIPolicyState) {
  state.value = value
  draft.value = {...value.draft, decision_platform: value.draft.decision_platform || (value.draft.engine === 'jev' ? 'typesafe' : 'openai'), candidates: (value.draft.candidates || []).map(x => ({...x}))}
  clean.value = JSON.stringify(draft.value)
}
async function action(fn: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    await fn()
  } catch (e) {
    error.value = extractApiErrorMessage(e, tr('error'))
    await nextTick()
    errorPanel.value?.scrollIntoView?.({block: 'nearest'})
  } finally {
    busy.value = false
  }
}
function reload() {
  if (dirty.value && !window.confirm(tr('discard'))) return
  return action(async () => apply(await aiRouting.load(props.group.id)))
}
function close() { if (!busy.value && (!dirty.value || window.confirm(tr('discard')))) emit('close') }
function setDeciderTarget(target: AITarget) {
  if (!draft.value) return
  draft.value.decision_account_id = target.target_account_id
  draft.value.decision_group_id = target.target_group_id
}
function changeEngine(engine: string) {
  if (!draft.value || (engine !== 'generic' && engine !== 'jev')) return
  draft.value.engine = engine
  draft.value.decision_platform = engine === 'jev' ? 'typesafe' : 'openai'
  draft.value.decision_model = engine === 'jev' ? 'jev-latest' : ''
  setDeciderTarget({target_account_id:null,target_group_id:null})
}
function addCandidate() {
  if (!draft.value) return
  let i = 1
  while (draft.value.candidates.some(c => c.id === 'c' + i)) i++
  draft.value.candidates.push({id: 'c' + i, platform: 'openai', model:'', description:'', enabled:true, target_account_id:null,target_group_id:null})
}
function remove(id: string) {
  if (!draft.value) return
  draft.value.candidates = draft.value.candidates.filter(c => c.id !== id)
  if (draft.value.fallback_id === id) draft.value.fallback_id = ''
}
function runCheck(live: boolean) {
  const policy = draft.value
  if (!policy) return
  preview.value = null
  trial.value = null
  return action(async () => {
    if (live) trial.value = await aiRouting.test(props.group.id, policy, sample.value)
    else preview.value = await aiRouting.check(props.group.id, policy, sample.value)
  })
}
function write(op: 'save' | 'publish' | 'disable') {
  const policy = draft.value
  const revision = state.value?.revision
  if (!policy || revision == null) return
  return action(async () => {
    apply(op === 'save' ? await aiRouting.save(props.group.id, revision, policy) : await aiRouting[op](props.group.id, revision))
    if (op !== 'save') emit('changed')
  })
}
watch([draft, sample], () => {preview.value = null; trial.value = null}, {deep:true})
onMounted(reload)
</script>

<template>
  <div class="grid gap-5 lg:grid-cols-[minmax(0,1.15fr)_minmax(320px,0.85fr)]" data-testid="ai-candidate-pool">
    <section class="min-w-0">
      <div class="mb-3 flex items-center justify-between gap-3">
        <h4 class="text-sm font-semibold text-gray-900 dark:text-white">{{ tr('candidates') }} · {{ modelValue.length }}/64</h4>
        <button v-if="editingId" type="button" class="btn btn-secondary btn-sm" :disabled="disabled || modelValue.length >= 64" @click="startNew">
          <Icon name="plus" size="sm" />{{ tr('add') }}
        </button>
      </div>
      <div class="overflow-hidden rounded-lg border border-gray-200 dark:border-dark-600">
        <div v-if="!modelValue.length" class="flex h-36 items-center justify-center px-3 text-sm text-gray-500 dark:text-gray-400">{{ tr('emptyPool') }}</div>
        <div v-else class="overflow-x-auto">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-600">
            <thead class="bg-gray-50 text-left text-xs font-medium uppercase tracking-wide text-gray-500 dark:bg-dark-800 dark:text-gray-400">
              <tr>
                <th class="px-3 py-2">{{ tr('candidate') }}</th>
                <th class="px-3 py-2">{{ t('admin.groups.compositeRoutes.target') }}</th>
                <th class="px-3 py-2 text-right">{{ t('admin.groups.columns.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-700 dark:bg-dark-900">
              <tr v-for="candidate in modelValue" :key="candidate.id" :class="[!candidate.enabled && 'opacity-60', editingId === candidate.id && 'bg-primary-50/50 dark:bg-primary-900/10']" :data-testid="'candidate-row-' + candidate.id">
                <td class="max-w-[15rem] px-3 py-2 align-top">
                  <div class="break-all font-medium text-gray-900 dark:text-white">{{ candidate.id }}</div>
                  <p class="mt-1 whitespace-pre-wrap break-words text-xs text-gray-500 dark:text-gray-400">{{ candidate.description || '—' }}</p>
                  <p v-if="candidate.cost_tier" class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ tr('cost') }}: {{ candidate.cost_tier }}</p>
                  <span v-if="!candidate.enabled" class="badge badge-danger mt-1">{{ t('admin.accounts.status.inactive') }}</span>
                </td>
                <td class="px-3 py-2 align-top">
                  <div class="flex items-center gap-1.5 text-gray-900 dark:text-white">
                    <PlatformIcon :platform="candidate.platform" size="xs" />{{ platformLabel(candidate.platform) }}
                  </div>
                  <div class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ targetLabel(candidate) }}</div>
                  <div class="mt-1 break-all font-mono text-xs text-gray-500 dark:text-gray-400">{{ candidate.model }}</div>
                </td>
                <td class="px-3 py-2 align-top">
                  <div class="flex justify-end gap-1">
                    <button type="button" class="rounded p-1.5 text-gray-500 hover:bg-gray-100 hover:text-primary-600 dark:hover:bg-dark-700 dark:hover:text-primary-400" :disabled="disabled" :title="t('common.edit')" :aria-label="t('common.edit') + ' ' + candidate.id" :data-testid="'candidate-edit-' + candidate.id" @click="edit(candidate)">
                      <Icon name="edit" size="sm" />
                    </button>
                    <button type="button" class="rounded p-1.5 text-gray-500 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400" :disabled="disabled" :title="t('common.delete')" :aria-label="t('common.delete') + ' ' + candidate.id" :data-testid="'candidate-delete-' + candidate.id" @click="remove(candidate)">
                      <Icon name="trash" size="sm" />
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
      <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ tr('poolDraftHint') }}</p>
    </section>

    <form class="min-w-0 space-y-3" data-testid="ai-candidate-form" @submit.prevent="applyCandidate">
      <div class="flex items-center justify-between gap-3">
        <h4 class="text-sm font-semibold text-gray-900 dark:text-white">{{ editingId ? tr('editCandidate') : tr('add') }}</h4>
        <button v-if="editingId || pending" type="button" class="text-xs font-medium text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200" :disabled="disabled" @click="startNew">{{ t('common.cancel') }}</button>
      </div>
      <p v-if="formError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ formError }}</p>
      <label class="block">
        <span class="input-label">{{ tr('candidateId') }}</span>
        <input v-model="form.id" class="input font-mono" maxlength="32" :readonly="!!editingId" :disabled="disabled" data-testid="candidate-id" />
      </label>
      <AIRoutingTarget :key="formKey" :group-id="groupId" v-model:platform="form.platform" v-model:model="form.model" :target="form" :disabled="disabled" @update:target="Object.assign(form, $event)" />
      <label class="block">
        <span class="input-label">{{ tr('capability') }}</span>
        <textarea v-model="form.description" class="input" rows="2" maxlength="2048" :disabled="disabled" data-testid="candidate-description" />
      </label>
      <label class="block">
        <span class="input-label">{{ tr('cost') }}</span>
        <input v-model="form.cost_tier" class="input" maxlength="64" :disabled="disabled" />
      </label>
      <details>
        <summary class="cursor-pointer text-sm text-gray-700 dark:text-gray-300">{{ tr('limits') }}</summary>
        <p class="mt-2 input-hint">{{ tr('capabilityDeclarationHint') }}</p>
        <div class="mt-2 grid gap-3 sm:grid-cols-2">
          <label class="sm:col-span-2"><span class="input-label">{{ tr('contextWindow') }}</span><input :value="form.context_window_tokens || ''" class="input" type="number" min="0" step="1" :disabled="disabled" data-testid="candidate-context-window" @input="form.context_window_tokens = ($event.target as HTMLInputElement).valueAsNumber || 0" /></label>
          <label><span class="input-label">{{ tr('supportsTools') }}</span><Select :model-value="capabilityValue(form.supports_tools)" :options="capabilityOptions" :disabled="disabled" data-testid="candidate-supports-tools" @update:model-value="form.supports_tools = capabilityFromValue($event)" /></label>
          <label><span class="input-label">{{ tr('supportsImages') }}</span><Select :model-value="capabilityValue(form.supports_images)" :options="capabilityOptions" :disabled="disabled" data-testid="candidate-supports-images" @update:model-value="form.supports_images = capabilityFromValue($event)" /></label>
          <label><span class="input-label">{{ tr('maxInput') }}</span><input :value="form.max_input_tokens || ''" class="input" type="number" min="0" step="1" :disabled="disabled" @input="form.max_input_tokens = ($event.target as HTMLInputElement).valueAsNumber || 0" /></label>
          <label><span class="input-label">{{ tr('maxOutput') }}</span><input :value="form.max_output_tokens || ''" class="input" type="number" min="0" step="1" :disabled="disabled" @input="form.max_output_tokens = ($event.target as HTMLInputElement).valueAsNumber || 0" /></label>
          <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300"><input v-model="form.disable_tools" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-700" :disabled="disabled" />{{ tr('excludeTools') }}</label>
          <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300"><input v-model="form.disable_images" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-700" :disabled="disabled" />{{ tr('excludeImages') }}</label>
        </div>
      </details>
      <div class="flex items-center justify-between gap-3">
        <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300"><input v-model="form.enabled" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-700" :disabled="disabled" />{{ tr('include') }}</label>
        <button type="submit" class="btn btn-primary" :disabled="disabled || (!editingId && modelValue.length >= 64) || (!!editingId && !pending)" data-testid="candidate-apply"><Icon name="check" size="sm" class="mr-2" />{{ editingId ? tr('updateCandidate') : tr('addToDraft') }}</button>
      </div>
      <p v-if="pending" class="text-xs text-amber-700 dark:text-amber-400" data-testid="candidate-pending">{{ tr('pendingCandidate') }}</p>
    </form>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import { platformLabel } from '@/utils/platformColors'
import Select from '@/components/common/Select.vue'
import type { AICandidate, AITarget } from '@/api/admin/aiRouting'
import { validCompositeDestination } from '@/views/admin/groupsCompositeRoutes'
import AIRoutingTarget from './AIRoutingTarget.vue'

const props = defineProps<{ groupId: number; modelValue: AICandidate[]; disabled?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [AICandidate[]]; 'pending-change': [boolean] }>()
const { t } = useI18n()
const tr = (key: string) => t('admin.groups.aiRouting.' + key)
const capabilityOptions = computed(() => [
  { value: 'unknown', label: tr('capabilityUnknown') },
  { value: 'supported', label: tr('capabilitySupported') },
  { value: 'unsupported', label: tr('capabilityUnsupported') }
])
const capabilityValue = (value?: boolean | null) => value == null ? 'unknown' : value ? 'supported' : 'unsupported'
const capabilityFromValue = (value: unknown) => value === 'unknown' ? null : value === 'supported'
const editingId = ref<string | null>(null)
const formKey = ref(0)
const formError = ref('')
function emptyCandidate(): AICandidate {
  let i = 1
  while (props.modelValue.some(c => c.id === 'c' + i)) i++
  return { id: 'c' + i, platform: 'openai', model: '', description: '', cost_tier: '', enabled: true, target_account_id: null, target_group_id: null }
}
const form = ref<AICandidate>(emptyCandidate())
const clean = ref(JSON.stringify(form.value))
const pending = computed(() => JSON.stringify(form.value) !== clean.value)
watch(pending, value => emit('pending-change', value), { immediate: true })
const targetLabel = (target: AITarget) => target.target_group_id ? tr('group') + ' #' + target.target_group_id : tr('account') + ' #' + target.target_account_id
function reset(candidate?: AICandidate) {
  editingId.value = candidate?.id || null
  form.value = candidate ? { ...candidate } : emptyCandidate()
  clean.value = JSON.stringify(form.value)
  formError.value = ''
  formKey.value++
}
function canDiscard() { return !pending.value || window.confirm(tr('discardCandidate')) }
function startNew() { if (!props.disabled && canDiscard()) reset() }
function edit(candidate: AICandidate) {
  if (!props.disabled && candidate.id !== editingId.value && canDiscard()) reset(candidate)
}
function remove(candidate: AICandidate) {
  if (props.disabled || !window.confirm(tr('removeConfirm'))) return
  if (editingId.value === candidate.id) reset()
  emit('update:modelValue', props.modelValue.filter(c => c.id !== candidate.id))
}
function applyCandidate() {
  if (props.disabled) return
  const candidate = { ...form.value, id: form.value.id.trim(), model: form.value.model.trim() }
  if (!/^[A-Za-z0-9_-]{1,32}$/.test(candidate.id) || props.modelValue.some(c => c.id === candidate.id && c.id !== editingId.value)) {
    formError.value = tr('invalidCandidateId')
    return
  }
  if (!candidate.model || !validCompositeDestination({ ...candidate, mode: candidate.target_group_id !== null ? 'group' : 'account' }, props.groupId)) {
    formError.value = tr('candidateRequired')
    return
  }
  if ([candidate.max_input_tokens, candidate.max_output_tokens, candidate.context_window_tokens].some(value => value != null && (!Number.isSafeInteger(value) || value < 0))) {
    formError.value = tr('invalidCandidateLimits')
    return
  }
  if (props.modelValue.some(c => c.id !== editingId.value && c.platform === candidate.platform && c.model === candidate.model && c.target_account_id === candidate.target_account_id && c.target_group_id === candidate.target_group_id)) {
    formError.value = tr('duplicateCandidate')
    return
  }
  if (!editingId.value && props.modelValue.length >= 64) return
  const next = editingId.value ? props.modelValue.map(c => c.id === editingId.value ? candidate : c) : [...props.modelValue, candidate]
  emit('update:modelValue', next)
  // Keep the applied item selected; subsequent edits remain local until applied again.
  reset(candidate)
}
</script>

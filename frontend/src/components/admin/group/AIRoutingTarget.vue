<template>
  <div class="space-y-2">
    <label class="block">
      <span class="input-label">{{ t('admin.groups.form.platform') }}</span>
      <Select :model-value="platform" :options="platforms" :disabled="disabled"
        :aria-label="t('admin.groups.form.platform')" :placeholder="t('admin.groups.aiRouting.choosePlatform')"
        @update:model-value="changePlatform($event as AccountPlatform)" />
    </label>
    <CompositeRouteDestinationField
      v-if="platform" :model-value="destination" :platform="platform" :source-group-id="groupId"
      explicit-only :decision-engine="engine" :disabled="disabled" @update:model-value="changeTarget"
    />
    <label class="block"><span class="input-label">{{ t('admin.groups.aiRouting.model') }}</span>
      <input :value="model" class="input font-mono" :list="modelList" maxlength="256" autocomplete="off" :disabled="disabled"
        :placeholder="t('admin.groups.aiRouting.customModel')" @input="emit('update:model', ($event.target as HTMLInputElement).value)" />
      <datalist :id="modelList"><option v-for="name in models" :key="name" :value="name" /></datalist>
    </label>
    <p v-if="modelLoadFailed" class="input-hint">{{ t('admin.groups.aiRouting.modelsUnavailable') }}</p>
  </div>
</template>
<script setup lang="ts">
import { computed, ref, watch, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import { aiRouting, type AITarget } from '@/api/admin/aiRouting'
import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'
import type { AccountPlatform } from '@/types'
import type { CompositeRouteDestination } from '@/views/admin/groupsCompositeRoutes'
import CompositeRouteDestinationField from './CompositeRouteDestinationField.vue'
import Select from '@/components/common/Select.vue'
import { supportsAIDecisionCredential } from './aiRoutingOptions'

const props = defineProps<{ groupId: number; platform: AccountPlatform; target: AITarget; model: string; engine?: 'jev' | 'generic'; disabled?: boolean }>()
const emit = defineEmits<{
  'update:platform': [AccountPlatform]
  'update:model': [string]
  'update:target': [AITarget]
}>()
const { t } = useI18n()
const modelList = useId()
const mode = ref<'account'|'group'>('account')
const models = ref<string[]>([])
const modelLoadFailed = ref(false)
const platforms = computed(() => CONCRETE_PLATFORM_OPTIONS
  .filter(p => props.engine === 'jev' ? p.value === 'typesafe' : p.value !== 'typesafe')
  .map(p => {
    const disabled = !!props.engine && !supportsAIDecisionCredential(props.engine, p.value, 'apikey')
    return {...p, disabled, label: p.label + (disabled ? ' · ' + t('admin.groups.aiRouting.unsupportedPlatform') : '')}
  }))
watch(() => [props.target.target_group_id, props.target.target_account_id], ([group, account]) => {
  if (group) mode.value = 'group'
  else if (account) mode.value = 'account'
}, {immediate: true})
const destination = computed<CompositeRouteDestination>(() => ({ ...props.target, mode: mode.value }))
function changeTarget(value: CompositeRouteDestination) {
  if (props.disabled) return
  mode.value = value.mode === 'group' ? 'group' : 'account'
  emit('update:target', {target_group_id: value.target_group_id, target_account_id: value.target_account_id})
}
function changePlatform(value: AccountPlatform) {
  if (props.disabled) return
  emit('update:platform', value)
  emit('update:model', props.engine === 'jev' ? 'jev-latest' : '')
  emit('update:target', {target_account_id: null, target_group_id: null})
}
watch(() => [props.platform, props.target.target_group_id, props.target.target_account_id] as const,
  async ([platform, group, account], _, onCleanup) => {
    models.value = props.engine === 'jev' ? ['jev-latest'] : []
    modelLoadFailed.value = false
    if (!platform || (!group && !account)) return
    const controller = new AbortController()
    onCleanup(() => controller.abort())
    try {
      const names = await aiRouting.models(props.groupId, platform, {target_group_id:group, target_account_id:account}, controller.signal)
      if (!controller.signal.aborted) models.value = names || []
    } catch { if (!controller.signal.aborted) modelLoadFailed.value = true }
  }, {immediate: true})
</script>

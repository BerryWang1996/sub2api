<template>
  <details class="rounded border p-2 text-sm dark:border-dark-600" @toggle="open">
    <summary>{{ tr('history') }}</summary>
    <div class="mt-2 space-y-2">
      <button type="button" class="btn btn-secondary" :disabled="loading" @click="load">{{ t('common.refresh') }}</button>
      <p v-if="loading">{{ t('common.loading') }}</p><p v-else-if="error" role="alert">{{ error }}</p>
      <p v-else-if="!records.length">{{ tr('emptyHistory') }}</p>
      <div v-else class="overflow-x-auto">
        <table class="w-full text-left text-xs">
          <thead><tr><th>{{ tr('request') }}</th><th>{{ tr('decider') }}</th><th>{{ tr('selected') }}</th><th>{{ tr('result') }}</th></tr></thead>
          <tbody><tr v-for="(r,i) in records" :key="r.request_id + ':' + i" class="border-t align-top dark:border-dark-600">
            <td class="p-2"><p>{{ r.at }}</p><code :title="r.request_id">{{ r.request_id }}</code><p>{{ r.public_model }}</p>
              <p v-if="r.usage_request_id" class="mt-1">
                <span>{{ tr('usageRequest') }} · </span>
                <button type="button" class="break-all text-left font-mono text-primary-600 underline dark:text-primary-400"
                  :title="r.usage_request_id" :aria-label="tr('copyUsageRequest') + ': ' + r.usage_request_id"
                  @click="copyToClipboard(r.usage_request_id)">{{ r.usage_request_id }}</button>
              </p>
            </td>
            <td class="p-2">{{ r.decision_model || '—' }}<p v-if="r.decision_account_id">{{ tr('account') }} #{{ r.decision_account_id }}</p></td>
            <td class="p-2">{{ r.candidate_id }} · {{ r.execution_model }}<p>{{ tr('published') }} #{{ r.policy_version }}</p></td>
            <td class="p-2">{{ sourceLabel(r.source) }} · {{ r.latency_ms }} ms
              <p>{{ tr('usage') }} {{ r.input_tokens }} / {{ r.output_tokens }}</p>
              <p v-if="r.failure_reason">{{ tr('failureReason') }} · {{ tr('failure_' + r.failure_reason) }}</p>
              <p v-if="r.decision_cost_usd != null">{{ tr('estimatedCost') }} · USD {{ r.decision_cost_usd }}</p>
            </td>
          </tr></tbody>
        </table>
      </div>
    </div>
  </details>
</template>
<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { aiRouting, type AIDecisionRecord } from '@/api/admin/aiRouting'
import { extractApiErrorMessage } from '@/utils/apiError'
import { useClipboard } from '@/composables/useClipboard'
const props = defineProps<{groupId:number}>()
const {t} = useI18n()
const { copyToClipboard } = useClipboard()
const tr = (key: string) => t('admin.groups.aiRouting.' + key)
const sourceLabel = (source: string) => ['model', 'fallback', 'single', 'rejected', 'continuation', 'cancelled', 'empty', 'failed'].includes(source) ? tr('reason_' + source) : source
const records = ref<AIDecisionRecord[]>([])
const loading = ref(false)
const loaded = ref(false)
const error = ref('')
async function load() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try { records.value = await aiRouting.decisions(props.groupId); loaded.value = true }
  catch (e) { error.value = extractApiErrorMessage(e, tr('error')) }
  finally { loading.value = false }
}
function open(event: Event) { if ((event.target as HTMLDetailsElement).open && !loaded.value) void load() }
</script>

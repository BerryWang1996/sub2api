<template>
  <details class="rounded border p-2 text-sm dark:border-dark-600" @toggle="open">
    <summary>{{ tr('backgroundHistory') }}</summary>
    <div class="mt-2 space-y-2">
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ tr('backgroundHistoryHint') }}</p>
      <button type="button" class="btn btn-secondary" :disabled="loading" @click="load">{{ t('common.refresh') }}</button>
      <p v-if="loading">{{ t('common.loading') }}</p>
      <p v-else-if="error" role="alert">{{ error }}</p>
      <p v-else-if="!records.length">{{ tr('emptyHistory') }}</p>
      <div v-else class="overflow-x-auto">
        <table class="w-full text-left text-xs">
          <thead><tr><th>{{ tr('request') }}</th><th>{{ tr('selected') }}</th><th>{{ tr('backgroundState') }}</th><th>{{ tr('backgroundBilling') }}</th></tr></thead>
          <tbody><tr v-for="row in records" :key="row.id" class="border-t align-top dark:border-dark-600">
            <td class="p-2">
              <p>{{ formatDateTime(row.created_at) }}</p>
              <code class="break-all">{{ row.id }}</code>
              <p v-if="row.request_id" class="break-all">{{ tr('request') }} · {{ row.request_id }}</p>
              <button type="button" class="mt-1 break-all text-left font-mono text-primary-600 underline dark:text-primary-400"
                :aria-label="tr('copyUsageRequest') + ': ' + row.usage_request_id" @click="copyToClipboard(row.usage_request_id)">{{ row.usage_request_id }}</button>
            </td>
            <td class="p-2">{{ row.public_model }} → {{ row.execution_model }}<p>{{ tr('account') }} #{{ row.account_id }}</p></td>
            <td class="p-2">
              <p>{{ tr('backgroundStatus_' + row.status) }}</p>
              <p v-if="row.cache_sync_pending" class="text-amber-700 dark:text-amber-400">{{ tr('backgroundSyncPending') }}</p>
              <p v-if="row.failure_reason">{{ tr('backgroundIssue') }} · {{ row.failure_reason }}</p>
              <p>{{ formatDateTime(row.updated_at) }}</p>
            </td>
            <td class="p-2">
              <p>{{ tr(row.subscription_id ? 'backgroundSubscription' : 'backgroundBalance') }}</p>
              <p v-if="!['settled', 'released'].includes(row.status)">{{ tr('backgroundHold') }} · USD {{ row.hold_amount }}</p>
              <p v-else>{{ tr('backgroundHoldReleased') }}</p>
              <p>{{ tr('backgroundActual') }} · {{ row.actual_cost == null ? tr('usageUnknown') : 'USD ' + row.actual_cost }}</p>
              <p>{{ tr('usage') }} · {{ row.usage_known ? `${row.input_tokens} / ${row.output_tokens}` : tr('usageUnknown') }}</p>
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
import { aiRouting, type AIBackgroundJob } from '@/api/admin/aiRouting'
import { extractApiErrorMessage } from '@/utils/apiError'
import { useClipboard } from '@/composables/useClipboard'
import { formatDateTime } from '@/utils/format'

const props = defineProps<{ groupId: number }>()
const { t } = useI18n()
const { copyToClipboard } = useClipboard()
const tr = (key: string) => t('admin.groups.aiRouting.' + key)
const records = ref<AIBackgroundJob[]>([])
const loading = ref(false)
const loaded = ref(false)
const error = ref('')
async function load() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try { records.value = await aiRouting.backgroundJobs(props.groupId); loaded.value = true }
  catch (e) { error.value = extractApiErrorMessage(e, tr('error')) }
  finally { loading.value = false }
}
function open(event: Event) { if ((event.target as HTMLDetailsElement).open && !loaded.value) void load() }
</script>

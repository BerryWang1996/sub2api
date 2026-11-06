<template>
  <details v-if="checks.length" class="pt-2" data-testid="ai-capacity-checks">
    <summary class="cursor-pointer">{{ tr('capacityChecks') }}</summary>
    <p class="input-hint mt-2">{{ tr('capacityChecksHint') }}</p>
    <details v-for="(candidate, index) in checks" :key="candidate.candidate_id + ':' + index" class="mt-2 border-t border-gray-200 pt-2 dark:border-dark-600" :data-testid="'ai-capacity-candidate-' + candidate.candidate_id">
      <summary class="cursor-pointer break-words text-sm font-medium">{{ candidate.candidate_id }} · {{ candidate.model }}</summary>
      <p v-if="candidate.reason" class="mt-1 break-words text-xs">{{ tr('capacityCheckReason') }}: {{ codeLabel('capacityReason', candidate.reason) }}</p>
      <p v-if="!candidate.accounts.length" class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ tr('capacityNoAccounts') }}</p>
      <details v-for="account in candidate.accounts" :key="account.account_id" class="mt-2 border-t border-gray-200 pt-2 text-xs dark:border-dark-600" :data-testid="'ai-capacity-account-' + candidate.candidate_id + '-' + account.account_id">
        <summary class="cursor-pointer break-words">
          {{ tr('account') }} #{{ account.account_id }} · {{ tr(account.allowed ? 'capacityAllowed' : 'capacityDenied') }}
          <span class="mt-1 block text-amber-700 dark:text-amber-400">{{ tr(account.capacity_verified ? 'capacityVerified' : 'capacityEstimated') }}</span>
        </summary>
        <div class="mt-2 space-y-1 break-words text-gray-600 dark:text-gray-400">
          <p v-if="account.reason">{{ tr('capacityCheckReason') }}: {{ codeLabel('capacityReason', account.reason) }}</p>
          <p>{{ tr(account.final_payload_checked ? 'capacityFinalChecked' : 'capacityFinalUnchecked') }}</p>
          <p>{{ tr('capacityMethod') }}: {{ codeLabel('capacityMethod', account.capacity_method) }}</p>
          <p>{{ tr('capacityWindow') }}: {{ positiveLimit(account.context_window_tokens) }}</p>
          <p>{{ tr('capacityInputBytes') }}: {{ account.input_bytes ?? tr('capabilityUnknown') }} · {{ tr('capacityOutputReserve') }}: {{ positiveLimit(account.output_reserve_tokens) }}</p>
          <p>{{ tr('capacityMetadataSource') }}: {{ account.metadata_source?.trim() || tr('capabilityUnknown') }}</p>
          <p>{{ tr('capacityMetadataSynced') }}: <time v-if="formattedDate(account.metadata_synced_at)" :datetime="account.metadata_synced_at" :title="account.metadata_synced_at">{{ formattedDate(account.metadata_synced_at) }}</time><span v-else>{{ tr('capacityDateUnknown') }}</span></p>
          <ul v-if="account.warnings?.length" class="list-disc space-y-1 pl-4 text-amber-700 dark:text-amber-400">
            <li v-for="(warning, warningIndex) in account.warnings" :key="warningIndex">{{ codeLabel('capacityWarning', warning) }}</li>
          </ul>
        </div>
      </details>
    </details>
  </details>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { AICandidateCapacityCheck } from '@/api/admin/aiRouting'
import { formatDateTime } from '@/utils/format'

defineProps<{ checks: AICandidateCapacityCheck[] }>()
const { t, te } = useI18n()
const tr = (key: string) => t('admin.groups.aiRouting.' + key)
function codeLabel(category: string, value?: string) {
  if (!value) return tr('capabilityUnknown')
  const key = `admin.groups.aiRouting.${category}_${value}`
  return te(key) ? t(key) : value
}
const positiveLimit = (value?: number) => value && value > 0 ? String(value) : tr('capabilityUnknown')
const formattedDate = (value?: string) => formatDateTime(value)
</script>

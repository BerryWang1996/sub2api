<template>
  <div class="space-y-1 text-xs text-gray-600 dark:text-gray-400" data-testid="ai-context-summary">
    <template v-if="context">
      <p class="font-medium">{{ tr('contextSummary') }}</p>
      <p>{{ tr('contextSource') }}: {{ diagnosticLabel('source', context.source) }} · {{ tr('coverage') }}: {{ diagnosticLabel('coverage', context.coverage) }}</p>
      <p v-if="context.mode">{{ tr('contextProjection') }}: {{ diagnosticLabel('projection', context.mode) }}</p>
      <p>{{ tr('historySize') }}: {{ context.history_turns ?? '—' }} / {{ context.history_bytes ?? '—' }} B</p>
      <p>{{ tr('spanCoverage') }}: {{ context.scanned_spans ?? '—' }}/{{ context.total_spans ?? '—' }} · {{ tr('segmentCoverage') }}: {{ context.scanned_segments ?? '—' }}/{{ context.segments ?? '—' }}</p>
      <p v-if="context.selected_span_ids?.length" class="break-words">{{ tr('selectedSpans') }}: {{ context.selected_span_ids.join(', ') }}</p>
      <p>{{ tr('decisionInputBytes') }}: {{ context.input_bytes ?? '—' }} B · {{ tr('classifierCalls') }}: {{ context.classifier_calls ?? '—' }}</p>
      <p>{{ tr('excludedBlocks') }}: {{ context.excluded_blocks ?? '—' }} · {{ tr('redactedSpans') }}: {{ context.redacted_spans ?? '—' }}</p>
      <p v-if="context.failure_reason">{{ tr('failureReason') }}: {{ diagnosticLabel('failure', context.failure_reason) }}</p>
      <p v-if="context.projection_version">{{ tr('projectionVersion') }}: {{ context.projection_version }}</p>
    </template>
    <p v-if="decisionCalls != null">{{ tr('decisionCalls') }}: {{ decisionCalls }}</p>
    <details v-if="stages?.length" class="pt-1">
      <summary class="cursor-pointer">{{ tr('decisionStages') }} · {{ stages.length }}</summary>
      <div v-for="(stage, index) in stages" :key="index" class="mt-2 border-t border-gray-200 pt-2 dark:border-dark-600">
        <p>{{ index + 1 }}. {{ stageLabel(stage.stage) }} · {{ stage.protocol }} · {{ tr('account') }} #{{ stage.account_id }}</p>
        <p class="break-all">{{ tr('decider') }}: {{ stage.model }}<span v-if="stage.reported_model"> · {{ tr('reportedModel') }}: {{ stage.reported_model }}</span></p>
        <p>{{ tr('decisionInputBytes') }}: {{ stage.input_bytes }} B</p>
        <p v-if="stage.output_bytes != null">{{ tr('decisionOutputBytes') }}: {{ stage.output_bytes }} B</p>
        <p>{{ tr('usage') }}: {{ stage.attempted === false ? tr('usageNotCalled') : stage.usage_known ? stage.input_tokens + ' / ' + stage.output_tokens : tr('usageUnknown') }}</p>
        <p v-if="stage.attempted != null">{{ tr('externalCall') }}: {{ stage.attempted ? tr('yes') : tr('no') }}</p>
        <p v-if="stage.failure_reason">{{ tr('failureReason') }}: {{ diagnosticLabel('failure', stage.failure_reason) }}</p>
      </div>
    </details>
  </div>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { AIContextDiagnostic, AIDecisionStage } from '@/api/admin/aiRouting'
defineProps<{ context?: AIContextDiagnostic; decisionCalls?: number; stages?: AIDecisionStage[] }>()
const { t } = useI18n()
const tr = (key: string) => t('admin.groups.aiRouting.' + key)
function diagnosticLabel(category: string, value?: string) {
  if (!value) return '—'
  const key = `admin.groups.aiRouting.${category}_${value}`
  const translated = t(key)
  return translated === key ? value : translated
}
const stageLabel = (stage: string) => stage.startsWith('evidence:') ? tr('stage_evidence') + ' · ' + stage.slice('evidence:'.length) : diagnosticLabel('stage', stage)
</script>

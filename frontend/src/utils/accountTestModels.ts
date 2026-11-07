import type { ClaudeModel } from '@/types'

const currentGeminiPriority = [
  'gemini-3.8-flash',
  'gemini-3.5-flash-lite',
  'gemini-3.1-pro-preview',
  'gemini-4-argon'
]

// Keep Antigravity's existing image-test order separate from Gemini OAuth.
const antigravityPriority = [
  'gemini-3.1-flash-image', 'gemini-2.5-flash-image', 'gemini-3.5-flash',
  'gemini-2.5-flash', 'gemini-2.5-pro', 'gemini-3-flash-preview',
  'gemini-3-pro-preview', 'gemini-2.0-flash'
]

export function sortAccountTestModels(models: ClaudeModel[], platform: string): ClaudeModel[] {
  if (platform !== 'gemini' && platform !== 'antigravity') return [...models]
  const priority = new Map((platform === 'gemini' ? [...currentGeminiPriority, ...antigravityPriority] : antigravityPriority)
    .map((id, index) => [id, index]))
  return [...models].sort((a, b) =>
    (priority.get(a.id) ?? Number.MAX_SAFE_INTEGER) -
    (priority.get(b.id) ?? Number.MAX_SAFE_INTEGER))
}

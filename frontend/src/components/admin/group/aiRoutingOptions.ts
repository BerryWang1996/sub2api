import { isMultiProtocolApiKeyPlatform } from '@/components/account/credentialsBuilder'
import type { AccountPlatform, AccountType } from '@/types'

export function supportsAIDecisionCredential(engine: 'generic' | 'jev', platform: AccountPlatform, type: AccountType): boolean {
  if (engine === 'jev') return platform === 'typesafe' && type === 'apikey'
  if (platform === 'anthropic') return ['apikey','oauth','setup-token'].includes(type)
  if (platform === 'openai') return ['apikey','oauth'].includes(type)
  return type === 'apikey' && (platform === 'gemini' || isMultiProtocolApiKeyPlatform(platform))
}

import { describe, expect, it } from 'vitest'
import { sortAccountTestModels } from '../accountTestModels'
import type { ClaudeModel } from '@/types'

const models = (ids: string[]) => ids.map(id => ({ id, display_name: id }) as ClaudeModel)

describe('account test model choices', () => {
  it('prioritizes current Gemini text models over old and image models', () => {
    const input = models(['gemini-2.5-flash', 'gemini-3.1-flash-image', 'gemini-4-argon',
      'gemini-3.1-pro-preview', 'gemini-3.5-flash-lite', 'gemini-3.8-flash'])
    const original = input.map(m => m.id)
    expect(sortAccountTestModels(input, 'gemini').map(m => m.id)).toEqual([
      'gemini-3.8-flash', 'gemini-3.5-flash-lite', 'gemini-3.1-pro-preview',
      'gemini-4-argon', 'gemini-3.1-flash-image', 'gemini-2.5-flash'
    ])
    expect(input.map(m => m.id)).toEqual(original)
  })

  it('keeps explicitly configured model choices without injecting defaults', () => {
    const input = models(['custom-gemini', 'custom-image'])
    expect(sortAccountTestModels(input, 'gemini')).toEqual(input)
    expect(sortAccountTestModels([], 'gemini')).toEqual([])
  })

  it('preserves Antigravity image priority and other platforms ordering', () => {
    const input = models(['gemini-3.8-flash', 'gemini-2.5-flash-image', 'gemini-3.1-flash-image'])
    expect(sortAccountTestModels(input, 'antigravity').map(m => m.id)).toEqual([
      'gemini-3.1-flash-image', 'gemini-2.5-flash-image', 'gemini-3.8-flash'
    ])
    expect(sortAccountTestModels(input, 'openai')).toEqual(input)
  })
})

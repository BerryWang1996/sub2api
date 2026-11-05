import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useAppStore } from '../app'
import { checkUpdates } from '@/api/admin/system'
import { FORK_IMAGE_URL } from '@/utils/version'

vi.mock('@/api/admin/system', () => ({ checkUpdates: vi.fn() }))
vi.mock('@/i18n', () => ({ i18n: { global: { t: (key: string) => key } } }))

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
})

describe('application version metadata', () => {
  it.each([
    ['v0.2.19-barry.2', 'v0.2.19-barry.10', true],
    ['v0.2.19-barry.10', 'v0.2.19-barry.2', false]
  ])('uses the backend update result from %s to %s', async (current, latest, hasUpdate) => {
    const response = {
      current_version: current as string, latest_version: latest as string, has_update: hasUpdate as boolean,
      cached: false, build_type: 'release', update_channel: 'fork' as const, can_auto_update: false,
      manual_update_url: 'https://github.com/BerryWang1996/sub2api/pkgs/container/sub2api', warning: 'Image deployment required'
    }
    vi.mocked(checkUpdates).mockResolvedValue(response)
    const store = useAppStore()
    await store.fetchVersion()
    expect(store.currentVersion).toBe(current)
    expect(store.latestVersion).toBe(latest)
    expect(store.hasUpdate).toBe(hasUpdate)
    expect(store.canAutoUpdate).toBe(false)
    expect(await store.fetchVersion()).toEqual({ ...response, cached: true })
    expect(checkUpdates).toHaveBeenCalledTimes(1)
  })

  it('accepts earlier upstream metadata without fork capability fields', async () => {
    vi.mocked(checkUpdates).mockResolvedValue({ current_version: '0.2.18', latest_version: '0.2.19', has_update: true, cached: false, build_type: 'release' })
    const store = useAppStore()
    await store.fetchVersion()
    expect(store.updateChannel).toBe('upstream')
    expect(store.canAutoUpdate).toBe(true)
    expect(store.hasUpdate).toBe(true)
  })

  it.each(['v0.2.19-barry.2', 'barry-20261028.1', '0.2.19-barry.10b98d188867'])(
    'does not use upstream release metadata from an older fork backend: %s', async (version) => {
      vi.mocked(checkUpdates).mockResolvedValue({
        current_version: version, latest_version: '0.2.20', has_update: true, cached: false, build_type: 'release',
        release_info: { name: 'Upstream', body: '', published_at: '', html_url: 'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.20' }
      })
      const store = useAppStore()
      const info = await store.fetchVersion()
      expect(info).toMatchObject({ update_channel: 'fork', can_auto_update: false, has_update: false, latest_version: version })
      expect(store.updateChannel).toBe('fork')
      expect(store.canAutoUpdate).toBe(false)
      expect(store.manualUpdateUrl).toBe(FORK_IMAGE_URL)
      expect(store.releaseInfo).toBeNull()
      expect(store.versionWarning).not.toBe('')
      expect(await store.fetchVersion()).toMatchObject({ update_channel: 'fork', can_auto_update: false, has_update: false, cached: true })
      expect(checkUpdates).toHaveBeenCalledTimes(1)
    }
  )

  it('keeps a public fork identity safe even when the update response says upstream', async () => {
    vi.mocked(checkUpdates).mockResolvedValue({
      current_version: '0.2.19', latest_version: '0.2.20', has_update: true, cached: false, build_type: 'release',
      update_channel: 'upstream', can_auto_update: true
    })
    const store = useAppStore()
    store.siteVersion = 'v0.2.19-barry.2'
    await store.fetchVersion()
    expect(store.updateChannel).toBe('fork')
    expect(store.canAutoUpdate).toBe(false)
    expect(store.hasUpdate).toBe(false)
    expect(store.versionWarning).not.toBe('')
  })

  it('retains the public fork image channel when the first version request fails', async () => {
    const errorLog = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      vi.mocked(checkUpdates).mockRejectedValue(new Error('offline test'))
      const store = useAppStore()
      store.siteVersion = 'barry-20261028.1'
      expect(await store.fetchVersion()).toBeNull()
      expect(store.versionLoaded).toBe(false)
      expect(store.updateChannel).toBe('fork')
      expect(store.canAutoUpdate).toBe(false)
      expect(store.manualUpdateUrl).toBe(FORK_IMAGE_URL)
      expect(store.hasUpdate).toBe(false)
      expect(store.releaseInfo).toBeNull()
      expect(store.versionWarning).not.toBe('')
    } finally {
      errorLog.mockRestore()
    }
  })
})

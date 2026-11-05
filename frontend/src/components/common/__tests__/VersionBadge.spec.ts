import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import VersionBadge from '../VersionBadge.vue'
import { FORK_IMAGE_URL } from '@/utils/version'

const { auth, app, getRollbackVersions, rollback, performUpdate } = vi.hoisted(() => ({
  auth: { isAdmin: true },
  app: {
    currentVersion: '', latestVersion: '', hasUpdate: false, buildType: 'release',
    versionLoading: false, updateChannel: 'upstream', canAutoUpdate: true,
    manualUpdateUrl: '', versionWarning: '', releaseInfo: null,
    fetchVersion: vi.fn(), clearVersionCache: vi.fn()
  },
  getRollbackVersions: vi.fn(), rollback: vi.fn(), performUpdate: vi.fn()
}))

vi.mock('@/stores', () => ({ useAuthStore: () => auth, useAppStore: () => app }))
vi.mock('@/api/admin/system', () => ({ getRollbackVersions, rollback, performUpdate, restartService: vi.fn() }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copied: false, copyToClipboard: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string, values?: { version?: string }) => values?.version ? `${key} ${values.version}` : key })
}))

const mountBadge = (version = '') => mount(VersionBadge, { props: { version }, global: { stubs: { Icon: true } } })
const button = (wrapper: ReturnType<typeof mountBadge>, label: string) => wrapper.findAll('button').find(item => item.text() === label)!

beforeEach(() => {
  vi.clearAllMocks()
  auth.isAdmin = true
  Object.assign(app, {
    currentVersion: '', latestVersion: '', hasUpdate: false, buildType: 'release',
    versionLoading: false, updateChannel: 'upstream', canAutoUpdate: true,
    manualUpdateUrl: '', versionWarning: '', releaseInfo: null
  })
})

describe('VersionBadge fork versions', () => {
  it.each(['v0.2.19-barry.2', '0.2.19-barry.2'])('shows one v and the complete fork suffix for %s', async (version) => {
    app.currentVersion = version
    const wrapper = mountBadge()
    expect(wrapper.get('button').text()).toBe('v0.2.19-barry.2')
    await wrapper.get('button').trigger('click')
    expect(wrapper.text()).not.toContain('vv0.')
    expect(wrapper.text()).toContain('v0.2.19-barry.2')
    wrapper.unmount()
  })

  it('shows the full public settings version without calling admin endpoints for ordinary users', () => {
    auth.isAdmin = false
    const wrapper = mountBadge('v0.2.19-barry.2')
    expect(wrapper.text()).toBe('v0.2.19-barry.2')
    expect(app.fetchVersion).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('directs fork releases to their image channel and keeps backend update ordering', async () => {
    Object.assign(app, {
      currentVersion: 'v0.2.19-barry.2', latestVersion: 'v0.2.19-barry.10', hasUpdate: true,
      updateChannel: 'fork', canAutoUpdate: false,
      manualUpdateUrl: 'https://github.com/BerryWang1996/sub2api/pkgs/container/sub2api'
    })
    const wrapper = mountBadge()
    expect(wrapper.get('button').attributes('title')).toBe('version.updateAvailable')
    await wrapper.get('button').trigger('click')
    expect(wrapper.text()).toContain('version.latestVersion: v0.2.19-barry.10')
    expect(wrapper.get('a').attributes('href')).toBe(app.manualUpdateUrl)
    expect(wrapper.text()).toContain('version.forkImageHint')
    expect(wrapper.findAll('button').map(item => item.text())).not.toContain('version.updateNow')
    expect(wrapper.findAll('button').map(item => item.text())).not.toContain('version.rollback')
    expect(wrapper.text()).not.toContain('weishaw/sub2api')
    expect(getRollbackVersions).not.toHaveBeenCalled()
    expect(performUpdate).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('shows an inconclusive update check when release metadata is unavailable', async () => {
    Object.assign(app, { currentVersion: 'v0.2.19-barry.2', updateChannel: 'fork', canAutoUpdate: false, versionWarning: 'No fork release metadata' })
    const wrapper = mountBadge()
    expect(wrapper.get('button').attributes('title')).toBe('version.checkUnavailable')
    await wrapper.get('button').trigger('click')
    expect(wrapper.text()).toContain('No fork release metadata')
    expect(wrapper.text()).not.toContain('version.upToDate')
    wrapper.unmount()
  })

  it.each([
    ['v0.2.19-barry.2', ''],
    ['barry-20261028.1', ''],
    ['0.2.19-barry.10b98d188867', ''],
    ['', 'v0.2.19-barry.2'],
    ['', 'barry-20261028.1'],
    ['', '0.2.19-barry.10b98d188867']
  ])('blocks old upstream capabilities for fork current=%s public=%s', async (current, publicVersion) => {
    Object.assign(app, {
      currentVersion: current, latestVersion: '0.2.20', hasUpdate: true,
      updateChannel: 'upstream', canAutoUpdate: true,
      releaseInfo: { html_url: 'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.20' }
    })
    const wrapper = mountBadge(publicVersion)
    expect(wrapper.get('button').attributes('title')).toBe('version.checkUnavailable')
    await wrapper.get('button').trigger('click')
    expect(wrapper.text()).toContain('version.forkImageHint')
    expect(wrapper.text()).not.toContain('version.latestVersion: v0.2.20')
    expect(wrapper.findAll('a').map(item => item.attributes('href'))).toEqual([FORK_IMAGE_URL])
    expect(wrapper.findAll('button').map(item => item.text())).not.toContain('version.updateNow')
    expect(wrapper.findAll('button').map(item => item.text())).not.toContain('version.rollback')
    expect(getRollbackVersions).not.toHaveBeenCalled()
    expect(performUpdate).not.toHaveBeenCalled()
    expect(rollback).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('keeps the public fork image hint after the first version request fails', async () => {
    Object.assign(app, { buildType: 'source', versionWarning: 'Version request failed' })
    app.fetchVersion.mockResolvedValue(null)
    const wrapper = mountBadge('v0.2.19-barry.2')
    await flushPromises()
    await wrapper.get('button').trigger('click')
    expect(wrapper.get('button').attributes('title')).toBe('version.checkUnavailable')
    expect(wrapper.text()).toContain('version.forkImageHint')
    expect(wrapper.text()).toContain('Version request failed')
    expect(wrapper.findAll('a').map(item => item.attributes('href'))).toEqual([FORK_IMAGE_URL])
    expect(wrapper.text()).not.toContain('version.rollback')
    expect(performUpdate).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('preserves the update action for upstream releases', async () => {
    Object.assign(app, { currentVersion: '0.2.18', latestVersion: '0.2.19', hasUpdate: true })
    performUpdate.mockResolvedValue({ need_restart: false })
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')
    await button(wrapper, 'version.updateNow').trigger('click')
    await flushPromises()
    expect(performUpdate).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('keeps upstream rollback order and submits the original version while formatting labels and commands once', async () => {
    app.currentVersion = '0.2.19'
    getRollbackVersions.mockResolvedValue({ versions: ['v0.2.18', 'v0.2.17', 'v0.2.9'].map(version => ({ version, published_at: '', html_url: '' })) })
    rollback.mockResolvedValue({ need_restart: false })
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')
    await button(wrapper, 'version.rollback').trigger('click')
    await flushPromises()
    const versionButtons = wrapper.findAll('button').filter(item => /^v0\.2\.(18|17|9)$/.test(item.text()))
    expect(versionButtons.map(item => item.text())).toEqual(['v0.2.18', 'v0.2.17', 'v0.2.9'])
    await versionButtons[0].trigger('click')
    expect(wrapper.get('code').text()).toContain('/v0.2.18/deploy/install.sh')
    await button(wrapper, 'version.deployDocker').trigger('click')
    expect(wrapper.get('code').text()).toContain('image: weishaw/sub2api:0.2.18')
    await button(wrapper, 'version.rollbackConfirm v0.2.18').trigger('click')
    await flushPromises()
    expect(rollback).toHaveBeenCalledWith('v0.2.18')
    wrapper.unmount()
  })
})

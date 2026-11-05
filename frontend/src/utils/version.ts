export const FORK_IMAGE_URL = 'https://github.com/BerryWang1996/sub2api/pkgs/container/sub2api'

// Retain legacy date and commit-SHA identities when capability fields are absent.
export function isForkVersion(version?: string): boolean {
  const normalized = (version || '').trim().replace(/^v/i, '')
  return normalized.includes('-barry.') || normalized.startsWith('barry-')
}

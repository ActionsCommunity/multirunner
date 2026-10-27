import { beforeEach, expect, test, vi } from 'vitest'
import type { Overview } from './history-api'
import {
  getSnapshotSession,
  operationalSnapshotSource,
  purgeOperationalSnapshot,
  readOperationalSnapshot,
  saveOperationalSnapshot,
  snapshotAgeLabel,
} from './offline-snapshot'

const overview: Overview = {
  runner_sessions: 20,
  pending_sessions: 2,
  workflow_runs: 18,
  workflow_jobs: 21,
  successful_jobs: 19,
  failed_jobs: 1,
  cancelled_jobs: 1,
  average_duration_seconds: 92,
}

beforeEach(() => {
  window.localStorage.clear()
  window.sessionStorage.clear()
  vi.unstubAllGlobals()
})

test('stores only the explicit nonsensitive operational allowlist', () => {
  const now = new Date('2026-10-10T12:00:00Z')
  const snapshot = saveOperationalSnapshot(
    overview,
    {
      runners: 4,
      pending: 1,
      failed: 0,
      last_event_at: '2026-10-10T11:59:00Z',
    },
    {
      issued_at: '2026-10-10T11:00:00Z',
      revocation_generation: 3,
    },
    now,
  )

  expect(snapshot).toEqual({
    version: 1,
    saved_at: now.toISOString(),
    expires_at: '2026-10-10T23:00:00.000Z',
    source: operationalSnapshotSource,
    session_issued_at: '2026-10-10T11:00:00.000Z',
    revocation_generation: 3,
    overview,
    live: {
      runners: 4,
      pending: 1,
      failed: 0,
      last_event_at: '2026-10-10T11:59:00.000Z',
    },
  })
  expect(JSON.stringify(snapshot)).not.toMatch(
    /repository|workflow_name|audit|payload|csrf|proof|database/i,
  )
})

test('labels stale age and purges expired snapshots before reading', () => {
  const savedAt = '2026-10-10T10:00:00Z'
  saveOperationalSnapshot(
    overview,
    { runners: 1, pending: 0, failed: 0 },
    { issued_at: savedAt, revocation_generation: 1 },
    new Date(savedAt),
  )

  expect(
    snapshotAgeLabel(savedAt, new Date('2026-10-10T11:32:00Z')),
  ).toBe('1 hour old')
  expect(
    readOperationalSnapshot(new Date('2026-10-10T21:59:00Z')),
  ).toBeDefined()
  expect(
    readOperationalSnapshot(new Date('2026-10-10T22:00:00Z')),
  ).toBeUndefined()
  expect(window.localStorage.length).toBe(0)
})

test('purges snapshot when session validation returns 401', async () => {
  saveOperationalSnapshot(
    overview,
    { runners: 1, pending: 0, failed: 0 },
    {
      issued_at: new Date(Date.now() - 60_000).toISOString(),
      revocation_generation: 1,
    },
  )
  window.sessionStorage.setItem(
    'multirunner.console.origin-proof',
    'p'.repeat(43),
  )
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(new Response('', { status: 401 }))),
  )

  await expect(getSnapshotSession()).rejects.toThrow(
    'Session validation failed',
  )
  expect(readOperationalSnapshot()).toBeUndefined()
  expect(window.sessionStorage.length).toBe(0)
})

test('supports explicit purge', () => {
  saveOperationalSnapshot(
    overview,
    { runners: 1, pending: 0, failed: 0 },
    {
      issued_at: new Date(Date.now() - 60_000).toISOString(),
      revocation_generation: 1,
    },
  )
  purgeOperationalSnapshot()
  expect(readOperationalSnapshot()).toBeUndefined()
})

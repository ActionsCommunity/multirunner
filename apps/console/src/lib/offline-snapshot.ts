import type { Overview } from './history-api'
import type { RunnerState } from './operations-api'
import { consoleFetch, isJSONRecord, responseError } from './console-fetch'

const storageKey = 'multirunner.console.operational-snapshot.v1'
const maximumAgeMilliseconds = 12 * 60 * 60 * 1000

export const operationalSnapshotSource = 'Local console API'

export interface SnapshotSession {
  issued_at: string
  revocation_generation: number
}

export interface LiveSummary {
  runners: number
  pending: number
  failed: number
  last_event_at?: string
}

export interface OperationalSnapshot {
  version: 1
  saved_at: string
  expires_at: string
  source: typeof operationalSnapshotSource
  session_issued_at: string
  revocation_generation: number
  overview: Overview
  live: LiveSummary
}

function finiteCount(value: unknown) {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
    ? Math.floor(value)
    : 0
}

function isCount(value: unknown): value is number {
  return (
    typeof value === 'number' &&
    Number.isFinite(value) &&
    value >= 0 &&
    Number.isInteger(value)
  )
}

function isOverview(value: unknown): value is Overview {
  if (!value || typeof value !== 'object') return false
  const overview = value as Partial<Overview>
  return (
    isCount(overview.runner_sessions) &&
    isCount(overview.pending_sessions) &&
    isCount(overview.workflow_runs) &&
    isCount(overview.workflow_jobs) &&
    isCount(overview.successful_jobs) &&
    isCount(overview.failed_jobs) &&
    isCount(overview.cancelled_jobs) &&
    typeof overview.average_duration_seconds === 'number' &&
    Number.isFinite(overview.average_duration_seconds) &&
    overview.average_duration_seconds >= 0
  )
}

function isLiveSummary(value: unknown): value is LiveSummary {
  if (!value || typeof value !== 'object') return false
  const live = value as Partial<LiveSummary>
  return (
    isCount(live.runners) &&
    isCount(live.pending) &&
    isCount(live.failed) &&
    (live.last_event_at === undefined ||
      (typeof live.last_event_at === 'string' &&
        !Number.isNaN(Date.parse(live.last_event_at))))
  )
}

function sanitizeOverview(value: Overview): Overview {
  return {
    runner_sessions: finiteCount(value.runner_sessions),
    pending_sessions: finiteCount(value.pending_sessions),
    workflow_runs: finiteCount(value.workflow_runs),
    workflow_jobs: finiteCount(value.workflow_jobs),
    successful_jobs: finiteCount(value.successful_jobs),
    failed_jobs: finiteCount(value.failed_jobs),
    cancelled_jobs: finiteCount(value.cancelled_jobs),
    average_duration_seconds:
      typeof value.average_duration_seconds === 'number' &&
      Number.isFinite(value.average_duration_seconds) &&
      value.average_duration_seconds >= 0
        ? value.average_duration_seconds
        : 0,
  }
}

export function summarizeRunners(
  runners: RunnerState[],
  lastEventAt?: string,
): LiveSummary {
  return {
    runners: runners.length,
    pending: runners.filter((runner) =>
      ['planned', 'pending', 'queued', 'registering', 'launching'].includes(
        runner.status,
      ),
    ).length,
    failed: runners.filter((runner) =>
      ['failed', 'startup_failure'].includes(runner.status),
    ).length,
    ...(lastEventAt && !Number.isNaN(Date.parse(lastEventAt))
      ? { last_event_at: new Date(lastEventAt).toISOString() }
      : {}),
  }
}

export function saveOperationalSnapshot(
  overview: Overview,
  live: LiveSummary,
  session: SnapshotSession,
  now = new Date(),
): OperationalSnapshot | undefined {
  if (typeof window === 'undefined' || Number.isNaN(Date.parse(session.issued_at))) {
    return undefined
  }
  const issuedAt = new Date(session.issued_at)
  const expiresAt = new Date(issuedAt.getTime() + maximumAgeMilliseconds)
  if (expiresAt.getTime() <= now.getTime()) {
    purgeOperationalSnapshot()
    return undefined
  }
  const snapshot: OperationalSnapshot = {
    version: 1,
    saved_at: now.toISOString(),
    expires_at: expiresAt.toISOString(),
    source: operationalSnapshotSource,
    session_issued_at: issuedAt.toISOString(),
    revocation_generation: finiteCount(session.revocation_generation),
    overview: sanitizeOverview(overview),
    live: {
      runners: finiteCount(live.runners),
      pending: finiteCount(live.pending),
      failed: finiteCount(live.failed),
      ...(live.last_event_at && !Number.isNaN(Date.parse(live.last_event_at))
        ? { last_event_at: new Date(live.last_event_at).toISOString() }
        : {}),
    },
  }
  try {
    window.localStorage.setItem(storageKey, JSON.stringify(snapshot))
    return snapshot
  } catch {
    return undefined
  }
}

export function readOperationalSnapshot(
  now = new Date(),
): OperationalSnapshot | undefined {
  if (typeof window === 'undefined') return undefined
  try {
    const raw = window.localStorage.getItem(storageKey)
    if (!raw) return undefined
    const value: unknown = JSON.parse(raw)
    if (
      !isJSONRecord(value) ||
      value.version !== 1 ||
      value.source !== operationalSnapshotSource ||
      !isOverview(value.overview) ||
      !isLiveSummary(value.live) ||
      typeof value.saved_at !== 'string' ||
      typeof value.expires_at !== 'string' ||
      typeof value.session_issued_at !== 'string' ||
      typeof value.revocation_generation !== 'number' ||
      !Number.isInteger(value.revocation_generation) ||
      value.revocation_generation < 0 ||
      Number.isNaN(Date.parse(value.saved_at)) ||
      Number.isNaN(Date.parse(value.expires_at)) ||
      Number.isNaN(Date.parse(value.session_issued_at)) ||
      Date.parse(value.expires_at) <= now.getTime()
    ) {
      purgeOperationalSnapshot()
      return undefined
    }
    return {
      version: 1,
      saved_at: new Date(value.saved_at).toISOString(),
      expires_at: new Date(value.expires_at).toISOString(),
      source: operationalSnapshotSource,
      session_issued_at: new Date(value.session_issued_at).toISOString(),
      revocation_generation: value.revocation_generation,
      overview: value.overview,
      live: {
        runners: value.live.runners,
        pending: value.live.pending,
        failed: value.live.failed,
        ...(value.live.last_event_at
          ? { last_event_at: new Date(value.live.last_event_at).toISOString() }
          : {}),
      },
    }
  } catch {
    purgeOperationalSnapshot()
    return undefined
  }
}

export function purgeOperationalSnapshot() {
  if (typeof window === 'undefined') return
  try {
    window.localStorage.removeItem(storageKey)
  } catch {
    // A blocked storage area is already effectively purged.
  }
}

export async function getSnapshotSession(
  signal?: AbortSignal,
): Promise<SnapshotSession> {
  const response = await consoleFetch('/api/v1/session', {
    headers: { Accept: 'application/json' },
    signal,
  })
  if (response.status === 401) purgeOperationalSnapshot()
  if (!response.ok) {
    throw await responseError(response, 'Session validation failed')
  }
  const session: unknown = await response.json()
  if (
    !isJSONRecord(session) ||
    typeof session.issued_at !== 'string' ||
    Number.isNaN(Date.parse(session.issued_at)) ||
    typeof session.revocation_generation !== 'number'
  ) {
    purgeOperationalSnapshot()
    throw new Error('Session validation response is incomplete.')
  }
  return {
    issued_at: session.issued_at,
    revocation_generation: session.revocation_generation,
  }
}

export function snapshotMatchesSession(
  snapshot: OperationalSnapshot,
  session: SnapshotSession,
) {
  return (
    snapshot.session_issued_at === new Date(session.issued_at).toISOString() &&
    snapshot.revocation_generation === session.revocation_generation
  )
}

export function snapshotAgeLabel(savedAt: string, now = new Date()) {
  const elapsed = Math.max(0, now.getTime() - Date.parse(savedAt))
  const minutes = Math.floor(elapsed / 60_000)
  if (minutes < 1) return 'less than a minute old'
  if (minutes < 60) return `${minutes} minute${minutes === 1 ? '' : 's'} old`
  const hours = Math.floor(minutes / 60)
  return `${hours} hour${hours === 1 ? '' : 's'} old`
}

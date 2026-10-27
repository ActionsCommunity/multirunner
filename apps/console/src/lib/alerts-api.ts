import { requestJSON, type JSONContract } from './console-fetch'
import { assertMutationAllowed } from './offline-policy'

export type AlertState = 'pending' | 'open' | 'resolved'

export interface AlertInstance {
  id: string
  rule_id: string
  dedup_key: string
  state: AlertState
  severity: 'critical' | 'high' | 'medium' | 'low'
  summary: string
  details: Record<string, unknown>
  source_event_id: string
  first_observed_at: string
  last_observed_at: string
  due_at: string
  opened_at?: string
  resolved_at?: string
  acknowledged_at?: string
  acknowledged_by?: string
  silenced_until?: string
  occurrence_count: number
  version: number
}

export interface AlertSilence {
  id: string
  alert_id: string
  starts_at: string
  expires_at: string
  created_at: string
  created_by: string
  reason: string
}

export interface AlertAnnotation {
  id: string
  alert_id: string
  body: string
  created_at: string
  created_by: string
}

interface AlertListResponse {
  items: AlertInstance[]
  count: number
  applied_filters: Record<string, unknown>
  cursor: string
  next_cursor: string
}

interface ConsoleSession {
  csrf_token: string
}

export async function listAlerts(
  state: AlertState | '',
  signal?: AbortSignal,
) {
  const parameters = new URLSearchParams({ limit: '200' })
  if (state) parameters.set('state', state)
  return requestJSON<AlertListResponse>(
    `/api/v1/alerts?${parameters}`,
    { signal },
    'Alert list request failed',
    { required: ['items', 'count'], arrays: ['items'], numbers: ['count'] },
  )
}

export async function acknowledgeAlert(
  alert: AlertInstance,
  reason: string,
  idempotencyKey: string,
  signal?: AbortSignal,
) {
  return mutateAlert<AlertInstance>(
    alert,
    'acknowledge',
    { reason },
    idempotencyKey,
    signal,
    { required: ['id', 'state', 'version'] },
  )
}

export async function silenceAlert(
  alert: AlertInstance,
  until: string,
  reason: string,
  idempotencyKey: string,
  signal?: AbortSignal,
) {
  return mutateAlert<{ alert: AlertInstance; silence: AlertSilence }>(
    alert,
    'silence',
    { until, reason },
    idempotencyKey,
    signal,
    { required: ['alert', 'silence'] },
  )
}

export async function resolveAlert(
  alert: AlertInstance,
  reason: string,
  idempotencyKey: string,
  signal?: AbortSignal,
) {
  return mutateAlert<AlertInstance>(
    alert,
    'resolve',
    { reason },
    idempotencyKey,
    signal,
    { required: ['id', 'state', 'version'] },
  )
}

export async function annotateAlert(
  alert: AlertInstance,
  body: string,
  idempotencyKey: string,
  signal?: AbortSignal,
) {
  assertMutationAllowed()
  const session = await getSession(signal)
  return requestJSON<AlertAnnotation>(
    `/api/v1/alerts/${encodeURIComponent(alert.id)}/annotations`,
    {
      method: 'POST',
      headers: mutationHeaders(session.csrf_token, idempotencyKey),
      body: JSON.stringify({ body }),
      signal,
    },
    'Alert annotation failed',
    { required: ['id', 'alert_id', 'body', 'created_at'] },
  )
}

async function mutateAlert<T>(
  alert: AlertInstance,
  action: string,
  body: Record<string, unknown>,
  idempotencyKey: string,
  signal?: AbortSignal,
  contract: JSONContract = {},
) {
  assertMutationAllowed()
  const session = await getSession(signal)
  return requestJSON<T>(
    `/api/v1/alerts/${encodeURIComponent(alert.id)}/${action}`,
    {
      method: 'POST',
      headers: {
        ...mutationHeaders(session.csrf_token, idempotencyKey),
        'If-Match': `"${alert.version}"`,
      },
      body: JSON.stringify(body),
      signal,
    },
    'Alert mutation failed',
    contract,
  )
}

async function getSession(signal?: AbortSignal) {
  return requestJSON<ConsoleSession>(
    '/api/v1/session',
    { signal },
    'Console session request failed',
    { required: ['csrf_token'], strings: ['csrf_token'] },
  )
}

function mutationHeaders(csrfToken: string, idempotencyKey: string) {
  return {
    Accept: 'application/json',
    'Content-Type': 'application/json',
    'X-CSRF-Token': csrfToken,
    'Idempotency-Key': idempotencyKey,
  }
}

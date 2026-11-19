import { requestJSON } from './console-fetch'

export interface NamedResource {
  name: string
  count: number
}

export interface SystemStatus {
  status: string
  mode: string
  listen: string
  database: string
  started_at: string
  app_version: string
  api_version: string
  configuration: string
}

export interface AuditRecord {
  id: string
  source: string
  occurred_at: string
  actor_kind: string
  actor_id: string
  action: string
  target_type: string
  target_id: string
  correlation_id: string
  outcome?: string
  payload?: Record<string, unknown>
}

export interface ResourceCollection<T> {
  items: T[]
  applied_filters: Record<string, unknown>
  cursor: string
  next_cursor: string
  count: number
}

export function listPools(signal?: AbortSignal) {
  return requestJSON<ResourceCollection<NamedResource>>(
    '/api/v1/pools?limit=200',
    { signal },
    'Pool history is unavailable',
    { required: ['items', 'count'], arrays: ['items'], numbers: ['count'] },
  )
}

export function getSystemStatus(signal?: AbortSignal) {
  return requestJSON<SystemStatus>(
    '/api/v1/system',
    { signal },
    'Host status is unavailable',
    { required: ['status', 'mode', 'app_version'], strings: ['status', 'mode', 'app_version'] },
  )
}

export function listAuditRecords(signal?: AbortSignal) {
  return requestJSON<ResourceCollection<AuditRecord>>(
    '/api/v1/audit?limit=100',
    { signal },
    'Audit history is unavailable',
    { required: ['items', 'count'], arrays: ['items'], numbers: ['count'] },
  )
}

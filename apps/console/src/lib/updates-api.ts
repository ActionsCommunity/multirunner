import { requestJSON } from './console-fetch'

export type UpdateState =
  | 'inspecting'
  | 'staging'
  | 'staged'
  | 'activating'
  | 'succeeded'
  | 'rolled_back'
  | 'failed'

export interface UpdateMetadata {
  id: string
  command_id: string
  state: UpdateState
  created_at: string
  updated_at: string
  activated_at?: string
  completed_at?: string
  version: string
  commit: string
  host_id: string
  target_path: string
  size_bytes: number
  sha256: string
  schema_min: number
  schema_max: number
  api_version: string
  rollback_reason?: string
  error?: string
}

export interface UpdateList {
  items: UpdateMetadata[]
  count: number
  applied_filters: Record<string, unknown>
  cursor: string
  next_cursor: string
}

export interface UpdateInspection {
  checked_at: string
  trust_configured: boolean
  verified: boolean
  metadata_consistent: boolean
  compatible: boolean
  apply_allowed: boolean
  version?: string
  commit?: string
  target_path?: string
  size_bytes?: number
  schema_min?: number
  schema_max?: number
  api_version?: string
  timestamp_expires?: string
  snapshot_expires?: string
  targets_expires?: string
  reason?: string
}

export function listUpdates(signal?: AbortSignal) {
  return requestJSON<UpdateList>(
    '/api/v1/updates?limit=100',
    { signal },
    'Update request failed',
    { required: ['items', 'count'], arrays: ['items'], numbers: ['count'] },
  )
}

export function inspectUpdates(signal?: AbortSignal) {
  return requestJSON<UpdateInspection>(
    '/api/v1/updates/check',
    { signal },
    'Update inspection failed',
    {
      required: ['checked_at', 'trust_configured', 'verified', 'compatible'],
      strings: ['checked_at'],
      booleans: ['trust_configured', 'verified', 'compatible'],
    },
  )
}

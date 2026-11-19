import { requestJSON } from './console-fetch'

export type RestoreState =
  | 'staging'
  | 'staged'
  | 'activating'
  | 'succeeded'
  | 'rolled_back'
  | 'failed'

export interface RestoreMetadata {
  id: string
  command_id: string
  backup_id: string
  state: RestoreState
  created_at: string
  updated_at: string
  activated_at?: string
  completed_at?: string
  host_id: string
  schema_version: number
  size_bytes: number
  sha256: string
  quick_check?: string
  rollback_reason?: string
  error?: string
}

export interface RestoreList {
  items: RestoreMetadata[]
  count: number
  applied_filters: Record<string, unknown>
  cursor: string
  next_cursor: string
}

export function listRestores(signal?: AbortSignal) {
  return requestJSON<RestoreList>(
    '/api/v1/restores?limit=100',
    { signal },
    'Restore request failed',
    { required: ['items', 'count'], arrays: ['items'], numbers: ['count'] },
  )
}

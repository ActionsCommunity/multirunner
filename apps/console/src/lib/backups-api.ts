import { requestJSON } from './console-fetch'

export type BackupState = 'creating' | 'succeeded' | 'failed'

export interface BackupMetadata {
  id: string
  command_id: string
  purpose: 'manual' | 'pre_restore' | 'pre_update'
  state: BackupState
  created_at: string
  completed_at?: string
  expires_at: string
  file_name?: string
  size_bytes?: number
  sha256?: string
  schema_version?: number
  app_version?: string
  host_id?: string
  audit_event_id?: string
  audit_integrity_hash?: string
  quick_check?: string
  error?: string
  download_url?: string
}

export interface BackupList {
  items: BackupMetadata[]
  count: number
  applied_filters: Record<string, unknown>
  cursor: string
  next_cursor: string
}

export function listBackups(signal?: AbortSignal) {
  return requestJSON<BackupList>(
    '/api/v1/backups?limit=100',
    { signal },
    'Backup request failed',
    { required: ['items', 'count'], arrays: ['items'], numbers: ['count'] },
  )
}

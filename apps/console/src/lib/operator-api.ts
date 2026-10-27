import { requestJSON } from './console-fetch'

export interface DiagnosticResult {
  id: string
  version: number
  category: string
  severity: string
  status: 'pass' | 'warn' | 'fail'
  observed_at: string
  duration_ms: number
  observed: string
  expected: string
  remediation?: string
  docs_route?: string
  evidence?: Record<string, unknown>
  error?: string
}

export interface DiagnosticReport {
  generated_at: string
  status: 'pass' | 'warn' | 'fail'
  results: DiagnosticResult[]
}

export interface ConfigurationField {
  path: string
  effective: unknown
  default?: unknown
  source: string
  secret: boolean
  restart_required: boolean
}

export interface ConfigurationSnapshot {
  source_file: string
  loaded_at: string
  inspected_at: string
  startup_sha256: string
  current_sha256: string
  drifted: boolean
  changed_paths: string[]
  validation_error?: string
  raw_redacted: string
  normalized_redacted: string
  fields: ConfigurationField[]
  guidance: string
}

export function getDiagnostics(signal?: AbortSignal) {
  return requestJSON<DiagnosticReport>(
    '/api/v1/diagnostics',
    { signal },
    'Diagnostics request failed',
    { required: ['generated_at', 'status', 'results'], arrays: ['results'] },
  )
}

export function getConfiguration(signal?: AbortSignal) {
  return requestJSON<ConfigurationSnapshot>(
    '/api/v1/configuration',
    { signal },
    'Configuration request failed',
    {
      required: ['source_file', 'loaded_at', 'fields'],
      strings: ['source_file', 'loaded_at'],
      arrays: ['fields'],
    },
  )
}

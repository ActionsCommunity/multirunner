import { requestJSON } from './console-fetch'

export interface RunInspectionStep {
  number: number
  name: string
  status: string
  conclusion: string
  started_at?: string
  completed_at?: string
}

export interface RunInspectionJob {
  id: number
  run_id: number
  run_attempt: number
  name: string
  workflow_name: string
  status: string
  conclusion: string
  runner_name: string
  pool_name: string
  local_session_id: string
  attribution_source: string
  attribution_confidence: number
  html_url: string
  created_at: string
  started_at?: string
  completed_at?: string
  steps: RunInspectionStep[]
}

export interface RunInspectionSession {
  id: string
  runner_name: string
  pool_name: string
  target: string
  status: string
  conclusion: string
  backend_id: string
  attribution_source: string
  attribution_confidence: number
  planned_at: string
  registered_at?: string
  launched_at?: string
  started_at: string
  completed_at?: string
  error?: string
}

export interface RunInspection {
  run: {
    id: number
    repository: string
    name: string
    workflow_name: string
    display_title: string
    event: string
    status: string
    conclusion: string
    head_branch: string
    head_sha: string
    actor: string
    triggering_actor: string
    html_url: string
    run_number: number
    run_attempt: number
    created_at: string
    started_at?: string
    completed_at?: string
  }
  jobs: RunInspectionJob[]
  runner_sessions: RunInspectionSession[]
}

export function getRunInspection(
  repository: string,
  runID: string,
  signal?: AbortSignal,
) {
  const parameters = new URLSearchParams({ repository })
  return requestJSON<RunInspection>(
    `/api/v1/runs/${encodeURIComponent(runID)}?${parameters}`,
    { signal },
    'Run inspection request failed',
    { required: ['run', 'jobs', 'runner_sessions'], arrays: ['jobs', 'runner_sessions'] },
  )
}

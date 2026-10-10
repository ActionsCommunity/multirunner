import { requestJSON } from './console-fetch'

export interface Overview {
  runner_sessions: number
  pending_sessions: number
  workflow_runs: number
  workflow_jobs: number
  successful_jobs: number
  failed_jobs: number
  cancelled_jobs: number
  average_duration_seconds: number
}

export interface RecentRun {
  id: number
  repository: string
  name: string
  workflow_name: string
  display_title: string
  status: string
  conclusion: string
  html_url: string
  run_attempt: number
  created_at: string
  started_at?: string
}

interface RunsResponse {
  runs: RecentRun[]
}

export async function getOverview(signal: AbortSignal) {
  const [overview, recent] = await Promise.all([
    requestJSON<Overview>(
      '/api/summary',
      { headers: { Accept: 'application/json' }, signal },
      'Console summary request failed',
      {
        required: ['runner_sessions', 'workflow_runs', 'workflow_jobs'],
        numbers: ['runner_sessions', 'workflow_runs', 'workflow_jobs'],
      },
    ),
    requestJSON<RunsResponse>(
      '/api/runs?page_size=6',
      { headers: { Accept: 'application/json' }, signal },
      'Recent runs request failed',
      { required: ['runs'], arrays: ['runs'] },
    ),
  ])
  return { overview, runs: recent.runs ?? [] }
}

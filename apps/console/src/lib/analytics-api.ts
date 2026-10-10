import { requestJSON } from './console-fetch'

export interface AnalyticsRow {
  key: string
  repository: string
  workflow?: string
  total_jobs: number
  successful_jobs: number
  failed_jobs: number
  cancelled_jobs: number
  infrastructure_failures: number
  exact_attributions: number
  success_rate: number
  average_duration_seconds: number
  p50_duration_seconds: number
  p95_duration_seconds: number
}

export interface AnalyticsReport {
  generated_at: string
  group_by: 'repository' | 'workflow'
  since: string
  until: string
  rows: AnalyticsRow[]
}

export async function getAnalytics(
  groupBy: 'repository' | 'workflow',
  signal?: AbortSignal,
) {
  const parameters = new URLSearchParams({ group_by: groupBy })
  return requestJSON<AnalyticsReport>(
    `/api/v1/analytics?${parameters}`,
    { headers: { Accept: 'application/json' }, signal },
    'Analytics request failed',
    {
      required: ['generated_at', 'group_by', 'rows'],
      strings: ['generated_at', 'group_by'],
      arrays: ['rows'],
    },
  )
}

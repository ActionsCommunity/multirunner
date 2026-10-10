import { consoleFetch, responseError } from './console-fetch'

export async function getJobLog(jobID: number, signal?: AbortSignal) {
  const response = await consoleFetch(
    `/api/v1/jobs/${encodeURIComponent(jobID)}/log`,
    {
      headers: { Accept: 'text/plain' },
      cache: 'no-store',
      signal,
    },
  )
  if (!response.ok) {
    throw await responseError(response, 'Job log request failed')
  }
  return response.text()
}

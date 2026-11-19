import { consoleFetch, requestJSON, responseError } from './console-fetch'
import { assertMutationAllowed } from './offline-policy'

export interface SearchResult {
  entity_type:
    | 'run'
    | 'job'
    | 'step'
    | 'runner'
    | 'command'
    | 'alert'
    | 'annotation'
  entity_key: string
  repository?: string
  title: string
  context: string
  timestamp: string
  state: string
  route: string
}

export interface SavedView {
  id: string
  name: string
  query: string
  entity_type?: string
  repository?: string
  state?: string
  created_by: string
  created_at: string
  updated_at: string
  version: number
}

export interface SearchExport {
  id: string
  created_at: string
  expires_at: string
  file_name: string
  size: number
  sha256: string
  record_count: number
  download_url: string
}

interface SearchResponse {
  items: SearchResult[]
  count: number
  cursor: string
  next_cursor: string
  applied_filters: {
    type?: string
    repository?: string
    state?: string
  }
}

interface SavedViewsResponse {
  items: SavedView[]
  count: number
  applied_filters: Record<string, unknown>
  cursor: string
  next_cursor: string
}

export async function searchHistory(
  query: string,
  entityType: string,
  signal?: AbortSignal,
) {
  const parameters = new URLSearchParams({ q: query, limit: '50' })
  if (entityType) parameters.set('type', entityType)
  return requestJSON<SearchResponse>(
    `/api/v1/search?${parameters}`,
    { headers: { Accept: 'application/json' }, signal },
    'Search request failed',
    { required: ['items', 'count'], arrays: ['items'], numbers: ['count'] },
  )
}

export async function listSavedViews(signal?: AbortSignal) {
  const response = await requestJSON<SavedViewsResponse>(
    '/api/v1/saved-views',
    { signal },
    'Saved views request failed',
    { required: ['items', 'count'], arrays: ['items'], numbers: ['count'] },
  )
  return response
}

export async function createSavedView(
  input: {
    name: string
    query: string
    entity_type?: string
    repository?: string
    state?: string
  },
  idempotencyKey: string,
  signal?: AbortSignal,
) {
  assertMutationAllowed()
  const session = await getSession(signal)
  return requestJSON<SavedView>(
    '/api/v1/saved-views',
    {
      method: 'POST',
      headers: mutationHeaders(session.csrf_token, idempotencyKey),
      body: JSON.stringify(input),
      signal,
    },
    'Saved view creation failed',
    { required: ['id', 'name', 'query', 'version'] },
  )
}

export async function deleteSavedView(
  view: SavedView,
  idempotencyKey: string,
  signal?: AbortSignal,
) {
  assertMutationAllowed()
  const session = await getSession(signal)
  const response = await consoleFetch(
    `/api/v1/saved-views/${encodeURIComponent(view.id)}`,
    {
      method: 'DELETE',
      headers: {
        ...mutationHeaders(session.csrf_token, idempotencyKey),
        'If-Match': `"${view.version}"`,
      },
      signal,
    },
  )
  if (!response.ok) throw await responseError(response, 'Saved view deletion failed')
}

export async function createSearchExport(
  input: {
    query: string
    entity_type?: string
    repository?: string
    state?: string
  },
  idempotencyKey: string,
  signal?: AbortSignal,
) {
  assertMutationAllowed()
  const session = await getSession(signal)
  return requestJSON<SearchExport>(
    '/api/v1/exports',
    {
      method: 'POST',
      headers: mutationHeaders(session.csrf_token, idempotencyKey),
      body: JSON.stringify(input),
      signal,
    },
    'Search export creation failed',
    { required: ['id', 'created_at', 'expires_at', 'download_url'] },
  )
}

interface ConsoleSession {
  csrf_token: string
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

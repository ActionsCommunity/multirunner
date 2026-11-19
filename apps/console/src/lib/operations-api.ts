import { consoleFetch, parseJSON, requestJSON } from './console-fetch'

export interface OperationalEvent {
  id: string
  schema_version: number
  host_id: string
  host_epoch: string
  sequence: number
  type: string
  entity_type: string
  entity_id: string
  timestamp: string
  correlation_id: string
  causation_id: string
  actor_kind: string
  actor_id: string
  payload: Record<string, unknown>
  command_id: string
}

export interface RunnerState {
  id: string
  host_id: string
  host_epoch: string
  pool: string
  target: string
  repository: string
  runner_name: string
  status: string
  backend_id: string
  error: string
  last_event_id: string
  sequence: number
  updated_at: string
}

export interface CollectionResponse<T> {
  items: T[]
  applied_filters: Record<string, unknown>
  cursor?: string
  next_cursor?: string
  count: number
  host_epoch: string
  max_sequence: number
}

export type StreamState =
  | 'connecting'
  | 'replaying'
  | 'current'
  | 'disconnected'

export interface OperationalStreamHandlers {
  onOpen: () => void
  onReplayComplete: () => void
  onEvent: (event: OperationalEvent) => void
  onError: () => void
}

interface ClosableStream {
  close: () => void
}

interface OperationalStreamOptions {
  fetcher?: typeof consoleFetch
  reconnectDelay?: number
}

export async function getOperationalRunners(
  signal?: AbortSignal,
): Promise<CollectionResponse<RunnerState>> {
  return requestJSON<CollectionResponse<RunnerState>>(
    '/api/v1/runners?limit=100',
    { headers: { Accept: 'application/json' }, signal },
    'Runner state request failed',
    { required: ['items', 'count'], arrays: ['items'], numbers: ['count'] },
  )
}

export function openOperationalStream(
  handlers: OperationalStreamHandlers,
  options: OperationalStreamOptions = {},
): ClosableStream {
  const fetcher = options.fetcher ?? consoleFetch
  const controller = new AbortController()
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined
  let reconnectAttempts = 0
  let lastEventID = ''
  let closed = false

  const scheduleReconnect = () => {
    if (closed) return
    const base = options.reconnectDelay ?? 500
    const delay = Math.min(base * 2 ** reconnectAttempts, 10_000)
    reconnectAttempts += 1
    reconnectTimer = setTimeout(connect, delay)
  }

  const dispatch = (eventType: string, data: string, id: string) => {
    if (eventType === 'replay-complete') {
      handlers.onReplayComplete()
      return
    }
    if (eventType !== 'operational-event') return
    try {
      const event = parseJSON<OperationalEvent>(data, {
        required: ['id', 'sequence', 'type', 'timestamp'],
        strings: ['id', 'type', 'timestamp'],
        numbers: ['sequence'],
      })
      handlers.onEvent(event)
      if (id) lastEventID = id
    } catch {
      handlers.onError()
    }
  }

  const consume = async (response: Response) => {
    if (!response.body) throw new Error('Event stream body is unavailable.')
    const reader = response.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    let eventType = ''
    let eventID = ''
    let data: string[] = []
    const processLine = (line: string) => {
      if (line === '') {
        if (data.length) dispatch(eventType, data.join('\n'), eventID)
        eventType = ''
        eventID = ''
        data = []
        return
      }
      if (line.startsWith(':')) return
      const separator = line.indexOf(':')
      const field = separator < 0 ? line : line.slice(0, separator)
      let value = separator < 0 ? '' : line.slice(separator + 1)
      if (value.startsWith(' ')) value = value.slice(1)
      if (field === 'event') eventType = value
      if (field === 'id' && !value.includes('\0')) eventID = value
      if (field === 'data') data.push(value)
    }
    while (!closed) {
      const { done, value } = await reader.read()
      buffer += decoder.decode(value, { stream: !done })
      let newline = buffer.indexOf('\n')
      while (newline >= 0) {
        processLine(buffer.slice(0, newline).replace(/\r$/, ''))
        buffer = buffer.slice(newline + 1)
        newline = buffer.indexOf('\n')
      }
      if (done) {
        if (buffer) processLine(buffer.replace(/\r$/, ''))
        processLine('')
        return
      }
    }
  }

  const connect = async () => {
    if (closed) return
    const headers = new Headers({ Accept: 'text/event-stream' })
    if (lastEventID) headers.set('Last-Event-ID', lastEventID)
    try {
      const response = await fetcher('/api/v1/events', {
        headers,
        signal: controller.signal,
      })
      if (response.status === 401) {
        closed = true
        handlers.onError()
        return
      }
      if (!response.ok) throw new Error(`Event stream failed (${response.status})`)
      reconnectAttempts = 0
      handlers.onOpen()
      await consume(response)
      scheduleReconnect()
    } catch {
      if (closed || controller.signal.aborted) return
      handlers.onError()
      scheduleReconnect()
    }
  }

  void connect()
  return {
    close: () => {
      closed = true
      if (reconnectTimer !== undefined) clearTimeout(reconnectTimer)
      controller.abort()
    },
  }
}

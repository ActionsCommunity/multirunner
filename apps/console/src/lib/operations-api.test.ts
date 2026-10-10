import { expect, test, vi } from 'vitest'
import {
  getOperationalRunners,
  openOperationalStream,
  type OperationalEvent,
} from './operations-api'

const proof = 'a'.repeat(43)

function streamResponse(chunks: string[]) {
  const encoder = new TextEncoder()
  return new Response(
    new ReadableStream({
      start(controller) {
        for (const chunk of chunks) controller.enqueue(encoder.encode(chunk))
        controller.close()
      },
    }),
    { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
  )
}

test('opens the fetch stream and parses named events', async () => {
  const onOpen = vi.fn()
  const onReplayComplete = vi.fn()
  const onError = vi.fn()
  const onEvent = vi.fn<(event: OperationalEvent) => void>()
  const fetcher = vi.fn(() =>
    Promise.resolve(
      streamResponse([
        ': heartbeat\n\n',
        'id: epoch:1\nevent: operational-event\ndata: {"id":"epoch:1","host_epoch":"epoch","sequence":1,"type":"runner.planned","timestamp":"2026-10-10T00:00:00Z"}\n\n',
        'event: replay-complete\ndata: {"host_epoch":"epoch","sequence":1,"replayed":1}\n\n',
      ]),
    ),
  )
  const stream = openOperationalStream(
    { onOpen, onReplayComplete, onError, onEvent },
    { fetcher, reconnectDelay: 60_000 },
  )
  await vi.waitFor(() => expect(onEvent).toHaveBeenCalledOnce())
  expect(onOpen).toHaveBeenCalledOnce()
  expect(onReplayComplete).toHaveBeenCalledOnce()
  expect(onEvent).toHaveBeenCalledWith(
    expect.objectContaining({ id: 'epoch:1', sequence: 1 }),
  )
  expect(onError).not.toHaveBeenCalled()
  stream.close()
})

test('reconnects with the last dispatched event ID', async () => {
  vi.useFakeTimers()
  const fetcher = vi
    .fn()
    .mockResolvedValueOnce(
      streamResponse([
        'id: epoch:7\nevent: operational-event\ndata: {"id":"epoch:7","host_epoch":"epoch","sequence":7,"type":"runner.launched","timestamp":"2026-10-10T00:00:00Z"}\n\n',
      ]),
    )
    .mockImplementation(
      (_input: RequestInfo | URL, init?: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () =>
            reject(new DOMException('Aborted', 'AbortError')),
          )
        }),
    )
  const stream = openOperationalStream(
    {
      onOpen: vi.fn(),
      onReplayComplete: vi.fn(),
      onEvent: vi.fn(),
      onError: vi.fn(),
    },
    { fetcher, reconnectDelay: 1 },
  )
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledOnce())
  await vi.runOnlyPendingTimersAsync()
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2))
  const headers = new Headers(fetcher.mock.calls[1]?.[1]?.headers)
  expect(headers.get('Last-Event-ID')).toBe('epoch:7')
  stream.close()
  vi.useRealTimers()
})

test('closing the stream aborts the active request', async () => {
  let signal: AbortSignal | undefined
  const fetcher = vi.fn(
    (_input: RequestInfo | URL, init?: RequestInit) =>
      new Promise<Response>((_resolve, reject) => {
        signal = init?.signal ?? undefined
        signal?.addEventListener('abort', () =>
          reject(new DOMException('Aborted', 'AbortError')),
        )
      }),
  )
  const stream = openOperationalStream(
    {
      onOpen: vi.fn(),
      onReplayComplete: vi.fn(),
      onEvent: vi.fn(),
      onError: vi.fn(),
    },
    { fetcher },
  )
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledOnce())
  stream.close()
  expect(signal?.aborted).toBe(true)
})

test('loads runner state with the stored origin proof', async () => {
  window.sessionStorage.setItem('multirunner.console.origin-proof', proof)
  const fetchMock = vi.fn(
    (_input: RequestInfo | URL, _init?: RequestInit) =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            items: [{ id: 'runner-1', status: 'launched' }],
            applied_filters: {},
            count: 1,
            host_epoch: 'epoch',
            max_sequence: 3,
          }),
          { status: 200 },
        ),
      ),
  )
  vi.stubGlobal('fetch', fetchMock)
  const response = await getOperationalRunners()
  const headers = new Headers(fetchMock.mock.calls[0]?.[1]?.headers)
  expect(headers.get('X-Multirunner-Console-Proof')).toBe(proof)
  expect(response.items[0]).toEqual(
    expect.objectContaining({ id: 'runner-1', status: 'launched' }),
  )
  vi.unstubAllGlobals()
  window.sessionStorage.clear()
})

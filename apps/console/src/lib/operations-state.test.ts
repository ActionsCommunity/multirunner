import { afterEach, expect, test, vi } from 'vitest'
import { createEventBatcher } from './event-batcher'
import type { OperationalEvent, RunnerState } from './operations-api'
import { applyOperationalEventBatch } from './operations-state'

function event(sequence: number): OperationalEvent {
  const runner = sequence % 25
  return {
    id: `epoch:${sequence}`,
    schema_version: 1,
    host_id: 'host',
    host_epoch: 'epoch',
    sequence,
    type: sequence % 2 ? 'runner.launched' : 'runner.stopped',
    entity_type: 'runner_session',
    entity_id: `runner-${runner}`,
    timestamp: `2026-10-09T12:${String(sequence % 60).padStart(2, '0')}:00Z`,
    correlation_id: '',
    causation_id: '',
    actor_kind: 'system',
    actor_id: 'multirunner',
    payload: {
      pool: `pool-${runner % 4}`,
      runner_name: `runner-${runner}`,
    },
    command_id: '',
  }
}

afterEach(() => {
  vi.useRealTimers()
})

test('coalesces an event burst into one bounded projection update', () => {
  vi.useFakeTimers()
  const flush = vi.fn()
  const batcher = createEventBatcher<OperationalEvent>(flush, 50)
  const events = Array.from({ length: 1000 }, (_, index) => event(index + 1))

  for (const item of events) batcher.push(item)
  expect(flush).not.toHaveBeenCalled()

  vi.advanceTimersByTime(50)
  expect(flush).toHaveBeenCalledOnce()
  expect(flush).toHaveBeenCalledWith(events)

  const next = applyOperationalEventBatch(
    {
      runners: [] as RunnerState[],
      events: [] as OperationalEvent[],
    },
    flush.mock.calls[0][0],
  )
  expect(next.events).toHaveLength(50)
  expect(next.events[0].sequence).toBe(1000)
  expect(next.runners).toHaveLength(25)
  expect(next.runners[0].sequence).toBe(1000)
  expect(next.runners[0].status).toBe('stopped')
})

test('preserves the runner projection reference for unrelated event batches', () => {
  const runners = [
    {
      id: 'runner-1',
      host_id: 'host',
      host_epoch: 'epoch',
      pool: 'linux',
      target: 'local',
      repository: 'actionscommunity/multirunner',
      runner_name: 'runner-1',
      status: 'launched',
      backend_id: 'vm-1',
      error: '',
      last_event_id: 'epoch:1',
      sequence: 1,
      updated_at: '2026-10-09T12:00:00Z',
    },
  ]
  const unrelated = {
    ...event(2),
    type: 'command.succeeded',
    entity_type: 'command',
    entity_id: 'command-1',
  }

  const next = applyOperationalEventBatch(
    { runners, events: [] as OperationalEvent[] },
    [unrelated],
  )

  expect(next.runners).toBe(runners)
  expect(next.events).toEqual([unrelated])
})

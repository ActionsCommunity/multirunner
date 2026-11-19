import type { OperationalEvent, RunnerState } from './operations-api'

export interface OperationsState {
  runners: RunnerState[]
  events: OperationalEvent[]
}

export function runnerFromEvent(
  current: RunnerState | undefined,
  event: OperationalEvent,
): RunnerState {
  const payload = event.payload
  const value = (key: string) =>
    typeof payload[key] === 'string' ? payload[key] : ''
  return {
    id: event.entity_id,
    host_id: event.host_id,
    host_epoch: event.host_epoch,
    pool: value('pool') || current?.pool || '',
    target: value('target') || current?.target || '',
    repository: value('repository') || current?.repository || '',
    runner_name: value('runner_name') || current?.runner_name || '',
    status: event.type.replace(/^runner\./, ''),
    backend_id: value('backend_instance_id') || current?.backend_id || '',
    error: value('error'),
    last_event_id: event.id,
    sequence: event.sequence,
    updated_at: event.timestamp,
  }
}

export function applyOperationalEventBatch<T extends OperationsState>(
  current: T,
  incoming: OperationalEvent[],
): T {
  if (!incoming.length) return current

  const runnersByID = new Map(
    current.runners.map((runner) => [runner.id, runner]),
  )
  const touchedRunnerIDs: string[] = []
  const touchedRunnerSet = new Set<string>()

  for (const event of incoming) {
    if (event.entity_type !== 'runner_session') continue
    runnersByID.set(
      event.entity_id,
      runnerFromEvent(runnersByID.get(event.entity_id), event),
    )
    if (touchedRunnerSet.has(event.entity_id)) {
      touchedRunnerIDs.splice(touchedRunnerIDs.indexOf(event.entity_id), 1)
    } else {
      touchedRunnerSet.add(event.entity_id)
    }
    touchedRunnerIDs.push(event.entity_id)
  }

  const runners = touchedRunnerIDs.length
    ? touchedRunnerIDs
        .toReversed()
        .map((id) => runnersByID.get(id)!)
        .concat(
          current.runners.filter(
            (runner) => !touchedRunnerSet.has(runner.id),
          ),
        )
    : current.runners

  return {
    ...current,
    events: incoming.toReversed().concat(current.events).slice(0, 50),
    runners,
  }
}

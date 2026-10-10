import { memo, useEffect, useState } from 'react'
import type {
  OperationalEvent,
  RunnerState,
  StreamState,
} from '../lib/operations-api'

interface LiveEventLedgerProps {
  events: OperationalEvent[]
  streamStatus: StreamState
  streamLabel: string
  eventLabel: (value: string) => string
  formatRelativeTime: (value: string) => string
}

export const LiveEventLedger = memo(function LiveEventLedger({
  events,
  streamStatus,
  streamLabel,
  eventLabel,
  formatRelativeTime,
}: LiveEventLedgerProps) {
  const [announcement, setAnnouncement] = useState('')
  const latestEvent = events[0]

  useEffect(() => {
    if (!latestEvent) return
    const timer = window.setTimeout(() => {
      setAnnouncement(
        `${events.length.toLocaleString()} recent operational events. Latest: ${eventLabel(
          latestEvent.type,
        )}, sequence ${latestEvent.sequence.toLocaleString()}.`,
      )
    }, 350)
    return () => window.clearTimeout(timer)
  }, [eventLabel, events.length, latestEvent])

  return (
    <section className="panel operations-module" aria-labelledby="live-title">
      <div className="panel-heading">
        <div>
          <span className="section-label">Ordered host sequence</span>
          <h2 id="live-title">Live event ledger</h2>
        </div>
        <span className={`connection-badge ${streamStatus}`}>
          <span className="status-dot" aria-hidden="true" />
          {streamLabel}
        </span>
      </div>
      <p
        className="sr-only"
        role="status"
        aria-live="polite"
        aria-atomic="true"
      >
        {announcement}
      </p>
      {events.length ? (
        <ol className="event-ledger">
          {events.map((event) => (
            <li key={event.id}>
              <time dateTime={event.timestamp}>
                {formatRelativeTime(event.timestamp)}
              </time>
              <div>
                <strong>{eventLabel(event.type)}</strong>
                <span>
                  {event.entity_type} · {event.entity_id}
                </span>
              </div>
              <code>#{event.sequence}</code>
            </li>
          ))}
        </ol>
      ) : (
        <p className="empty-state">
          Waiting for the authenticated operational stream.
        </p>
      )}
    </section>
  )
})

interface RunnerSessionsProps {
  runners: RunnerState[]
  loading: boolean
  error?: string
  isOnline: boolean
  onTerminate: (runner: RunnerState) => void
  eventLabel: (value: string) => string
  formatRelativeTime: (value: string) => string
}

export const RunnerSessions = memo(function RunnerSessions({
  runners,
  loading,
  error,
  isOnline,
  onTerminate,
  eventLabel,
  formatRelativeTime,
}: RunnerSessionsProps) {
  return (
    <section
      className="panel operations-module"
      aria-labelledby="runners-title"
    >
      <div className="panel-heading">
        <div>
          <span className="section-label">Lifecycle projection</span>
          <h2 id="runners-title">Runner sessions</h2>
        </div>
        <span className="collection-count">
          {runners.length.toLocaleString()} loaded
        </span>
      </div>
      {loading ? (
        <p className="empty-state" role="status">Loading runner state…</p>
      ) : error ? (
        <p className="empty-state error-state" role="alert">{error}</p>
      ) : runners.length ? (
        <div className="table-scroll">
          <table className="mobile-summary-table runners-table">
            <caption className="sr-only">Runner sessions</caption>
            <thead>
              <tr>
                <th scope="col">Runner</th>
                <th scope="col">Pool</th>
                <th scope="col">Repository</th>
                <th scope="col">State</th>
                <th scope="col">Updated</th>
                <th scope="col">Control</th>
              </tr>
            </thead>
            <tbody>
              {runners.map((runner) => (
                <tr key={runner.id}>
                  <td>
                    <strong>{runner.runner_name || runner.id}</strong>
                    <small>{runner.backend_id || runner.target}</small>
                  </td>
                  <td>{runner.pool || '—'}</td>
                  <td>{runner.repository || '—'}</td>
                  <td>
                    <span className={`pill ${runner.status}`}>
                      {eventLabel(runner.status)}
                    </span>
                    {runner.error ? (
                      <small className="runner-error">{runner.error}</small>
                    ) : null}
                  </td>
                  <td>{formatRelativeTime(runner.updated_at)}</td>
                  <td>
                    <button
                      className="danger-link"
                      type="button"
                      disabled={
                        !isOnline ||
                        !runner.pool ||
                        ['stopped', 'failed'].includes(runner.status)
                      }
                      onClick={() => onTerminate(runner)}
                    >
                      Terminate
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="empty-state">
          No runner lifecycle state has been recorded in this host epoch.
        </p>
      )}
    </section>
  )
})

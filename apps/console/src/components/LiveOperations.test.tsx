import { act, cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { LiveEventLedger, RunnerSessions } from './LiveOperations'
import type { OperationalEvent, RunnerState } from '../lib/operations-api'

const sampleEvent: OperationalEvent = {
  id: 'evt-101',
  sequence: 101,
  epoch: 'epoch-1',
  timestamp: '2026-11-17T12:00:00Z',
  type: 'runner.started',
  entity_type: 'runner',
  entity_id: 'runner-1',
  summary: 'Runner started',
}

const sampleRunner: RunnerState = {
  id: 'runner-1',
  runner_name: 'win-runner-01',
  pool: 'windows-pool',
  repository: 'ActionsCommunity/multirunner',
  target: 'https://github.com/ActionsCommunity/multirunner',
  backend_id: 'host-win-01',
  status: 'running',
  updated_at: '2026-11-17T12:00:00Z',
  sequence: 101,
}

describe('LiveOperations components', () => {
  afterEach(() => {
    cleanup()
    vi.useRealTimers()
  })

  it('renders empty and populated LiveEventLedger with debounced screen-reader announcement', () => {
    vi.useFakeTimers()
    const eventLabel = (val: string) => val.replace('.', ' ')
    const formatRelativeTime = () => 'just now'

    const { rerender } = render(
      <LiveEventLedger
        events={[]}
        streamStatus="connecting"
        streamLabel="Connecting"
        eventLabel={eventLabel}
        formatRelativeTime={formatRelativeTime}
      />,
    )

    expect(
      screen.getByText('Waiting for the authenticated operational stream.'),
    ).toBeInTheDocument()

    rerender(
      <LiveEventLedger
        events={[sampleEvent]}
        streamStatus="connected"
        streamLabel="Live"
        eventLabel={eventLabel}
        formatRelativeTime={formatRelativeTime}
      />,
    )

    expect(screen.getByText('runner started')).toBeInTheDocument()
    expect(screen.getByText('#101')).toBeInTheDocument()

    act(() => {
      vi.advanceTimersByTime(400)
    })

    expect(screen.getByRole('status')).toHaveTextContent(
      /1 recent operational events\. Latest: runner started, sequence 101\./i,
    )
  })

  it('renders RunnerSessions loading, error, empty, and interactive rows with termination guards', async () => {
    const user = userEvent.setup()
    const onTerminate = vi.fn()
    const eventLabel = (val: string) => val.toUpperCase()
    const formatRelativeTime = () => '1m ago'

    const { rerender } = render(
      <RunnerSessions
        runners={[]}
        loading={true}
        isOnline={true}
        onTerminate={onTerminate}
        eventLabel={eventLabel}
        formatRelativeTime={formatRelativeTime}
      />,
    )
    expect(screen.getByRole('status')).toHaveTextContent('Loading runner state…')

    rerender(
      <RunnerSessions
        runners={[]}
        loading={false}
        error="Stream disconnected"
        isOnline={true}
        onTerminate={onTerminate}
        eventLabel={eventLabel}
        formatRelativeTime={formatRelativeTime}
      />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent('Stream disconnected')

    rerender(
      <RunnerSessions
        runners={[]}
        loading={false}
        isOnline={true}
        onTerminate={onTerminate}
        eventLabel={eventLabel}
        formatRelativeTime={formatRelativeTime}
      />,
    )
    expect(
      screen.getByText(
        'No runner lifecycle state has been recorded in this host epoch.',
      ),
    ).toBeInTheDocument()

    const stoppedRunner: RunnerState = {
      ...sampleRunner,
      id: 'runner-2',
      runner_name: '',
      backend_id: '',
      pool: '',
      repository: '',
      status: 'failed',
      error: 'Exit code 1',
    }

    rerender(
      <RunnerSessions
        runners={[sampleRunner, stoppedRunner]}
        loading={false}
        isOnline={true}
        onTerminate={onTerminate}
        eventLabel={eventLabel}
        formatRelativeTime={formatRelativeTime}
      />,
    )

    expect(screen.getByText('win-runner-01')).toBeInTheDocument()
    expect(screen.getByText('Exit code 1')).toBeInTheDocument()

    const buttons = screen.getAllByRole('button', { name: 'Terminate' })
    expect(buttons[0]).toBeEnabled()
    expect(buttons[1]).toBeDisabled()

    await user.click(buttons[0])
    expect(onTerminate).toHaveBeenCalledWith(sampleRunner)
  })
})

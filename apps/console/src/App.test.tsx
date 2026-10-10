import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import axe from 'axe-core'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import App from './App'

const overview = {
  runner_sessions: 200,
  pending_sessions: 2,
  workflow_runs: 180,
  workflow_jobs: 210,
  successful_jobs: 190,
  failed_jobs: 10,
  cancelled_jobs: 10,
  average_duration_seconds: 92,
}

beforeEach(() => {
  window.localStorage.clear()
  window.sessionStorage.clear()
  Object.defineProperty(window.navigator, 'onLine', {
    configurable: true,
    value: true,
  })
  window.history.replaceState(null, '', '/')
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      const body =
        typeof init?.body === 'string'
          ? (JSON.parse(init.body) as { type?: string })
          : undefined
      const supportBundle = body?.type === 'support_bundle.generate'
      const backupCommand = body?.type === 'backup.create'
      const restoreCommand = body?.type === 'restore.stage'
      const updateCommand = body?.type === 'update.stage'
      const alert = {
        id: 'alert-1',
        rule_id: 'runner-failed',
        dedup_key: 'session-1',
        state: 'open',
        severity: 'high',
        summary: 'Runner provisioning failed',
        details: { pool: 'linux' },
        source_event_id: 'epoch:12',
        first_observed_at: new Date().toISOString(),
        last_observed_at: new Date().toISOString(),
        due_at: new Date().toISOString(),
        opened_at: new Date().toISOString(),
        occurrence_count: 2,
        version: 1,
      }
      if (url.includes('/api/v1/alerts?') && method === 'GET') {
        return Promise.resolve(
          new Response(JSON.stringify({ items: [alert], count: 1 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        )
      }
      if (url.includes('/api/v1/backups?') && method === 'GET') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              items: [
                {
                  id: 'backup-1',
                  command_id: 'backup-command',
                  purpose: 'manual',
                  state: 'succeeded',
                  created_at: new Date().toISOString(),
                  expires_at: new Date(Date.now() + 86_400_000).toISOString(),
                  size_bytes: 4096,
                  sha256: 'abcdef1234567890',
                  schema_version: 11,
                  quick_check: 'ok',
                  download_url: '/api/v1/backups/backup-1/download',
                },
              ],
              count: 1,
            }),
            {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            },
          ),
        )
      }
      if (url.includes('/api/v1/restores?') && method === 'GET') {
        return Promise.resolve(
          new Response(JSON.stringify({ items: [], count: 0 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        )
      }
      if (url.includes('/api/v1/updates?') && method === 'GET') {
        return Promise.resolve(
          new Response(JSON.stringify({ items: [], count: 0 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        )
      }
      if (url.endsWith('/api/v1/updates/check') && method === 'GET') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              checked_at: new Date().toISOString(),
              trust_configured: true,
              verified: true,
              metadata_consistent: true,
              compatible: true,
              apply_allowed: true,
              version: 'v1.2.0',
              commit: '0123456789abcdef0123456789abcdef01234567',
              target_path: 'multirunner_v1.2.0_windows_amd64.exe',
              size_bytes: 8192,
              schema_min: 12,
              schema_max: 13,
              api_version: 'v1',
            }),
            {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            },
          ),
        )
      }
      if (url.endsWith('/api/v1/alerts/alert-1/acknowledge')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              ...alert,
              acknowledged_at: new Date().toISOString(),
              acknowledged_by: 'local-test',
              version: 2,
            }),
            {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            },
          ),
        )
      }
      if (url.endsWith('/api/v1/alerts/alert-1/annotations')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: 'annotation-1',
              alert_id: 'alert-1',
              body: 'Investigating backend health.',
              created_at: new Date().toISOString(),
              created_by: 'local-test',
            }),
            {
              status: 201,
              headers: { 'Content-Type': 'application/json' },
            },
          ),
        )
      }
      if (url.endsWith('/api/v1/saved-views') && method === 'GET') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              items: [
                {
                  id: 'saved-release',
                  name: 'Release failures',
                  query: 'release failure',
                  entity_type: 'run',
                  created_by: 'local-test',
                  created_at: new Date().toISOString(),
                  updated_at: new Date().toISOString(),
                  version: 1,
                },
              ],
              count: 1,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          ),
        )
      }
      if (url.endsWith('/api/v1/saved-views') && method === 'POST') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: 'saved-console',
              name: 'Console foundation',
              query: 'console foundation',
              entity_type: 'run',
              created_by: 'local-test',
              created_at: new Date().toISOString(),
              updated_at: new Date().toISOString(),
              version: 1,
            }),
            { status: 201, headers: { 'Content-Type': 'application/json' } },
          ),
        )
      }
      if (
        url.includes('/api/v1/saved-views/') &&
        method === 'DELETE'
      ) {
        return Promise.resolve(new Response(null, { status: 204 }))
      }
      if (url.endsWith('/api/v1/exports') && method === 'POST') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: 'export-1',
              created_at: new Date().toISOString(),
              expires_at: new Date(Date.now() + 7 * 86400000).toISOString(),
              file_name: 'multirunner-search-export-1.jsonl',
              size: 256,
              sha256: 'abc123',
              record_count: 1,
              download_url: '/api/v1/exports/export-1',
            }),
            { status: 201, headers: { 'Content-Type': 'application/json' } },
          ),
        )
      }
      if (url.includes('/api/v1/jobs/84/log')) {
        return Promise.resolve(
          new Response(
            `${String.fromCharCode(27)}[31mRun tests failed${String.fromCharCode(27)}[0m\nmasked ***\n`,
            {
              status: 200,
              headers: {
                'Content-Type': 'text/plain; charset=utf-8',
                'Cache-Control': 'no-store',
              },
            },
          ),
        )
      }
      if (url.includes('/api/v1/analytics')) {
        const workflow = url.includes('group_by=workflow')
        return Promise.resolve(
          new Response(
            JSON.stringify({
              generated_at: new Date().toISOString(),
              group_by: workflow ? 'workflow' : 'repository',
              since: new Date(Date.now() - 30 * 86400000).toISOString(),
              until: new Date().toISOString(),
              rows: [
                {
                  key: workflow
                    ? 'actionscommunity/multirunner\u0000CI'
                    : 'actionscommunity/multirunner',
                  repository: 'actionscommunity/multirunner',
                  workflow: workflow ? 'CI' : undefined,
                  total_jobs: 20,
                  successful_jobs: 18,
                  failed_jobs: 2,
                  cancelled_jobs: 0,
                  infrastructure_failures: 1,
                  exact_attributions: 19,
                  success_rate: 0.9,
                  average_duration_seconds: 45,
                  p50_duration_seconds: 40,
                  p95_duration_seconds: 80,
                },
              ],
            }),
            {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            },
          ),
        )
      }
      if (url.endsWith('/api/v1/session')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              actor_id: 'local-test',
              csrf_token: 'csrf-test',
              issued_at: new Date(Date.now() - 60_000).toISOString(),
              paired_at: new Date(Date.now() - 60_000).toISOString(),
              revocation_generation: 1,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          ),
        )
      }
      if (url.includes('/api/v1/pools')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              items: [
                { name: 'linux', count: 120 },
                { name: 'windows', count: 80 },
              ],
              count: 2,
              applied_filters: {},
              cursor: '',
              next_cursor: '',
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          ),
        )
      }
      if (url.endsWith('/api/v1/system')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              status: 'operational',
              mode: 'local',
              listen: '127.0.0.1:9092',
              database: 'C:\\ProgramData\\multirunner\\history.db',
              started_at: new Date(Date.now() - 3_600_000).toISOString(),
              app_version: 'v1.2.0',
              api_version: 'v1',
              configuration: 'read_only',
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          ),
        )
      }
      if (url.includes('/api/v1/audit')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              items: [
                {
                  id: 'audit-1',
                  source: 'command',
                  occurred_at: new Date().toISOString(),
                  actor_kind: 'operator',
                  actor_id: 'local-test',
                  action: 'backup.create',
                  target_type: 'system',
                  target_id: 'backups',
                  correlation_id: 'correlation-1',
                  outcome: 'succeeded',
                },
              ],
              count: 1,
              applied_filters: {},
              cursor: '',
              next_cursor: '',
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          ),
        )
      }
      return Promise.resolve(
        new Response(
          JSON.stringify(
            url.includes('/api/v1/session')
              ? {
                  actor_id: 'local-test',
                  csrf_token: 'csrf-test',
                }
              : url.includes('/api/v1/commands/preview')
                ? updateCommand
                  ? {
                      conflict_domain: 'host-maintenance',
                      confirmation_phrase: 'stage trusted update v1.2.0',
                      reason_required: true,
                      impact:
                        'Download and stage the target only after threshold signatures, provenance, compatibility, and downgrade policy pass.',
                    }
                  : restoreCommand
                  ? {
                      conflict_domain: 'database-maintenance',
                      confirmation_phrase: 'stage restore backup-1',
                      reason_required: true,
                      impact:
                        'Stage a verified backup for activation on the next service restart. Startup automatically rolls back if the restored runtime does not become healthy.',
                    }
                  : backupCommand
                  ? {
                      conflict_domain: 'database-maintenance',
                      confirmation_phrase: 'create verified backup',
                      reason_required: true,
                      impact:
                        'Create and integrity-check an online SQLite backup without stopping runner provisioning.',
                    }
                  : supportBundle
                  ? {
                      conflict_domain: 'support-bundles',
                      confirmation_phrase: 'generate support bundle',
                      reason_required: true,
                      impact:
                        'Create a redacted local archive of diagnostics and bounded operational events.',
                    }
                  : {
                      conflict_domain: 'pool:linux',
                      confirmation_phrase: 'terminate session-1',
                      reason_required: true,
                      impact:
                        'Stop this active runner through its owned cleanup path; its current job may be interrupted.',
                    }
                : url.endsWith('/api/v1/commands') && method === 'POST'
                  ? updateCommand
                    ? {
                        id: 'update-command',
                        type: 'update.stage',
                        target_type: 'system',
                        target_id: 'updates',
                        state: 'queued',
                        reason: 'Apply security release',
                        created_at: new Date().toISOString(),
                      }
                    : restoreCommand
                    ? {
                        id: 'restore-command',
                        type: 'restore.stage',
                        target_type: 'system',
                        target_id: 'restore',
                        state: 'queued',
                        reason: 'Recover history',
                        created_at: new Date().toISOString(),
                      }
                    : backupCommand
                    ? {
                        id: 'backup-command',
                        type: 'backup.create',
                        target_type: 'system',
                        target_id: 'backups',
                        state: 'queued',
                        reason: 'Before maintenance',
                        created_at: new Date().toISOString(),
                      }
                    : supportBundle
                    ? {
                        id: 'bundle-command',
                        type: 'support_bundle.generate',
                        target_type: 'system',
                        target_id: 'support-bundles',
                        state: 'queued',
                        reason: 'Investigate host health',
                        created_at: new Date().toISOString(),
                      }
                    : {
                        id: 'command-1',
                        type: 'runner.terminate',
                        target_type: 'runner',
                        target_id: 'session-1',
                        state: 'queued',
                        reason: 'Runner is stuck',
                        created_at: new Date().toISOString(),
                      }
                  : url.includes('/api/v1/commands/update-command')
                    ? {
                        id: 'update-command',
                        type: 'update.stage',
                        target_type: 'system',
                        target_id: 'updates',
                        state: 'succeeded',
                        reason: 'Apply security release',
                        outcome: {
                          version: 'v1.2.0',
                          state: 'staged',
                        },
                        created_at: new Date().toISOString(),
                        completed_at: new Date().toISOString(),
                      }
                  : url.includes('/api/v1/commands/restore-command')
                    ? {
                        id: 'restore-command',
                        type: 'restore.stage',
                        target_type: 'system',
                        target_id: 'restore',
                        state: 'succeeded',
                        reason: 'Recover history',
                        outcome: {
                          backup_id: 'backup-1',
                          state: 'staged',
                        },
                        created_at: new Date().toISOString(),
                        completed_at: new Date().toISOString(),
                      }
                  : url.includes('/api/v1/commands/backup-command')
                    ? {
                        id: 'backup-command',
                        type: 'backup.create',
                        target_type: 'system',
                        target_id: 'backups',
                        state: 'succeeded',
                        reason: 'Before maintenance',
                        outcome: {
                          download_url:
                            '/api/v1/backups/backup-command/download',
                        },
                        created_at: new Date().toISOString(),
                        completed_at: new Date().toISOString(),
                      }
                  : url.includes('/api/v1/commands/bundle-command')
                    ? {
                        id: 'bundle-command',
                        type: 'support_bundle.generate',
                        target_type: 'system',
                        target_id: 'support-bundles',
                        state: 'succeeded',
                        reason: 'Investigate host health',
                        outcome: {
                          download_url:
                            '/api/v1/support-bundles/bundle-command',
                        },
                        created_at: new Date().toISOString(),
                        completed_at: new Date().toISOString(),
                      }
                  : url.includes('/api/v1/commands/command-1')
                    ? {
                        id: 'command-1',
                        type: 'runner.terminate',
                        target_type: 'runner',
                        target_id: 'session-1',
                        state: 'succeeded',
                        reason: 'Runner is stuck',
                        created_at: new Date().toISOString(),
                        completed_at: new Date().toISOString(),
                      }
                    : url.includes('/api/v1/diagnostics')
                      ? {
                          generated_at: new Date().toISOString(),
                          status: 'pass',
                          results: [
                            {
                              id: 'history.database',
                              version: 1,
                              category: 'storage',
                              severity: 'high',
                              status: 'pass',
                              observed_at: new Date().toISOString(),
                              duration_ms: 4,
                              observed: 'ok',
                              expected: 'ok',
                            },
                          ],
                        }
                      : url.includes('/api/v1/configuration')
                        ? {
                            source_file: 'C:\\multirunner\\config.yaml',
                            loaded_at: new Date().toISOString(),
                            inspected_at: new Date().toISOString(),
                            startup_sha256: 'a',
                            current_sha256: 'a',
                            drifted: false,
                            changed_paths: [],
                            raw_redacted: 'auth:\\n  pat: <redacted>\\n',
                            normalized_redacted: 'history:\\n  enabled: true\\n',
                            fields: [
                              {
                                path: 'history.enabled',
                                effective: true,
                                source: 'file',
                                secret: false,
                                restart_required: true,
                              },
                            ],
                            guidance: 'Edit the source YAML, then restart.',
                          }
                        : url.includes('/api/v1/search')
                          ? {
                              items: [
                                {
                                  entity_type: 'run',
                                  entity_key:
                                    'actionscommunity/multirunner:42',
                                  repository:
                                    'actionscommunity/multirunner',
                                  title: 'CI Verify console foundation',
                                  context: 'main abc123 octocat',
                                  timestamp: new Date().toISOString(),
                                  state: 'success',
                                  route:
                                    '/runs?repository=actionscommunity%2Fmultirunner&run_id=42',
                                },
                              ],
                              count: 1,
                              applied_filters: { type: 'run' },
                            }
                          : url.includes('/api/v1/runs/42')
                            ? {
                                run: {
                                  id: 42,
                                  repository:
                                    'actionscommunity/multirunner',
                                  name: 'CI',
                                  workflow_name: 'CI',
                                  display_title:
                                    'Verify console foundation',
                                  event: 'pull_request',
                                  status: 'completed',
                                  conclusion: 'failure',
                                  head_branch: 'feature/console',
                                  head_sha: 'abc123def456',
                                  actor: 'octocat',
                                  triggering_actor: 'octocat',
                                  html_url:
                                    'https://github.com/actionscommunity/multirunner/actions/runs/42',
                                  run_number: 21,
                                  run_attempt: 1,
                                  created_at: new Date().toISOString(),
                                },
                                jobs: [
                                  {
                                    id: 84,
                                    run_id: 42,
                                    run_attempt: 1,
                                    name: 'integration',
                                    workflow_name: 'CI',
                                    status: 'completed',
                                    conclusion: 'failure',
                                    runner_name: 'multirunner-linux-1',
                                    pool_name: 'linux',
                                    local_session_id: 'session-1',
                                    attribution_source: 'exact',
                                    attribution_confidence: 100,
                                    html_url: '',
                                    created_at: new Date().toISOString(),
                                    steps: [
                                      {
                                        number: 1,
                                        name: 'Run tests',
                                        status: 'completed',
                                        conclusion: 'failure',
                                      },
                                    ],
                                  },
                                ],
                                runner_sessions: [
                                  {
                                    id: 'session-1',
                                    runner_name: 'multirunner-linux-1',
                                    pool_name: 'linux',
                                    target: 'local',
                                    status: 'stopped',
                                    conclusion: 'failure',
                                    backend_id: 'vm-1',
                                    attribution_source: 'exact',
                                    attribution_confidence: 100,
                                    planned_at: new Date().toISOString(),
                                    started_at: new Date().toISOString(),
                                  },
                                ],
                              }
                        : url.includes('/api/v1/runners')
              ? {
                  items: [
                    {
                      id: 'session-1',
                      host_id: 'host',
                      host_epoch: 'epoch',
                      pool: 'linux',
                      target: 'local',
                      repository: 'actionscommunity/multirunner',
                      runner_name: 'multirunner-linux-1',
                      status: 'launched',
                      backend_id: 'vm-1',
                      error: '',
                      last_event_id: 'epoch:2',
                      sequence: 2,
                      updated_at: new Date().toISOString(),
                    },
                  ],
                  applied_filters: {},
                  count: 1,
                  host_epoch: 'epoch',
                  max_sequence: 2,
                }
              : url.includes('/api/runs')
              ? {
                  runs: [
                    {
                      id: 42,
                      repository: 'actionscommunity/multirunner',
                      name: 'test',
                      workflow_name: 'CI',
                      display_title: 'Verify console foundation',
                      status: 'completed',
                      conclusion: 'success',
                      html_url:
                        'https://github.com/actionscommunity/multirunner/actions/runs/42',
                      run_attempt: 1,
                      created_at: new Date().toISOString(),
                    },
                  ],
                }
              : url.includes('/api/summary')
              ? overview
              : (() => {
                  throw new Error(`Unhandled test route: ${method} ${url}`)
                })(),
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
      )
    }),
  )
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('renders live overview data', async () => {
  render(<App />)
  expect(await screen.findByText('210')).toBeInTheDocument()
  expect(screen.getByText('CI')).toBeInTheDocument()
  expect(screen.getByText('90%')).toBeInTheDocument()
})

test('renders a distinct visible icon for every navigation destination', async () => {
  render(<App />)
  await screen.findByText('CI')

  const icons = screen.getAllByTestId('navigation-icon')
  expect(icons).toHaveLength(13)
  expect(new Set(icons.map((icon) => icon.getAttribute('data-icon'))).size).toBe(
    13,
  )
  for (const icon of icons) {
    expect(icon.querySelector('path, circle, rect, line, polyline')).not.toBeNull()
  }
})

test('does not report host degradation when console data is unavailable', async () => {
  vi.mocked(fetch).mockRejectedValue(new Error('console API unavailable'))

  render(<App />)

  expect(
    await screen.findByRole('heading', {
      name: 'Console data unavailable',
      level: 2,
    }),
  ).toBeInTheDocument()
  expect(screen.queryByText('Degraded read-only')).not.toBeInTheDocument()
})

test('renders versioned runner projection', async () => {
  render(<App />)
  await screen.findByText('CI')
  fireEvent.click(screen.getByRole('link', { name: 'Runners' }))
  expect(
    await screen.findByText('multirunner-linux-1'),
  ).toBeInTheDocument()
  expect(screen.getByText('linux')).toBeInTheDocument()
  expect(screen.getByText('launched')).toBeInTheDocument()
})

test('renders pools, host, and audit resources with honest scope', async () => {
  render(<App />)
  await screen.findByText('CI')

  fireEvent.click(screen.getByRole('link', { name: 'Pools' }))
  expect(await screen.findByText('120')).toBeInTheDocument()
  expect(
    screen.getByText(/does not report configured capacity/i),
  ).toBeInTheDocument()

  fireEvent.click(screen.getByRole('link', { name: 'Host' }))
  expect(await screen.findByText('v1.2.0')).toBeInTheDocument()
  expect(screen.getByText('127.0.0.1:9092')).toBeInTheDocument()

  fireEvent.click(screen.getByRole('link', { name: 'Audit' }))
  expect(await screen.findByText('backup create')).toBeInTheDocument()
  expect(screen.getByText('correlation-1')).toBeInTheDocument()
})

test('searches durable execution history from the Runs workspace', async () => {
  render(<App />)
  await screen.findByText('CI')
  fireEvent.click(screen.getByRole('link', { name: 'Runs' }))
  fireEvent.change(screen.getByLabelText('Search history'), {
    target: { value: 'console foundation' },
  })

  fireEvent.change(screen.getByLabelText('Record type'), {
    target: { value: 'run' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Search' }))
  expect(
    await screen.findByRole('link', {
      name: 'CI Verify console foundation',
    }),
  ).toHaveAttribute(
    'href',
    '/runs?repository=actionscommunity%2Fmultirunner&run_id=42',
  )
})

test('operates durable alerts from the Alerts workspace', async () => {
  render(<App />)
  fireEvent.click(screen.getByRole('link', { name: 'Alerts' }))

  expect(
    await screen.findByRole('heading', { name: 'Alerts', level: 2 }),
  ).toBeInTheDocument()
  expect(
    (await screen.findAllByText('Runner provisioning failed')).length,
  ).toBeGreaterThan(0)

  fireEvent.change(screen.getByLabelText('Operator reason'), {
    target: { value: 'Investigating runner provisioning' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Acknowledge' }))
  await waitFor(() => {
    expect(screen.getByText('local-test')).toBeInTheDocument()
  })

  fireEvent.change(screen.getByLabelText('Annotation'), {
    target: { value: 'Investigating backend health.' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Add annotation' }))
  await waitFor(() => {
    expect(screen.getByLabelText('Annotation')).toHaveValue('')
  })
})

test('applies saves deletes and exports durable search views', async () => {
  render(<App />)
  await screen.findByText('CI')
  fireEvent.click(screen.getByRole('link', { name: 'Runs' }))
  const savedView = await screen.findByLabelText('Saved view')
  fireEvent.change(savedView, { target: { value: 'saved-release' } })
  expect(screen.getByLabelText('Search history')).toHaveValue(
    'release failure',
  )
  fireEvent.change(screen.getByLabelText('Search history'), {
    target: { value: 'console foundation' },
  })
  fireEvent.change(screen.getByLabelText('New view name'), {
    target: { value: 'Console foundation' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Save current view' }))
  expect(
    await screen.findByRole('option', { name: 'Console foundation' }),
  ).toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: 'Export JSONL' }))
  expect(
    await screen.findByRole('button', {
      name: 'Download 1 exported records',
    }),
  ).toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: 'Delete view' }))
  await waitFor(() =>
    expect(
      screen.queryByRole('option', { name: 'Console foundation' }),
    ).not.toBeInTheDocument(),
  )
})

test('renders repository and workflow reliability analytics', async () => {
  render(<App />)
  await screen.findByText('CI')
  fireEvent.click(screen.getByRole('link', { name: 'Repositories' }))
  expect(
    await screen.findByText('actionscommunity/multirunner'),
  ).toBeInTheDocument()
  expect(screen.getByText('90%')).toBeInTheDocument()
  fireEvent.click(screen.getByRole('link', { name: 'Workflows' }))
  expect(
    await screen.findByRole('heading', { name: 'Workflow analytics' }),
  ).toBeInTheDocument()
  expect(screen.getByText('1m 20s')).toBeInTheDocument()
})

test('loads a stable correlated run inspection route', async () => {
  window.history.replaceState(
    null,
    '',
    '/runs?repository=actionscommunity%2Fmultirunner&run_id=42',
  )
  render(<App />)
  expect(
    await screen.findByRole('heading', { name: 'CI' }),
  ).toBeInTheDocument()
  expect(screen.getByText('Verify console foundation')).toBeInTheDocument()
  expect(screen.getByText('integration')).toBeInTheDocument()
  expect(screen.getByText('Run tests')).toBeInTheDocument()
  expect(
    screen.getByText(
      (_, element) =>
        element?.tagName === 'SMALL' &&
        element.textContent?.replace(/\s+/g, ' ').trim() ===
          'exact · 100% confidence',
    ),
  ).toBeInTheDocument()
})

test('fetches searches and discards a transient masked job log', async () => {
  window.history.replaceState(
    null,
    '',
    '/runs?repository=actionscommunity%2Fmultirunner&run_id=42',
  )
  render(<App />)
  fireEvent.click(
    await screen.findByRole('button', { name: 'View transient log' }),
  )
  expect(await screen.findByText(/Run tests failed/)).toBeInTheDocument()
  expect(screen.getByText(/masked \*\*\*/)).toBeInTheDocument()
  fireEvent.change(screen.getByLabelText('Search this log'), {
    target: { value: 'masked' },
  })
  await waitFor(() =>
    expect(screen.queryByText(/Run tests failed/)).not.toBeInTheDocument(),
  )
  fireEvent.click(
    screen.getByRole('button', { name: 'Close and discard' }),
  )
  expect(screen.queryByText(/masked \*\*\*/)).not.toBeInTheDocument()
})

test('previews and queues a guarded runner command', async () => {
  render(<App />)
  await screen.findByText('CI')
  fireEvent.click(screen.getByRole('link', { name: 'Runners' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Terminate' }))
  expect(await screen.findByText(/current job may be interrupted/i)).toBeInTheDocument()
  fireEvent.change(screen.getByLabelText('Reason'), {
    target: { value: 'Runner is stuck' },
  })
  fireEvent.change(screen.getByLabelText(/terminate session-1/), {
    target: { value: 'terminate session-1' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Terminate runner' }))
  expect(await screen.findByText('queued')).toBeInTheDocument()
})

test('renders structured diagnostics and redacted configuration', async () => {
  render(<App />)
  await screen.findByText('CI')
  fireEvent.click(screen.getByRole('link', { name: 'Diagnostics' }))
  expect(await screen.findByText('history.database')).toBeInTheDocument()
  fireEvent.click(screen.getByRole('link', { name: 'Configuration' }))
  expect(await screen.findByText('history.enabled')).toBeInTheDocument()
  expect(screen.getByText('C:\\multirunner\\config.yaml')).toBeInTheDocument()
})

test('uses the same cached loaders for direct routes and popstate navigation', async () => {
  window.history.replaceState(null, '', '/diagnostics')
  render(<App />)

  expect(await screen.findByText('history.database')).toBeInTheDocument()
  const diagnosticsRequests = () =>
    vi.mocked(fetch).mock.calls.filter(([input]) =>
      String(input).includes('/api/v1/diagnostics'),
    )
  expect(diagnosticsRequests()).toHaveLength(1)

  window.history.pushState(null, '', '/configuration')
  fireEvent(window, new PopStateEvent('popstate'))
  expect(
    await screen.findByText('C:\\multirunner\\config.yaml'),
  ).toBeInTheDocument()

  window.history.pushState(null, '', '/diagnostics')
  fireEvent(window, new PopStateEvent('popstate'))
  expect(
    await screen.findByRole('heading', { name: 'Diagnostics', level: 1 }),
  ).toBeInTheDocument()
  expect(diagnosticsRequests()).toHaveLength(1)
})

test('queues an audited support bundle and exposes its download', async () => {
  render(<App />)
  await screen.findByText('CI')
  fireEvent.click(screen.getByRole('link', { name: 'Diagnostics' }))
  await screen.findByText('history.database')
  fireEvent.click(
    screen.getByRole('button', { name: 'Create support bundle' }),
  )
  expect(
    await screen.findByText(/redacted local archive/i),
  ).toBeInTheDocument()
  fireEvent.change(screen.getByLabelText('Reason'), {
    target: { value: 'Investigate host health' },
  })
  fireEvent.change(screen.getByLabelText(/generate support bundle/), {
    target: { value: 'generate support bundle' },
  })
  fireEvent.click(
    screen.getByRole('button', { name: 'Generate support bundle' }),
  )
  expect(await screen.findByText('queued')).toBeInTheDocument()
  expect(
    await screen.findByRole('button', { name: 'Download redacted ZIP' }),
  ).toBeInTheDocument()
})

test('traps focus, closes safely, inerts the background, and restores focus for dialogs', async () => {
  render(<App />)
  await screen.findByText('CI')

  const exerciseDialog = async (
    opener: HTMLElement,
    title: string,
    closeName: string,
  ) => {
    opener.focus()
    fireEvent.click(opener)
    const dialog = await screen.findByRole('dialog')
    const heading = within(dialog).getByRole('heading', { name: title })
    await waitFor(() => expect(heading).toHaveFocus())
    expect(document.querySelector('.console-background')).toHaveAttribute(
      'inert',
    )

    expect(
      within(dialog).getByRole('button', { name: closeName }),
    ).toBeInTheDocument()
    fireEvent.keyDown(dialog, { key: 'Tab', shiftKey: true })
    const enabledButtons = within(dialog)
      .getAllByRole('button')
      .filter((button) => !button.hasAttribute('disabled'))
    expect(enabledButtons.at(-1)).toHaveFocus()
    fireEvent.keyDown(dialog, { key: 'Tab' })
    expect(enabledButtons[0]).toHaveFocus()

    fireEvent.keyDown(dialog, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(document.querySelector('.console-background')).not.toHaveAttribute(
      'inert',
    )
    expect(opener).toHaveFocus()
  }

  fireEvent.click(screen.getByRole('link', { name: 'Runners' }))
  await exerciseDialog(
    await screen.findByRole('button', { name: 'Terminate' }),
    'Terminate runner',
    'Close command dialog',
  )

  fireEvent.click(screen.getByRole('link', { name: 'Backups' }))
  await screen.findByText('verified · abcdef123456')
  await exerciseDialog(
    screen.getByRole('button', { name: 'Create verified backup' }),
    'Create verified backup',
    'Close backup dialog',
  )
  await exerciseDialog(
    screen.getByRole('button', { name: 'Stage restore' }),
    'Stage database restore',
    'Close restore dialog',
  )

  fireEvent.click(screen.getByRole('button', { name: 'Check for update' }))
  await screen.findByText(/Threshold signatures and provenance verified/i)
  await exerciseDialog(
    screen.getByRole('button', { name: 'Stage trusted update' }),
    'Stage trusted update',
    'Close update dialog',
  )

  fireEvent.click(screen.getByRole('link', { name: 'Diagnostics' }))
  await screen.findByText('history.database')
  await exerciseDialog(
    screen.getByRole('button', { name: 'Create support bundle' }),
    'Create support bundle',
    'Close support bundle dialog',
  )
})

test('creates and downloads a verified backup', async () => {
  window.history.replaceState(null, '', '/backups')
  render(<App />)
  expect(
    await screen.findByRole('heading', { name: 'Backups', level: 2 }),
  ).toBeInTheDocument()
  expect(
    await screen.findByText('verified · abcdef123456'),
  ).toBeInTheDocument()
  fireEvent.click(
    screen.getByRole('button', { name: 'Create verified backup' }),
  )
  expect(
    await screen.findByText(
      'Create and integrity-check an online SQLite backup without stopping runner provisioning.',
    ),
  ).toBeInTheDocument()
  fireEvent.change(screen.getByLabelText('Reason'), {
    target: { value: 'Before maintenance' },
  })

  fireEvent.change(
    screen.getByLabelText(/Type create verified backup to confirm/),
    { target: { value: 'create verified backup' } },
  )
  fireEvent.click(
    within(screen.getByRole('dialog')).getByRole('button', {
      name: 'Create verified backup',
    }),
  )
  expect(await screen.findByText('queued')).toBeInTheDocument()
  expect(
    await screen.findByRole('button', {
      name: 'Download verified SQLite backup',
    }),
  ).toBeInTheDocument()
})

test('stages a verified backup for restart-gated restore', async () => {
  window.history.replaceState(null, '', '/backups')
  render(<App />)
  fireEvent.click(
    await screen.findByRole('button', { name: 'Stage restore' }),
  )
  expect(
    await screen.findByText(/activation on the next service restart/i),
  ).toBeInTheDocument()
  fireEvent.change(screen.getByLabelText('Reason'), {
    target: { value: 'Recover history' },
  })
  fireEvent.change(
    screen.getByLabelText(/Type stage restore backup-1 to confirm/),
    { target: { value: 'stage restore backup-1' } },
  )
  fireEvent.click(
    within(screen.getByRole('dialog')).getByRole('button', {
      name: 'Stage restore',
    }),
  )
  expect(await screen.findByText('queued')).toBeInTheDocument()
  expect(
    await screen.findByText(/Restart the multirunner service to activate it/i),
  ).toBeInTheDocument()
})

test('verifies and stages a restart-gated application update', async () => {
  window.history.replaceState(null, '', '/backups')
  render(<App />)
  fireEvent.click(
    await screen.findByRole('button', { name: 'Check for update' }),
  )
  expect(
    await screen.findByText(/Threshold signatures and provenance verified/i),
  ).toBeInTheDocument()
  fireEvent.click(
    screen.getByRole('button', { name: 'Stage trusted update' }),
  )
  expect(
    await screen.findByText(/threshold signatures, provenance/i),
  ).toBeInTheDocument()
  fireEvent.change(screen.getByLabelText('Reason'), {
    target: { value: 'Apply security release' },
  })
  fireEvent.change(
    screen.getByLabelText(/Type stage trusted update v1.2.0 to confirm/),
    { target: { value: 'stage trusted update v1.2.0' } },
  )
  fireEvent.click(
    within(screen.getByRole('dialog')).getByRole('button', {
      name: 'Stage trusted update',
    }),
  )
  expect(await screen.findByText('queued')).toBeInTheDocument()
  expect(
    await screen.findByText(/previous worker if startup health confirmation fails/i),
  ).toBeInTheDocument()
})

test('switches to offline read-only mode and disables mutations', async () => {
  window.history.replaceState(null, '', '/backups')
  render(<App />)
  await screen.findByRole('heading', { name: 'Backups', level: 2 })

  Object.defineProperty(window.navigator, 'onLine', {
    configurable: true,
    value: false,
  })
  fireEvent(window, new Event('offline'))

  expect(await screen.findByText(/^Offline\./)).toBeInTheDocument()
  expect(
    screen.getByRole('button', { name: 'Create verified backup' }),
  ).toBeDisabled()
  expect(screen.getAllByText('Offline')).not.toHaveLength(0)
})

test('announces a ready PWA update and reports registration or apply failures', async () => {
  const updateSW = vi.fn().mockRejectedValue(new Error('Update reload failed.'))
  render(
    <App
      pwaUpdate={{
        ready: true,
        updateSW,
        registrationError: 'Offline registration failed.',
      }}
    />,
  )
  await screen.findByText('CI')

  expect(
    screen.getByText('A new Operations Console version is ready.'),
  ).toBeInTheDocument()
  expect(screen.getByRole('alert')).toHaveTextContent(
    'Offline registration failed.',
  )
  fireEvent.click(screen.getByRole('button', { name: 'Update and reload' }))
  await waitFor(() => expect(updateSW).toHaveBeenCalledOnce())
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Update reload failed.',
  )
})

test('associates invalid search errors and focuses the search field', async () => {
  render(<App />)
  await screen.findByText('CI')
  fireEvent.click(screen.getByRole('link', { name: 'Runs' }))

  const input = screen.getByRole('searchbox', { name: 'Search history' })
  fireEvent.change(input, { target: { value: 'x' } })
  fireEvent.click(screen.getByRole('button', { name: 'Search' }))

  const error = await screen.findByRole('alert')
  expect(error).toHaveTextContent('Enter at least 2 characters.')
  expect(input).toHaveAttribute('aria-invalid', 'true')
  expect(input).toHaveAttribute('aria-describedby', error.id)
  await waitFor(() => expect(input).toHaveFocus())
})

test('reports authenticated download failures as persistent alerts', async () => {
  const fetchMock = vi.mocked(fetch)
  const originalFetch = fetchMock.getMockImplementation()
  fetchMock.mockImplementation((input, init) => {
    if (String(input).includes('/api/v1/backups/backup-1/download')) {
      return Promise.resolve(
        new Response(JSON.stringify({ message: 'Session expired.' }), {
          status: 401,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    }
    return originalFetch!(input, init)
  })

  window.history.replaceState(null, '', '/backups')
  render(<App />)
  fireEvent.click(await screen.findByRole('button', { name: 'Download' }))

  expect(await screen.findByRole('alert')).toHaveTextContent('Session expired.')
})

test('has no automatic accessibility violations', async () => {
  const { container } = render(<App />)
  await screen.findByText('CI')
  const results = await axe.run(container)
  expect(results.violations).toEqual([])
})

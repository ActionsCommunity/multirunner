import { expect, test, type Page } from '@playwright/test'
import axe from 'axe-core'

const now = '2026-10-09T05:00:00Z'
const proof = 'p'.repeat(43)

async function mockConsoleAPI(
  page: Page,
  options: { eventBurst?: number } = {},
) {
  await page.addInitScript(() => {
    const sources: TestEventSource[] = []
    class TestEventSource extends EventTarget {
      static readonly CONNECTING = 0
      static readonly OPEN = 1
      static readonly CLOSED = 2
      readonly CONNECTING = 0
      readonly OPEN = 1
      readonly CLOSED = 2
      readonly url: string
      readonly withCredentials = false
      readyState = TestEventSource.OPEN
      onopen: ((event: Event) => void) | null = null
      onerror: ((event: Event) => void) | null = null
      onmessage: ((event: MessageEvent) => void) | null = null

      constructor(url: string | URL) {
        super()
        this.url = String(url)
        sources.push(this)
        queueMicrotask(() => this.onopen?.(new Event('open')))
      }

      close() {
        this.readyState = TestEventSource.CLOSED
      }
    }

    Object.defineProperty(window, 'EventSource', {
      configurable: true,
      value: TestEventSource,
    })
    Object.defineProperty(window, '__emitOperationalEvents', {
      configurable: true,
      value: (events: unknown[]) => {
        const source = sources.at(-1)
        for (const event of events) {
          source?.dispatchEvent(
            new MessageEvent('operational-event', {
              data: JSON.stringify(event),
            }),
          )
        }
      },
    })
  })

  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    let body: unknown

    if (url.pathname === '/api/v1/events') {
      const events = Array.from(
        { length: options.eventBurst ?? 0 },
        (_, index) => {
          const sequence = index + 1
          const event = {
            id: `epoch:${sequence}`,
            schema_version: 1,
            host_id: 'host',
            host_epoch: 'epoch',
            sequence,
            type: sequence % 2 ? 'runner.launched' : 'runner.stopped',
            entity_type: 'runner_session',
            entity_id: `runner-${sequence % 25}`,
            timestamp: now,
            correlation_id: '',
            causation_id: '',
            actor_kind: 'system',
            actor_id: 'multirunner',
            payload: {
              pool: 'linux',
              runner_name: `runner-${sequence % 25}`,
            },
            command_id: '',
          }
          return `id: ${event.id}\nevent: operational-event\ndata: ${JSON.stringify(event)}\n\n`
        },
      ).join('')
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body:
          events +
          'event: replay-complete\ndata: {"host_epoch":"epoch","sequence":1000,"replayed":0}\n\n',
      })
      return
    }

    if (url.pathname === '/api/summary') {
      body = {
        runner_sessions: 200,
        pending_sessions: 2,
        workflow_runs: 180,
        workflow_jobs: 210,
        successful_jobs: 190,
        failed_jobs: 10,
        cancelled_jobs: 10,
        average_duration_seconds: 92,
      }

    } else if (url.pathname === '/api/runs') {
      body = {
        runs: [
          {
            id: 42,
            repository: 'actionscommunity/multirunner',
            name: 'CI',
            workflow_name: 'CI',
            display_title: 'Verify console foundation',
            status: 'completed',
            conclusion: 'success',
            html_url:
              'https://github.com/actionscommunity/multirunner/actions/runs/42',
            run_attempt: 1,
            created_at: now,
          },
        ],
      }
    } else if (url.pathname === '/api/v1/session') {
      body = {
        actor_id: 'local-test',
        csrf_token: 'csrf-test',
        issued_at: new Date(Date.now() - 60_000).toISOString(),
        paired_at: new Date(Date.now() - 60_000).toISOString(),
        revocation_generation: 1,
      }
    } else if (url.pathname === '/api/v1/pools') {
      body = {
        items: [
          { name: 'linux', count: 120 },
          { name: 'windows', count: 80 },
        ],
        applied_filters: {},
        cursor: '',
        next_cursor: '',
        count: 2,
      }
    } else if (url.pathname === '/api/v1/system') {
      body = {
        status: 'operational',
        mode: 'local',
        listen: '127.0.0.1:9092',
        database: 'C:\\ProgramData\\multirunner\\history.db',
        started_at: now,
        app_version: 'v1.2.0',
        api_version: 'v1',
        configuration: 'read_only',
      }
    } else if (url.pathname === '/api/v1/audit') {
      body = {
        items: [
          {
            id: 'audit-1',
            source: 'command',
            occurred_at: now,
            actor_kind: 'operator',
            actor_id: 'local-test',
            action: 'backup.create',
            target_type: 'system',
            target_id: 'backups',
            correlation_id: 'correlation-1',
            outcome: 'succeeded',
          },
        ],
        applied_filters: {},
        cursor: '',
        next_cursor: '',
        count: 1,
      }
    } else if (url.pathname === '/api/v1/saved-views') {
      body = {
        items: [],
        count: 0,
      }
    } else if (url.pathname === '/api/v1/runners') {
      body = {
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
            updated_at: now,
          },
        ],
        applied_filters: {},
        count: 1,
        host_epoch: 'epoch',
        max_sequence: 2,
      }
    } else if (url.pathname === '/api/v1/runs/42') {
      body = {
        run: {
          id: 42,
          repository: 'actionscommunity/multirunner',
          name: 'CI',
          workflow_name: 'CI',
          display_title: 'Large transient log',
          event: 'push',
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
          created_at: now,
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
            created_at: now,
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
        runner_sessions: [],
      }
    } else if (url.pathname === '/api/v1/jobs/84/log') {
      const lineCount = 8192
      const content = Array.from({ length: lineCount }, (_, index) => {
        const marker =
          index === 0
            ? '\u001b[31mmasked ***\u001b[0m'
            : index === lineCount - 1
              ? 'unique-final-line'
              : `ordinary-line-${index}`
        return marker.padEnd(1023, '.')
      }).join('\n')
      await route.fulfill({
        status: 200,
        contentType: 'text/plain; charset=utf-8',
        body: content,
      })
      return
    } else if (url.pathname === '/api/v1/analytics') {
      const workflow = url.searchParams.get('group_by') === 'workflow'
      body = {
        generated_at: now,
        group_by: workflow ? 'workflow' : 'repository',
        since: now,
        until: now,
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
      }
    } else if (url.pathname === '/api/v1/alerts') {
      body = { items: [], count: 0 }
    } else if (
      ['/api/v1/backups', '/api/v1/restores', '/api/v1/updates'].includes(
        url.pathname,
      )
    ) {
      body = { items: [], count: 0 }
    } else if (url.pathname === '/api/v1/diagnostics') {
      body = {
        generated_at: now,
        status: 'pass',
        results: [],
      }
    } else if (url.pathname === '/api/v1/configuration') {
      body = {
        source_file: 'C:\\multirunner\\config.yaml',
        loaded_at: now,
        inspected_at: now,
        startup_sha256: 'test',
        current_sha256: 'test',
        drifted: false,
        changed_paths: [],
        raw_redacted: 'history:\\n  enabled: true\\n',
        normalized_redacted: 'history:\\n  enabled: true\\n',
        fields: [],
        guidance: 'Read-only test configuration.',
      }
    } else {
      await route.fulfill({
        status: 404,
        contentType: 'application/json',
        body: JSON.stringify({ error: `unhandled test route ${url.pathname}` }),
      })
      return
    }

    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(body),
    })
  })
}

test('captures the pairing proof cookie and authenticates API transport', async ({
  page,
}) => {
  let observedProof = ''
  await mockConsoleAPI(page)
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/api/summary') {
      observedProof = request.headers()['x-multirunner-console-proof'] ?? ''
    }
  })
  await page.context().addCookies([
    {
      name: 'multirunner_console_pairing_proof',
      value: proof,
      url: 'http://127.0.0.1:4173/',
      sameSite: 'Strict',
    },
  ])
  await page.goto('/')
  await expect(page).toHaveURL(/\/$/)
  await expect(
    page.getByRole('heading', { name: 'Overview', level: 1 }),
  ).toBeVisible()
  expect(observedProof).toBe(proof)
})

test('renders the overview and navigates to durable runner state', async ({
  page,
}) => {
  const consoleErrors: string[] = []
  const pageErrors: string[] = []
  const failedRequests: string[] = []
  page.on('console', (message) => {
    if (message.type() === 'error') {
      const location = message.location().url
      consoleErrors.push(
        location ? `${message.text()} (${location})` : message.text(),
      )
    }
  })
  page.on('pageerror', (error) => pageErrors.push(error.message))
  page.on('requestfailed', (request) => {
    if (request.failure()?.errorText === 'net::ERR_ABORTED') return
    failedRequests.push(
      `${request.method()} ${request.url()}: ${request.failure()?.errorText}`,
    )
  })
  await mockConsoleAPI(page)

  await page.goto('/')

  await expect(
    page.getByRole('heading', { name: 'Overview', level: 1 }),
  ).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Recent activity' })).toBeVisible()
  await expect(page.getByText('210', { exact: true })).toBeVisible()
  await expect(page.getByText('Verify console foundation')).toBeVisible()
  const navigationIcons = page.getByTestId('navigation-icon')
  await expect(navigationIcons).toHaveCount(13)
  for (const icon of await navigationIcons.all()) {
    await expect(icon).toBeVisible()
    expect(await icon.getAttribute('data-icon')).not.toBeNull()
    const box = await icon.boundingBox()
    expect(box?.width).toBeGreaterThan(0)
    expect(box?.height).toBeGreaterThan(0)
  }
  await expect(page.getByText('Degraded read-only')).toHaveCount(0)

  await page.getByRole('link', { name: 'Runners' }).click()

  await expect(page).toHaveURL(/\/runners$/)
  await expect(
    page.getByRole('heading', { name: 'Runner sessions' }),
  ).toBeVisible()
  await expect(page.getByText('multirunner-linux-1')).toBeVisible()
  await expect(page.getByText('launched', { exact: true })).toBeVisible()

  expect(consoleErrors).toEqual([])
  expect(pageErrors).toEqual([])
  expect(failedRequests).toEqual([])
})

test('loads direct routes and reuses their loaders through browser history', async ({
  page,
}) => {
  await mockConsoleAPI(page)
  let diagnosticsRequests = 0
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/api/v1/diagnostics') {
      diagnosticsRequests += 1
    }
  })

  await page.goto('/diagnostics')
  await expect(
    page.getByRole('heading', { name: 'Diagnostics', level: 1 }),
  ).toBeVisible()
  await expect(page.getByText('pass', { exact: true })).toBeVisible()

  await page.getByRole('link', { name: 'Configuration' }).click()
  await expect(page).toHaveURL(/\/configuration$/)
  await expect(page.getByText('Read-only test configuration.')).toBeVisible()

  await page.goBack()
  await expect(page).toHaveURL(/\/diagnostics$/)
  await expect(
    page.getByRole('heading', { name: 'Diagnostics', level: 1 }),
  ).toBeVisible()
  expect(diagnosticsRequests).toBe(1)
})

test('windows and filters an 8 MiB masked transient log', async ({ page }) => {
  await mockConsoleAPI(page)
  await page.goto(
    '/runs?repository=actionscommunity%2Fmultirunner&run_id=42',
  )

  await page.getByRole('button', { name: 'View transient log' }).click()

  const lines = page.locator('.log-line')
  await expect(lines).toHaveCount(500)
  await expect(page.locator('.ansi-red')).toContainText('masked ***')
  await expect(
    page.getByText(/Showing 500 of 8,192 matching lines/),
  ).toBeVisible()

  await page.getByLabel('Search this log').fill('unique-final-line')
  await expect(lines).toHaveCount(1)
  await expect(page.getByText(/Showing 1 of 1 matching lines/)).toBeVisible()
})

test('coalesces an SSE burst into a bounded live ledger', async ({ page }) => {
  await mockConsoleAPI(page, { eventBurst: 1000 })
  await page.goto('/live')

  const entries = page.locator('.event-ledger li')
  await expect(entries).toHaveCount(50)
  await expect(entries.first()).toContainText('#1000')
  await expect(entries.last()).toContainText('#951')
  await expect(page.locator('.event-ledger')).not.toHaveAttribute('aria-live')
  await expect(page.locator('.sr-only[role="status"]')).toContainText(
    '50 recent operational events',
  )
})

test('passes focused axe checks and names every rendered table', async ({
  page,
}) => {
  await mockConsoleAPI(page)
  for (const path of ['/', '/runners', '/alerts']) {
    await page.goto(path)
    await page.addScriptTag({ content: axe.source })
    const violations = await page.evaluate(async () => {
      const axeAPI = (
        globalThis as unknown as {
          axe: {
            run: (
              root: Document,
              options: unknown,
            ) => Promise<{ violations: Array<{ id: string }> }>
          }
        }
      ).axe
      const results = await axeAPI.run(document, {
        runOnly: {
          type: 'tag',
          values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'],
        },
      })
      return results.violations.map((violation) => violation.id)
    })
    expect(violations).toEqual([])
    const unnamedTables = await page.locator('table').evaluateAll((tables) =>
      tables.filter(
        (table) =>
          !table.querySelector('caption') &&
          !table.getAttribute('aria-label') &&
          !table.getAttribute('aria-labelledby'),
      ).length,
    )
    expect(unnamedTables).toBe(0)
  }
})

test('demo mode is clearly labeled and does not invent machine data', async ({
  page,
}) => {
  const consoleErrors: string[] = []
  const badResponses: string[] = []
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text())
  })
  page.on('response', (response) => {
    if (response.status() >= 400) {
      badResponses.push(`${response.status()} ${response.url()}`)
    }
  })

  await page.goto('/')

  await expect(
    page.getByRole('heading', { name: 'Preview mode', level: 2 }),
  ).toBeVisible()
  await expect(page.getByText('Degraded read-only')).toHaveCount(0)
  await expect(page.getByTestId('navigation-icon')).toHaveCount(13)
  await expect(
    page.getByText('No retained workflow runs are available yet.'),
  ).toBeVisible()
  await expect(page.getByText('actionscommunity/multirunner')).toHaveCount(0)
  await page.getByRole('link', { name: 'Runners' }).click()
  await expect(
    page.getByText(
      'No runner lifecycle state has been recorded in this host epoch.',
    ),
  ).toBeVisible()
  expect(consoleErrors).toEqual([])
  expect(badResponses).toEqual([])
})

test('[matrix:offline] shows a sourced stale snapshot offline and sends no mutation', async ({
  page,
}) => {
  await page.addInitScript(() => {
    const current = Date.now()
    Object.defineProperty(window.navigator, 'onLine', {
      configurable: true,
      get: () => false,
    })
    window.localStorage.setItem(
      'multirunner.console.operational-snapshot.v1',
      JSON.stringify({
        version: 1,
        saved_at: new Date(current - 65 * 60_000).toISOString(),
        expires_at: new Date(current + 10 * 60 * 60_000).toISOString(),
        source: 'Local console API',
        session_issued_at: new Date(current - 2 * 60 * 60_000).toISOString(),
        revocation_generation: 1,
        overview: {
          runner_sessions: 20,
          pending_sessions: 2,
          workflow_runs: 18,
          workflow_jobs: 21,
          successful_jobs: 19,
          failed_jobs: 1,
          cancelled_jobs: 1,
          average_duration_seconds: 92,
        },
        live: {
          runners: 4,
          pending: 1,
          failed: 0,
          last_event_at: new Date(current - 70 * 60_000).toISOString(),
        },
      }),
    )
  })
  await mockConsoleAPI(page)
  const mutations: string[] = []
  page.on('request', (request) => {
    if (request.method() !== 'GET') {
      mutations.push(`${request.method()} ${request.url()}`)
    }
  })

  await page.goto('/')
  await expect(page.getByText('Stale offline snapshot')).toBeVisible()
  await expect(
    page.getByText(/Source: Local console API · Saved 1 hour old/),
  ).toBeVisible()
  await page.getByRole('link', { name: 'Live' }).click()
  await expect(page.getByRole('definition')).toHaveCount(8)
  await expect(
    page.getByText(/Live event details are not persisted offline/),
  ).toBeVisible()

  await page.getByRole('link', { name: 'Backups' }).click()
  await expect(
    page.getByRole('button', { name: 'Create verified backup' }),
  ).toBeDisabled()
  expect(mutations).toEqual([])
})

test('[matrix:mobile] keeps primary navigation and content usable at phone width', async ({
  page,
}) => {
  await mockConsoleAPI(page)
  await page.goto('/')
  await expect(
    page.getByRole('heading', { name: 'Overview', level: 1 }),
  ).toBeVisible()
  await page.getByRole('button', { name: 'Open navigation' }).click()
  await page.getByRole('link', { name: 'Runners' }).click()
  await expect(
    page.getByRole('heading', { name: 'Runners', level: 1 }),
  ).toBeVisible()
  await expect(
    page.getByRole('heading', { name: 'Runner sessions', level: 2 }),
  ).toBeVisible()
  await page.getByRole('button', { name: 'Open navigation' }).click()
  await page.getByRole('link', { name: 'Alerts' }).click()
  await expect(
    page.getByRole('heading', { name: 'Alerts', level: 1 }),
  ).toBeVisible()
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  )
  expect(overflow).toBeLessThanOrEqual(1)
})

test('[matrix:high-zoom] remains operable at 200 percent zoom', async ({ page }) => {
  await mockConsoleAPI(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Open navigation' }).click()
  await page.getByRole('link', { name: 'Diagnostics' }).click()
  await expect(
    page.getByRole('heading', { name: 'Diagnostics', level: 1 }),
  ).toBeVisible()
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  )
  expect(overflow).toBeLessThanOrEqual(1)
})

test('[matrix:keyboard] supports keyboard-only navigation with visible focus', async ({
  page,
}) => {
  await mockConsoleAPI(page)
  await page.goto('/')
  const runners = page.getByRole('link', { name: 'Runners' })
  for (let index = 0; index < 20; index += 1) {
    if (await runners.evaluate((element) => element === document.activeElement)) break
    await page.keyboard.press('Tab')
  }
  await expect(runners).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(
    page.getByRole('heading', { name: 'Runners', level: 1 }),
  ).toBeVisible()
  await expect(
    page.getByRole('heading', { name: 'Runners', level: 1 }),
  ).toBeFocused()
  await expect(
    page.getByRole('heading', { name: 'Runner sessions', level: 2 }),
  ).toBeVisible()
})

test('[matrix:reduced-motion] honors reduced-motion preferences', async ({ page }) => {
  await mockConsoleAPI(page)
  await page.goto('/')
  const preference = await page.evaluate(
    () => window.matchMedia('(prefers-reduced-motion: reduce)').matches,
  )
  expect(preference).toBe(true)
  const animated = await page.evaluate(
    () => document.getAnimations().filter((animation) => animation.playState === 'running').length,
  )
  expect(animated).toBe(0)
})

test('[smoke] traverses every console module without runtime failures', async ({
  page,
}) => {
  const consoleErrors: string[] = []
  const pageErrors: string[] = []
  const failedResponses: string[] = []
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text())
  })
  page.on('pageerror', (error) => pageErrors.push(error.message))
  page.on('response', (response) => {
    if (response.status() >= 400) {
      failedResponses.push(`${response.status()} ${response.url()}`)
    }
  })
  await mockConsoleAPI(page)
  await page.goto('/')

  for (const destination of [
    'Overview',
    'Live',
    'Runs',
    'Repositories',
    'Workflows',
    'Pools',
    'Runners',
    'Host',
    'Backups',
    'Alerts',
    'Diagnostics',
    'Configuration',
    'Audit',
  ]) {
    await page.getByRole('link', { name: destination }).click()
    await expect(
      page.getByRole('heading', { name: destination, level: 1 }),
    ).toBeVisible()
    await expect(page.locator('.error-state:visible')).toHaveCount(0)
  }

  await expect(page.getByTestId('navigation-icon')).toHaveCount(13)
  expect(consoleErrors).toEqual([])
  expect(pageErrors).toEqual([])
  expect(failedResponses).toEqual([])
})

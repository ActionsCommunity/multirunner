import { beforeEach, expect, test, vi } from 'vitest'
import { createCommand } from './commands-api'

beforeEach(() => {
  vi.unstubAllGlobals()
})

test('denies mutations offline before session or command requests are sent', async () => {
  Object.defineProperty(window.navigator, 'onLine', {
    configurable: true,
    value: false,
  })
  const fetch = vi.fn()
  vi.stubGlobal('fetch', fetch)

  await expect(
    createCommand(
      {
        type: 'runner.terminate',
        target_type: 'runner',
        target_id: 'runner-1',
        parameters: { pool: 'linux' },
        reason: 'stuck',
        confirmation: 'terminate runner-1',
      },
      'offline-idempotency-key',
    ),
  ).rejects.toMatchObject({ code: 'offline_mutation_denied' })
  expect(fetch).not.toHaveBeenCalled()
})

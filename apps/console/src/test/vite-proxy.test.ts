import { expect, test } from 'vitest'
import {
  consoleWorkbox,
  localConsoleProxy,
  resolveLocalConsoleTarget,
} from '../../vite.config'

test('configures the local console proxy for the real loopback listener', () => {
  const target = resolveLocalConsoleTarget()
  const proxy = localConsoleProxy(target)

  expect(target).toBe('http://127.0.0.1:9092')
  expect(proxy.target).toBe(target)
  expect(proxy.changeOrigin).toBe(true)
  expect(proxy.configure).toBeTypeOf('function')
})

test('keeps API responses and pairing proof out of service-worker storage', () => {
  expect(consoleWorkbox.navigateFallbackDenylist).toHaveLength(1)
  expect(consoleWorkbox.navigateFallbackDenylist[0]?.test('/api/v1/session')).toBe(
    true,
  )
  expect(consoleWorkbox.runtimeCaching).toEqual([
    expect.objectContaining({
      handler: 'NetworkOnly',
      method: 'GET',
    }),
  ])
  expect(
    consoleWorkbox.runtimeCaching[0]?.urlPattern.test(
      'http://127.0.0.1:9092/api/v1/session',
    ),
  ).toBe(true)
  expect(JSON.stringify(consoleWorkbox)).not.toContain('origin-proof')
})

test('rejects non-loopback and HTTPS console proxy targets', () => {
  expect(() =>
    resolveLocalConsoleTarget('http://example.com:9092'),
  ).toThrow('local console proxy target must use HTTP on loopback')
  expect(() =>
    resolveLocalConsoleTarget('https://127.0.0.1:9092'),
  ).toThrow('local console proxy target must use HTTP on loopback')
})

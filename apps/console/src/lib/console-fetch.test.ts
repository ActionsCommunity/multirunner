import { expect, test, vi } from 'vitest'
import {
  consoleFetch,
  consoleProofHeader,
  downloadAuthenticated,
  requestJSON,
  responseError,
  StepUpRequiredError,
} from './console-fetch'
import {
  getOriginProof,
  installOriginProof,
} from './session-proof'

const proof = 'b'.repeat(43)

test('rejects malformed pairing proof cookies', () => {
  window.sessionStorage.clear()
  document.cookie = 'multirunner_console_pairing_proof=not-valid; Path=/'
  installOriginProof()
  expect(getOriginProof()).toBe('')
  expect(document.cookie).not.toContain('multirunner_console_pairing_proof=')
})

test('captures pairing proof from a host cookie and immediately expires it', () => {
  window.sessionStorage.clear()
  document.cookie = `multirunner_console_pairing_proof=${proof}; Path=/; SameSite=Strict`
  window.history.replaceState({}, '', '/')
  installOriginProof()
  expect(getOriginProof()).toBe(proof)
  expect(document.cookie).not.toContain('multirunner_console_pairing_proof=')
  expect(window.location.pathname + window.location.search + window.location.hash).toBe('/')
})

test('maps the typed step-up response for frontend command handling', async () => {
  const error = await responseError(
    new Response(
      JSON.stringify({
        code: 'step_up_required',
        message: 'Run `multirunner console open` and retry.',
      }),
      { status: 403, headers: { 'Content-Type': 'application/json' } },
    ),
    'Command request failed',
  )
  expect(error).toBeInstanceOf(StepUpRequiredError)
  expect(error.message).toContain('console open')
})

test('consoleFetch preserves headers, sends proof, and clears it on 401', async () => {
  window.sessionStorage.setItem('multirunner.console.origin-proof', proof)
  const fetchMock = vi.fn(
    (_input: RequestInfo | URL, _init?: RequestInit) =>
      Promise.resolve(new Response('', { status: 401 })),
  )
  vi.stubGlobal('fetch', fetchMock)
  await consoleFetch('/api/v1/session', {
    headers: { Accept: 'application/json' },
  })
  const headers = new Headers(fetchMock.mock.calls[0]?.[1]?.headers)
  expect(headers.get('Accept')).toBe('application/json')
  expect(headers.get(consoleProofHeader)).toBe(proof)
  expect(fetchMock.mock.calls[0]?.[1]?.credentials).toBe('same-origin')
  expect(getOriginProof()).toBe('')
  vi.unstubAllGlobals()
})

test('authenticated downloads use consoleFetch and revoke their object URL', async () => {
  window.sessionStorage.setItem('multirunner.console.origin-proof', proof)
  const fetchMock = vi.fn(
    (_input: RequestInfo | URL, _init?: RequestInit) =>
      Promise.resolve(
        new Response(new Blob(['backup']), {
          status: 200,
          headers: { 'Content-Disposition': 'attachment; filename="backup.db"' },
        }),
      ),
  )
  const createObjectURL = vi.fn(() => 'blob:test')
  const revokeObjectURL = vi.fn()
  const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('URL', {
    ...URL,
    createObjectURL,
    revokeObjectURL,
  })
  await downloadAuthenticated('/api/v1/backups/backup-1/download')
  const headers = new Headers(fetchMock.mock.calls[0]?.[1]?.headers)
  expect(headers.get(consoleProofHeader)).toBe(proof)
  expect(createObjectURL).toHaveBeenCalledOnce()
  expect(click).toHaveBeenCalledOnce()
  expect(revokeObjectURL).toHaveBeenCalledWith('blob:test')
  click.mockRestore()
  vi.unstubAllGlobals()
})

test('requestJSON preserves proof transport and validates response contracts', async () => {
  window.sessionStorage.setItem('multirunner.console.origin-proof', proof)
  const fetchMock = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) =>
    Promise.resolve(
      new Response(JSON.stringify({ items: null, count: 0 }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  )
  vi.stubGlobal('fetch', fetchMock)
  await expect(
    requestJSON(
      '/api/v1/saved-views',
      undefined,
      'Saved views request failed',
      { required: ['items'], arrays: ['items'] },
    ),
  ).rejects.toThrow('items must be an array')
  const headers = new Headers(fetchMock.mock.calls[0]?.[1]?.headers)
  expect(headers.get(consoleProofHeader)).toBe(proof)
  vi.unstubAllGlobals()
})

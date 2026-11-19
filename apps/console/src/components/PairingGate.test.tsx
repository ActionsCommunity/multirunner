import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { PairingGate } from './PairingGate'

beforeEach(() => {
  window.localStorage.clear()
  window.sessionStorage.clear()
  window.history.replaceState({}, '', '/')
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('pairs through POST without putting the token in URLs or browser storage', async () => {
  const token = 'pair.secret-token'
  const onPaired = vi.fn()
  const fetchMock = vi
    .fn()
    .mockResolvedValueOnce(new Response('', { status: 401 }))
    .mockResolvedValueOnce(new Response(null, { status: 204 }))
  vi.stubGlobal('fetch', fetchMock)

  render(
    <PairingGate onPaired={onPaired}>
      <p>Authenticated console</p>
    </PairingGate>,
  )

  expect(
    await screen.findByRole('heading', { name: 'Open Multirunner Console' }),
  ).toBeInTheDocument()
  expect(screen.getByText(/multirunner console/)).toBeInTheDocument()
  const input = await screen.findByLabelText('One-time pairing token')
  await waitFor(() => expect(input).toHaveFocus())
  fireEvent.change(input, { target: { value: token } })
  fireEvent.click(screen.getByRole('button', { name: 'Open console' }))

  await waitFor(() => expect(onPaired).toHaveBeenCalledOnce())
  const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit]
  expect(url).toBe('/auth/pair')
  expect(init.method).toBe('POST')
  expect(JSON.parse(String(init.body))).toEqual({ token })
  expect(window.location.href).not.toContain(token)
  expect(window.localStorage.length).toBe(0)
  expect(window.sessionStorage.length).toBe(0)
  expect(input).toHaveValue('')
})

test('reports bounded pairing attempts and clears the submitted token', async () => {
  const fetchMock = vi
    .fn()
    .mockResolvedValueOnce(new Response('', { status: 401 }))
    .mockResolvedValueOnce(new Response('', { status: 429 }))
  vi.stubGlobal('fetch', fetchMock)

  render(
    <PairingGate onPaired={vi.fn()}>
      <p>Authenticated console</p>
    </PairingGate>,
  )

  const input = await screen.findByLabelText('One-time pairing token')
  fireEvent.change(input, { target: { value: 'invalid-token' } })
  fireEvent.click(screen.getByRole('button', { name: 'Open console' }))

  expect(
    await screen.findByText(/Too many attempts\. Wait two minutes/),
  ).toBeInTheDocument()
  expect(input).toHaveValue('')
})

test('keeps the console locked after a failed session check and rejects blank tokens locally', async () => {
  const onPaired = vi.fn()
  const fetchMock = vi.fn().mockResolvedValueOnce(new Response(null, { status: 503 }))
  vi.stubGlobal('fetch', fetchMock)

  render(
    <PairingGate onPaired={onPaired}>
      <p>Authenticated console</p>
    </PairingGate>,
  )

  expect(await screen.findByRole('alert')).toHaveTextContent('The console session could not be checked')
  expect(screen.queryByText('Authenticated console')).not.toBeInTheDocument()
  const input = screen.getByLabelText('One-time pairing token')
  expect(input).toHaveFocus()
  fireEvent.change(input, { target: { value: '   ' } })
  fireEvent.click(screen.getByRole('button', { name: 'Open console' }))

  expect(screen.getByRole('alert')).toHaveTextContent('Enter the pairing token')
  expect(fetchMock).toHaveBeenCalledTimes(1)
  expect(onPaired).not.toHaveBeenCalled()
})

test('clears a token after a transport failure and accepts a fresh token on retry', async () => {
  const onPaired = vi.fn()
  const fetchMock = vi
    .fn()
    .mockResolvedValueOnce(new Response(null, { status: 401 }))
    .mockRejectedValueOnce(new TypeError('Fixture transport unavailable'))
    .mockResolvedValueOnce(new Response(null, { status: 204 }))
  vi.stubGlobal('fetch', fetchMock)

  render(
    <PairingGate onPaired={onPaired}>
      <p>Authenticated console</p>
    </PairingGate>,
  )

  const input = await screen.findByLabelText('One-time pairing token')
  fireEvent.change(input, { target: { value: 'pair.first-fixture' } })
  fireEvent.click(screen.getByRole('button', { name: 'Open console' }))

  expect(await screen.findByRole('alert')).toHaveTextContent('Pairing could not be completed')
  expect(input).toHaveValue('')
  expect(input).toHaveFocus()
  expect(onPaired).not.toHaveBeenCalled()
  expect(screen.queryByText('Authenticated console')).not.toBeInTheDocument()

  fireEvent.change(input, { target: { value: 'pair.fresh-fixture' } })
  fireEvent.click(screen.getByRole('button', { name: 'Open console' }))

  await waitFor(() => expect(onPaired).toHaveBeenCalledOnce())
  expect(fetchMock).toHaveBeenNthCalledWith(
    3,
    '/auth/pair',
    expect.objectContaining({ method: 'POST', body: JSON.stringify({ token: 'pair.fresh-fixture' }) }),
  )
})

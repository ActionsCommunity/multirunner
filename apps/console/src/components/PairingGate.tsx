import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { consoleFetch } from '../lib/console-fetch'

type PairingState = 'checking' | 'required' | 'submitting' | 'authenticated' | 'error'

interface PairingGateProps {
  children: ReactNode
  onPaired?: () => void
}

export function PairingGate({
  children,
  onPaired = () => window.location.replace('/'),
}: PairingGateProps) {
  const [state, setState] = useState<PairingState>('checking')
  const [token, setToken] = useState('')
  const [message, setMessage] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    const controller = new AbortController()
    consoleFetch('/api/v1/session', { signal: controller.signal })
      .then((response) => {
        if (response.ok) {
          setState('authenticated')
        } else if (response.status === 401) {
          setState('required')
        } else {
          setMessage('The console session could not be checked. Try again.')
          setState('error')
        }
      })
      .catch((error: unknown) => {
        if (error instanceof DOMException && error.name === 'AbortError') return
        setMessage('The console session could not be checked. Try again.')
        setState('error')
      })
    return () => controller.abort()
  }, [])

  useEffect(() => {
    if (state === 'required' || state === 'error') inputRef.current?.focus()
  }, [state])

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const submittedToken = token.trim()
    if (!submittedToken) {
      setMessage('Enter the pairing token shown in the initiating terminal.')
      inputRef.current?.focus()
      return
    }
    setState('submitting')
    setMessage('')
    try {
      const response = await consoleFetch('/auth/pair', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: submittedToken }),
      })
      setToken('')
      if (response.ok) {
        onPaired()
        return
      }
      if (response.status === 429) {
        setMessage('Too many attempts. Wait two minutes, then generate a new pairing token.')
      } else {
        setMessage('The pairing token is invalid, expired, or already used.')
      }
      setState('required')
    } catch {
      setToken('')
      setMessage('Pairing could not be completed. Check the local console and try again.')
      setState('required')
    }
  }

  if (state === 'authenticated') return children

  return (
    <main className="pairing-page">
      <section className="pairing-card" aria-labelledby="pairing-title">
        <p className="eyebrow">Local Operations Console</p>
        <h1 id="pairing-title">Pair this browser</h1>
        <p>
          Run <code>multirunner console open</code> on this host, then enter the
          one-time token displayed in that terminal. Tokens expire after two minutes.
        </p>
        {state === 'checking' ? (
          <p role="status">Checking console session…</p>
        ) : (
          <form onSubmit={submit} noValidate>
            <label htmlFor="pairing-token">One-time pairing token</label>
            <input
              ref={inputRef}
              id="pairing-token"
              name="pairing-token"
              type="text"
              value={token}
              onChange={(event) => setToken(event.target.value)}
              autoComplete="one-time-code"
              autoCapitalize="none"
              spellCheck={false}
              disabled={state === 'submitting'}
              required
            />
            <button type="submit" disabled={state === 'submitting'}>
              {state === 'submitting' ? 'Pairing…' : 'Pair browser'}
            </button>
          </form>
        )}
        {message ? (
          <p className="pairing-error" role="alert">
            {message}
          </p>
        ) : null}
        <p className="pairing-privacy">
          The token is submitted only to this loopback console and is never stored in
          browser history or local storage.
        </p>
      </section>
    </main>
  )
}

import { StrictMode, useEffect, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { registerSW } from 'virtual:pwa-register'
import './index.css'
import App from './App.tsx'
import { PairingGate } from './components/PairingGate'
import { installOriginProof } from './lib/session-proof'

installOriginProof()

export function RegisteredApp() {
  const [updateReady, setUpdateReady] = useState(false)
  const [registrationError, setRegistrationError] = useState('')
  const [registrationAttempt, setRegistrationAttempt] = useState(0)
  const updateSW = useRef<() => Promise<void>>(async () => {})

  useEffect(() => {
    const applyUpdate = registerSW({
      immediate: true,
      onNeedRefresh: () => setUpdateReady(true),
      onRegisterError: (error) => {
        setRegistrationError(
          error instanceof Error
            ? error.message
            : 'The offline application service could not be registered.',
        )
      },
    })
    updateSW.current = async () => {
      await applyUpdate(true)
    }
  }, [registrationAttempt])

  return (
    <PairingGate>
      <App
        pwaUpdate={{
          ready: updateReady,
          updateSW: () => updateSW.current(),
          retryRegistration: () => {
            setRegistrationError('')
            setRegistrationAttempt((attempt) => attempt + 1)
          },
          registrationError: registrationError || undefined,
        }}
      />
    </PairingGate>
  )
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <RegisteredApp />
  </StrictMode>,
)

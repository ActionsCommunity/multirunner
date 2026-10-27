const storageKey = 'multirunner.console.origin-proof'
const pairingCookie = 'multirunner_console_pairing_proof'
const proofPattern = /^[A-Za-z0-9_-]{43}$/

export function installOriginProof(): void {
  if (typeof window === 'undefined') return
  const cookiePrefix = `${pairingCookie}=`
  const cookieProof =
    document.cookie
      .split(';')
      .map((value) => value.trim())
      .find((value) => value.startsWith(cookiePrefix))
      ?.slice(cookiePrefix.length) ?? ''
  if (proofPattern.test(cookieProof)) {
    try {
      window.sessionStorage.setItem(storageKey, cookieProof)
    } catch {
      // A blocked storage area leaves the API safely unauthenticated.
    }
  }
  if (cookieProof) {
    document.cookie = `${pairingCookie}=; Path=/; Max-Age=0; SameSite=Strict`
  }
}

export function getOriginProof(): string {
  if (typeof window === 'undefined') return ''
  try {
    const proof = window.sessionStorage.getItem(storageKey) ?? ''
    return proofPattern.test(proof) ? proof : ''
  } catch {
    return ''
  }
}

export function clearOriginProof(): void {
  if (typeof window === 'undefined') return
  try {
    window.sessionStorage.removeItem(storageKey)
  } catch {
    // The proof is already unavailable when storage access is blocked.
  }
}

import { clearOriginProof, getOriginProof } from './session-proof'

export const consoleProofHeader = 'X-Multirunner-Console-Proof'

export interface JSONContract {
  required?: readonly string[]
  arrays?: readonly string[]
  strings?: readonly string[]
  numbers?: readonly string[]
  booleans?: readonly string[]
}

export class StepUpRequiredError extends Error {
  readonly code = 'step_up_required'

  constructor(message: string) {
    super(message)
    this.name = 'StepUpRequiredError'
  }
}

export async function consoleFetch(
  input: RequestInfo | URL,
  init: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(input instanceof Request ? input.headers : undefined)
  new Headers(init.headers).forEach((value, name) => headers.set(name, value))
  const proof = getOriginProof()
  if (proof) headers.set(consoleProofHeader, proof)
  const response = await fetch(input, {
    ...init,
    credentials: 'same-origin',
    headers,
  })
  if (response.status === 401) clearOriginProof()
  return response
}

export async function responseError(
  response: Response,
  fallback: string,
): Promise<Error> {
  let message = `${fallback} (${response.status})`
  let code = ''
  try {
    const error: unknown = await response.json()
    if (isJSONRecord(error)) {
      if (typeof error.message === 'string') message = error.message
      if (typeof error.code === 'string') code = error.code
    }
  } catch {
    // Keep the bounded status message.
  }
  if (response.status === 403 && code === 'step_up_required') {
    return new StepUpRequiredError(message)
  }
  return new Error(message)
}

export async function requestJSON<T>(
  input: RequestInfo | URL,
  init: RequestInit | undefined,
  fallback: string,
  contract: JSONContract,
): Promise<T> {
  const response = await consoleFetch(input, init)
  if (!response.ok) throw await responseError(response, fallback)
  const value: unknown = await response.json()
  validateJSONContract(value, contract)
  return value as T
}

export function parseJSON<T>(
  input: string,
  contract: JSONContract,
): T {
  const value: unknown = JSON.parse(input)
  validateJSONContract(value, contract)
  return value as T
}

export function isJSONRecord(
  value: unknown,
): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function validateJSONContract(
  value: unknown,
  contract: JSONContract,
): asserts value is Record<string, unknown> {
  if (!isJSONRecord(value)) {
    throw new Error('JSON response must be an object.')
  }
  for (const key of contract.required ?? []) {
    if (!(key in value)) throw new Error(`JSON response is missing ${key}.`)
  }
  validateProperties(value, contract.arrays, Array.isArray, 'an array')
  validateProperties(value, contract.strings, (item) => typeof item === 'string', 'a string')
  validateProperties(value, contract.numbers, (item) => typeof item === 'number', 'a number')
  validateProperties(value, contract.booleans, (item) => typeof item === 'boolean', 'a boolean')
}

function validateProperties(
  value: Record<string, unknown>,
  keys: readonly string[] | undefined,
  valid: (item: unknown) => boolean,
  expected: string,
) {
  for (const key of keys ?? []) {
    if (!valid(value[key])) {
      throw new Error(`JSON response property ${key} must be ${expected}.`)
    }
  }
}

export async function downloadAuthenticated(
  url: string,
  suggestedName?: string,
): Promise<void> {
  const response = await consoleFetch(url, {
    headers: { Accept: 'application/octet-stream' },
  })
  if (!response.ok) throw await responseError(response, 'Download failed')
  const declaredSize = Number(response.headers.get('Content-Length') ?? 0)
  const maximumBytes = 512 * 1024 * 1024
  if (declaredSize > maximumBytes) throw new Error('Download exceeds the 512 MiB client limit.')
  const blob = await response.blob()
  if (blob.size > maximumBytes) throw new Error('Download exceeds the 512 MiB client limit.')
  const disposition = response.headers.get('Content-Disposition') ?? ''
  const headerName = /filename="([^"]+)"/i.exec(disposition)?.[1]
  const name = suggestedName || headerName || 'multirunner-download'
  const objectURL = URL.createObjectURL(blob)
  try {
    const anchor = document.createElement('a')
    anchor.href = objectURL
    anchor.download = name
    anchor.hidden = true
    document.body.append(anchor)
    anchor.click()
    anchor.remove()
  } finally {
    URL.revokeObjectURL(objectURL)
  }
}

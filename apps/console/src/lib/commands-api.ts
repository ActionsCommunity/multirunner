import { requestJSON } from './console-fetch'
import { assertMutationAllowed } from './offline-policy'

export interface CommandInput {
  type: string
  target_type: string
  target_id: string
  parameters: Record<string, unknown>
  reason: string
  confirmation: string
}

export interface CommandPlan {
  conflict_domain: string
  confirmation_phrase?: string
  reason_required: boolean
  impact: string
}

export interface OperatorCommand {
  id: string
  type: string
  target_type: string
  target_id: string
  state: string
  reason: string
  error?: string
  outcome?: Record<string, unknown>
  created_at: string
  completed_at?: string
}

interface ConsoleSession {
  actor_id: string
  csrf_token: string
}

export async function previewCommand(
  input: CommandInput,
  signal?: AbortSignal,
): Promise<CommandPlan> {
  assertMutationAllowed()
  const session = await getSession(signal)
  return requestJSON<CommandPlan>(
    '/api/v1/commands/preview',
    {
      method: 'POST',
      headers: mutationHeaders(session.csrf_token),
      body: JSON.stringify(input),
      signal,
    },
    'Command preview failed',
    { required: ['conflict_domain', 'reason_required', 'impact'] },
  )
}

export async function createCommand(
  input: CommandInput,
  idempotencyKey: string,
  signal?: AbortSignal,
): Promise<OperatorCommand> {
  assertMutationAllowed()
  const session = await getSession(signal)
  return requestJSON<OperatorCommand>(
    '/api/v1/commands',
    {
      method: 'POST',
      headers: {
        ...mutationHeaders(session.csrf_token),
        'Idempotency-Key': idempotencyKey,
      },
      body: JSON.stringify(input),
      signal,
    },
    'Command creation failed',
    { required: ['id', 'type', 'state', 'created_at'] },
  )
}

export function getCommand(
  id: string,
  signal?: AbortSignal,
): Promise<OperatorCommand> {
  return requestJSON<OperatorCommand>(
    `/api/v1/commands/${encodeURIComponent(id)}`,
    { signal },
    'Command status request failed',
    { required: ['id', 'type', 'state', 'created_at'] },
  )
}

export function newIdempotencyKey() {
  if (typeof globalThis.crypto?.randomUUID === 'function') {
    return globalThis.crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}

async function getSession(signal?: AbortSignal): Promise<ConsoleSession> {
  return requestJSON<ConsoleSession>(
    '/api/v1/session',
    { signal },
    'Console session request failed',
    { required: ['actor_id', 'csrf_token'], strings: ['actor_id', 'csrf_token'] },
  )
}

function mutationHeaders(csrfToken: string) {
  return {
    Accept: 'application/json',
    'Content-Type': 'application/json',
    'X-CSRF-Token': csrfToken,
  }
}

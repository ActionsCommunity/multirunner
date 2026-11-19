export class OfflineMutationError extends Error {
  readonly code = 'offline_mutation_denied'

  constructor() {
    super('Mutations are disabled while the console is offline.')
    this.name = 'OfflineMutationError'
  }
}

export function assertMutationAllowed() {
  if (typeof navigator !== 'undefined' && !navigator.onLine) {
    throw new OfflineMutationError()
  }
}

export interface EventBatcher<T> {
  push: (item: T) => void
  close: () => void
}

export function createEventBatcher<T>(
  flush: (items: T[]) => void,
  delay = 50,
): EventBatcher<T> {
  let queued: T[] = []
  let timer: number | undefined

  const flushQueued = () => {
    timer = undefined
    if (!queued.length) return
    const items = queued
    queued = []
    flush(items)
  }

  return {
    push(item) {
      queued.push(item)
      timer ??= window.setTimeout(flushQueued, delay)
    },
    close() {
      if (timer !== undefined) {
        window.clearTimeout(timer)
      }
      timer = undefined
      queued = []
    },
  }
}

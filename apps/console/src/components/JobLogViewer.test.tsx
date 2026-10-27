import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { expect, test, vi } from 'vitest'
import { JobLogViewer } from './JobLogViewer'

test('windows an 8 MiB masked log and debounces filtering', async () => {
  const lineCount = 8192
  const content = Array.from({ length: lineCount }, (_, index) => {
    const marker =
      index === 0
        ? `${String.fromCharCode(27)}[31mmasked ***${String.fromCharCode(27)}[0m`
        : index === lineCount - 1
          ? 'unique-final-line'
          : `ordinary-line-${index}`
    return marker.padEnd(1023, '.')
  }).join('\n')
  const onClose = vi.fn()

  const { container } = render(
    <JobLogViewer
      jobName="large job"
      steps={['Run tests']}
      content={content}
      onClose={onClose}
    />,
  )

  expect(new TextEncoder().encode(content).byteLength).toBeGreaterThanOrEqual(
    8 * 1024 * 1024 - 1,
  )
  expect(container.querySelectorAll('.log-line')).toHaveLength(500)
  expect(container.querySelector('.ansi-red')).toHaveTextContent('masked ***')
  expect(screen.getByText(/Showing 500 of 8,192 matching lines/)).toBeVisible()

  fireEvent.click(
    screen.getByRole('button', { name: 'Show next 500 lines' }),
  )
  expect(container.querySelectorAll('.log-line')).toHaveLength(1000)

  fireEvent.change(screen.getByLabelText('Search this log'), {
    target: { value: 'unique-final-line' },
  })
  expect(container.querySelectorAll('.log-line')).toHaveLength(500)
  await waitFor(() =>
    expect(container.querySelectorAll('.log-line')).toHaveLength(1),
  )
  expect(screen.getByText(/Showing 1 of 1 matching lines/)).toBeVisible()

  fireEvent.click(screen.getByRole('button', { name: 'Close and discard' }))
  expect(onClose).toHaveBeenCalledOnce()
})

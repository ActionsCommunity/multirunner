import { memo, useEffect, useMemo, useState } from 'react'
import {
  ansiSegments,
  filterLogLines,
  prepareLog,
} from '../lib/log-rendering'

const lineWindowSize = 500
const searchDebounceMilliseconds = 200

interface JobLogViewerProps {
  jobName: string
  steps: string[]
  content: string
  onClose: () => void
}

function useDebouncedValue<T>(value: T, delay: number) {
  const [debounced, setDebounced] = useState(value)

  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), delay)
    return () => window.clearTimeout(timer)
  }, [delay, value])

  return debounced
}

function JobLogViewerComponent({
  jobName,
  steps,
  content,
  onClose,
}: JobLogViewerProps) {
  const [query, setQuery] = useState('')
  const [wrap, setWrap] = useState(false)
  const [visibleLineCount, setVisibleLineCount] = useState(lineWindowSize)
  const debouncedQuery = useDebouncedValue(
    query,
    searchDebounceMilliseconds,
  )
  const prepared = useMemo(() => prepareLog(content), [content])
  const matchingLines = useMemo(
    () => filterLogLines(prepared, debouncedQuery),
    [debouncedQuery, prepared],
  )
  const visibleLines = matchingLines.slice(0, visibleLineCount)
  const hasMore = visibleLines.length < matchingLines.length
  const summaryID = 'job-log-summary'

  const updateQuery = (value: string) => {
    setQuery(value)
    setVisibleLineCount(lineWindowSize)
  }

  return (
    <section className="job-log-viewer" aria-labelledby="job-log-title">
      <div className="job-log-heading">
        <div>
          <span className="section-label">Transient · never retained</span>
          <h3 id="job-log-title">{jobName} log</h3>
        </div>
        <button type="button" onClick={onClose}>
          Close and discard
        </button>
      </div>
      <div className="log-controls">
        <label>
          Search this log
          <input
            name="job_log_search"
            type="search"
            value={query}
            onChange={(event) => updateQuery(event.target.value)}
          />
        </label>
        <label className="wrap-control">
          <input
            name="job_log_wrap"
            type="checkbox"
            checked={wrap}
            onChange={(event) => setWrap(event.target.checked)}
          />
          Wrap lines
        </label>
      </div>
      {steps.length ? (
        <div className="step-filters" aria-label="Filter log by step">
          {steps.map((step) => (
            <button
              key={step}
              type="button"
              onClick={() => updateQuery(step)}
            >
              {step}
            </button>
          ))}
        </div>
      ) : null}
      <pre
        className={wrap ? 'wrap' : ''}
        aria-label={`Masked transient log for ${jobName}`}
        aria-describedby={summaryID}
        tabIndex={0}
      >
        <code>
          {visibleLines.map((line) => (
            <span className="log-line" key={line.index}>
              {ansiSegments(line.raw).map((segment, index) => (
                <span
                  className={segment.className || undefined}
                  key={`${index}:${segment.text.length}`}
                >
                  {segment.text}
                </span>
              ))}
            </span>
          ))}
        </code>
      </pre>
      {!matchingLines.length ? (
        <p className="empty-state log-empty">No matching log lines.</p>
      ) : null}
      {hasMore ? (
        <button
          className="log-more"
          type="button"
          onClick={() =>
            setVisibleLineCount((count) => count + lineWindowSize)
          }
        >
          Show next {Math.min(lineWindowSize, matchingLines.length - visibleLines.length)} lines
        </button>
      ) : null}
      <p className="log-summary" id={summaryID} aria-live="polite">
        Showing {visibleLines.length.toLocaleString()} of{' '}
        {matchingLines.length.toLocaleString()} matching lines ·{' '}
        {prepared.lines.length.toLocaleString()} total · masked source{' '}
        {(prepared.characters / (1024 * 1024)).toFixed(1)} MiB
      </p>
    </section>
  )
}

export const JobLogViewer = memo(JobLogViewerComponent)

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type MouseEvent,
} from 'react'
import './App.css'
import { JobLogViewer } from './components/JobLogViewer'
import {
  LiveEventLedger,
  RunnerSessions,
} from './components/LiveOperations'
import {
  getOverview,
  type Overview,
  type RecentRun,
} from './lib/history-api'
import {
  getOperationalRunners,
  openOperationalStream,
  type OperationalEvent,
  type RunnerState,
  type StreamState,
} from './lib/operations-api'
import {
  createCommand,
  getCommand,
  newIdempotencyKey,
  previewCommand,
  type CommandPlan,
  type OperatorCommand,
} from './lib/commands-api'
import {
  getConfiguration,
  getDiagnostics,
  type ConfigurationSnapshot,
  type DiagnosticReport,
} from './lib/operator-api'
import {
  createSavedView,
  createSearchExport,
  deleteSavedView,
  listSavedViews,
  searchHistory,
  type SavedView,
  type SearchExport,
  type SearchResult,
} from './lib/search-api'
import {
  getRunInspection,
  type RunInspection,
} from './lib/runs-api'
import { getJobLog } from './lib/logs-api'
import {
  getAnalytics,
  type AnalyticsReport,
} from './lib/analytics-api'
import {
  acknowledgeAlert,
  annotateAlert,
  listAlerts,
  resolveAlert,
  silenceAlert,
  type AlertInstance,
  type AlertState,
} from './lib/alerts-api'
import {
  listBackups,
  type BackupMetadata,
} from './lib/backups-api'
import {
  listRestores,
  type RestoreMetadata,
} from './lib/restores-api'
import {
  inspectUpdates,
  listUpdates,
  type UpdateInspection,
  type UpdateMetadata,
} from './lib/updates-api'
import { createEventBatcher } from './lib/event-batcher'
import { applyOperationalEventBatch } from './lib/operations-state'
import { downloadAuthenticated } from './lib/console-fetch'
import {
  getSystemStatus,
  listAuditRecords,
  listPools,
  type AuditRecord,
  type NamedResource,
  type SystemStatus,
} from './lib/operator-resources-api'
import {
  getSnapshotSession,
  purgeOperationalSnapshot,
  readOperationalSnapshot,
  saveOperationalSnapshot,
  snapshotAgeLabel,
  snapshotMatchesSession,
  summarizeRunners,
  type OperationalSnapshot,
  type SnapshotSession,
} from './lib/offline-snapshot'

const navigation = [
  ['Overview', 'overview'],
  ['Live', 'live'],
  ['Runs', 'runs'],
  ['Repositories', 'repositories'],
  ['Workflows', 'workflows'],
  ['Pools', 'pools'],
  ['Runners', 'runners'],
  ['Host', 'host'],
  ['Backups', 'backups'],
  ['Alerts', 'alerts'],
  ['Diagnostics', 'diagnostics'],
  ['Configuration', 'configuration'],
  ['Audit', 'audit'],
] as const

type NavigationView = (typeof navigation)[number][1]

function routeForView(view: NavigationView) {
  return view === 'overview' ? '/' : `/${view}`
}

const legacyUnavailableViews = new Set<NavigationView>([
  'live',
  'repositories',
  'workflows',
  'runners',
  'backups',
  'alerts',
  'diagnostics',
  'configuration',
])

const navigationIconPaths: Record<NavigationView, string> = {
  overview: 'M4 13h6V4H4v9Zm10 7h6V11h-6v9ZM4 20h6v-3H4v3Zm10-13h6V4h-6v3Z',
  live: 'M4 12h3l2-5 4 10 2-5h5',
  runs: 'M8 5v14l11-7L8 5Z',
  repositories: 'M4 5h6l2 2h8v12H4V5Z',
  workflows: 'M6 5h5v5H6V5Zm7 9h5v5h-5v-5ZM8.5 10v4H15',
  pools: 'M5 7h14M5 12h14M5 17h14M8 5v14M16 5v14',
  runners: 'M8 7a4 4 0 1 0 8 0 4 4 0 0 0-8 0Zm-3 13c0-4 3-6 7-6s7 2 7 6',
  host: 'M5 4h14v6H5V4Zm0 10h14v6H5v-6Zm3-7h.01M8 17h.01',
  backups: 'M12 4a8 8 0 1 1-7.4 5M4 4v5h5M12 8v5l3 2',
  alerts: 'M12 4 3 20h18L12 4Zm0 6v4m0 3h.01',
  diagnostics: 'M4 12h4l2-6 4 12 2-6h4',
  configuration: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8Zm0-5v3m0 12v3M3 12h3m12 0h3M5.6 5.6l2.1 2.1m8.6 8.6 2.1 2.1m0-12.8-2.1 2.1m-8.6 8.6-2.1 2.1',
  audit: 'M5 4h14v16H5V4Zm4 4h6M9 12h6M9 16h4',
}

function NavigationIcon({ icon }: { icon: NavigationView }) {
  return (
    <svg
      className="nav-glyph"
      data-icon={icon}
      data-testid="navigation-icon"
      viewBox="0 0 24 24"
      aria-hidden="true"
    >
      <path d={navigationIconPaths[icon]} />
    </svg>
  )
}

function viewFromLocation(): NavigationView {
  const candidate = window.location.pathname.split('/').filter(Boolean)[0]
  return navigation.some(([, id]) => id === candidate)
    ? (candidate as NavigationView)
    : 'overview'
}

function runTargetFromLocation() {
  if (window.location.pathname !== '/runs') return null
  const parameters = new URLSearchParams(window.location.search)
  const repository = parameters.get('repository')?.trim() ?? ''
  const runID = parameters.get('run_id')?.trim() ?? ''
  return repository && /^\d+$/.test(runID) ? { repository, runID } : null
}

function alertTargetFromLocation() {
  if (window.location.pathname !== '/alerts') return ''
  return new URLSearchParams(window.location.search).get('alert_id')?.trim() ?? ''
}

const numberFormat = new Intl.NumberFormat()
const previewMode = import.meta.env.MODE === 'demo'
const legacyBackend = import.meta.env.MODE === 'machine-legacy'

function formatNumber(value: number) {
  return numberFormat.format(value)
}

function formatBytes(value = 0) {
  if (value < 1024) return `${value} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  let amount = value
  let index = -1
  do {
    amount /= 1024
    index += 1
  } while (amount >= 1024 && index < units.length - 1)
  return `${amount.toFixed(amount >= 10 ? 0 : 1)} ${units[index]}`
}

function successRate(overview: Overview) {
  if (overview.workflow_jobs === 0) {
    return '—'
  }
  return `${Math.round((overview.successful_jobs / overview.workflow_jobs) * 100)}%`
}

function formatDuration(seconds: number) {
  if (seconds <= 0) {
    return '—'
  }
  const minutes = Math.floor(seconds / 60)
  const remainder = Math.round(seconds % 60)
  return minutes > 0 ? `${minutes}m ${remainder}s` : `${remainder}s`
}

function formatRelativeTime(value: string) {
  const timestamp = new Date(value).getTime()
  const elapsed = Math.max(0, Date.now() - timestamp)
  const minutes = Math.round(elapsed / 60_000)
  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  return `${Math.round(hours / 24)}d ago`
}

function conclusionLabel(run: RecentRun) {
  return run.conclusion || run.status || 'unknown'
}

function eventLabel(value: string) {
  return value.replaceAll('.', ' ').replaceAll('_', ' ')
}

function displayValue(value: unknown) {
  if (value === null || value === undefined || value === '') return '—'
  if (typeof value === 'object') return JSON.stringify(value)
  return String(value)
}

function localDateTimeValue(value: Date) {
  const offset = value.getTimezoneOffset() * 60_000
  return new Date(value.getTime() - offset).toISOString().slice(0, 16)
}

function supportBundleDownload(command?: OperatorCommand) {
  const value = command?.outcome?.download_url
  return typeof value === 'string' &&
    value.startsWith('/api/v1/support-bundles/')
    ? value
    : ''
}

function backupDownload(command?: OperatorCommand) {
  const value = command?.outcome?.download_url
  return typeof value === 'string' && value.startsWith('/api/v1/backups/')
    ? value
    : ''
}

const terminalCommandStates = new Set([
  'succeeded',
  'failed',
  'cancelled',
  'interrupted',
  'unknown_outcome',
])

interface CommandDialogState {
  runner: RunnerState
  plan?: CommandPlan
  reason: string
  confirmation: string
  idempotencyKey: string
  command?: OperatorCommand
  error?: string
  loading: boolean
  submitting: boolean
}

interface SupportBundleDialogState {
  plan?: CommandPlan
  from: string
  to: string
  reason: string
  confirmation: string
  idempotencyKey: string
  command?: OperatorCommand
  error?: string
  loading: boolean
  submitting: boolean
}

interface BackupDialogState {
  plan?: CommandPlan
  reason: string
  confirmation: string
  idempotencyKey: string
  command?: OperatorCommand
  error?: string
  loading: boolean
  submitting: boolean
}

interface RestoreDialogState extends BackupDialogState {
  backup: BackupMetadata
}

interface UpdateDialogState extends BackupDialogState {
  inspection: UpdateInspection
}

interface AppProps {
  pwaUpdate?: {
    ready: boolean
    updateSW: () => Promise<void>
    retryRegistration?: () => void
    registrationError?: string
  }
}

interface ViewLoadState<T> {
  status: 'idle' | 'loading' | 'ready' | 'empty' | 'degraded' | 'error'
  data?: T
  message?: string
}

function SnapshotProvenance({
  snapshot,
  isOnline,
}: {
  snapshot?: OperationalSnapshot
  isOnline: boolean
}) {
  if (isOnline) {
    return (
      <div className="snapshot-provenance current" role="status">
        <strong>Current</strong>
        <span>Source: Local console API</span>
      </div>
    )
  }
  if (!snapshot) {
    return (
      <div className="snapshot-provenance unavailable" role="status">
        <strong>Offline · no snapshot</strong>
        <span>No operational data is stored for offline display.</span>
      </div>
    )
  }
  return (
    <div className="snapshot-provenance stale" role="status">
      <strong>Stale offline snapshot</strong>
      <span>
        Source: {snapshot.source} · Saved {snapshotAgeLabel(snapshot.saved_at)}
      </span>
    </div>
  )
}

function UnknownOutcomeDetails({
  command,
  plan,
  target,
}: {
  command: OperatorCommand
  plan?: CommandPlan
  target: string
}) {
  if (command.state !== 'unknown_outcome') return null
  return (
    <section className="unknown-outcome" role="alert">
      <h3>Outcome Unknown</h3>
      <p>
        Multirunner cannot prove whether the external effect completed. The
        target remains blocked until an operator verifies its actual state.
      </p>
      <dl>
        <div>
          <dt>Blocked target</dt>
          <dd>{target}</dd>
        </div>
        <div>
          <dt>Conflict domain</dt>
          <dd>{plan?.conflict_domain || 'Unavailable'}</dd>
        </div>
        <div>
          <dt>Evidence</dt>
          <dd>
            {command.error ||
              (command.outcome
                ? JSON.stringify(command.outcome)
                : 'No conclusive completion evidence was recorded.')}
          </dd>
        </div>
      </dl>
      <p>
        Verify the target in Host, Runners, and Audit. Reconcile the underlying
        runtime or service manually before clearing the blocked command domain.
      </p>
    </section>
  )
}

const focusableSelector = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function useModalBehavior(
  open: boolean,
  onClose: () => void,
  canDismiss = true,
) {
  const dialogRef = useRef<HTMLElement>(null)
  const closeRef = useRef(onClose)
  const restoreFocusRef = useRef<HTMLElement | null>(null)

  useEffect(() => {
    closeRef.current = onClose
  }, [onClose])

  useEffect(() => {
    if (!open) return
    const dialog = dialogRef.current
    if (!dialog) return
    restoreFocusRef.current =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null

    const focusDialog = window.requestAnimationFrame(() => {
      const initial = dialog.querySelector<HTMLElement>(
        '[data-modal-initial-focus]',
      )
      const target =
        initial ??
        dialog.querySelector<HTMLElement>(focusableSelector) ??
        dialog
      target.focus()
    })
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && canDismiss) {
        event.preventDefault()
        closeRef.current()
        return
      }
      if (event.key !== 'Tab') return
      const focusable = Array.from(
        dialog.querySelectorAll<HTMLElement>(focusableSelector),
      )
      if (!focusable.length) {
        event.preventDefault()
        dialog.focus()
        return
      }
      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      const activeIndex = focusable.indexOf(
        document.activeElement as HTMLElement,
      )
      if (
        event.shiftKey &&
        (document.activeElement === first || activeIndex < 0)
      ) {
        event.preventDefault()
        last.focus()
      } else if (
        !event.shiftKey &&
        (document.activeElement === last || activeIndex < 0)
      ) {
        event.preventDefault()
        first.focus()
      } else if (!dialog.contains(document.activeElement)) {
        event.preventDefault()
        first.focus()
      }
    }
    dialog.addEventListener('keydown', handleKeyDown)
    return () => {
      window.cancelAnimationFrame(focusDialog)
      dialog.removeEventListener('keydown', handleKeyDown)
      restoreFocusRef.current?.focus()
      restoreFocusRef.current = null
    }
  }, [canDismiss, open])

  return dialogRef
}

function App({ pwaUpdate }: AppProps = {}) {
  const [activeView, setActiveView] =
    useState<NavigationView>(viewFromLocation)
  const [locationRevision, setLocationRevision] = useState(0)
  const routeFocusPending = useRef(false)
  const pageHeadingRef = useRef<HTMLHeadingElement>(null)
  const loadedViews = useRef(new Set<string>())
  const routeLoads = useRef(new Map<string, Promise<unknown>>())
  const [isOnline, setIsOnline] = useState(() => navigator.onLine)
  const [offlineSnapshot, setOfflineSnapshot] = useState(() =>
    readOperationalSnapshot(),
  )
  const [snapshotSession, setSnapshotSession] =
    useState<SnapshotSession | null>(null)
  const [pwaUpdateError, setPwaUpdateError] = useState('')
  const [pwaUpdating, setPwaUpdating] = useState(false)
  const [paletteOpen, setPaletteOpen] = useState(false)
  const [paletteQuery, setPaletteQuery] = useState('')
  const [navigationOpen, setNavigationOpen] = useState(false)
  const navigationCloseRef = useRef<HTMLButtonElement>(null)
  const [state, setState] = useState<{
    data?: { overview: Overview; runs: RecentRun[] }
    error?: string
    loading: boolean
  }>(() => ({
    data:
      !navigator.onLine && offlineSnapshot
        ? { overview: offlineSnapshot.overview, runs: [] }
        : undefined,
    error:
      !navigator.onLine && !offlineSnapshot
        ? 'No offline operational snapshot is available.'
        : undefined,
    loading: navigator.onLine,
  }))
  const [stream, setStream] = useState<{
    status: StreamState
    lastEvent?: OperationalEvent
    received: number
  }>({
    status: legacyBackend ? 'disconnected' : 'connecting',
    received: 0,
  })
  const [operations, setOperations] = useState<{
    runners: RunnerState[]
    events: OperationalEvent[]
    error?: string
    loading: boolean
  }>({
    runners: [],
    events: [],
    error: legacyBackend
      ? 'Live runner state requires a newer installed Multirunner backend.'
      : undefined,
    loading: !legacyBackend,
  })
  const [commandDialog, setCommandDialog] =
    useState<CommandDialogState | null>(null)
  const commandRunnerID = commandDialog?.runner.id
  const commandRunnerPool = commandDialog?.runner.pool
  const [diagnostics, setDiagnostics] = useState<{
    report?: DiagnosticReport
    error?: string
    loading: boolean
  }>({ loading: false })
  const [configuration, setConfiguration] = useState<{
    snapshot?: ConfigurationSnapshot
    error?: string
    loading: boolean
  }>({ loading: false })
  const [pools, setPools] = useState<ViewLoadState<NamedResource[]>>({
    status: 'idle',
  })
  const [systemStatus, setSystemStatus] =
    useState<ViewLoadState<SystemStatus>>({ status: 'idle' })
  const [audit, setAudit] = useState<ViewLoadState<AuditRecord[]>>({
    status: 'idle',
  })
  const [supportBundleDialog, setSupportBundleDialog] =
    useState<SupportBundleDialogState | null>(null)
  const [backupDialog, setBackupDialog] =
    useState<BackupDialogState | null>(null)
  const [restoreDialog, setRestoreDialog] =
    useState<RestoreDialogState | null>(null)
  const [updateDialog, setUpdateDialog] =
    useState<UpdateDialogState | null>(null)
  const [backups, setBackups] = useState<{
    items: BackupMetadata[]
    loading: boolean
    error?: string
  }>({ items: [], loading: false })
  const [restores, setRestores] = useState<{
    items: RestoreMetadata[]
    loading: boolean
    error?: string
  }>({ items: [], loading: false })
  const [updates, setUpdates] = useState<{
    items: UpdateMetadata[]
    inspection?: UpdateInspection
    loading: boolean
    checking: boolean
    error?: string
  }>({
    items: [],
    loading: false,
    checking: false,
  })
  const [search, setSearch] = useState<{
    query: string
    entityType: string
    items: SearchResult[]
    loading: boolean
    submitted: boolean
    error?: string
  }>({
    query: '',
    entityType: '',
    items: [],
    loading: false,
    submitted: false,
  })
  const [savedViews, setSavedViews] = useState<{
    items: SavedView[]
    name: string
    loading: boolean
    saving: boolean
    deleting: boolean
    error?: string
  }>({
    items: [],
    name: '',
    loading: !legacyBackend,
    saving: false,
    deleting: false,
  })
  const [selectedSavedViewID, setSelectedSavedViewID] = useState('')
  const searchInputRef = useRef<HTMLInputElement>(null)
  const [searchExport, setSearchExport] = useState<{
    metadata?: SearchExport
    loading: boolean
    error?: string
  }>({ loading: false })
  const [downloadNotice, setDownloadNotice] = useState<{
    kind: 'status' | 'error'
    message: string
  } | null>(null)
  const [runInspection, setRunInspection] = useState<{
    data?: RunInspection
    loading: boolean
    error?: string
  }>({ loading: false })
  const [jobLog, setJobLog] = useState<{
    jobID: number
    jobName: string
    steps: string[]
    content?: string
    loading: boolean
    error?: string
  } | null>(null)
  const [analytics, setAnalytics] = useState<{
    repositories?: AnalyticsReport
    workflows?: AnalyticsReport
    loading?: 'repository' | 'workflow'
    error?: string
  }>({})
  const [incidents, setIncidents] = useState<{
    items: AlertInstance[]
    filter: AlertState | ''
    selectedID: string
    reason: string
    annotation: string
    silenceUntil: string
    loading: boolean
    submitting?: 'acknowledge' | 'annotate' | 'silence' | 'resolve'
    error?: string
  }>(() => ({
    items: [],
    filter: 'open',
    selectedID: alertTargetFromLocation(),
    reason: '',
    annotation: '',
    silenceUntil: localDateTimeValue(new Date(Date.now() + 60 * 60 * 1000)),
    loading: false,
  }))
  const hasOpenDialog = Boolean(
    paletteOpen ||
      commandDialog ||
      backupDialog ||
      restoreDialog ||
      updateDialog ||
      supportBundleDialog,
  )
  const paletteModalRef = useModalBehavior(
    paletteOpen,
    () => setPaletteOpen(false),
  )
  const commandModalRef = useModalBehavior(
    commandDialog !== null,
    () => setCommandDialog(null),
    !commandDialog?.submitting,
  )
  const backupModalRef = useModalBehavior(
    backupDialog !== null,
    () => setBackupDialog(null),
    !backupDialog?.submitting,
  )
  const restoreModalRef = useModalBehavior(
    restoreDialog !== null,
    () => setRestoreDialog(null),
    !restoreDialog?.submitting,
  )
  const updateModalRef = useModalBehavior(
    updateDialog !== null,
    () => setUpdateDialog(null),
    !updateDialog?.submitting,
  )
  const supportBundleModalRef = useModalBehavior(
    supportBundleDialog !== null,
    () => setSupportBundleDialog(null),
    !supportBundleDialog?.submitting,
  )

  const loadIncidents = (filter: AlertState | '' = incidents.filter) => {
    setIncidents((current) => ({
      ...current,
      filter,
      loading: true,
      error: undefined,
    }))
    void listAlerts(filter)
      .then((response) =>
        {
          loadedViews.current.add('alerts')
          setIncidents((current) => ({
            ...current,
            items: response.items,
            selectedID: response.items.some(
              (item) => item.id === current.selectedID,
            )
              ? current.selectedID
              : response.items[0]?.id ?? '',
            loading: false,
          }))
        },
      )
      .catch((error: unknown) =>
        setIncidents((current) => ({
          ...current,
          loading: false,
          error:
            error instanceof Error ? error.message : 'Alerts are unavailable.',
        })),
      )
  }

  const loadBackups = () => {
    setBackups((current) => ({ ...current, loading: true, error: undefined }))
    setRestores((current) => ({ ...current, loading: true, error: undefined }))
    setUpdates((current) => ({ ...current, loading: true, error: undefined }))
    void Promise.all([listBackups(), listRestores(), listUpdates()])
      .then(([backupResponse, restoreResponse, updateResponse]) => {
        loadedViews.current.add('backups')
        setBackups({ items: backupResponse.items, loading: false })
        setRestores({ items: restoreResponse.items, loading: false })
        setUpdates((current) => ({
          ...current,
          items: updateResponse.items,
          loading: false,
        }))
      })
      .catch((error: unknown) => {
        const message =
          error instanceof Error
            ? error.message
            : 'Recovery data is unavailable.'
        setBackups((current) => ({ ...current, loading: false, error: message }))
        setRestores((current) => ({ ...current, loading: false, error: message }))
        setUpdates((current) => ({ ...current, loading: false, error: message }))
      })
  }

  const inspectAvailableUpdate = () => {
    setUpdates((current) => ({
      ...current,
      checking: true,
      error: undefined,
    }))
    void inspectUpdates()
      .then((inspection) =>
        setUpdates((current) => ({
          ...current,
          inspection,
          checking: false,
        })),
      )
      .catch((error: unknown) =>
        setUpdates((current) => ({
          ...current,
          checking: false,
          error:
            error instanceof Error
              ? error.message
              : 'Update metadata is unavailable.',
        })),
      )
  }

  const openBackupDialog = () => {
    if (!isOnline) return
    const next: BackupDialogState = {
      reason: '',
      confirmation: '',
      idempotencyKey: newIdempotencyKey(),
      loading: true,
      submitting: false,
    }

    setBackupDialog(next)
    void previewCommand({
      type: 'backup.create',
      target_type: 'system',
      target_id: 'backups',
      parameters: { purpose: 'manual' },
      reason: '',
      confirmation: '',
    })
      .then((plan) =>
        setBackupDialog((current) =>
          current ? { ...current, plan, loading: false } : current,
        ),
      )
      .catch((error: unknown) =>
        setBackupDialog((current) =>
          current
            ? {
                ...current,
                loading: false,
                error:
                  error instanceof Error
                    ? error.message
                    : 'Backup preview is unavailable.',
              }
            : current,
        ),
      )
  }

  const openRestoreDialog = (backup: BackupMetadata) => {
    if (!isOnline) return
    const next: RestoreDialogState = {
      backup,
      reason: '',
      confirmation: '',
      idempotencyKey: newIdempotencyKey(),
      loading: true,
      submitting: false,
    }
    setRestoreDialog(next)
    void previewCommand({
      type: 'restore.stage',
      target_type: 'system',
      target_id: 'restore',
      parameters: { backup_id: backup.id },
      reason: '',
      confirmation: '',
    })
      .then((plan) =>
        setRestoreDialog((current) =>
          current ? { ...current, plan, loading: false } : current,
        ),
      )
      .catch((error: unknown) =>
        setRestoreDialog((current) =>
          current
            ? {
                ...current,
                loading: false,
                error:
                  error instanceof Error
                    ? error.message
                    : 'Restore preview is unavailable.',
              }
            : current,
        ),
      )
  }

  const openUpdateDialog = (inspection: UpdateInspection) => {
    if (!isOnline) return
    const next: UpdateDialogState = {
      inspection,
      reason: '',
      confirmation: '',
      idempotencyKey: newIdempotencyKey(),
      loading: true,
      submitting: false,
    }
    setUpdateDialog(next)
    void previewCommand({
      type: 'update.stage',
      target_type: 'system',
      target_id: 'updates',
      parameters: { version: inspection.version },
      reason: '',
      confirmation: '',
    })
      .then((plan) =>
        setUpdateDialog((current) =>
          current ? { ...current, plan, loading: false } : current,
        ),
      )
      .catch((error: unknown) =>
        setUpdateDialog((current) =>
          current
            ? {
                ...current,
                loading: false,
                error:
                  error instanceof Error
                    ? error.message
                    : 'Update preview is unavailable.',
              }
            : current,
        ),
      )
  }

  const activateView = (view: NavigationView) => {
    const route = routeForView(view)
    if (
      window.location.pathname !== route ||
      window.location.search ||
      window.location.hash
    ) {
      window.history.pushState(null, '', route)
    }
    if (view === 'runs') {
      setRunInspection({ loading: false })
    }
    routeFocusPending.current = true
    setActiveView(view)
    setNavigationOpen(false)
    setPaletteOpen(false)
  }

  const handleNavigation = (
    event: MouseEvent<HTMLAnchorElement>,
    view: NavigationView,
  ) => {
    if (
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    ) {
      return
    }
    event.preventDefault()
    activateView(view)
  }

  const focusSearchInput = () => {
    window.requestAnimationFrame(() => searchInputRef.current?.focus())
  }

  const startDownload = async (
    url: string,
    suggestedName: string | undefined,
    label: string,
  ) => {
    setDownloadNotice({ kind: 'status', message: `Downloading ${label}…` })
    try {
      await downloadAuthenticated(url, suggestedName)
      setDownloadNotice({
        kind: 'status',
        message: `${label} download started.`,
      })
    } catch (error: unknown) {
      setDownloadNotice({
        kind: 'error',
        message:
          error instanceof Error
            ? error.message
            : `${label} could not be downloaded.`,
      })
    }
  }

  const paletteRoutes = useMemo(() => {
    const query = paletteQuery.trim().toLocaleLowerCase()
    return navigation
      .filter(([label]) => !query || label.toLocaleLowerCase().includes(query))
      .slice(0, 8)
  }, [paletteQuery])

  const searchFromPalette = () => {
    const query = paletteQuery.trim()
    if (query.length < 2) return
    setSearch((current) => ({
      ...current,
      query,
      submitted: false,
      error: undefined,
    }))
    activateView('runs')
  }

  /* oxlint-disable react/set-state-in-effect -- Route changes intentionally initialize external data loads. */
  useEffect(() => {
    const online = () => {
      loadedViews.current.delete('pools')
      loadedViews.current.delete('host')
      loadedViews.current.delete('audit')
      setIsOnline(true)
      setState((current) => ({
        ...current,
        error: undefined,
        loading: true,
      }))
      setOperations((current) => ({
        ...current,
        error: undefined,
        loading: true,
      }))
    }
    const offline = () => {
      setIsOnline(false)
      const snapshot = readOperationalSnapshot()
      setOfflineSnapshot(snapshot)
      if (snapshot) {
        setState({
          data: { overview: snapshot.overview, runs: [] },
          loading: false,
        })
      } else {
        setState({
          error: 'No offline operational snapshot is available.',
          loading: false,
        })
      }
    }
    window.addEventListener('online', online)
    window.addEventListener('offline', offline)
    return () => {
      window.removeEventListener('online', online)
      window.removeEventListener('offline', offline)
    }
  }, [])

  useEffect(() => {
    const handleGlobalKeyDown = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault()
        setPaletteOpen(true)
        setPaletteQuery('')
      }
      if (event.key === 'Escape' && navigationOpen) {
        setNavigationOpen(false)
      }
    }
    window.addEventListener('keydown', handleGlobalKeyDown)
    return () => window.removeEventListener('keydown', handleGlobalKeyDown)
  }, [navigationOpen])

  useEffect(() => {
    if (!navigationOpen) return
    window.requestAnimationFrame(() => navigationCloseRef.current?.focus())
  }, [navigationOpen])

  useEffect(() => {
    if (!isOnline || legacyBackend) return
    const controller = new AbortController()
    const validate = () => {
      void getSnapshotSession(controller.signal)
        .then((session) => {
          if (controller.signal.aborted) return
          const stored = readOperationalSnapshot()
          if (stored && !snapshotMatchesSession(stored, session)) {
            purgeOperationalSnapshot()
            setOfflineSnapshot(undefined)
          } else {
            setOfflineSnapshot(stored)
          }
          setSnapshotSession(session)
        })
        .catch(() => {
          if (!controller.signal.aborted) {
            setSnapshotSession(null)
            setOfflineSnapshot(readOperationalSnapshot())
          }
        })
    }
    validate()
    window.addEventListener('focus', validate)
    const validationTimer = window.setInterval(validate, 60_000)
    return () => {
      controller.abort()
      window.removeEventListener('focus', validate)
      window.clearInterval(validationTimer)
    }
  }, [isOnline])

  useEffect(() => {
    if (!isOnline || !snapshotSession || !state.data) return
    const snapshot = saveOperationalSnapshot(
      state.data.overview,
      summarizeRunners(operations.runners, stream.lastEvent?.timestamp),
      snapshotSession,
    )
    if (snapshot) setOfflineSnapshot(snapshot)
  }, [
    isOnline,
    operations.runners,
    snapshotSession,
    state.data,
    stream.lastEvent?.timestamp,
  ])

  useEffect(() => {
    if (!offlineSnapshot) return
    const delay = Math.max(0, Date.parse(offlineSnapshot.expires_at) - Date.now())
    const expiryTimer = window.setTimeout(() => {
      purgeOperationalSnapshot()
      setOfflineSnapshot(undefined)
      if (!navigator.onLine) {
        setState({
          error: 'The offline operational snapshot expired.',
          loading: false,
        })
      }
    }, delay)
    return () => window.clearTimeout(expiryTimer)
  }, [offlineSnapshot])

  useEffect(() => {
    const handlePopState = () => {
      const view = viewFromLocation()
      if (view === 'runs' && !runTargetFromLocation()) {
        setRunInspection({ loading: false })
      }
      routeFocusPending.current = true
      setActiveView(view)
      setLocationRevision((value) => value + 1)
    }
    window.addEventListener('popstate', handlePopState)
    return () => window.removeEventListener('popstate', handlePopState)
  }, [])

  useEffect(() => {
    if (!routeFocusPending.current) return
    routeFocusPending.current = false
    const frame = window.requestAnimationFrame(() => pageHeadingRef.current?.focus())
    return () => window.cancelAnimationFrame(frame)
  }, [activeView, locationRevision])

  useEffect(() => {
    if (legacyBackend) return
    const controller = new AbortController()
    void listSavedViews(controller.signal)
      .then((response) =>
        setSavedViews((current) => ({
          ...current,
          items: response.items,
          loading: false,
        })),
      )
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setSavedViews((current) => ({
            ...current,
            loading: false,
            error:
              error instanceof Error
                ? error.message
                : 'Saved views are unavailable.',
          }))
        }
      })
    return () => controller.abort()
  }, [])

  useEffect(() => {
    if (legacyBackend && legacyUnavailableViews.has(activeView)) return
    let cancelled = false
    const loadRoute = <T,>(key: string, loader: () => Promise<T>) => {
      const existing = routeLoads.current.get(key) as Promise<T> | undefined
      if (existing) return existing
      const request = loader().catch((error: unknown) => {
        routeLoads.current.delete(key)
        throw error
      })
      routeLoads.current.set(key, request)
      return request
    }
    const fail = (error: unknown, fallback: string) =>
      error instanceof Error ? error.message : fallback

    if (activeView === 'pools') {
      if (!isOnline) {
        setPools({
          status: 'degraded',
          message:
            'Pool history is not included in the offline snapshot. Reconnect to load it.',
        })
      } else if (!loadedViews.current.has('pools')) {
        setPools({ status: 'loading' })
        void loadRoute('pools', () => listPools())
          .then((response) => {
            if (cancelled) return
            loadedViews.current.add('pools')
            setPools({
              status: response.items.length ? 'ready' : 'empty',
              data: response.items,
            })
          })
          .catch((error: unknown) => {
            if (!cancelled) {
              setPools({
                status: 'error',
                message: fail(error, 'Pool history is unavailable.'),
              })
            }
          })
      }
    } else if (activeView === 'host') {
      if (!isOnline) {
        setSystemStatus({
          status: 'degraded',
          message:
            'Detailed host status is not included in the offline snapshot. Reconnect to load it.',
        })
      } else if (!loadedViews.current.has('host')) {
        setSystemStatus({ status: 'loading' })
        void loadRoute('host', () => getSystemStatus())
          .then((response) => {
            if (cancelled) return
            loadedViews.current.add('host')
            setSystemStatus({ status: 'ready', data: response })
          })
          .catch((error: unknown) => {
            if (!cancelled) {
              setSystemStatus({
                status: 'error',
                message: fail(error, 'Host status is unavailable.'),
              })
            }
          })
      }
    } else if (activeView === 'audit') {
      if (!isOnline) {
        setAudit({
          status: 'degraded',
          message:
            'Audit history is deliberately excluded from offline storage. Reconnect to load it.',
        })
      } else if (!loadedViews.current.has('audit')) {
        setAudit({ status: 'loading' })
        void loadRoute('audit', () => listAuditRecords())
          .then((response) => {
            if (cancelled) return
            loadedViews.current.add('audit')
            setAudit({
              status: response.items.length ? 'ready' : 'empty',
              data: response.items,
            })
          })
          .catch((error: unknown) => {
            if (!cancelled) {
              setAudit({
                status: 'error',
                message: fail(error, 'Audit history is unavailable.'),
              })
            }
          })
      }
    } else if (activeView === 'runs') {
      const target = runTargetFromLocation()
      if (target) {
        const routeKey = `run:${target.repository}:${target.runID}`
        setRunInspection({ loading: true })
        void loadRoute(routeKey, () =>
          getRunInspection(target.repository, target.runID),
        )
          .then((data) => {
            const currentTarget = runTargetFromLocation()
            if (
              !cancelled &&
              currentTarget?.repository === target.repository &&
              currentTarget.runID === target.runID
            ) {
              setRunInspection({ data, loading: false })
            }
          })
          .catch((error: unknown) => {
            if (!cancelled) {
              setRunInspection({
                loading: false,
                error: fail(error, 'Run inspection is unavailable.'),
              })
            }
          })
      }
    } else if (
      activeView === 'diagnostics' &&
      !loadedViews.current.has('diagnostics')
    ) {
      setDiagnostics({ loading: true })
      void loadRoute('diagnostics', () => getDiagnostics())
        .then((report) => {
          if (cancelled) return
          loadedViews.current.add('diagnostics')
          setDiagnostics({ report, loading: false })
        })
        .catch((error: unknown) => {
          if (!cancelled) {
            setDiagnostics({
              error: fail(error, 'Diagnostics are unavailable.'),
              loading: false,
            })
          }
        })
    } else if (
      activeView === 'configuration' &&
      !loadedViews.current.has('configuration')
    ) {
      setConfiguration({ loading: true })
      void loadRoute('configuration', () => getConfiguration())
        .then((snapshot) => {
          if (cancelled) return
          loadedViews.current.add('configuration')
          setConfiguration({ snapshot, loading: false })
        })
        .catch((error: unknown) => {
          if (!cancelled) {
            setConfiguration({
              error: fail(error, 'Configuration inspection is unavailable.'),
              loading: false,
            })
          }
        })
    } else if (activeView === 'alerts') {
      const targetID = alertTargetFromLocation()
      if (loadedViews.current.has('alerts')) {
        setIncidents((current) => ({
          ...current,
          selectedID: current.items.some((item) => item.id === targetID)
            ? targetID
            : current.selectedID || current.items[0]?.id || '',
        }))
      } else {
        setIncidents((current) => ({
          ...current,
          loading: true,
          error: undefined,
        }))
        void loadRoute('alerts', () => listAlerts('open'))
          .then((response) => {
            if (cancelled) return
            loadedViews.current.add('alerts')
            setIncidents((current) => ({
              ...current,
              items: response.items,
              selectedID: response.items.some((item) => item.id === targetID)
                ? targetID
                : response.items[0]?.id ?? '',
              loading: false,
            }))
          })
          .catch((error: unknown) => {
            if (!cancelled) {
              setIncidents((current) => ({
                ...current,
                loading: false,
                error: fail(error, 'Alerts are unavailable.'),
              }))
            }
          })
      }
    } else if (
      activeView === 'backups' &&
      !loadedViews.current.has('backups')
    ) {
      setBackups((current) => ({ ...current, loading: true, error: undefined }))
      setRestores((current) => ({ ...current, loading: true, error: undefined }))
      setUpdates((current) => ({ ...current, loading: true, error: undefined }))
      void loadRoute('backups', () =>
        Promise.all([listBackups(), listRestores(), listUpdates()]),
      )
        .then(([backupResponse, restoreResponse, updateResponse]) => {
          if (cancelled) return
          loadedViews.current.add('backups')
          setBackups({ items: backupResponse.items, loading: false })
          setRestores({ items: restoreResponse.items, loading: false })
          setUpdates((current) => ({
            ...current,
            items: updateResponse.items,
            loading: false,
          }))
        })
        .catch((error: unknown) => {
          if (!cancelled) {
            const message = fail(error, 'Recovery data is unavailable.')
            setBackups({ items: [], loading: false, error: message })
            setRestores({ items: [], loading: false, error: message })
            setUpdates((current) => ({
              ...current,
              items: [],
              loading: false,
              error: message,
            }))
          }
        })
    } else {
      const groupBy =
        activeView === 'repositories'
          ? 'repository'
          : activeView === 'workflows'
            ? 'workflow'
            : null
      const analyticsKey = groupBy ? `analytics:${groupBy}` : ''
      if (groupBy && !loadedViews.current.has(analyticsKey)) {
        setAnalytics((value) => ({
          ...value,
          loading: groupBy,
          error: undefined,
        }))
        void loadRoute(analyticsKey, () => getAnalytics(groupBy))
          .then((report) => {
            if (cancelled) return
            loadedViews.current.add(analyticsKey)
            setAnalytics((value) => ({
              ...value,
              [groupBy === 'repository' ? 'repositories' : 'workflows']:
                report,
              loading: undefined,
            }))
          })
          .catch((error: unknown) => {
            if (!cancelled) {
              setAnalytics((value) => ({
                ...value,
                loading: undefined,
                error: fail(error, 'Analytics are unavailable.'),
              }))
            }
          })
      }
    }

    return () => {
      cancelled = true
    }
  }, [activeView, isOnline, locationRevision])
  /* oxlint-enable react/set-state-in-effect */

  const openSupportBundleDialog = () => {
    if (!isOnline) return
    const to = new Date()
    const from = new Date(to.getTime() - 24 * 60 * 60 * 1000)
    const next: SupportBundleDialogState = {
      from: localDateTimeValue(from),
      to: localDateTimeValue(to),
      reason: '',
      confirmation: '',
      idempotencyKey: newIdempotencyKey(),
      loading: true,
      submitting: false,
    }
    setSupportBundleDialog(next)
    void previewCommand({
      type: 'support_bundle.generate',
      target_type: 'system',
      target_id: 'support-bundles',
      parameters: {
        from: new Date(next.from).toISOString(),
        to: new Date(next.to).toISOString(),
      },
      reason: '',
      confirmation: '',
    })
      .then((plan) =>
        setSupportBundleDialog((current) =>
          current ? { ...current, plan, loading: false } : current,
        ),
      )
      .catch((error: unknown) =>
        setSupportBundleDialog((current) =>
          current
            ? {
                ...current,
                loading: false,
                error:
                  error instanceof Error
                    ? error.message
                    : 'Support bundle preview is unavailable.',
              }
            : current,
        ),
      )
  }

  useEffect(() => {
    if (!isOnline) return
    const controller = new AbortController()
    void getOverview(controller.signal)
      .then((data) => setState({ data, loading: false }))
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setState({
            error:
              error instanceof Error
                ? error.message
                : 'The console could not load host data.',
            loading: false,
          })
        }
      })
    return () => controller.abort()
  }, [isOnline])

  useEffect(() => {
    if (!commandRunnerID || !commandRunnerPool) return
    const controller = new AbortController()
    const input = {
      type: 'runner.terminate',
      target_type: 'runner',
      target_id: commandRunnerID,
      parameters: { pool: commandRunnerPool },
      reason: '',
      confirmation: '',
    }
    void previewCommand(input, controller.signal)
      .then((plan) =>
        setCommandDialog((current) =>
          current?.runner.id === commandRunnerID
            ? { ...current, plan, loading: false }
            : current,
        ),
      )
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setCommandDialog((current) =>
            current?.runner.id === commandRunnerID
              ? {
                  ...current,
                  loading: false,
                  error:
                    error instanceof Error
                      ? error.message
                      : 'Command preview is unavailable.',
                }
              : current,
          )
        }
      })
    return () => controller.abort()
  }, [commandRunnerID, commandRunnerPool])

  useEffect(() => {
    const command = commandDialog?.command
    if (!command || terminalCommandStates.has(command.state)) return
    const controller = new AbortController()
    const timer = window.setTimeout(() => {
      void getCommand(command.id, controller.signal)
        .then((next) =>
          setCommandDialog((current) =>
            current?.command?.id === next.id
              ? { ...current, command: next }
              : current,
          ),
        )
        .catch((error: unknown) => {
          if (!controller.signal.aborted) {
            setCommandDialog((current) =>
              current?.command?.id === command.id
                ? {
                    ...current,
                    error:
                      error instanceof Error
                        ? error.message
                        : 'Command status is unavailable.',
                  }
                : current,
            )
          }
        })
    }, 750)
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [commandDialog?.command])

  useEffect(() => {
    const command = supportBundleDialog?.command
    if (!command || terminalCommandStates.has(command.state)) return
    const controller = new AbortController()
    const timer = window.setTimeout(() => {
      void getCommand(command.id, controller.signal)
        .then((next) =>
          setSupportBundleDialog((current) =>
            current?.command?.id === next.id
              ? { ...current, command: next }
              : current,
          ),
        )
        .catch((error: unknown) => {
          if (!controller.signal.aborted) {
            setSupportBundleDialog((current) =>
              current?.command?.id === command.id
                ? {
                    ...current,
                    error:
                      error instanceof Error
                        ? error.message
                        : 'Support bundle status is unavailable.',
                  }
                : current,
            )
          }
        })
    }, 750)
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [supportBundleDialog?.command])

  useEffect(() => {
    const command = backupDialog?.command
    if (!command || terminalCommandStates.has(command.state)) {
      if (command?.state === 'succeeded') {
        void listBackups()
          .then((response) =>
            setBackups({ items: response.items, loading: false }),
          )
          .catch((error: unknown) =>
            setBackups((current) => ({
              ...current,
              error:
                error instanceof Error
                  ? error.message
                  : 'Backups are unavailable.',
            })),
          )
      }
      return
    }
    const controller = new AbortController()
    const timer = window.setTimeout(() => {
      void getCommand(command.id, controller.signal)
        .then((next) =>
          setBackupDialog((current) =>
            current?.command?.id === next.id
              ? { ...current, command: next }
              : current,
          ),
        )
        .catch((error: unknown) => {
          if (!controller.signal.aborted) {
            setBackupDialog((current) =>
              current?.command?.id === command.id
                ? {
                    ...current,
                    error:
                      error instanceof Error
                        ? error.message
                        : 'Backup status is unavailable.',
                  }
                : current,
            )
          }
        })
    }, 750)
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [backupDialog?.command])

  useEffect(() => {
    const command = restoreDialog?.command
    if (!command || terminalCommandStates.has(command.state)) {
      if (command?.state === 'succeeded') {
        void listRestores()
          .then((response) =>
            setRestores({ items: response.items, loading: false }),
          )
          .catch((error: unknown) =>
            setRestores((current) => ({
              ...current,
              error:
                error instanceof Error
                  ? error.message
                  : 'Restore status is unavailable.',
            })),
          )
      }
      return
    }
    const controller = new AbortController()
    const timer = window.setTimeout(() => {
      void getCommand(command.id, controller.signal)
        .then((next) =>
          setRestoreDialog((current) =>
            current?.command?.id === next.id
              ? { ...current, command: next }
              : current,
          ),
        )
        .catch((error: unknown) => {
          if (!controller.signal.aborted) {
            setRestoreDialog((current) =>
              current?.command?.id === command.id
                ? {
                    ...current,
                    error:
                      error instanceof Error
                        ? error.message
                        : 'Restore command status is unavailable.',
                  }
                : current,
            )
          }
        })
    }, 750)
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [restoreDialog?.command])

  useEffect(() => {
    const command = updateDialog?.command
    if (!command || terminalCommandStates.has(command.state)) {
      if (command?.state === 'succeeded') {
        void listUpdates()
          .then((response) =>
            setUpdates((current) => ({
              ...current,
              items: response.items,
              loading: false,
            })),
          )
          .catch((error: unknown) =>
            setUpdates((current) => ({
              ...current,
              error:
                error instanceof Error
                  ? error.message
                  : 'Update status is unavailable.',
            })),
          )
      }
      return
    }
    const controller = new AbortController()
    const timer = window.setTimeout(() => {
      void getCommand(command.id, controller.signal)
        .then((next) =>
          setUpdateDialog((current) =>
            current?.command?.id === next.id
              ? { ...current, command: next }
              : current,
          ),
        )
        .catch((error: unknown) => {
          if (!controller.signal.aborted) {
            setUpdateDialog((current) =>
              current?.command?.id === command.id
                ? {
                    ...current,
                    error:
                      error instanceof Error
                        ? error.message
                        : 'Update command status is unavailable.',
                  }
                : current,
            )
          }
        })
    }, 750)
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [updateDialog?.command])

  useEffect(() => {
    if (legacyBackend || !isOnline) return
    const controller = new AbortController()
    void getOperationalRunners(controller.signal)
      .then((response) =>
        setOperations((current) => ({
          ...current,
          runners: response.items,
          loading: false,
        })),
      )
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setOperations((current) => ({
            ...current,
            error:
              error instanceof Error
                ? error.message
                : 'Runner state could not be loaded.',
            loading: false,
          }))
        }
      })
    return () => controller.abort()
  }, [isOnline])

  useEffect(() => {
    if (legacyBackend || !isOnline) return
    const batcher = createEventBatcher<OperationalEvent>((events) => {
      const lastEvent = events.at(-1)
      if (!lastEvent) return
      setStream((current) => ({
        status: current.status,
        lastEvent,
        received: current.received + events.length,
      }))
      setOperations((current) =>
        applyOperationalEventBatch(current, events),
      )
    })
    const source = openOperationalStream({
      onOpen: () =>
        setStream((current) => ({ ...current, status: 'replaying' })),
      onReplayComplete: () =>
        setStream((current) => ({ ...current, status: 'current' })),
      onEvent: batcher.push,
      onError: () =>
        setStream((current) => ({ ...current, status: 'disconnected' })),
    })
    return () => {
      batcher.close()
      source.close()
    }
  }, [isOnline])

  const metrics = useMemo(() => {
    const overview = state.data?.overview
    return [
      {
        label: 'Active and pending',
        value: overview ? formatNumber(overview.pending_sessions) : '—',
        detail: 'runner sessions',
      },
      {
        label: 'Handled jobs',
        value: overview ? formatNumber(overview.workflow_jobs) : '—',
        detail: 'retained locally',
      },
      {
        label: 'Success rate',
        value: overview ? successRate(overview) : '—',
        detail: 'all retained jobs',
      },
      {
        label: 'Average duration',
        value: overview ? formatDuration(overview.average_duration_seconds) : '—',
        detail: 'completed jobs',
      },
    ]
  }, [state.data])

  const closeJobLog = useCallback(() => setJobLog(null), [])
  const openRunnerCommand = useCallback((runner: RunnerState) => {
    setCommandDialog({
      runner,
      reason: '',
      confirmation: '',
      idempotencyKey: newIdempotencyKey(),
      loading: true,
      submitting: false,
    })
  }, [])

  const saveCurrentView = () => {
    if (!isOnline) return
    const name = savedViews.name.trim()
    const query = search.query.trim()
    if (!name || query.length < 2) {
      setSavedViews((current) => ({
        ...current,
        error: 'Enter a view name and a search with at least 2 characters.',
      }))
      return
    }
    setSavedViews((current) => ({
      ...current,
      saving: true,
      error: undefined,
    }))
    void createSavedView(
      {
        name,
        query,
        entity_type: search.entityType || undefined,
      },
      newIdempotencyKey(),
    )
      .then((view) => {
        setSelectedSavedViewID(view.id)
        setSavedViews((current) => ({
          ...current,
          items: [...current.items.filter((item) => item.id !== view.id), view].sort(
            (left, right) => left.name.localeCompare(right.name),
          ),
          name: '',
          saving: false,
        }))
      })
      .catch((error: unknown) =>
        setSavedViews((current) => ({
          ...current,
          saving: false,
          error:
            error instanceof Error ? error.message : 'Saved view could not be created.',
        })),
      )
  }

  const removeCurrentView = () => {
    if (!isOnline) return
    const view = savedViews.items.find(
      (item) => item.id === selectedSavedViewID,
    )
    if (!view) return
    setSavedViews((current) => ({
      ...current,
      deleting: true,
      error: undefined,
    }))
    void deleteSavedView(view, newIdempotencyKey())
      .then(() => {
        setSelectedSavedViewID('')
        setSavedViews((current) => ({
          ...current,
          items: current.items.filter((item) => item.id !== view.id),
          deleting: false,
        }))
      })
      .catch((error: unknown) =>
        setSavedViews((current) => ({
          ...current,
          deleting: false,
          error:
            error instanceof Error ? error.message : 'Saved view could not be deleted.',
        })),
      )
  }

  const exportCurrentSearch = () => {
    if (!isOnline) return
    const query = search.query.trim()
    if (query.length < 2) {
      setSearchExport({
        loading: false,
        error: 'Enter at least 2 characters before exporting.',
      })
      setSearch((current) => ({
        ...current,
        error: 'Enter at least 2 characters before exporting.',
      }))
      focusSearchInput()
      return
    }
    setSearchExport({ loading: true })
    void createSearchExport(
      {
        query,
        entity_type: search.entityType || undefined,
      },
      newIdempotencyKey(),
    )
      .then((metadata) => setSearchExport({ metadata, loading: false }))
      .catch((error: unknown) =>
        setSearchExport({
          loading: false,
          error:
            error instanceof Error
              ? error.message
              : 'Search export could not be generated.',
        }),
      )
  }

  const activeLabel = navigation.find(([, id]) => id === activeView)?.[0]
  const streamLabel =
    legacyBackend
      ? 'History only'
      : !isOnline
      ? 'Offline'
      : stream.status === 'current'
      ? 'Current'
      : stream.status === 'replaying'
        ? 'Replaying'
      : stream.status === 'connecting'
        ? 'Connecting'
        : 'Reconnecting'
  const hostState = previewMode
    ? 'Preview mode'
    : state.error
      ? 'Console data unavailable'
      : legacyBackend
        ? 'History connected'
    : !isOnline
      ? 'Offline read-only'
    : stream.status === 'disconnected'
      ? 'Live updates interrupted'
      : 'Operational'
  const liveSummary =
    !isOnline && offlineSnapshot
      ? offlineSnapshot.live
      : summarizeRunners(operations.runners, stream.lastEvent?.timestamp)
  const selectedIncident = incidents.items.find(
    (incident) => incident.id === incidents.selectedID,
  )

  const updateIncident = (next: AlertInstance) =>
    setIncidents((current) => ({
      ...current,
      items:
        current.filter && next.state !== current.filter
          ? current.items.filter((item) => item.id !== next.id)
          : current.items.map((item) => (item.id === next.id ? next : item)),
      selectedID:
        current.filter && next.state !== current.filter
          ? current.items.find((item) => item.id !== next.id)?.id ?? ''
          : next.id,
      reason: '',
      error: undefined,
      submitting: undefined,
    }))

  const incidentMutationFailed = (error: unknown) =>
    setIncidents((current) => ({
      ...current,
      submitting: undefined,
      error:
        error instanceof Error ? error.message : 'Alert mutation failed.',
    }))

  const acknowledgeSelectedIncident = () => {
    if (!isOnline || !selectedIncident) return
    setIncidents((current) => ({
      ...current,
      submitting: 'acknowledge',
      error: undefined,
    }))
    void acknowledgeAlert(
      selectedIncident,
      incidents.reason,
      newIdempotencyKey(),
    )
      .then(updateIncident)
      .catch(incidentMutationFailed)
  }

  const silenceSelectedIncident = () => {
    if (!isOnline || !selectedIncident) return
    setIncidents((current) => ({
      ...current,
      submitting: 'silence',
      error: undefined,
    }))
    void silenceAlert(
      selectedIncident,
      new Date(incidents.silenceUntil).toISOString(),
      incidents.reason,
      newIdempotencyKey(),
    )
      .then((result) => updateIncident(result.alert))
      .catch(incidentMutationFailed)
  }

  const annotateSelectedIncident = () => {
    if (!isOnline || !selectedIncident || !incidents.annotation.trim()) return
    setIncidents((current) => ({
      ...current,
      submitting: 'annotate',
      error: undefined,
    }))
    void annotateAlert(
      selectedIncident,
      incidents.annotation,
      newIdempotencyKey(),
    )
      .then(() =>
        setIncidents((current) => ({
          ...current,
          annotation: '',
          submitting: undefined,
        })),
      )
      .catch(incidentMutationFailed)
  }

  const resolveSelectedIncident = () => {
    if (!isOnline || !selectedIncident || !incidents.reason.trim()) return
    setIncidents((current) => ({
      ...current,
      submitting: 'resolve',
      error: undefined,
    }))
    void resolveAlert(
      selectedIncident,
      incidents.reason,
      newIdempotencyKey(),
    )
      .then(updateIncident)
      .catch(incidentMutationFailed)
  }

  return (
    <div className="console-shell">
      <div
        className="console-background"
        inert={hasOpenDialog ? true : undefined}
      >
        <a className="skip-link" href="#main-content">
          Skip to content
        </a>
        {navigationOpen ? (
          <button
            className="navigation-backdrop"
            type="button"
            aria-label="Close navigation"
            onClick={() => setNavigationOpen(false)}
          />
        ) : null}
        <aside
          className={`sidebar ${navigationOpen ? 'open' : ''}`}
          aria-label="Primary navigation"
        >
        <div className="brand">
          <div className="brand-mark" aria-hidden="true">
            MR
          </div>
          <div>
            <strong>Multirunner</strong>
            <span>Operations Console</span>
          </div>
          <button
            ref={navigationCloseRef}
            className="navigation-close"
            type="button"
            aria-label="Close navigation"
            onClick={() => setNavigationOpen(false)}
          >
            ×
          </button>
        </div>
        <nav>
          <p className="nav-label">Operate</p>
          <ul>
            {navigation.map(([label, id]) => (
              <li key={id}>
                <a
                  href={routeForView(id)}
                  aria-current={activeView === id ? 'page' : undefined}
                  aria-label={label}
                  onClick={(event) => handleNavigation(event, id)}
                >
                  <NavigationIcon icon={id} />
                  <span>{label}</span>
                </a>
              </li>
            ))}
          </ul>
        </nav>
        <div className="sidebar-footer">
          <span className="status-dot" aria-hidden="true" />
          <div>
            <strong>Local host</strong>
            <span>Loopback connection</span>
          </div>
        </div>
        </aside>

        <div className="workspace">
        <div className="workspace-content">
          <header className="topbar">
            <button
              className="navigation-toggle"
              type="button"
              aria-label="Open navigation"
              aria-expanded={navigationOpen}
              onClick={() => setNavigationOpen(true)}
            >
              <span aria-hidden="true">☰</span>
            </button>
            <div className="topbar-title">
              <p className="scope-label">Local host</p>
              <h1 ref={pageHeadingRef} tabIndex={-1}>{activeLabel}</h1>
            </div>
            <div className="topbar-actions">
              <button
                className="command-button"
                type="button"
                onClick={() => {
                  setPaletteQuery('')
                  setPaletteOpen(true)
                }}
              >
                <kbd>Ctrl</kbd>
                <span aria-hidden="true">/</span>
                <kbd>⌘</kbd>
                <kbd>K</kbd>
                Search
              </button>
              <div
                className={`connection-badge ${isOnline ? stream.status : 'offline'}`}
                aria-live="polite"
              >
                <span className="status-dot" aria-hidden="true" />
                {streamLabel}
              </div>
            </div>
          </header>

          {!isOnline ? (
            <div className="offline-banner" role="status">
              {offlineSnapshot
                ? 'Offline. A bounded stale operational snapshot is displayed; commands and local state changes are disabled until connectivity returns.'
                : 'Offline. The application shell is available, but no operational snapshot is stored. Commands and local state changes are disabled.'}
            </div>
          ) : null}
          {pwaUpdate?.ready ? (
            <div className="pwa-update-banner" role="status">
              <span>
                A new Operations Console version is ready.
              </span>
              <button
                type="button"
                disabled={pwaUpdating}
                onClick={() => {
                  setPwaUpdateError('')
                  setPwaUpdating(true)
                  void pwaUpdate
                    .updateSW()
                    .catch((error: unknown) =>
                      setPwaUpdateError(
                        error instanceof Error
                          ? error.message
                          : 'The console update could not be applied.',
                      ),
                    )
                    .finally(() => setPwaUpdating(false))
                }}
              >
                {pwaUpdating ? 'Applying update…' : 'Update and reload'}
              </button>
            </div>
          ) : null}
          {pwaUpdate?.registrationError || pwaUpdateError ? (
            <div className="pwa-update-error" role="alert">
              <span>{pwaUpdateError || pwaUpdate?.registrationError}</span>
              <button
                type="button"
                onClick={() => {
                  setPwaUpdateError('')
                  pwaUpdate?.retryRegistration?.()
                }}
              >
                Retry update service
              </button>
            </div>
          ) : null}
          {downloadNotice ? (
            <p
              className={`download-notice ${downloadNotice.kind}`}
              role={downloadNotice.kind === 'error' ? 'alert' : 'status'}
            >
              {downloadNotice.message}
            </p>
          ) : null}

          <main id="main-content" tabIndex={-1}>
            {activeView === 'overview' ? (
              <>
                <section
                  className="operational-header"
                  aria-labelledby="host-status-title"
                >
                  <div>
                    <span className="section-label">Host status</span>
                    <h2 id="host-status-title">{hostState}</h2>
                    <p>
                      {previewMode
                        ? 'No machine data is connected. This preview contains no configured repositories, runs, or runners.'
                        : legacyBackend
                          ? 'Real retained history is connected. Live runner state and newer operator APIs require an updated installed backend.'
                        : state.error ??
                          'Runner history is available and the live operational stream is active.'}
                    </p>
                  </div>
                  <dl className="header-facts">
                    <div>
                      <dt>Last event</dt>
                      <dd>
                        {stream.lastEvent
                          ? formatRelativeTime(stream.lastEvent.timestamp)
                          : 'Waiting'}
                      </dd>
                    </div>
                    <div>
                      <dt>Live events</dt>
                      <dd>{formatNumber(stream.received)}</dd>
                    </div>
                    <div>
                      <dt>Configuration</dt>
                      <dd>Read-only</dd>
                    </div>
                  </dl>
                </section>

                <SnapshotProvenance
                  snapshot={offlineSnapshot}
                  isOnline={isOnline && !state.loading && !state.error}
                />

                <dl className="metric-ledger" aria-label="Host summary">
                  {metrics.map((metric) => (
                    <div key={metric.label}>
                      <dt>{metric.label}</dt>
                      <dd className="metric-value">
                        {state.loading ? '···' : metric.value}
                      </dd>
                      <dd className="metric-detail">{metric.detail}</dd>
                    </div>
                  ))}
                </dl>

                <div className="content-grid">
                  <section className="panel recent-panel" aria-labelledby="recent-title">
                    <div className="panel-heading">
                      <div>
                        <span className="section-label">Durable history</span>
                        <h2 id="recent-title">Recent activity</h2>
                      </div>
                      <button type="button" onClick={() => activateView('runs')}>
                        View all runs
                      </button>
                    </div>
                    {state.loading ? (
                      <p className="empty-state" role="status">Loading recent runs…</p>
                    ) : state.data?.runs.length ? (
                      <div className="table-scroll">
                        <table className="mobile-summary-table recent-runs-table">
                          <caption className="sr-only">Recent workflow activity</caption>
                          <thead>
                            <tr>
                              <th scope="col">Workflow</th>
                              <th scope="col">Repository</th>
                              <th scope="col">State</th>
                              <th scope="col">Started</th>
                            </tr>
                          </thead>
                          <tbody>
                            {state.data.runs.map((run) => (
                              <tr key={`${run.repository}-${run.id}-${run.run_attempt}`}>
                                <td>
                                  <a href={run.html_url} rel="noreferrer" target="_blank">
                                    {run.workflow_name || run.name || `Run ${run.id}`}
                                  </a>
                                  <small>{run.display_title}</small>
                                </td>
                                <td>{run.repository}</td>
                                <td>
                                  <span className={`pill ${conclusionLabel(run)}`}>
                                    {conclusionLabel(run).replaceAll('_', ' ')}
                                  </span>
                                </td>
                                <td>{formatRelativeTime(run.started_at ?? run.created_at)}</td>
                              </tr>
                            ))}
                          </tbody>
                        </table>
                      </div>
                    ) : (
                      <p className="empty-state">
                        No retained workflow runs are available yet.
                      </p>
                    )}
                  </section>

                  <aside className="panel attention-panel" aria-labelledby="attention-title">
                    <div className="panel-heading">
                      <div>
                        <span className="section-label">Attention</span>
                        <h2 id="attention-title">Operator queue</h2>
                      </div>
                    </div>
                    <ul className="attention-list">
                      <li>
                        <span className="attention-icon ok" aria-hidden="true">✓</span>
                        <div>
                          <strong>History database</strong>
                          <span>Durable storage is available</span>
                        </div>
                      </li>
                      <li>
                        <span className="attention-icon" aria-hidden="true">i</span>
                        <div>
                          <strong>Configuration</strong>
                          <span>Read-only inspection by design</span>
                        </div>
                      </li>
                      <li>
                        <span className="attention-icon" aria-hidden="true">↗</span>
                        <div>
                          <strong>GitHub logs</strong>
                          <span>Transient viewing, never retained</span>
                        </div>
                      </li>
                    </ul>
                  </aside>
                </div>
              </>
            ) : legacyBackend && legacyUnavailableViews.has(activeView) ? (
              <section className="panel module-placeholder">
                <span className="section-label">Installed backend capability</span>
                <h2>{activeLabel}</h2>
                <p>
                  This module requires the newer Operations Console API. The
                  connected service remains unchanged; retained overview and
                  run history are available from its legacy read-only API.
                </p>
              </section>
            ) : activeView === 'live' ? (
              <div className="live-workspace">
                <SnapshotProvenance
                  snapshot={offlineSnapshot}
                  isOnline={
                    isOnline &&
                    stream.status === 'current' &&
                    !operations.loading &&
                    !operations.error
                  }
                />
                <dl className="metric-ledger" aria-label="Live summary">
                  <div>
                    <dt>Tracked runners</dt>
                    <dd className="metric-value">
                      {formatNumber(liveSummary.runners)}
                    </dd>
                    <dd className="metric-detail">current projection</dd>
                  </div>
                  <div>
                    <dt>Pending runners</dt>
                    <dd className="metric-value">
                      {formatNumber(liveSummary.pending)}
                    </dd>
                    <dd className="metric-detail">pre-launch states</dd>
                  </div>
                  <div>
                    <dt>Failed runners</dt>
                    <dd className="metric-value">
                      {formatNumber(liveSummary.failed)}
                    </dd>
                    <dd className="metric-detail">failed states</dd>
                  </div>
                  <div>
                    <dt>Last event</dt>
                    <dd className="metric-value live-summary-time">
                      {liveSummary.last_event_at
                        ? formatRelativeTime(liveSummary.last_event_at)
                        : '—'}
                    </dd>
                    <dd className="metric-detail">snapshot watermark</dd>
                  </div>
                </dl>
                {isOnline ? (
                  <LiveEventLedger
                    events={operations.events}
                    streamStatus={stream.status}
                    streamLabel={streamLabel}
                    eventLabel={eventLabel}
                    formatRelativeTime={formatRelativeTime}
                  />
                ) : (
                  <section className="panel operations-module">
                    <p className="empty-state">
                      Live event details are not persisted offline. Only the
                      redacted aggregate summary above is available.
                    </p>
                  </section>
                )}
              </div>
            ) : activeView === 'runs' ? (
              <div className="runs-workspace">
                {runInspection.loading ? (
                  <section className="panel operations-module">
                    <p className="empty-state" role="status">Loading run inspection…</p>
                  </section>
                ) : runInspection.error ? (
                  <section className="panel operations-module">
                    <p className="empty-state error-state" role="alert">
                      {runInspection.error}
                    </p>
                  </section>
                ) : runInspection.data ? (
                  <section className="panel run-inspector" aria-labelledby="run-inspector-title">
                    <div className="panel-heading">
                      <div>
                        <a className="back-link" href="/runs">← All runs</a>
                        <span className="section-label">
                          {runInspection.data.run.repository}
                        </span>
                        <h2 id="run-inspector-title">
                          {runInspection.data.run.workflow_name ||
                            runInspection.data.run.name}
                        </h2>
                        <p>{runInspection.data.run.display_title}</p>
                      </div>
                      <span
                        className={`pill ${
                          runInspection.data.run.conclusion ||
                          runInspection.data.run.status
                        }`}
                      >
                        {eventLabel(
                          runInspection.data.run.conclusion ||
                            runInspection.data.run.status,
                        )}
                      </span>
                    </div>
                    <dl className="run-facts">
                      <div>
                        <dt>Run</dt>
                        <dd>
                          #{runInspection.data.run.run_number} · attempt{' '}
                          {runInspection.data.run.run_attempt}
                        </dd>
                      </div>
                      <div>
                        <dt>Branch</dt>
                        <dd>{runInspection.data.run.head_branch || '—'}</dd>
                      </div>
                      <div>
                        <dt>Commit</dt>
                        <dd>
                          <code>
                            {runInspection.data.run.head_sha.slice(0, 12) ||
                              '—'}
                          </code>
                        </dd>
                      </div>
                      <div>
                        <dt>Actor</dt>
                        <dd>
                          {runInspection.data.run.triggering_actor ||
                            runInspection.data.run.actor ||
                            '—'}
                        </dd>
                      </div>
                    </dl>
                    <div className="inspector-grid">
                      <div>
                        <h3>Jobs and steps</h3>
                        {runInspection.data.jobs.length ? (
                          <ol className="job-inspection-list">
                            {runInspection.data.jobs.map((job) => (
                              <li key={job.id}>
                                <div className="job-heading">
                                  <div>
                                    <strong>{job.name}</strong>
                                    <span>
                                      {job.pool_name || 'unassigned pool'} ·{' '}
                                      {job.runner_name || 'no runner'}
                                    </span>
                                  </div>
                                  <span
                                    className={`pill ${
                                      job.conclusion || job.status
                                    }`}
                                  >
                                    {eventLabel(
                                      job.conclusion || job.status,
                                    )}
                                  </span>
                                </div>
                                <ol className="step-list">
                                  {job.steps.map((step) => (
                                    <li key={step.number}>
                                      <span>{step.number}</span>
                                      <strong>{step.name}</strong>
                                      <small>
                                        {eventLabel(
                                          step.conclusion || step.status,
                                        )}
                                      </small>
                                    </li>
                                  ))}
                                </ol>
                                <button
                                  className="log-button"
                                  type="button"
                                  onClick={() => {
                                    setJobLog({
                                      jobID: job.id,
                                      jobName: job.name,
                                      steps: job.steps.map((step) => step.name),
                                      loading: true,
                                    })
                                    void getJobLog(job.id)
                                      .then((content) =>
                                        setJobLog((current) =>
                                          current?.jobID === job.id
                                            ? {
                                                ...current,
                                                content,
                                                loading: false,
                                              }
                                            : current,
                                        ),
                                      )
                                      .catch((error: unknown) =>
                                        setJobLog((current) =>
                                          current?.jobID === job.id
                                            ? {
                                                ...current,
                                                loading: false,
                                                error:
                                                  error instanceof Error
                                                    ? error.message
                                                    : 'Job log is unavailable.',
                                              }
                                            : current,
                                        ),
                                      )
                                  }}
                                >
                                  View transient log
                                </button>
                              </li>
                            ))}
                          </ol>
                        ) : (
                          <p className="empty-state">No retained jobs.</p>
                        )}
                      </div>
                      <aside>
                        <h3>Local runner correlation</h3>
                        {runInspection.data.runner_sessions.length ? (
                          <ul className="runner-correlation-list">
                            {runInspection.data.runner_sessions.map(
                              (session) => (
                                <li key={session.id}>
                                  <strong>
                                    {session.runner_name || session.id}
                                  </strong>
                                  <span>
                                    {session.pool_name || '—'} ·{' '}
                                    {eventLabel(session.status)}
                                  </span>
                                  <small>
                                    {session.attribution_source || 'unknown'} ·{' '}
                                    {session.attribution_confidence}% confidence
                                  </small>
                                  {session.error ? (
                                    <p>{session.error}</p>
                                  ) : null}
                                </li>
                              ),
                            )}
                          </ul>
                        ) : (
                          <p className="empty-state">
                            No local runner session was correlated.
                          </p>
                        )}
                      </aside>
                    </div>
                    {jobLog?.loading ? (
                      <section
                        className="job-log-viewer"
                        aria-labelledby="job-log-title"
                      >
                        <h3 id="job-log-title">{jobLog.jobName} log</h3>
                        <p className="empty-state" role="status">
                          Fetching masked GitHub log…
                        </p>
                      </section>
                    ) : jobLog?.error ? (
                      <section
                        className="job-log-viewer"
                        aria-labelledby="job-log-title"
                      >
                        <h3 id="job-log-title">{jobLog.jobName} log</h3>
                        <p className="empty-state error-state" role="alert">
                          {jobLog.error}
                        </p>
                      </section>
                    ) : jobLog?.content !== undefined ? (
                      <JobLogViewer
                        key={jobLog.jobID}
                        jobName={jobLog.jobName}
                        steps={jobLog.steps}
                        content={jobLog.content}
                        onClose={closeJobLog}
                      />
                    ) : null}
                  </section>
                ) : null}
                <section className="panel operations-module" aria-labelledby="runs-title">
                  <div className="panel-heading">
                    <div>
                      <span className="section-label">Durable history index</span>
                      <h2 id="runs-title">Search runs and execution records</h2>
                    </div>
                    {search.submitted ? (
                      <span className="collection-count">
                        {formatNumber(search.items.length)} matches
                      </span>
                    ) : null}
                  </div>
                  {legacyBackend ? (
                    <p className="empty-state">
                      Full-text search, saved views, and exports require the
                      newer Operations Console API. Recent real runs remain
                      available above.
                    </p>
                  ) : null}
                  <form
                    className="history-search"
                    role="search"
                    onSubmit={(event) => {
                    event.preventDefault()
                    if (legacyBackend) return
                    const query = search.query.trim()
                    if (query.length < 2) {
                      setSearch((current) => ({
                        ...current,
                        error: 'Enter at least 2 characters.',
                      }))
                      focusSearchInput()
                      return
                    }
                    setSearch((current) => ({
                      ...current,
                      loading: true,
                      submitted: true,
                      error: undefined,
                    }))
                    const controller = new AbortController()
                    void searchHistory(
                      query,
                      search.entityType,
                      controller.signal,
                    )
                      .then((response) =>
                        setSearch((current) => ({
                          ...current,
                          items: response.items,
                          loading: false,
                        })),
                      )
                      .catch((error: unknown) =>
                        setSearch((current) => ({
                          ...current,
                          loading: false,
                          error:
                            error instanceof Error
                              ? error.message
                              : 'Search is unavailable.',
                        })),
                      )
                    }}
                  >
                  <label>
                    Search history
                    <input
                      ref={searchInputRef}
                      name="history_search"
                      type="search"
                      disabled={legacyBackend}
                      value={search.query}
                      aria-invalid={search.error ? 'true' : undefined}
                      aria-describedby={search.error ? 'history-search-error' : undefined}
                      onChange={(event) =>
                        setSearch((current) => ({
                          ...current,
                          query: event.target.value,
                          error: undefined,
                        }))
                      }
                      placeholder="Workflow, job, branch, SHA, runner, or command"
                    />
                  </label>
                  <label>
                    Record type
                    <select
                      name="record_type"
                      disabled={legacyBackend}
                      value={search.entityType}
                      onChange={(event) =>
                        setSearch((current) => ({
                          ...current,
                          entityType: event.target.value,
                        }))
                      }
                    >
                      <option value="">All records</option>
                      <option value="run">Runs</option>
                      <option value="job">Jobs</option>
                      <option value="step">Steps</option>
                      <option value="runner">Runners</option>
                      <option value="command">Commands</option>
                      <option value="alert">Alerts</option>
                      <option value="annotation">Annotations</option>
                    </select>
                  </label>
                  <button
                    type="submit"
                    disabled={legacyBackend || search.loading}
                  >
                    {search.loading ? 'Searching…' : 'Search'}
                  </button>
                  </form>
                  <div className="saved-view-toolbar">
                    <label>
                      Saved view
                      <select
                        name="saved_view"
                        value={selectedSavedViewID}
                        disabled={legacyBackend || savedViews.loading}
                        onChange={(event) => {
                          const id = event.target.value
                          setSelectedSavedViewID(id)
                          const view = savedViews.items.find(
                            (item) => item.id === id,
                          )
                          if (view) {
                            setSearch((current) => ({
                              ...current,
                              query: view.query,
                              entityType: view.entity_type ?? '',
                              submitted: false,
                              error: undefined,
                            }))
                            setSearchExport({ loading: false })
                          }
                        }}
                      >
                        <option value="">
                          {savedViews.loading
                            ? 'Loading saved views…'
                            : 'Choose a saved view'}
                        </option>
                        {savedViews.items.map((view) => (
                          <option key={view.id} value={view.id}>
                            {view.name}
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      New view name
                      <input
                        name="saved_view_name"
                        disabled={legacyBackend}
                        value={savedViews.name}
                        maxLength={100}
                        onChange={(event) =>
                          setSavedViews((current) => ({
                            ...current,
                            name: event.target.value,
                          }))
                        }
                      />
                    </label>
                    <div className="saved-view-actions">
                      <button
                        type="button"
                        onClick={saveCurrentView}
                        disabled={
                          legacyBackend || !isOnline || savedViews.saving
                        }
                      >
                        {savedViews.saving ? 'Saving…' : 'Save current view'}
                      </button>
                      <button
                        type="button"
                        onClick={removeCurrentView}
                        disabled={
                          !isOnline ||
                          legacyBackend ||
                          !selectedSavedViewID ||
                          savedViews.deleting
                        }
                      >
                        {savedViews.deleting ? 'Deleting…' : 'Delete view'}
                      </button>
                      <button
                        type="button"
                        onClick={exportCurrentSearch}
                        disabled={
                          legacyBackend || !isOnline || searchExport.loading
                        }
                      >
                        {searchExport.loading ? 'Generating…' : 'Export JSONL'}
                      </button>
                    </div>
                  </div>
                  {savedViews.error ? (
                    <p className="form-error" role="alert">
                      {savedViews.error}
                    </p>
                  ) : null}
                  {searchExport.error ? (
                    <p className="form-error" role="alert">
                      {searchExport.error}
                    </p>
                  ) : searchExport.metadata ? (
                    <p className="export-ready" role="status">
                      <button
                        type="button"
                        onClick={() =>
                          void startDownload(
                            searchExport.metadata!.download_url,
                            searchExport.metadata!.file_name,
                            'Search export',
                          )
                        }
                      >
                        Download {formatNumber(searchExport.metadata.record_count)} exported records
                      </button>
                      <span>
                        Expires {formatRelativeTime(searchExport.metadata.expires_at)}
                      </span>
                    </p>
                  ) : null}
                  {search.error ? (
                  <p
                    className="form-error"
                    id="history-search-error"
                    role="alert"
                  >
                    {search.error}
                  </p>
                ) : search.loading ? (
                  <p className="empty-state" role="status">
                    Searching durable history…
                  </p>
                ) : search.submitted && search.items.length ? (
                  <ol className="search-results">
                    {search.items.map((result) => (
                      <li key={`${result.entity_type}:${result.entity_key}`}>
                        <span className="search-type">{result.entity_type}</span>
                        <div>
                          <a href={result.route}>{result.title}</a>
                          <p>{result.context || result.repository || 'Local host record'}</p>
                          <small>
                            {result.repository ? `${result.repository} · ` : ''}
                            {result.state || 'recorded'} · {formatRelativeTime(result.timestamp)}
                          </small>
                        </div>
                      </li>
                    ))}
                  </ol>
                ) : search.submitted ? (
                  <p className="empty-state">No matching records were found.</p>
                ) : (
                  <p className="empty-state">
                    Search retained runs, jobs, steps, runner sessions, and audited commands.
                  </p>
                  )}
                </section>
              </div>
            ) : activeView === 'repositories' || activeView === 'workflows' ? (
              <section
                className="panel operations-module"
                aria-labelledby="analytics-title"
              >
                <div className="panel-heading">
                  <div>
                    <span className="section-label">
                      Rolling 30-day reliability
                    </span>
                    <h2 id="analytics-title">
                      {activeView === 'repositories'
                        ? 'Repository analytics'
                        : 'Workflow analytics'}
                    </h2>
                  </div>
                </div>
                {analytics.loading ===
                (activeView === 'repositories'
                  ? 'repository'
                  : 'workflow') ? (
                  <p className="empty-state" role="status">Calculating durable analytics…</p>
                ) : analytics.error ? (
                  <p className="empty-state error-state" role="alert">{analytics.error}</p>
                ) : (
                  (() => {
                    const report =
                      activeView === 'repositories'
                        ? analytics.repositories
                        : analytics.workflows
                    return report?.rows.length ? (
                      <div className="table-scroll">
                        <table className="analytics-table mobile-summary-table">
                          <caption className="sr-only">
                            {activeView === 'repositories'
                              ? 'Repository reliability analytics'
                              : 'Workflow reliability analytics'}
                          </caption>
                          <thead>
                            <tr>
                              <th scope="col">
                                {activeView === 'repositories'
                                  ? 'Repository'
                                  : 'Workflow'}
                              </th>
                              <th scope="col">Jobs</th>
                              <th scope="col">Success</th>
                              <th scope="col">Failures</th>
                              <th scope="col">Infra</th>
                              <th scope="col">p50</th>
                              <th scope="col">p95</th>
                            </tr>
                          </thead>
                          <tbody>
                            {report.rows.map((row) => (
                              <tr key={row.key}>
                                <td>
                                  <strong>
                                    {activeView === 'repositories'
                                      ? row.repository
                                      : row.workflow || 'Unnamed workflow'}
                                  </strong>
                                  {activeView === 'workflows' ? (
                                    <small>{row.repository}</small>
                                  ) : null}
                                </td>
                                <td>{formatNumber(row.total_jobs)}</td>
                                <td>
                                  {Math.round(row.success_rate * 100)}%
                                </td>
                                <td>{formatNumber(row.failed_jobs)}</td>
                                <td>
                                  {formatNumber(
                                    row.infrastructure_failures,
                                  )}
                                </td>
                                <td>
                                  {formatDuration(
                                    row.p50_duration_seconds,
                                  )}
                                </td>
                                <td>
                                  {formatDuration(
                                    row.p95_duration_seconds,
                                  )}
                                </td>
                              </tr>
                            ))}
                          </tbody>
                        </table>
                      </div>
                    ) : (
                      <p className="empty-state">
                        No completed jobs are available in this analytics
                        window.
                      </p>
                    )
                  })()
                )}
              </section>
            ) : activeView === 'pools' ? (
              <section className="panel operations-module" aria-labelledby="pools-title">
                <div className="panel-heading">
                  <div>
                    <span className="section-label">
                      Retained runner-session history
                    </span>
                    <h2 id="pools-title">Pools</h2>
                  </div>
                  {pools.status === 'ready' || pools.status === 'empty' ? (
                    <span className="collection-count">
                      {(pools.data?.length ?? 0).toLocaleString()} recorded
                    </span>
                  ) : null}
                </div>
                {pools.status === 'idle' || pools.status === 'loading' ? (
                  <p className="empty-state" role="status">Loading pool history…</p>
                ) : pools.status === 'degraded' ? (
                  <p className="empty-state degraded-state" role="status">{pools.message}</p>
                ) : pools.status === 'error' ? (
                  <p className="empty-state error-state" role="alert">{pools.message}</p>
                ) : pools.status === 'empty' ? (
                  <p className="empty-state">
                    No pool names have been recorded in retained runner
                    sessions.
                  </p>
                ) : (
                  <>
                    <p className="resource-context">
                      Counts describe retained runner sessions. This endpoint
                      does not report configured capacity, saturation, or pool
                      policy.
                    </p>
                    <div className="table-scroll">
                      <table className="mobile-summary-table pools-table">
                        <caption className="sr-only">Retained runner sessions by pool</caption>
                        <thead>
                          <tr>
                            <th scope="col">Pool</th>
                            <th scope="col">Retained sessions</th>
                          </tr>
                        </thead>
                        <tbody>
                          {pools.data?.map((pool) => (
                            <tr key={pool.name}>
                              <td><strong>{pool.name}</strong></td>
                              <td>{formatNumber(pool.count)}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  </>
                )}
              </section>
            ) : activeView === 'host' ? (
              <section className="panel operations-module" aria-labelledby="host-view-title">
                <div className="panel-heading">
                  <div>
                    <span className="section-label">Versioned system resource</span>
                    <h2 id="host-view-title">Host</h2>
                  </div>
                  {systemStatus.status === 'ready' ? (
                    <span className={`health-summary ${systemStatus.data?.status === 'operational' ? 'pass' : 'warn'}`}>
                      {systemStatus.data?.status}
                    </span>
                  ) : null}
                </div>
                {systemStatus.status === 'idle' ||
                systemStatus.status === 'loading' ? (
                  <p className="empty-state" role="status">Loading host status…</p>
                ) : systemStatus.status === 'degraded' ? (
                  <p className="empty-state degraded-state" role="status">
                    {systemStatus.message}
                  </p>
                ) : systemStatus.status === 'error' ? (
                  <p className="empty-state error-state" role="alert">
                    {systemStatus.message}
                  </p>
                ) : systemStatus.status === 'empty' ? (
                  <p className="empty-state">No host resource is available.</p>
                ) : (
                  <dl className="resource-facts">
                    <div><dt>Mode</dt><dd>{systemStatus.data?.mode}</dd></div>
                    <div><dt>Application</dt><dd>{systemStatus.data?.app_version || 'unreported'}</dd></div>
                    <div><dt>API</dt><dd>{systemStatus.data?.api_version}</dd></div>
                    <div><dt>Started</dt><dd>{systemStatus.data?.started_at ? formatRelativeTime(systemStatus.data.started_at) : 'unreported'}</dd></div>
                    <div><dt>Configuration</dt><dd>{systemStatus.data?.configuration ? eventLabel(systemStatus.data.configuration) : 'unreported'}</dd></div>
                    <div><dt>Listener</dt><dd><code>{systemStatus.data?.listen || 'unreported'}</code></dd></div>
                    <div className="wide"><dt>History database</dt><dd><code>{systemStatus.data?.database || 'unreported'}</code></dd></div>
                  </dl>
                )}
              </section>
            ) : activeView === 'audit' ? (
              <section className="panel operations-module" aria-labelledby="audit-title">
                <div className="panel-heading">
                  <div>
                    <span className="section-label">Durable local record</span>
                    <h2 id="audit-title">Audit</h2>
                  </div>
                  {audit.status === 'ready' || audit.status === 'empty' ? (
                    <span className="collection-count">
                      {(audit.data?.length ?? 0).toLocaleString()} loaded
                    </span>
                  ) : null}
                </div>
                {audit.status === 'idle' || audit.status === 'loading' ? (
                  <p className="empty-state" role="status">Loading audit history…</p>
                ) : audit.status === 'degraded' ? (
                  <p className="empty-state degraded-state" role="status">{audit.message}</p>
                ) : audit.status === 'error' ? (
                  <p className="empty-state error-state" role="alert">{audit.message}</p>
                ) : audit.status === 'empty' ? (
                  <p className="empty-state">
                    No command, authentication, or recovery audit records are
                    available.
                  </p>
                ) : (
                  <div className="table-scroll">
                    <table className="mobile-summary-table audit-table">
                      <caption className="sr-only">Operations audit history</caption>
                      <thead>
                        <tr>
                          <th scope="col">Occurred</th>
                          <th scope="col">Action</th>
                          <th scope="col">Target</th>
                          <th scope="col">Actor</th>
                          <th scope="col">Outcome</th>
                          <th scope="col">Source</th>
                        </tr>
                      </thead>
                      <tbody>
                        {audit.data?.map((record) => (
                          <tr key={record.id}>
                            <td>{formatRelativeTime(record.occurred_at)}</td>
                            <td>
                              <strong>{eventLabel(record.action)}</strong>
                              <small>{record.correlation_id || record.id}</small>
                            </td>
                            <td>
                              {record.target_type || 'system'}
                              <small>{record.target_id || '—'}</small>
                            </td>
                            <td>
                              {record.actor_id || 'system'}
                              <small>{record.actor_kind || 'unknown'}</small>
                            </td>
                            <td>
                              <span className={`pill ${record.outcome || 'recorded'}`}>
                                {eventLabel(record.outcome || 'recorded')}
                              </span>
                            </td>
                            <td>{record.source}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </section>
            ) : activeView === 'runners' ? (
              <RunnerSessions
                runners={operations.runners}
                loading={operations.loading}
                error={operations.error}
                isOnline={isOnline}
                onTerminate={openRunnerCommand}
                eventLabel={eventLabel}
                formatRelativeTime={formatRelativeTime}
              />
            ) : activeView === 'alerts' ? (
              <div className="incident-workspace">
                <section
                  className="panel operations-module"
                  aria-labelledby="alerts-title"
                >
                  <div className="panel-heading">
                    <div>
                      <span className="section-label">
                        Durable incident lifecycle
                      </span>
                      <h2 id="alerts-title">Alerts</h2>
                    </div>
                    <div className="incident-filters">
                      <label>
                        State
                        <select
                          name="alert_state"
                          value={incidents.filter}
                          onChange={(event) =>
                            loadIncidents(
                              event.target.value as AlertState | '',
                            )
                          }
                        >
                          <option value="open">Open</option>
                          <option value="pending">Pending</option>
                          <option value="resolved">Resolved</option>
                          <option value="">All</option>
                        </select>
                      </label>
                      <button
                        type="button"
                        onClick={() => loadIncidents()}
                        disabled={incidents.loading}
                      >
                        Refresh
                      </button>
                    </div>
                  </div>
                  {incidents.loading ? (
                    <p className="empty-state" role="status">Loading durable alerts…</p>
                  ) : incidents.error && !incidents.items.length ? (
                    <p className="empty-state error-state" role="alert">
                      {incidents.error}
                    </p>
                  ) : incidents.items.length ? (
                    <div className="table-scroll">
                      <table className="incident-table mobile-summary-table">
                        <caption className="sr-only">Durable alerts</caption>
                        <thead>
                          <tr>
                            <th scope="col">Alert</th>
                            <th scope="col">Severity</th>
                            <th scope="col">State</th>
                            <th scope="col">Observed</th>
                            <th scope="col">Count</th>
                          </tr>
                        </thead>
                        <tbody>
                          {incidents.items.map((incident) => (
                            <tr
                              key={incident.id}
                              className={
                                incidents.selectedID === incident.id
                                  ? 'selected'
                                  : undefined
                              }
                            >
                              <td>
                                <button
                                  type="button"
                                  onClick={() =>
                                    setIncidents((current) => ({
                                      ...current,
                                      selectedID: incident.id,
                                      error: undefined,
                                    }))
                                  }
                                >
                                  {incident.summary}
                                </button>
                                <small>{incident.rule_id}</small>
                              </td>
                              <td>
                                <span
                                  className={`pill severity-${incident.severity}`}
                                >
                                  {incident.severity}
                                </span>
                              </td>
                              <td>
                                <span className={`pill ${incident.state}`}>
                                  {incident.state}
                                </span>
                              </td>
                              <td>
                                {formatRelativeTime(
                                  incident.last_observed_at,
                                )}
                              </td>
                              <td>
                                {formatNumber(incident.occurrence_count)}
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  ) : (
                    <p className="empty-state">
                      No alerts match this lifecycle state.
                    </p>
                  )}
                </section>

                <aside
                  className="panel incident-detail"
                  aria-labelledby="incident-detail-title"
                >
                  {selectedIncident ? (
                    <>
                      <div className="panel-heading">
                        <div>
                          <span className="section-label">
                            {selectedIncident.severity} severity
                          </span>
                          <h2 id="incident-detail-title">
                            {selectedIncident.summary}
                          </h2>
                        </div>
                        <span className={`pill ${selectedIncident.state}`}>
                          {selectedIncident.state}
                        </span>
                      </div>
                      <dl className="incident-facts">
                        <div>
                          <dt>First observed</dt>
                          <dd>
                            {formatRelativeTime(
                              selectedIncident.first_observed_at,
                            )}
                          </dd>
                        </div>
                        <div>
                          <dt>Occurrences</dt>
                          <dd>
                            {formatNumber(
                              selectedIncident.occurrence_count,
                            )}
                          </dd>
                        </div>
                        <div>
                          <dt>Acknowledged</dt>
                          <dd>
                            {selectedIncident.acknowledged_by || 'No'}
                          </dd>
                        </div>
                        <div>
                          <dt>Silenced until</dt>
                          <dd>
                            {selectedIncident.silenced_until
                              ? new Date(
                                  selectedIncident.silenced_until,
                                ).toLocaleString()
                              : 'Not silenced'}
                          </dd>
                        </div>
                      </dl>
                      <details className="incident-evidence">
                        <summary>Source evidence</summary>
                        <pre>
                          {JSON.stringify(
                            selectedIncident.details,
                            null,
                            2,
                          )}
                        </pre>
                      </details>
                      {selectedIncident.state !== 'resolved' ? (
                        <div className="incident-actions">
                          <label>
                            Operator reason
                            <textarea
                              name="alert_reason"
                              value={incidents.reason}
                              onChange={(event) =>
                                setIncidents((current) => ({
                                  ...current,
                                  reason: event.target.value,
                                }))
                              }
                              maxLength={500}
                            />
                          </label>
                          <div className="incident-action-row">
                            <button
                              type="button"
                              onClick={acknowledgeSelectedIncident}
                              disabled={
                                !isOnline || Boolean(incidents.submitting)
                              }
                            >
                              Acknowledge
                            </button>
                            <button
                              className="danger-button"
                              type="button"
                              onClick={resolveSelectedIncident}
                              disabled={
                                !isOnline ||
                                Boolean(incidents.submitting) ||
                                !incidents.reason.trim()
                              }
                            >
                              Resolve
                            </button>
                          </div>
                          <label>
                            Silence until
                            <input
                              name="silence_until"
                              type="datetime-local"
                              value={incidents.silenceUntil}
                              onChange={(event) =>
                                setIncidents((current) => ({
                                  ...current,
                                  silenceUntil: event.target.value,
                                }))
                              }
                            />
                          </label>
                          <button
                            type="button"
                            onClick={silenceSelectedIncident}
                            disabled={
                              !isOnline ||
                              Boolean(incidents.submitting) ||
                              !incidents.reason.trim()
                            }
                          >
                            Silence
                          </button>
                        </div>
                      ) : null}
                      <div className="incident-actions">
                        <label>
                          Annotation
                          <textarea
                            name="alert_annotation"
                            value={incidents.annotation}
                            onChange={(event) =>
                              setIncidents((current) => ({
                                ...current,
                                annotation: event.target.value,
                              }))
                            }
                            maxLength={4000}
                          />
                        </label>
                        <button
                          type="button"
                          onClick={annotateSelectedIncident}
                          disabled={
                            !isOnline ||
                            Boolean(incidents.submitting) ||
                            !incidents.annotation.trim()
                          }
                        >
                          Add annotation
                        </button>
                      </div>
                      {incidents.error ? (
                        <p className="form-error" role="alert">{incidents.error}</p>
                      ) : null}
                    </>
                  ) : (
                    <p className="empty-state">
                      Select an alert to inspect its incident lifecycle.
                    </p>
                  )}
                </aside>
              </div>
            ) : activeView === 'backups' ? (
              <section className="panel operations-module" aria-labelledby="backups-title">
                <div className="panel-heading">
                  <div>
                    <span className="section-label">Verified recovery artifacts</span>
                    <h2 id="backups-title">Backups</h2>
                  </div>
                  <div className="panel-actions">
                    <button type="button" onClick={loadBackups}>
                      Refresh
                    </button>
                    <button
                      type="button"
                      onClick={openBackupDialog}
                      disabled={!isOnline}
                    >
                      Create verified backup
                    </button>
                  </div>
                </div>
                {backups.loading ? (
                  <p className="empty-state" role="status">Loading backups…</p>
                ) : backups.error ? (
                  <p className="empty-state error-state" role="alert">{backups.error}</p>
                ) : backups.items.length ? (
                  <div className="table-scroll">
                    <table className="mobile-summary-table backups-table">
                      <caption className="sr-only">Verified backups</caption>
                      <thead>
                        <tr>
                          <th scope="col">Created</th>
                          <th scope="col">Purpose</th>
                          <th scope="col">State</th>
                          <th scope="col">Schema</th>
                          <th scope="col">Size</th>
                          <th scope="col">Integrity</th>
                          <th scope="col">Artifact</th>
                          <th scope="col">Recovery</th>
                        </tr>
                      </thead>
                      <tbody>
                        {backups.items.map((item) => (
                          <tr key={item.id}>
                            <td>{new Date(item.created_at).toLocaleString()}</td>
                            <td>{item.purpose.replace('_', ' ')}</td>
                            <td>
                              <span className={`state-badge ${item.state}`}>
                                {item.state}
                              </span>
                            </td>
                            <td>{item.schema_version ?? '—'}</td>
                            <td>{formatBytes(item.size_bytes)}</td>
                            <td>
                              {item.quick_check === 'ok' && item.sha256
                                ? `verified · ${item.sha256.slice(0, 12)}`
                                : item.error || 'pending'}
                            </td>
                            <td>
                              {item.download_url ? (
                                <button
                                  className="mobile-unvalidated-action"
                                  type="button"
                                  onClick={() =>
                                    void startDownload(
                                      item.download_url!,
                                      item.file_name,
                                      'Verified backup',
                                    )
                                  }
                                >
                                  Download
                                </button>
                              ) : (
                                '—'
                              )}
                            </td>
                            <td>
                              {item.state === 'succeeded' ? (
                                <button
                                  type="button"
                                  onClick={() => openRestoreDialog(item)}
                                  disabled={!isOnline}
                                >
                                  Stage restore
                                </button>
                              ) : (
                                '—'
                              )}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                ) : (
                  <p className="empty-state">
                    No verified backups have been created.
                  </p>
                )}
                <div className="panel-heading">
                  <div>
                    <span className="section-label">Supervisor handoffs</span>
                    <h3>Restore history</h3>
                  </div>
                </div>
                {restores.loading ? (
                  <p className="empty-state" role="status">Loading restore history…</p>
                ) : restores.error ? (
                  <p className="empty-state error-state" role="alert">{restores.error}</p>
                ) : restores.items.length ? (
                  <div className="table-scroll">
                    <table className="mobile-summary-table restores-table">
                      <caption className="sr-only">Database restore history</caption>
                      <thead>
                        <tr>
                          <th scope="col">Requested</th>
                          <th scope="col">Backup</th>
                          <th scope="col">State</th>
                          <th scope="col">Schema</th>
                          <th scope="col">Outcome</th>
                        </tr>
                      </thead>
                      <tbody>
                        {restores.items.map((item) => (
                          <tr key={item.id}>
                            <td>{new Date(item.created_at).toLocaleString()}</td>
                            <td>{item.backup_id.slice(0, 12)}</td>
                            <td>
                              <span className={`state-badge ${item.state}`}>
                                {eventLabel(item.state)}
                              </span>
                            </td>
                            <td>{item.schema_version}</td>
                            <td>
                              {item.state === 'staged'
                                ? 'Restart the service to activate'
                                : item.rollback_reason ||
                                  item.error ||
                                  (item.completed_at
                                    ? new Date(item.completed_at).toLocaleString()
                                    : 'Pending supervisor health check')}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                ) : (
                  <p className="empty-state">
                    No restores have been staged.
                  </p>
                )}
                <div className="panel-heading">
                  <div>
                    <span className="section-label">Signed release channel</span>
                    <h3>Application updates</h3>
                  </div>
                  <div className="panel-actions">
                    <button
                      className="mobile-unvalidated-action"
                      type="button"
                      onClick={inspectAvailableUpdate}
                      disabled={updates.checking}
                    >
                      {updates.checking ? 'Checking…' : 'Check for update'}
                    </button>
                    {updates.inspection?.apply_allowed ? (
                      <button
                        type="button"
                        onClick={() =>
                          openUpdateDialog(updates.inspection as UpdateInspection)
                        }
                        disabled={!isOnline}
                      >
                        Stage trusted update
                      </button>
                    ) : null}
                  </div>
                </div>
                {updates.inspection ? (
                  <div className="configuration-source" aria-live="polite">
                    <div>
                      <span>
                        {updates.inspection.verified
                          ? 'Verified target'
                          : 'Inspection-only target'}
                      </span>
                      <code>{updates.inspection.version ?? 'No compatible target'}</code>
                    </div>
                    <p>
                      {updates.inspection.verified
                        ? `Threshold signatures and provenance verified · ${formatBytes(
                            updates.inspection.size_bytes,
                          )}`
                        : updates.inspection.reason ??
                          'Artifact execution remains blocked without configured trust.'}
                    </p>
                    {updates.inspection.schema_min ? (
                      <small>
                        API {updates.inspection.api_version} · schemas{' '}
                        {updates.inspection.schema_min}–
                        {updates.inspection.schema_max}
                      </small>
                    ) : null}
                  </div>
                ) : null}
                {updates.loading ? (
                  <p className="empty-state" role="status">Loading update history…</p>
                ) : updates.error ? (
                  <p className="empty-state error-state" role="alert">{updates.error}</p>
                ) : updates.items.length ? (
                  <div className="table-scroll">
                    <table className="mobile-summary-table updates-table">
                      <caption className="sr-only">Application update history</caption>
                      <thead>
                        <tr>
                          <th scope="col">Requested</th>
                          <th scope="col">Version</th>
                          <th scope="col">State</th>
                          <th scope="col">Compatibility</th>
                          <th scope="col">Integrity</th>
                          <th scope="col">Outcome</th>
                        </tr>
                      </thead>
                      <tbody>
                        {updates.items.map((item) => (
                          <tr key={item.id}>
                            <td>{new Date(item.created_at).toLocaleString()}</td>
                            <td>
                              {item.version}
                              <br />
                              <small>{item.commit.slice(0, 12)}</small>
                            </td>
                            <td>
                              <span className={`state-badge ${item.state}`}>
                                {eventLabel(item.state)}
                              </span>
                            </td>
                            <td>
                              API {item.api_version} · schemas {item.schema_min}–
                              {item.schema_max}
                            </td>
                            <td>
                              {formatBytes(item.size_bytes)} ·{' '}
                              {item.sha256.slice(0, 12)}
                            </td>
                            <td>
                              {item.state === 'staged'
                                ? 'Restart the service to activate'
                                : item.rollback_reason ||
                                  item.error ||
                                  (item.completed_at
                                    ? new Date(item.completed_at).toLocaleString()
                                    : 'Pending supervisor health check')}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                ) : (
                  <p className="empty-state">
                    No application updates have been staged.
                  </p>
                )}
              </section>
            ) : activeView === 'diagnostics' ? (
              <section className="panel operations-module" aria-labelledby="diagnostics-title">
                <div className="panel-heading">
                  <div>
                    <span className="section-label">Isolated host checks</span>
                    <h2 id="diagnostics-title">Diagnostics</h2>
                  </div>
                  {diagnostics.report ? (
                    <div className="panel-actions">
                      <span className={`health-summary ${diagnostics.report.status}`}>
                        {diagnostics.report.status}
                      </span>
                      <button
                        type="button"
                        onClick={openSupportBundleDialog}
                        disabled={!isOnline}
                      >
                        Create support bundle
                      </button>
                    </div>
                  ) : null}
                </div>
                {diagnostics.loading ? (
                  <p className="empty-state" role="status">Running host checks…</p>
                ) : diagnostics.error ? (
                  <p className="empty-state error-state" role="alert">{diagnostics.error}</p>
                ) : diagnostics.report ? (
                  <ul className="diagnostic-list">
                    {diagnostics.report.results.map((result) => (
                      <li key={result.id}>
                        <div className={`diagnostic-status ${result.status}`}>
                          {result.status}
                        </div>
                        <div>
                          <strong>{result.id}</strong>
                          <span>
                            {result.observed} · expected {result.expected}
                          </span>
                          {result.error ? <p>{result.error}</p> : null}
                          {result.remediation ? (
                            <small>{result.remediation}</small>
                          ) : null}
                        </div>
                        <time dateTime={result.observed_at}>
                          {result.duration_ms} ms
                        </time>
                      </li>
                    ))}
                  </ul>
                ) : (
                  <p className="empty-state">No diagnostic report is available.</p>
                )}
              </section>
            ) : activeView === 'configuration' ? (
              <section className="panel operations-module" aria-labelledby="configuration-title">
                <div className="panel-heading">
                  <div>
                    <span className="section-label">Read-only inspection</span>
                    <h2 id="configuration-title">Configuration</h2>
                  </div>
                  {configuration.snapshot ? (
                    <span
                      className={`health-summary ${
                        configuration.snapshot.drifted ? 'warn' : 'pass'
                      }`}
                    >
                      {configuration.snapshot.drifted ? 'drifted' : 'current'}
                    </span>
                  ) : null}
                </div>
                {configuration.loading ? (
                  <p className="empty-state" role="status">Inspecting configuration…</p>
                ) : configuration.error ? (
                  <p className="empty-state error-state" role="alert">{configuration.error}</p>
                ) : configuration.snapshot ? (
                  <>
                    <div className="configuration-source">
                      <div>
                        <span>Source file</span>
                        <code>{configuration.snapshot.source_file}</code>
                      </div>
                      <p>{configuration.snapshot.guidance}</p>
                      {configuration.snapshot.validation_error ? (
                        <p className="form-error" role="alert">
                          {configuration.snapshot.validation_error}
                        </p>
                      ) : null}
                    </div>
                    <div className="table-scroll">
                      <table className="mobile-summary-table configuration-table">
                        <caption className="sr-only">Effective configuration settings</caption>
                        <thead>
                          <tr>
                            <th scope="col">Setting</th>
                            <th scope="col">Effective value</th>
                            <th scope="col">Source</th>
                            <th scope="col">Restart</th>
                          </tr>
                        </thead>
                        <tbody>
                          {configuration.snapshot.fields.map((field) => (
                            <tr key={field.path}>
                              <td><code>{field.path}</code></td>
                              <td>{displayValue(field.effective)}</td>
                              <td>{field.source}</td>
                              <td>{field.restart_required ? 'Required' : 'No'}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                    <div className="configuration-views">
                      <details>
                        <summary>Redacted source YAML</summary>
                        <pre>{configuration.snapshot.raw_redacted}</pre>
                      </details>
                      <details>
                        <summary>Redacted normalized YAML</summary>
                        <pre>{configuration.snapshot.normalized_redacted}</pre>
                      </details>
                    </div>
                  </>
                ) : (
                  <p className="empty-state">
                    Configuration inspection is not available.
                  </p>
                )}
              </section>
            ) : (
              <section className="panel module-placeholder">
                <span className="section-label">Console module</span>
                <h2>{activeLabel}</h2>
                <p>
                  This production module is part of the approved console
                  architecture and will attach to the versioned local API.
                </p>
              </section>
            )}
          </main>
        </div>

        <aside
          className={`signal-rail ${stream.status} ${state.error ? 'unavailable' : ''}`}
          aria-label="Operational signal"
        >
          <span className="signal-track" aria-hidden="true" />
          <strong>{hostState}</strong>
          <span>{streamLabel}</span>
          <time dateTime={stream.lastEvent?.timestamp}>
            {stream.lastEvent
              ? formatRelativeTime(stream.lastEvent.timestamp)
              : 'No event yet'}
          </time>
        </aside>
        </div>
      </div>
      {paletteOpen ? (
        <div className="dialog-backdrop palette-backdrop">
          <section
            ref={paletteModalRef}
            className="command-palette"
            role="dialog"
            aria-modal="true"
            aria-labelledby="command-palette-title"
            tabIndex={-1}
          >
            <div className="dialog-heading">
              <div>
                <span className="section-label">Route and history search</span>
                <h2
                  id="command-palette-title"
                  data-modal-initial-focus
                  tabIndex={-1}
                >
                  Search Operations Console
                </h2>
              </div>
              <button
                type="button"
                aria-label="Close search"
                onClick={() => setPaletteOpen(false)}
              >
                ×
              </button>
            </div>
            <label className="palette-search">
              Search routes or durable history
              <input
                name="global_search"
                type="search"
                autoComplete="off"
                value={paletteQuery}
                onChange={(event) => setPaletteQuery(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter' && paletteQuery.trim().length >= 2) {
                    event.preventDefault()
                    searchFromPalette()
                  }
                }}
              />
            </label>
            <ul className="palette-results">
              {paletteRoutes.map(([label, id]) => (
                <li key={id}>
                  <button type="button" onClick={() => activateView(id)}>
                    <NavigationIcon icon={id} />
                    <span>{label}</span>
                    <small>Open route</small>
                  </button>
                </li>
              ))}
              {paletteQuery.trim().length >= 2 ? (
                <li>
                  <button type="button" onClick={searchFromPalette}>
                    <NavigationIcon icon="runs" />
                    <span>Search history for “{paletteQuery.trim()}”</span>
                    <small>Runs</small>
                  </button>
                </li>
              ) : null}
            </ul>
            {!paletteRoutes.length && paletteQuery.trim().length < 2 ? (
              <p className="empty-state">
                Enter at least 2 characters to search durable history.
              </p>
            ) : null}
          </section>
        </div>
      ) : null}
      {commandDialog ? (
        <div className="dialog-backdrop">
          <section
            ref={commandModalRef}
            className="command-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="command-dialog-title"
            tabIndex={-1}
          >
            <div className="dialog-heading">
              <div>
                <span className="section-label">High-impact control</span>
                <h2
                  id="command-dialog-title"
                  data-modal-initial-focus
                  tabIndex={-1}
                >
                  Terminate runner
                </h2>
              </div>
              <button
                type="button"
                aria-label="Close command dialog"
                onClick={() => setCommandDialog(null)}
              >
                ×
              </button>
            </div>
            <dl className="command-target">
              <div>
                <dt>Runner</dt>
                <dd>
                  {commandDialog.runner.runner_name ||
                    commandDialog.runner.id}
                </dd>
              </div>
              <div>
                <dt>Pool</dt>
                <dd>{commandDialog.runner.pool}</dd>
              </div>
            </dl>
            {commandDialog.loading ? (
              <p className="empty-state" role="status">Checking current runtime state…</p>
            ) : commandDialog.plan ? (
              <>
                <p className="impact-preview">{commandDialog.plan.impact}</p>
                {commandDialog.command ? (
                  <div
                    className={`command-progress ${commandDialog.command.state}`}
                    aria-live="polite"
                  >
                    <strong>
                      {eventLabel(commandDialog.command.state)}
                    </strong>
                    <span>
                      Command {commandDialog.command.id.slice(0, 12)}
                    </span>
                    {commandDialog.command.error ? (
                      <p>{commandDialog.command.error}</p>
                    ) : null}
                    <UnknownOutcomeDetails
                      command={commandDialog.command}
                      plan={commandDialog.plan}
                      target={
                        commandDialog.runner.runner_name ||
                        commandDialog.runner.id
                      }
                    />
                  </div>
                ) : (
                  <form
                    onSubmit={(event) => {
                      event.preventDefault()
                      if (!isOnline) return
                      setCommandDialog((current) =>
                        current
                          ? { ...current, submitting: true, error: undefined }
                          : current,
                      )
                      const input = {
                        type: 'runner.terminate',
                        target_type: 'runner',
                        target_id: commandDialog.runner.id,
                        parameters: { pool: commandDialog.runner.pool },
                        reason: commandDialog.reason,
                        confirmation: commandDialog.confirmation,
                      }
                      void createCommand(
                        input,
                        commandDialog.idempotencyKey,
                      )
                        .then((command) =>
                          setCommandDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  command,
                                  submitting: false,
                                }
                              : current,
                          ),
                        )
                        .catch((error: unknown) =>
                          setCommandDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  submitting: false,
                                  error:
                                    error instanceof Error
                                      ? error.message
                                      : 'Command could not be queued.',
                                }
                              : current,
                          ),
                        )
                    }}
                  >
                    <label>
                      Reason
                      <textarea
                        name="runner_termination_reason"
                        value={commandDialog.reason}
                        onChange={(event) =>
                          setCommandDialog((current) =>
                            current
                              ? { ...current, reason: event.target.value }
                              : current,
                          )
                        }
                        required
                      />
                    </label>
                    <label>
                      Type{' '}
                      <code>
                        {commandDialog.plan.confirmation_phrase}
                      </code>{' '}
                      to confirm
                      <input
                        name="runner_termination_confirmation"
                        value={commandDialog.confirmation}
                        onChange={(event) =>
                          setCommandDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  confirmation: event.target.value,
                                }
                              : current,
                          )
                        }
                        required
                        autoComplete="off"
                      />
                    </label>
                    {commandDialog.error ? (
                      <p className="form-error" role="alert">
                        {commandDialog.error}
                      </p>
                    ) : null}
                    <div className="dialog-actions">
                      <button
                        type="button"
                        onClick={() => setCommandDialog(null)}
                      >
                        Cancel
                      </button>
                      <button
                        className="danger-button"
                        type="submit"
                        disabled={
                          !isOnline ||
                          commandDialog.submitting ||
                          !commandDialog.reason.trim() ||
                          commandDialog.confirmation !==
                            commandDialog.plan.confirmation_phrase
                        }
                      >
                        {commandDialog.submitting
                          ? 'Queuing…'
                          : 'Terminate runner'}
                      </button>
                    </div>
                  </form>
                )}
              </>
            ) : (
              <p className="empty-state error-state" role="alert">
                {commandDialog.error ?? 'Command preview is unavailable.'}
              </p>
            )}
          </section>
        </div>
      ) : null}
      {backupDialog ? (
        <div className="dialog-backdrop">
          <section
            ref={backupModalRef}
            className="command-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="backup-dialog-title"
            tabIndex={-1}
          >
            <div className="dialog-heading">
              <div>
                <span className="section-label">Database recovery</span>
                <h2
                  id="backup-dialog-title"
                  data-modal-initial-focus
                  tabIndex={-1}
                >
                  Create verified backup
                </h2>
              </div>
              <button
                type="button"
                aria-label="Close backup dialog"
                onClick={() => setBackupDialog(null)}
              >
                ×
              </button>
            </div>
            {backupDialog.loading ? (
              <p className="empty-state" role="status">Checking backup capability…</p>
            ) : backupDialog.plan ? (
              <>
                <p className="impact-preview">{backupDialog.plan.impact}</p>
                {backupDialog.command ? (
                  <div
                    className={`command-progress ${backupDialog.command.state}`}
                    aria-live="polite"
                  >
                    <strong>{eventLabel(backupDialog.command.state)}</strong>
                    <span>Command {backupDialog.command.id.slice(0, 12)}</span>
                    {backupDialog.command.error ? (
                      <p>{backupDialog.command.error}</p>
                    ) : null}
                    <UnknownOutcomeDetails
                      command={backupDialog.command}
                      plan={backupDialog.plan}
                      target="Local history database backup"
                    />
                    {backupDownload(backupDialog.command) ? (
                      <button
                        type="button"
                        onClick={() =>
                          void startDownload(
                            backupDownload(backupDialog.command),
                            undefined,
                            'Verified SQLite backup',
                          )
                        }
                      >
                        Download verified SQLite backup
                      </button>
                    ) : null}
                  </div>
                ) : (
                  <form
                    onSubmit={(event) => {
                      event.preventDefault()
                      if (!isOnline) return
                      setBackupDialog((current) =>
                        current
                          ? { ...current, submitting: true, error: undefined }
                          : current,
                      )
                      void createCommand(
                        {
                          type: 'backup.create',
                          target_type: 'system',
                          target_id: 'backups',
                          parameters: { purpose: 'manual' },
                          reason: backupDialog.reason,
                          confirmation: backupDialog.confirmation,
                        },
                        backupDialog.idempotencyKey,
                      )
                        .then((command) =>
                          setBackupDialog((current) =>
                            current
                              ? { ...current, command, submitting: false }
                              : current,
                          ),
                        )
                        .catch((error: unknown) =>
                          setBackupDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  submitting: false,
                                  error:
                                    error instanceof Error
                                      ? error.message
                                      : 'Backup could not be queued.',
                                }
                              : current,
                          ),
                        )
                    }}
                  >
                    <label>
                      Reason
                      <textarea
                        name="backup_reason"
                        value={backupDialog.reason}
                        onChange={(event) =>
                          setBackupDialog((current) =>
                            current
                              ? { ...current, reason: event.target.value }
                              : current,
                          )
                        }
                        required
                      />
                    </label>
                    <label>
                      Type <code>{backupDialog.plan.confirmation_phrase}</code>{' '}
                      to confirm
                      <input
                        name="backup_confirmation"
                        value={backupDialog.confirmation}
                        onChange={(event) =>
                          setBackupDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  confirmation: event.target.value,
                                }
                              : current,
                          )
                        }
                        required
                        autoComplete="off"
                      />
                    </label>
                    {backupDialog.error ? (
                      <p className="form-error" role="alert">
                        {backupDialog.error}
                      </p>
                    ) : null}
                    <div className="dialog-actions">
                      <button type="button" onClick={() => setBackupDialog(null)}>
                        Cancel
                      </button>
                      <button
                        className="primary-button"
                        type="submit"
                        disabled={
                          !isOnline ||
                          backupDialog.submitting ||
                          !backupDialog.reason.trim() ||
                          backupDialog.confirmation !==
                            backupDialog.plan.confirmation_phrase
                        }
                      >
                        {backupDialog.submitting
                          ? 'Queuing…'
                          : 'Create verified backup'}
                      </button>
                    </div>
                  </form>
                )}
              </>
            ) : (
              <p className="empty-state error-state" role="alert">
                {backupDialog.error ?? 'Backup capability is unavailable.'}
              </p>
            )}
          </section>
        </div>
      ) : null}
      {restoreDialog ? (
        <div className="dialog-backdrop">
          <section
            ref={restoreModalRef}
            className="command-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="restore-dialog-title"
            tabIndex={-1}
          >
            <div className="dialog-heading">
              <div>
                <span className="section-label">Restart-gated recovery</span>
                <h2
                  id="restore-dialog-title"
                  data-modal-initial-focus
                  tabIndex={-1}
                >
                  Stage database restore
                </h2>
              </div>
              <button
                type="button"
                aria-label="Close restore dialog"
                onClick={() => setRestoreDialog(null)}
              >
                ×
              </button>
            </div>
            {restoreDialog.loading ? (
              <p className="empty-state" role="status">Checking restore capability…</p>
            ) : restoreDialog.plan ? (
              <>
                <p className="impact-preview">{restoreDialog.plan.impact}</p>
                <p>
                  Backup <code>{restoreDialog.backup.id}</code> will be copied
                  into a protected staging area. The running database is not
                  changed by this command.
                </p>
                {restoreDialog.command ? (
                  <div
                    className={`command-progress ${restoreDialog.command.state}`}
                    aria-live="polite"
                  >
                    <strong>{eventLabel(restoreDialog.command.state)}</strong>
                    <span>Command {restoreDialog.command.id.slice(0, 12)}</span>
                    {restoreDialog.command.state === 'succeeded' ? (
                      <p>
                        Restore staged. Restart the multirunner service to
                        activate it; startup rolls back automatically if health
                        confirmation fails.
                      </p>
                    ) : null}
                    {restoreDialog.command.error ? (
                      <p>{restoreDialog.command.error}</p>
                    ) : null}
                    <UnknownOutcomeDetails
                      command={restoreDialog.command}
                      plan={restoreDialog.plan}
                      target={`Restore ${restoreDialog.backup.id}`}
                    />
                  </div>
                ) : (
                  <form
                    onSubmit={(event) => {
                      event.preventDefault()
                      if (!isOnline) return
                      setRestoreDialog((current) =>
                        current
                          ? { ...current, submitting: true, error: undefined }
                          : current,
                      )
                      void createCommand(
                        {
                          type: 'restore.stage',
                          target_type: 'system',
                          target_id: 'restore',
                          parameters: {
                            backup_id: restoreDialog.backup.id,
                          },
                          reason: restoreDialog.reason,
                          confirmation: restoreDialog.confirmation,
                        },
                        restoreDialog.idempotencyKey,
                      )
                        .then((command) =>
                          setRestoreDialog((current) =>
                            current
                              ? { ...current, command, submitting: false }
                              : current,
                          ),
                        )
                        .catch((error: unknown) =>
                          setRestoreDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  submitting: false,
                                  error:
                                    error instanceof Error
                                      ? error.message
                                      : 'Restore could not be staged.',
                                }
                              : current,
                          ),
                        )
                    }}
                  >
                    <label>
                      Reason
                      <textarea
                        name="restore_reason"
                        value={restoreDialog.reason}
                        onChange={(event) =>
                          setRestoreDialog((current) =>
                            current
                              ? { ...current, reason: event.target.value }
                              : current,
                          )
                        }
                        required
                      />
                    </label>
                    <label>
                      Type <code>{restoreDialog.plan.confirmation_phrase}</code>{' '}
                      to confirm
                      <input
                        name="restore_confirmation"
                        value={restoreDialog.confirmation}
                        onChange={(event) =>
                          setRestoreDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  confirmation: event.target.value,
                                }
                              : current,
                          )
                        }
                        required
                        autoComplete="off"
                      />
                    </label>
                    {restoreDialog.error ? (
                      <p className="form-error" role="alert">
                        {restoreDialog.error}
                      </p>
                    ) : null}
                    <div className="dialog-actions">
                      <button
                        type="button"
                        onClick={() => setRestoreDialog(null)}
                      >
                        Cancel
                      </button>
                      <button
                        className="primary-button mobile-unvalidated-action"
                        type="submit"
                        disabled={
                          !isOnline ||
                          restoreDialog.submitting ||
                          !restoreDialog.reason.trim() ||
                          restoreDialog.confirmation !==
                            restoreDialog.plan.confirmation_phrase
                        }
                      >
                        {restoreDialog.submitting
                          ? 'Staging…'
                          : 'Stage restore'}
                      </button>
                    </div>
                  </form>
                )}
              </>
            ) : (
              <p className="empty-state error-state" role="alert">
                {restoreDialog.error ?? 'Restore capability is unavailable.'}
              </p>
            )}
          </section>
        </div>
      ) : null}
      {updateDialog ? (
        <div className="dialog-backdrop">
          <section
            ref={updateModalRef}
            className="command-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="update-dialog-title"
            tabIndex={-1}
          >
            <div className="dialog-heading">
              <div>
                <span className="section-label">Restart-gated maintenance</span>
                <h2
                  id="update-dialog-title"
                  data-modal-initial-focus
                  tabIndex={-1}
                >
                  Stage trusted update
                </h2>
              </div>
              <button
                type="button"
                aria-label="Close update dialog"
                onClick={() => setUpdateDialog(null)}
              >
                ×
              </button>
            </div>
            {updateDialog.loading ? (
              <p className="empty-state" role="status">Checking update capability…</p>
            ) : updateDialog.plan ? (
              <>
                <p className="impact-preview">{updateDialog.plan.impact}</p>
                <p>
                  Target <code>{updateDialog.inspection.version}</code> is
                  signature- and provenance-verified for this host. The running
                  executable is not changed by this command.
                </p>
                {updateDialog.command ? (
                  <div
                    className={`command-progress ${updateDialog.command.state}`}
                    aria-live="polite"
                  >
                    <strong>{eventLabel(updateDialog.command.state)}</strong>
                    <span>Command {updateDialog.command.id.slice(0, 12)}</span>
                    {updateDialog.command.state === 'succeeded' ? (
                      <p>
                        Update staged. Restart the multirunner service to
                        activate it; the supervisor automatically returns to the
                        previous worker if startup health confirmation fails.
                      </p>
                    ) : null}
                    {updateDialog.command.error ? (
                      <p>{updateDialog.command.error}</p>
                    ) : null}
                    <UnknownOutcomeDetails
                      command={updateDialog.command}
                      plan={updateDialog.plan}
                      target={`Update ${updateDialog.inspection.version}`}
                    />
                  </div>
                ) : (
                  <form
                    onSubmit={(event) => {
                      event.preventDefault()
                      if (!isOnline) return
                      setUpdateDialog((current) =>
                        current
                          ? { ...current, submitting: true, error: undefined }
                          : current,
                      )
                      void createCommand(
                        {
                          type: 'update.stage',
                          target_type: 'system',
                          target_id: 'updates',
                          parameters: {
                            version: updateDialog.inspection.version,
                          },
                          reason: updateDialog.reason,
                          confirmation: updateDialog.confirmation,
                        },
                        updateDialog.idempotencyKey,
                      )
                        .then((command) =>
                          setUpdateDialog((current) =>
                            current
                              ? { ...current, command, submitting: false }
                              : current,
                          ),
                        )
                        .catch((error: unknown) =>
                          setUpdateDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  submitting: false,
                                  error:
                                    error instanceof Error
                                      ? error.message
                                      : 'Update could not be staged.',
                                }
                              : current,
                          ),
                        )
                    }}
                  >
                    <label>
                      Reason
                      <textarea
                        name="update_reason"
                        value={updateDialog.reason}
                        onChange={(event) =>
                          setUpdateDialog((current) =>
                            current
                              ? { ...current, reason: event.target.value }
                              : current,
                          )
                        }
                        required
                      />
                    </label>
                    <label>
                      Type <code>{updateDialog.plan.confirmation_phrase}</code>{' '}
                      to confirm
                      <input
                        name="update_confirmation"
                        value={updateDialog.confirmation}
                        onChange={(event) =>
                          setUpdateDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  confirmation: event.target.value,
                                }
                              : current,
                          )
                        }
                        required
                        autoComplete="off"
                      />
                    </label>
                    {updateDialog.error ? (
                      <p className="form-error" role="alert">
                        {updateDialog.error}
                      </p>
                    ) : null}
                    <div className="dialog-actions">
                      <button
                        type="button"
                        onClick={() => setUpdateDialog(null)}
                      >
                        Cancel
                      </button>
                      <button
                        className="primary-button mobile-unvalidated-action"
                        type="submit"
                        disabled={
                          !isOnline ||
                          updateDialog.submitting ||
                          !updateDialog.reason.trim() ||
                          updateDialog.confirmation !==
                            updateDialog.plan.confirmation_phrase
                        }
                      >
                        {updateDialog.submitting
                          ? 'Staging…'
                          : 'Stage trusted update'}
                      </button>
                    </div>
                  </form>
                )}
              </>
            ) : (
              <p className="empty-state error-state" role="alert">
                {updateDialog.error ?? 'Update capability is unavailable.'}
              </p>
            )}
          </section>
        </div>
      ) : null}
      {supportBundleDialog ? (
        <div className="dialog-backdrop">
          <section
            ref={supportBundleModalRef}
            className="command-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="support-bundle-title"
            tabIndex={-1}
          >
            <div className="dialog-heading">
              <div>
                <span className="section-label">Audited diagnostic export</span>
                <h2
                  id="support-bundle-title"
                  data-modal-initial-focus
                  tabIndex={-1}
                >
                  Create support bundle
                </h2>
              </div>
              <button
                type="button"
                aria-label="Close support bundle dialog"
                onClick={() => setSupportBundleDialog(null)}
              >
                ×
              </button>
            </div>
            {supportBundleDialog.loading ? (
              <p className="empty-state" role="status">Checking bundle capability…</p>
            ) : supportBundleDialog.plan ? (
              <>
                <p className="impact-preview">
                  {supportBundleDialog.plan.impact}
                </p>
                {supportBundleDialog.command ? (
                  <div
                    className={`command-progress ${supportBundleDialog.command.state}`}
                    aria-live="polite"
                  >
                    <strong>
                      {eventLabel(supportBundleDialog.command.state)}
                    </strong>
                    <span>
                      Command {supportBundleDialog.command.id.slice(0, 12)}
                    </span>
                    {supportBundleDialog.command.error ? (
                      <p>{supportBundleDialog.command.error}</p>
                    ) : null}
                    <UnknownOutcomeDetails
                      command={supportBundleDialog.command}
                      plan={supportBundleDialog.plan}
                      target="Support bundle generation"
                    />
                    {supportBundleDownload(supportBundleDialog.command) ? (
                      <button
                        type="button"
                        onClick={() =>
                          void startDownload(
                            supportBundleDownload(
                              supportBundleDialog.command,
                            ),
                            undefined,
                            'Redacted support bundle',
                          )
                        }
                      >
                        Download redacted ZIP
                      </button>
                    ) : null}
                  </div>
                ) : (
                  <form
                    onSubmit={(event) => {
                      event.preventDefault()
                      if (!isOnline) return
                      setSupportBundleDialog((current) =>
                        current
                          ? { ...current, submitting: true, error: undefined }
                          : current,
                      )
                      const input = {
                        type: 'support_bundle.generate',
                        target_type: 'system',
                        target_id: 'support-bundles',
                        parameters: {
                          from: new Date(
                            supportBundleDialog.from,
                          ).toISOString(),
                          to: new Date(supportBundleDialog.to).toISOString(),
                        },
                        reason: supportBundleDialog.reason,
                        confirmation: supportBundleDialog.confirmation,
                      }
                      void createCommand(
                        input,
                        supportBundleDialog.idempotencyKey,
                      )
                        .then((command) =>
                          setSupportBundleDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  command,
                                  submitting: false,
                                }
                              : current,
                          ),
                        )
                        .catch((error: unknown) =>
                          setSupportBundleDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  submitting: false,
                                  error:
                                    error instanceof Error
                                      ? error.message
                                      : 'Support bundle could not be queued.',
                                }
                              : current,
                          ),
                        )
                    }}
                  >
                    <div className="date-range">
                      <label>
                        From
                        <input
                          name="support_bundle_from"
                          type="datetime-local"
                          value={supportBundleDialog.from}
                          onChange={(event) =>
                            setSupportBundleDialog((current) =>
                              current
                                ? { ...current, from: event.target.value }
                                : current,
                            )
                          }
                          required
                        />
                      </label>
                      <label>
                        To
                        <input
                          name="support_bundle_to"
                          type="datetime-local"
                          value={supportBundleDialog.to}
                          onChange={(event) =>
                            setSupportBundleDialog((current) =>
                              current
                                ? { ...current, to: event.target.value }
                                : current,
                            )
                          }
                          required
                        />
                      </label>
                    </div>
                    <label>
                      Reason
                      <textarea
                        name="support_bundle_reason"
                        value={supportBundleDialog.reason}
                        onChange={(event) =>
                          setSupportBundleDialog((current) =>
                            current
                              ? { ...current, reason: event.target.value }
                              : current,
                          )
                        }
                        required
                      />
                    </label>
                    <label>
                      Type{' '}
                      <code>
                        {supportBundleDialog.plan.confirmation_phrase}
                      </code>{' '}
                      to confirm
                      <input
                        name="support_bundle_confirmation"
                        value={supportBundleDialog.confirmation}
                        onChange={(event) =>
                          setSupportBundleDialog((current) =>
                            current
                              ? {
                                  ...current,
                                  confirmation: event.target.value,
                                }
                              : current,
                          )
                        }
                        required
                        autoComplete="off"
                      />
                    </label>
                    {supportBundleDialog.error ? (
                      <p className="form-error" role="alert">
                        {supportBundleDialog.error}
                      </p>
                    ) : null}
                    <div className="dialog-actions">
                      <button
                        type="button"
                        onClick={() => setSupportBundleDialog(null)}
                      >
                        Cancel
                      </button>
                      <button
                        className="primary-button"
                        type="submit"
                        disabled={
                          !isOnline ||
                          supportBundleDialog.submitting ||
                          !supportBundleDialog.reason.trim() ||
                          supportBundleDialog.confirmation !==
                            supportBundleDialog.plan.confirmation_phrase
                        }
                      >
                        {supportBundleDialog.submitting
                          ? 'Queuing…'
                          : 'Generate support bundle'}
                      </button>
                    </div>
                  </form>
                )}
              </>
            ) : (
              <p className="empty-state error-state" role="alert">
                {supportBundleDialog.error ??
                  'Support bundle preview is unavailable.'}
              </p>
            )}
          </section>
        </div>
      ) : null}
    </div>
  )
}

export default App

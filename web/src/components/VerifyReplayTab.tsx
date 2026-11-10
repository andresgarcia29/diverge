import { useState } from 'react'
import { Link } from 'react-router-dom'
import {
  useVerifyEnvironment,
  useReplayEnvironment,
  type VerifyEnvironmentResponse,
  type ReplayEnvironmentResponse,
  type ReplayEntryResult,
} from '@/api/queries'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { AlertCircle, CheckCircle2, Play, Sparkles, ShieldCheck, ArrowRightLeft } from 'lucide-react'

interface VerifyReplayTabProps {
  namespace: string
  environmentName: string
}

const DEFAULT_SAMPLE_TRACE = `{"method":"GET","path":"/healthz"}
{"method":"GET","path":"/readyz"}`

/**
 * Splits a comma- or newline-delimited user input string into trimmed non-empty tokens.
 */
function splitListInput(raw: string): string[] {
  return raw
    .split(/[\n,]+/)
    .map(item => item.trim())
    .filter(Boolean)
}

/**
 * Renders the summary banner and self-healing AgentTask link when auto-fix triggers.
 */
function AutoFixNotice({ namespace, taskName }: { namespace: string; taskName?: string }) {
  if (!taskName) return null
  return (
    <div
      className="bg-primary/10 border border-primary/20 text-primary px-4 py-3 rounded-md flex items-center justify-between gap-2 text-sm"
      role="status"
    >
      <div className="flex items-center gap-2">
        <Sparkles className="h-4 w-4 flex-shrink-0" />
        <span>
          Self-healing AgentTask <strong>{taskName}</strong> was automatically spawned to diagnose and fix this failure.
        </span>
      </div>
      <Link
        to={`/tasks/${namespace}/${taskName}`}
        className="underline hover:no-underline font-medium whitespace-nowrap"
      >
        View AgentTask →
      </Link>
    </div>
  )
}

/**
 * Renders the verification checks table returned from POST /api/v1/environments/verify.
 */
function VerifyResultView({
  namespace,
  result,
}: {
  namespace: string
  result: VerifyEnvironmentResponse
}) {
  return (
    <div className="space-y-4 mt-4" data-testid="verify-result-panel">
      <div
        className={`px-4 py-3 rounded-md border flex items-center justify-between text-sm ${
          result.passed
            ? 'bg-green-500/10 border-green-500/20 text-green-700 dark:text-green-400'
            : 'bg-destructive/10 border-destructive/20 text-destructive'
        }`}
      >
        <div className="flex items-center gap-2 font-medium">
          {result.passed ? (
            <CheckCircle2 className="h-4 w-4 flex-shrink-0" />
          ) : (
            <AlertCircle className="h-4 w-4 flex-shrink-0" />
          )}
          <span>
            Verification {result.passed ? 'Passed' : 'Failed'} ({result.checks.filter(c => c.passed).length}/
            {result.checks.length} checks passed)
          </span>
        </div>
        {result.routingHeader && (
          <span className="font-mono text-xs opacity-80">{result.routingHeader}</span>
        )}
      </div>

      <AutoFixNotice namespace={namespace} taskName={result.autoFixTaskName} />

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Check</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Latency</TableHead>
            <TableHead>Details</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {result.checks.map((check, idx) => (
            <TableRow key={`${check.name}-${idx}`}>
              <TableCell className="font-mono text-xs font-medium">{check.name}</TableCell>
              <TableCell>
                <span
                  className={`inline-flex items-center gap-1 text-xs font-medium ${
                    check.passed ? 'text-green-600 dark:text-green-400' : 'text-destructive'
                  }`}
                >
                  {check.passed ? 'PASS' : 'FAIL'} ({check.status})
                </span>
              </TableCell>
              <TableCell className="font-mono text-xs">
                {check.durationMs ? `${check.durationMs}ms` : '—'}
              </TableCell>
              <TableCell className="text-xs text-muted-foreground">{check.message}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

/**
 * Formats contract diffs and failure reasons for a single replayed request row.
 */
function renderReplayRowDetails(row: ReplayEntryResult) {
  const diffs = row.contractDiffs ?? []
  const reasons = row.failureReasons ?? []
  const extractedEntries = Object.entries(row.extractedVars ?? {})
  if (diffs.length === 0 && reasons.length === 0 && extractedEntries.length === 0) {
    return <span className="text-xs text-muted-foreground">No contract drift</span>
  }
  return (
    <div className="space-y-1">
      {extractedEntries.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {extractedEntries.map(([key, val]) => (
            <span
              key={`ext-${key}`}
              className="inline-flex items-center rounded bg-primary/10 px-1.5 py-0.5 font-mono text-[11px] text-primary"
            >
              {key}={val}
            </span>
          ))}
        </div>
      )}
      {reasons.map((reason, idx) => (
        <div key={`reason-${idx}`} className="text-xs text-destructive">
          {reason}
        </div>
      ))}
      {diffs.map((diff, idx) => (
        <div key={`diff-${idx}`} className="font-mono text-xs">
          <span className={diff.breaking ? 'text-destructive font-semibold' : 'text-amber-600 dark:text-amber-400'}>
            [{diff.breaking ? 'BREAKING' : 'INFO'} {diff.kind}]
          </span>{' '}
          <span>{diff.path}</span>
          {(diff.baseline || diff.preview) && (
            <span className="text-muted-foreground">
              {' '}
              ({diff.baseline ?? '∅'} → {diff.preview ?? '∅'})
            </span>
          )}
        </div>
      ))}
    </div>
  )
}

/**
 * Renders the contract replay report table returned from POST /api/v1/environments/replay.
 */
function ReplayResultView({
  namespace,
  result,
}: {
  namespace: string
  result: ReplayEnvironmentResponse
}) {
  return (
    <div className="space-y-4 mt-4" data-testid="replay-result-panel">
      <div
        className={`px-4 py-3 rounded-md border flex flex-wrap items-center justify-between gap-2 text-sm ${
          result.passed
            ? 'bg-green-500/10 border-green-500/20 text-green-700 dark:text-green-400'
            : 'bg-destructive/10 border-destructive/20 text-destructive'
        }`}
      >
        <div className="flex items-center gap-2 font-medium">
          {result.passed ? (
            <CheckCircle2 className="h-4 w-4 flex-shrink-0" />
          ) : (
            <AlertCircle className="h-4 w-4 flex-shrink-0" />
          )}
          <span>
            Contract Replay {result.passed ? 'Passed' : 'Failed'} ({result.passedRequests}/{result.totalRequests}{' '}
            passed, {result.breakingDiffs} breaking diffs, {result.statusMismatches} status mismatches)
          </span>
        </div>
        <span className="font-mono text-xs opacity-80">
          p95: baseline {result.baselineP95Ms}ms vs preview {result.previewP95Ms}ms
        </span>
      </div>

      <AutoFixNotice namespace={namespace} taskName={result.autoFixTaskName} />

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Request</TableHead>
            <TableHead>Protocol</TableHead>
            <TableHead>Baseline</TableHead>
            <TableHead>Preview</TableHead>
            <TableHead>Contract Diffs / Status</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(result.results ?? []).map(row => (
            <TableRow key={row.index}>
              <TableCell className="font-mono text-xs font-medium">
                {row.method} {row.path}
              </TableCell>
              <TableCell className="font-mono text-xs">{row.protocol || 'http'}</TableCell>
              <TableCell className="font-mono text-xs">
                {row.baselineStatus} ({row.baselineLatencyMs}ms)
              </TableCell>
              <TableCell className="font-mono text-xs">
                {row.previewStatus} ({row.previewLatencyMs}ms)
              </TableCell>
              <TableCell>{renderReplayRowDetails(row)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

/**
 * Interactive tab for running environment verification probes and deterministic contract replay from the Web UI.
 */
export function VerifyReplayTab({ namespace, environmentName }: VerifyReplayTabProps) {
  const verifyMutation = useVerifyEnvironment()
  const replayMutation = useReplayEnvironment()

  // Verification state
  const [verifyPaths, setVerifyPaths] = useState('/healthz, /readyz')
  const [verifyBaseUrl, setVerifyBaseUrl] = useState('')
  const [verifyMaxLatencyMs, setVerifyMaxLatencyMs] = useState('2000')
  const [compareBaseline, setCompareBaseline] = useState(true)
  const [skipDoctor, setSkipDoctor] = useState(false)
  const [verifyAutoFix, setVerifyAutoFix] = useState(false)

  // Replay state
  const [traceContent, setTraceContent] = useState(DEFAULT_SAMPLE_TRACE)
  const [replayBaseUrl, setReplayBaseUrl] = useState('')
  const [ignoreFields, setIgnoreFields] = useState('')
  const [redactQueryParams, setRedactQueryParams] = useState('')
  const [redactBodyFields, setRedactBodyFields] = useState('')
  const [compareValues, setCompareValues] = useState(false)
  const [failOnExtraFields, setFailOnExtraFields] = useState(false)
  const [stripAuth, setStripAuth] = useState(true)
  const [replayAutoFix, setReplayAutoFix] = useState(false)

  const handleRunVerify = () => {
    const parsedLatency = parseInt(verifyMaxLatencyMs, 10)
    verifyMutation.mutate({
      name: environmentName,
      namespace,
      baseUrl: verifyBaseUrl.trim() || undefined,
      paths: splitListInput(verifyPaths),
      maxLatencyMs: Number.isFinite(parsedLatency) && parsedLatency > 0 ? parsedLatency : undefined,
      compareBaseline,
      skipDoctor,
      autoFix: verifyAutoFix,
    })
  }

  const handleRunReplay = () => {
    replayMutation.mutate({
      name: environmentName,
      namespace,
      baseUrl: replayBaseUrl.trim() || undefined,
      traceContent,
      compareValues,
      failOnExtraFields,
      stripAuth,
      ignoreFields: splitListInput(ignoreFields),
      redactQueryParams: splitListInput(redactQueryParams),
      redactBodyFields: splitListInput(redactBodyFields),
      autoFix: replayAutoFix,
    })
  }

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center gap-2">
            <ShieldCheck className="h-4 w-4 text-primary" />
            Health, Readiness &amp; Baseline Verification
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid gap-4 md:grid-cols-3">
            <div className="space-y-1.5">
              <label htmlFor="verify-paths" className="text-xs font-medium text-muted-foreground">
                Probe Paths (comma-separated)
              </label>
              <Input
                id="verify-paths"
                value={verifyPaths}
                onChange={e => setVerifyPaths(e.target.value)}
                placeholder="/healthz, /readyz"
              />
            </div>
            <div className="space-y-1.5">
              <label htmlFor="verify-base-url" className="text-xs font-medium text-muted-foreground">
                Baseline URL Override (optional)
              </label>
              <Input
                id="verify-base-url"
                value={verifyBaseUrl}
                onChange={e => setVerifyBaseUrl(e.target.value)}
                placeholder="https://staging.example.com"
              />
            </div>
            <div className="space-y-1.5">
              <label htmlFor="verify-latency" className="text-xs font-medium text-muted-foreground">
                Max Probe Latency (ms)
              </label>
              <Input
                id="verify-latency"
                type="number"
                value={verifyMaxLatencyMs}
                onChange={e => setVerifyMaxLatencyMs(e.target.value)}
              />
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-6 text-sm">
            <label className="inline-flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={compareBaseline}
                onChange={e => setCompareBaseline(e.target.checked)}
              />
              <span>Compare against Baseline</span>
            </label>
            <label className="inline-flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={skipDoctor}
                onChange={e => setSkipDoctor(e.target.checked)}
              />
              <span>Skip Kubernetes Doctor</span>
            </label>
            <label className="inline-flex items-center gap-2 cursor-pointer font-medium text-primary">
              <input
                type="checkbox"
                checked={verifyAutoFix}
                onChange={e => setVerifyAutoFix(e.target.checked)}
              />
              <span>Auto-Fix on Failure (Spawn AgentTask)</span>
            </label>
          </div>

          <div className="flex items-center gap-3">
            <Button onClick={handleRunVerify} disabled={verifyMutation.isPending}>
              <Play className="h-4 w-4 mr-2" />
              {verifyMutation.isPending ? 'Running Verification…' : 'Run Verification'}
            </Button>
          </div>

          {verifyMutation.error && (
            <div
              className="bg-destructive/10 border border-destructive/20 text-destructive px-4 py-3 rounded-md flex items-center gap-2 text-sm"
              role="alert"
            >
              <AlertCircle className="h-4 w-4 flex-shrink-0" />
              <span>
                {verifyMutation.error instanceof Error ? verifyMutation.error.message : 'Verification failed'}
              </span>
            </div>
          )}

          {verifyMutation.data && <VerifyResultView namespace={namespace} result={verifyMutation.data} />}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center gap-2">
            <ArrowRightLeft className="h-4 w-4 text-primary" />
            Deterministic Traffic Replay &amp; Contract Diffing
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-1.5">
            <label htmlFor="replay-trace-input" className="text-xs font-medium text-muted-foreground">
              Trace Payload (.jsonl, .json, or .har)
            </label>
            <textarea
              id="replay-trace-input"
              rows={5}
              value={traceContent}
              onChange={e => setTraceContent(e.target.value)}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-xs font-mono shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
              placeholder='{"method":"GET","path":"/api/orders"}'
            />
          </div>

          <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
            <div className="space-y-1.5">
              <label htmlFor="replay-base-url" className="text-xs font-medium text-muted-foreground">
                Baseline URL Override (optional)
              </label>
              <Input
                id="replay-base-url"
                value={replayBaseUrl}
                onChange={e => setReplayBaseUrl(e.target.value)}
                placeholder="https://staging.example.com"
              />
            </div>
            <div className="space-y-1.5">
              <label htmlFor="replay-ignore-fields" className="text-xs font-medium text-muted-foreground">
                Ignore Response Fields
              </label>
              <Input
                id="replay-ignore-fields"
                value={ignoreFields}
                onChange={e => setIgnoreFields(e.target.value)}
                placeholder="$.timestamp, $.requestId"
              />
            </div>
            <div className="space-y-1.5">
              <label htmlFor="replay-redact-query" className="text-xs font-medium text-muted-foreground">
                Redact Query Params
              </label>
              <Input
                id="replay-redact-query"
                value={redactQueryParams}
                onChange={e => setRedactQueryParams(e.target.value)}
                placeholder="custom_token, session_key"
              />
            </div>
            <div className="space-y-1.5">
              <label htmlFor="replay-redact-body" className="text-xs font-medium text-muted-foreground">
                Redact Request Body Fields
              </label>
              <Input
                id="replay-redact-body"
                value={redactBodyFields}
                onChange={e => setRedactBodyFields(e.target.value)}
                placeholder="$.password, cardNumber"
              />
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-6 text-sm">
            <label className="inline-flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={stripAuth}
                onChange={e => setStripAuth(e.target.checked)}
              />
              <span>Strip Auth Headers &amp; Credential Query Params</span>
            </label>
            <label className="inline-flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={compareValues}
                onChange={e => setCompareValues(e.target.checked)}
              />
              <span>Compare Exact Values</span>
            </label>
            <label className="inline-flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={failOnExtraFields}
                onChange={e => setFailOnExtraFields(e.target.checked)}
              />
              <span>Fail on Extra Preview Fields</span>
            </label>
            <label className="inline-flex items-center gap-2 cursor-pointer font-medium text-primary">
              <input
                type="checkbox"
                checked={replayAutoFix}
                onChange={e => setReplayAutoFix(e.target.checked)}
              />
              <span>Auto-Fix on Contract Drift (Spawn AgentTask)</span>
            </label>
          </div>

          <div className="flex items-center gap-3">
            <Button onClick={handleRunReplay} disabled={replayMutation.isPending}>
              <Play className="h-4 w-4 mr-2" />
              {replayMutation.isPending ? 'Replaying Traffic…' : 'Run Contract Replay'}
            </Button>
          </div>

          {replayMutation.error && (
            <div
              className="bg-destructive/10 border border-destructive/20 text-destructive px-4 py-3 rounded-md flex items-center gap-2 text-sm"
              role="alert"
            >
              <AlertCircle className="h-4 w-4 flex-shrink-0" />
              <span>
                {replayMutation.error instanceof Error ? replayMutation.error.message : 'Contract replay failed'}
              </span>
            </div>
          )}

          {replayMutation.data && <ReplayResultView namespace={namespace} result={replayMutation.data} />}
        </CardContent>
      </Card>
    </div>
  )
}

import { useParams, useNavigate, Link } from 'react-router-dom'
import { useGetEnvironment, useDeleteEnvironment, useExtendTTL, useListHookJobs, useUpdateEnvironment } from '@/api/queries'
import { StatusBadge } from '@/components/StatusBadge'
import { TTLCountdown } from '@/components/TTLCountdown'
import { LogViewer } from '@/components/LogViewer'
import { HooksTab } from '@/components/HooksTab'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter } from '@/components/ui/dialog'
import { Select } from '@/components/ui/select'
import { useState, useEffect } from 'react'
import { stringify as yamlStringify } from 'yaml'
import { TopologyView } from '@/components/topology/TopologyView'
import { FlagsTab } from '@/components/FlagsTab'
import { VerifyReplayTab } from '@/components/VerifyReplayTab'
import { ArrowLeft, ExternalLink, Copy, Trash2, Clock, AlertCircle, CheckCircle, Wand2, Moon, Sun } from 'lucide-react'
import {
  estimateEnvironmentHourlyCostUSD,
  formatHourlyCostUSD,
  isEnvironmentHibernated,
  buildMagicPreviewUrl,
} from '@/lib/finops'

const TTL_DURATIONS = [
  { label: '1 hour', value: '3600s' },
  { label: '2 hours', value: '7200s' },
  { label: '4 hours', value: '14400s' },
  { label: '8 hours', value: '28800s' },
  { label: '24 hours', value: '86400s' },
  { label: '72 hours', value: '259200s' },
] as const

export default function EnvironmentDetail() {
  const { namespace = '', name = '' } = useParams()
  const navigate = useNavigate()
  const { data, isLoading, error, refetch } = useGetEnvironment(namespace, name)
  const deleteEnv = useDeleteEnvironment()
  const extendTtl = useExtendTTL()
  const updateEnv = useUpdateEnvironment()
  const hookJobs = useListHookJobs(namespace, name)
  const failedHookCount = (hookJobs.data?.jobs ?? []).filter(j => j.phase === 'Failed').length
  const [mutationError, setMutationError] = useState<string | null>(null)
  const [activeTab, setActiveTab] = useState('overview')
  const [extendDialogOpen, setExtendDialogOpen] = useState(false)
  const [selectedDuration, setSelectedDuration] = useState('3600s')
  const [successMessage, setSuccessMessage] = useState<string | null>(null)

  useEffect(() => {
    if (!successMessage) return
    const timer = setTimeout(() => setSuccessMessage(null), 4000)
    return () => clearTimeout(timer)
  }, [successMessage])

  if (isLoading) {
    return <div className="flex justify-center py-16"><div className="animate-spin rounded-full h-8 w-8 border-b-2 border-primary" /></div>
  }

  if (error) {
    return (
      <div className="text-center py-16 space-y-4" role="alert">
        <AlertCircle className="h-10 w-10 text-destructive mx-auto" />
        <h2 className="text-xl font-semibold">Failed to load environment</h2>
        <p className="text-muted-foreground">{error.message}</p>
        <Button variant="outline" onClick={() => refetch()}>Retry</Button>
      </div>
    )
  }

  const env = data?.environment
  if (!env) {
    return <div className="text-center py-16"><h2 className="text-xl font-semibold">Environment not found</h2></div>
  }

  const annotations = env.annotations ?? (env as { metadata?: { annotations?: Record<string, string> } }).metadata?.annotations ?? {}
  const featureFlagCount = Object.keys(env.spec?.features?.overrides ?? {}).length
  const hibernated = isEnvironmentHibernated(env)
  const magicUrl = buildMagicPreviewUrl(env)
  const estimatedCost = formatHourlyCostUSD(estimateEnvironmentHourlyCostUSD(env))
  const hibernationLabel = hibernated
    ? 'Hibernated (0 replicas)'
    : annotations['divergedev.com/hibernate-after']
      ? `Auto after ${annotations['divergedev.com/hibernate-after']}`
      : 'Active'
  const isDrifted = Boolean(env.status?.conditions?.some(c => c.type === 'BaselineDriftDetected' && c.status === 'True'))
  const driftCondition = env.status?.conditions?.find(c => c.type === 'BaselineDriftDetected')
  const driftPolicy = annotations['divergedev.com/drift-policy'] || 'warn'
  const baselineFingerprint = annotations['divergedev.com/baseline-fingerprint'] || ''
  const dbMode = env.spec?.database?.mode && env.spec.database.mode !== 'none' ? env.spec.database.mode : ''
  const dbProvider = env.status?.databaseProvider || ''
  const asyncRoutes = env.spec?.routing?.asyncRoutes ?? []
  const asyncEnvVars = env.status?.asyncEnvVars ?? {}
  const asyncEnvEntries = Object.entries(asyncEnvVars)
  const showIsolationCard = Boolean(dbMode || dbProvider || asyncEnvEntries.length > 0 || asyncRoutes.length > 0)

  const handleCopyMagicLink = async () => {
    if (!magicUrl) return
    try {
      await navigator.clipboard.writeText(magicUrl)
      setSuccessMessage('Magic preview link copied to clipboard')
    } catch {
      setMutationError('Failed to copy magic link to clipboard')
    }
  }

  const handleToggleHibernate = async () => {
    setMutationError(null)
    const nextHibernated = !hibernated
    try {
      await updateEnv.mutateAsync({
        environment: {
          name: env.name,
          namespace: env.namespace,
          resourceVersion: env.resourceVersion,
          labels: env.labels,
          annotations: {
            ...annotations,
            'divergedev.com/hibernated': nextHibernated ? 'true' : 'false',
          },
          spec: env.spec,
        },
        updateMask: { paths: ['annotations'] },
      })
      setSuccessMessage(nextHibernated ? 'Environment hibernated (scaled to 0 replicas)' : 'Environment waking up')
    } catch (err) {
      setMutationError(err instanceof Error ? err.message : 'Failed to update hibernation state')
    }
  }

  const handleSyncBaseline = async () => {
    setMutationError(null)
    try {
      await updateEnv.mutateAsync({
        environment: {
          name: env.name,
          namespace: env.namespace,
          resourceVersion: env.resourceVersion,
          labels: env.labels,
          annotations: {
            ...annotations,
            'divergedev.com/drift-policy': 'sync',
          },
          spec: env.spec,
        },
        updateMask: { paths: ['annotations'] },
      })
      setSuccessMessage('Baseline drift policy set to sync')
    } catch (err) {
      setMutationError(err instanceof Error ? err.message : 'Failed to sync with baseline')
    }
  }

  const handleDelete = async () => {
    if (!confirm(`Delete environment ${namespace}/${name}?`)) return
    setMutationError(null)
    try {
      await deleteEnv.mutateAsync({ namespace, name })
      navigate('/')
    } catch (err) {
      setMutationError(err instanceof Error ? err.message : 'Failed to delete environment')
    }
  }

  const handleExtendTTL = async () => {
    setMutationError(null)
    try {
      await extendTtl.mutateAsync({ namespace, name, duration: selectedDuration })
      setExtendDialogOpen(false)
      const label = TTL_DURATIONS.find(d => d.value === selectedDuration)?.label ?? selectedDuration
      setSuccessMessage(`TTL extended by ${label}`)
    } catch (err) {
      setMutationError(err instanceof Error ? err.message : 'Failed to extend TTL')
      setExtendDialogOpen(false)
    }
  }

  const conditionPhase = (type: string, status: string): string => {
    if (type === 'BaselineDriftDetected') {
      switch (status) {
        case 'False': return 'Ready'
        case 'True': return 'Pending'
        default: return 'Pending'
      }
    }
    switch (status) {
      case 'True': return 'Ready'
      case 'False': return 'Error'
      case 'Unknown': return 'Pending'
      default: return 'Pending'
    }
  }

  return (
    <div className="space-y-6">
      {mutationError && (
        <div className="bg-destructive/10 border border-destructive/20 text-destructive px-4 py-3 rounded-md flex items-center gap-2 text-sm" role="alert">
          <AlertCircle className="h-4 w-4 flex-shrink-0" />
          {mutationError}
        </div>
      )}

      {successMessage && (
        <div className="bg-emerald-500/10 border border-emerald-500/20 text-emerald-700 dark:text-emerald-400 px-4 py-3 rounded-md flex items-center gap-2 text-sm animate-in fade-in slide-in-from-top-2" role="status">
          <CheckCircle className="h-4 w-4 flex-shrink-0" />
          {successMessage}
        </div>
      )}

      {isDrifted && (
        <div className="bg-amber-500/10 border border-amber-500/20 text-amber-700 dark:text-amber-400 px-4 py-3 rounded-md flex items-center justify-between gap-4 text-sm">
          <div className="flex items-center gap-2">
            <AlertCircle className="h-4 w-4 flex-shrink-0" />
            <span>
              <strong>Baseline Drift Detected:</strong>{' '}
              {driftCondition?.message || 'Baseline workload, ConfigMap, or Secret changed since this preview was deployed.'}
            </span>
          </div>
          <Button variant="outline" size="sm" onClick={handleSyncBaseline} disabled={updateEnv.isPending}>
            Sync with Baseline
          </Button>
        </div>
      )}

      <div className="flex items-center gap-4">
        <Link to="/" aria-label="Back to environments" className="inline-flex items-center justify-center h-9 w-9 rounded-md text-sm font-medium hover:bg-accent hover:text-accent-foreground"><ArrowLeft className="h-4 w-4" /></Link>
        <div className="flex-1">
          <div className="flex items-center gap-3">
            <h1 className="text-2xl font-bold">{env.name}</h1>
            <StatusBadge phase={env.status?.phase ?? ''} />
          </div>
          <p className="text-muted-foreground">{env.namespace}</p>
        </div>
        <div className="flex items-center gap-2">
          {env.status?.url && (
            <>
              <a href={env.status.url} target="_blank" rel="noreferrer">
                <Button variant="outline" size="sm"><ExternalLink className="h-4 w-4 mr-2" />Open Preview</Button>
              </a>
              <Button variant="ghost" size="icon" aria-label="Copy preview URL" onClick={() => navigator.clipboard.writeText(env.status?.url ?? '')}>
                <Copy className="h-4 w-4" />
              </Button>
            </>
          )}
          {magicUrl && (
            <Button variant="outline" size="sm" aria-label="Copy magic link" onClick={handleCopyMagicLink}>
              <Wand2 className="h-4 w-4 mr-2" />Copy Magic Link
            </Button>
          )}
          <Button
            variant="outline"
            size="sm"
            aria-label={hibernated ? 'Wake environment' : 'Hibernate environment'}
            onClick={handleToggleHibernate}
            disabled={updateEnv.isPending}
          >
            {hibernated ? (
              <><Sun className="h-4 w-4 mr-2" />Wake Up</>
            ) : (
              <><Moon className="h-4 w-4 mr-2" />Hibernate</>
            )}
          </Button>
          <Button variant="outline" size="sm" onClick={() => setExtendDialogOpen(true)}>
            <Clock className="h-4 w-4 mr-2" />Extend TTL
          </Button>
          <Button variant="destructive" size="sm" onClick={handleDelete} disabled={deleteEnv.isPending}>
            <Trash2 className="h-4 w-4 mr-2" />Delete
          </Button>
        </div>
      </div>

      <div className="flex items-center gap-4 text-sm">
        <span className="text-muted-foreground">Branch:</span>
        <span>{env.spec?.source?.branch ?? '—'}</span>
        <span className="text-muted-foreground">TTL:</span>
        <TTLCountdown expiresAt={env.status?.expiresAt?.toDate?.()?.toISOString?.()} />
      </div>

      <Tabs value={activeTab} onValueChange={setActiveTab}>
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="topology">Topology</TabsTrigger>
          <TabsTrigger value="features">
            Features
            {featureFlagCount > 0 && (
              <span className="ml-1.5 inline-flex items-center justify-center h-5 min-w-5 px-1 rounded-full bg-primary/10 text-primary text-xs font-medium">
                {featureFlagCount}
              </span>
            )}
          </TabsTrigger>
          <TabsTrigger value="hooks">
            Hooks
            {failedHookCount > 0 && (
              <span className="ml-1.5 inline-flex items-center justify-center h-5 min-w-5 px-1 rounded-full bg-destructive text-destructive-foreground text-xs font-medium">
                {failedHookCount}
              </span>
            )}
          </TabsTrigger>
          <TabsTrigger value="verify-replay">Verify &amp; Replay</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          <TabsTrigger value="yaml">YAML</TabsTrigger>
        </TabsList>

        <TabsContent value="verify-replay">
          <VerifyReplayTab namespace={namespace} environmentName={name} />
        </TabsContent>

        <TabsContent value="topology">
          <Card>
            <CardContent className="p-0">
              <TopologyView environment={env} />
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="features">
          <FlagsTab environment={env} />
        </TabsContent>

        <TabsContent value="overview">
          {failedHookCount > 0 && (
            <div className="bg-destructive/10 border border-destructive/20 text-destructive px-4 py-3 rounded-md flex items-center gap-2 text-sm mb-4" role="alert">
              <AlertCircle className="h-4 w-4 flex-shrink-0" />
              <span>{failedHookCount} hook{failedHookCount > 1 ? 's' : ''} failed.</span>
              <button
                className="underline hover:no-underline font-medium"
                onClick={() => setActiveTab('hooks')}
              >
                View Hooks tab →
              </button>
            </div>
          )}
          <div className="grid gap-4 md:grid-cols-3">
            <Card>
              <CardHeader><CardTitle className="text-base">Details</CardTitle></CardHeader>
              <CardContent className="space-y-2 text-sm">
                <div className="flex items-center justify-between">
                  <span className="text-muted-foreground">Estimated Cost</span>
                  <span className="font-mono font-medium">{estimatedCost}</span>
                </div>
                <div className="flex items-center justify-between">
                  <span className="text-muted-foreground">Hibernation</span>
                  <span className="font-medium">{hibernationLabel}</span>
                </div>
                <div className="flex items-center justify-between">
                  <span className="text-muted-foreground">Baseline Drift Policy</span>
                  <span className="font-mono font-medium">{driftPolicy}</span>
                </div>
                {baselineFingerprint && (
                  <div className="flex items-center justify-between gap-2">
                    <span className="text-muted-foreground">Baseline Fingerprint</span>
                    <span className="font-mono text-xs truncate max-w-[180px]" title={baselineFingerprint}>
                      {baselineFingerprint}
                    </span>
                  </div>
                )}
              </CardContent>
            </Card>
            <Card>
              <CardHeader><CardTitle className="text-base">Spec</CardTitle></CardHeader>
              <CardContent>
                <pre className="text-xs bg-muted p-3 rounded overflow-auto max-h-64">{JSON.stringify(env.spec?.toJson?.() ?? env.spec, null, 2)}</pre>
              </CardContent>
            </Card>
            <Card>
              <CardHeader><CardTitle className="text-base">Conditions</CardTitle></CardHeader>
              <CardContent>
                {env.status?.conditions?.length ? (
                  <div className="space-y-2">
                    {env.status.conditions.map((c: { type: string; status: string }, i: number) => (
                      <div key={i} className="flex items-center justify-between text-sm">
                        <span>{c.type}</span>
                        <StatusBadge phase={conditionPhase(c.type, c.status)} />
                      </div>
                    ))}
                  </div>
                ) : <p className="text-sm text-muted-foreground">No conditions</p>}
              </CardContent>
            </Card>
          </div>
          {showIsolationCard && (
            <Card className="mt-4">
              <CardHeader><CardTitle className="text-base">Isolation &amp; Data Prefixes</CardTitle></CardHeader>
              <CardContent className="space-y-3 text-sm">
                {(dbMode || dbProvider) && (
                  <div className="flex items-center justify-between">
                    <span className="text-muted-foreground">Database Isolation</span>
                    <span className="font-mono font-medium">
                      {dbMode || dbProvider}
                      {dbProvider && dbMode && dbProvider !== dbMode ? ` (${dbProvider})` : ''}
                    </span>
                  </div>
                )}
                {asyncRoutes.length > 0 && (
                  <div className="space-y-1">
                    <span className="text-muted-foreground text-xs uppercase tracking-wider">Async Routes</span>
                    <div className="flex flex-wrap gap-2">
                      {asyncRoutes.map((route, idx) => (
                        <span key={idx} className="inline-flex items-center rounded bg-muted px-2 py-0.5 font-mono text-xs">
                          {route.protocol}: {route.target}
                        </span>
                      ))}
                    </div>
                  </div>
                )}
                {asyncEnvEntries.length > 0 && (
                  <div className="space-y-1">
                    <span className="text-muted-foreground text-xs uppercase tracking-wider">Injected Prefix Variables</span>
                    <div className="space-y-1">
                      {asyncEnvEntries.map(([key, val]) => (
                        <div key={key} className="flex items-center justify-between bg-muted/50 px-2.5 py-1 rounded font-mono text-xs">
                          <span>{key}</span>
                          <span className="text-muted-foreground">{val}</span>
                        </div>
                      ))}
                    </div>
                  </div>
                )}
              </CardContent>
            </Card>
          )}
        </TabsContent>

        <TabsContent value="hooks">
          <HooksTab namespace={namespace} environmentName={name} />
        </TabsContent>

        <TabsContent value="logs">
          <LogViewer namespace={namespace} environmentName={name} className="h-[600px]" />
        </TabsContent>

        <TabsContent value="yaml">
          <Card>
            <CardContent className="p-0">
              <pre className="text-xs p-4 overflow-auto max-h-[600px] font-mono">
                {yamlStringify(env.toJson?.() ?? env, { indent: 2 })}
              </pre>
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>

      <Dialog open={extendDialogOpen} onClose={() => setExtendDialogOpen(false)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Extend TTL</DialogTitle>
            <DialogDescription>Choose how long to extend the environment&apos;s time-to-live.</DialogDescription>
          </DialogHeader>
          <Select
            value={selectedDuration}
            onChange={e => setSelectedDuration(e.target.value)}
            aria-label="Extension duration"
          >
            {TTL_DURATIONS.map(d => (
              <option key={d.value} value={d.value}>{d.label}</option>
            ))}
          </Select>
          <DialogFooter>
            <Button variant="outline" onClick={() => setExtendDialogOpen(false)}>Cancel</Button>
            <Button onClick={handleExtendTTL} disabled={extendTtl.isPending}>
              {extendTtl.isPending ? 'Extending…' : 'Extend'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

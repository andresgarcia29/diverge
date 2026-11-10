import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import type { PartialMessage } from '@bufbuild/protobuf'
import type { AgentTaskSpec } from '@/api/gen/diverge/v1alpha1/agenttask_pb'
import { environmentClient, previewGroupClient, clusterClient, authClient, agentTaskClient, getToken } from '@/api/client'

export const queryKeys = {
  environments: (ns?: string) => ['environments', ns ?? 'all'] as const,
  environment: (ns: string, name: string) => ['environment', ns, name] as const,
  previewGroups: (ns?: string) => ['previewGroups', ns ?? 'all'] as const,
  previewGroup: (ns: string, name: string) => ['previewGroup', ns, name] as const,
  agentTasks: (ns?: string, pageSize = 100, pageToken = '') => ['agentTasks', ns ?? 'all', pageSize, pageToken] as const,
  agentTask: (ns: string, name: string) => ['agentTask', ns, name] as const,
  hookJobs: (ns: string, envName: string) => ['hookJobs', ns, envName] as const,
  clusterInfo: ['clusterInfo'] as const,
  currentUser: ['currentUser'] as const,
  permissions: (ns?: string) => ['permissions', ns ?? 'all'] as const,
}

export function useListEnvironments(params: { namespace?: string; phase?: string; pageSize?: number; pageToken?: string } = {}) {
  return useQuery({
    queryKey: queryKeys.environments(params.namespace),
    queryFn: () => environmentClient.listEnvironments({
      namespace: params.namespace ?? '',
      phase: params.phase ?? '',
      pageSize: params.pageSize ?? 100,
      pageToken: params.pageToken ?? '',
    }),
  })
}

export function useGetEnvironment(namespace: string, name: string) {
  return useQuery({
    queryKey: queryKeys.environment(namespace, name),
    queryFn: () => environmentClient.getEnvironment({ namespace, name }),
    enabled: !!name && !!namespace,
  })
}

export function useListPreviewGroups(params: { namespace?: string } = {}) {
  return useQuery({
    queryKey: queryKeys.previewGroups(params.namespace),
    queryFn: () => previewGroupClient.listPreviewGroups({ namespace: params.namespace ?? '' }),
  })
}

export function useGetPreviewGroup(namespace: string, name: string) {
  return useQuery({
    queryKey: queryKeys.previewGroup(namespace, name),
    queryFn: () => previewGroupClient.getPreviewGroup({ namespace, name }),
    enabled: !!name && !!namespace,
  })
}

export function useGetClusterInfo(namespace?: string) {
  return useQuery({
    queryKey: [...queryKeys.clusterInfo, namespace ?? ''] as const,
    queryFn: () => clusterClient.getClusterInfo({ namespace: namespace ?? '' }),
  })
}

export function useGetCurrentUser() {
  return useQuery({
    queryKey: queryKeys.currentUser,
    queryFn: () => authClient.getCurrentUser({}),
    retry: false,
  })
}

export function useListPermissions(namespace?: string) {
  return useQuery({
    queryKey: queryKeys.permissions(namespace),
    queryFn: () => authClient.listPermissions({ namespace: namespace ?? '' }),
  })
}

export function useCreateEnvironment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { name: string; namespace: string; spec: Record<string, unknown> }) =>
      environmentClient.createEnvironment(req),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['environments'] }) },
  })
}

export function useDeleteEnvironment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { namespace: string; name: string }) =>
      environmentClient.deleteEnvironment(req),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['environments'] }) },
  })
}

export function useUpdateEnvironment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: Parameters<typeof environmentClient.updateEnvironment>[0]) =>
      environmentClient.updateEnvironment(req),
    onSuccess: (data, vars) => {
      const ns = data.environment?.namespace || vars.environment?.namespace || ''
      const name = data.environment?.name || vars.environment?.name || ''
      if (ns && name) {
        if (data.environment) {
          qc.setQueryData(queryKeys.environment(ns, name), data)
        }
        qc.invalidateQueries({ queryKey: queryKeys.environment(ns, name) })
      }
      qc.invalidateQueries({ queryKey: ['environments'] })
    },
  })
}

export function useExtendTTL() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { namespace: string; name: string; duration: string }) =>
      environmentClient.extendTTL(req),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.environment(vars.namespace, vars.name) })
    },
  })
}

export function useCreatePreviewGroup() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { name: string; namespace: string; spec: Record<string, unknown> }) =>
      previewGroupClient.createPreviewGroup(req),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['previewGroups'] }) },
  })
}

export function useDeletePreviewGroup() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { namespace: string; name: string }) =>
      previewGroupClient.deletePreviewGroup(req),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['previewGroups'] }) },
  })
}

export function useListHookJobs(namespace: string, environmentName: string) {
  return useQuery({
    queryKey: queryKeys.hookJobs(namespace, environmentName),
    queryFn: () => environmentClient.listHookJobs({ namespace, environmentName }),
    enabled: !!namespace && !!environmentName,
    refetchInterval: 5000,
  })
}

export function useRetryHook() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { namespace: string; environmentName: string; hookType: string }) =>
      environmentClient.retryHook(req),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.hookJobs(vars.namespace, vars.environmentName) })
    },
  })
}

/**
 * Fetches AgentTasks for a namespace (or all namespaces) and automatically follows pagination tokens when pageToken is omitted.
 */
export function useListAgentTasks(params: { namespace?: string; pageSize?: number; pageToken?: string } = {}) {
  const pageSize = params.pageSize ?? 100
  const pageToken = params.pageToken ?? ''
  return useQuery({
    queryKey: queryKeys.agentTasks(params.namespace, pageSize, pageToken),
    queryFn: async () => {
      if (pageToken) {
        return agentTaskClient.listTasks({
          namespace: params.namespace ?? '',
          pagination: { pageSize, pageToken },
        })
      }

      const allTasks: Awaited<ReturnType<typeof agentTaskClient.listTasks>>['tasks'] = []
      let currentToken = ''
      let lastResp: Awaited<ReturnType<typeof agentTaskClient.listTasks>> | undefined
      const seenTokens = new Set<string>()

      for (let page = 0; page < 50; page++) {
        const resp = await agentTaskClient.listTasks({
          namespace: params.namespace ?? '',
          pagination: { pageSize, pageToken: currentToken },
        })
        lastResp = resp
        allTasks.push(...(resp.tasks ?? []))
        const nextToken = resp.pagination?.nextPageToken ?? ''
        if (!nextToken || seenTokens.has(nextToken)) {
          break
        }
        seenTokens.add(nextToken)
        currentToken = nextToken
      }

      if (lastResp) {
        const combined = typeof lastResp.clone === 'function' ? lastResp.clone() : { ...lastResp }
        combined.tasks = allTasks
        return combined
      }
      return agentTaskClient.listTasks({
        namespace: params.namespace ?? '',
        pagination: { pageSize, pageToken: '' },
      })
    },
    refetchInterval: 5000,
  })
}

/**
 * Fetches a single AgentTask by namespace and name with periodic polling.
 */
export function useGetAgentTask(namespace: string, name: string) {
  return useQuery({
    queryKey: queryKeys.agentTask(namespace, name),
    queryFn: () => agentTaskClient.getTask({ namespace, name }),
    enabled: !!name && !!namespace,
    refetchInterval: 5000,
  })
}

/**
 * Mutation hook for creating a new AgentTask.
 */
export function useCreateAgentTask() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { name: string; namespace: string; spec: PartialMessage<AgentTaskSpec> }) =>
      agentTaskClient.createTask(req),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['agentTasks'] }) },
  })
}

/**
 * Mutation hook for deleting an AgentTask.
 */
export function useDeleteAgentTask() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { namespace: string; name: string }) =>
      agentTaskClient.deleteTask(req),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['agentTasks'] }) },
  })
}

/**
 * Mutation hook for pausing an active AgentTask.
 */
export function usePauseAgentTask() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { namespace: string; name: string }) =>
      agentTaskClient.pauseTask(req),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.agentTask(vars.namespace, vars.name) })
      qc.invalidateQueries({ queryKey: ['agentTasks'] })
    },
  })
}

/**
 * Mutation hook for resuming a paused AgentTask with optional budget bumps.
 */
export function useResumeAgentTask() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { namespace: string; name: string; budgetUsdBump?: string; budgetTokensBump?: bigint }) =>
      agentTaskClient.resumeTask(req),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.agentTask(vars.namespace, vars.name) })
      qc.invalidateQueries({ queryKey: ['agentTasks'] })
    },
  })
}

/**
 * Mutation hook for sending live steering guidance to an active AgentTask.
 */
export function useGuideAgentTask() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: { namespace: string; name: string; message: string }) =>
      agentTaskClient.guideTask(req),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.agentTask(vars.namespace, vars.name) })
    },
  })
}

export interface VerifyCheckItem {
  name: string
  passed: boolean
  status: string
  durationMs?: number
  message: string
}

export interface VerifyEnvironmentRequest {
  name: string
  namespace?: string
  baseUrl?: string
  previewUrl?: string
  paths?: string[]
  maxLatencyMs?: number
  compareBaseline?: boolean
  skipDoctor?: boolean
  autoFix?: boolean
}

export interface ReplayContractDiff {
  path: string
  kind: string
  baseline?: string
  preview?: string
  breaking: boolean
}

export interface ReplayEntryResult {
  index: number
  id?: string
  method: string
  path: string
  baselineStatus: number
  previewStatus: number
  baselineLatencyMs: number
  previewLatencyMs: number
  protocol: string
  statusMatch: boolean
  contractDiffs?: ReplayContractDiff[]
  breakingDiffCount: number
  extractedVars?: Record<string, string>
  passed: boolean
  failureReasons?: string[]
}

export interface ReplayEnvironmentResponse {
  environment?: string
  namespace?: string
  targetUrl?: string
  previewUrl?: string
  routingHeader?: string
  totalRequests: number
  passedRequests: number
  failedRequests: number
  statusMismatches: number
  breakingDiffs: number
  baselineP50Ms: number
  baselineP95Ms: number
  previewP50Ms: number
  previewP95Ms: number
  passed: boolean
  results: ReplayEntryResult[]
  markdown?: string
  autoFixTaskName?: string
  autoFixError?: string
}

export interface VerifyEnvironmentResponse {
  environment: string
  namespace: string
  targetUrl?: string
  previewUrl?: string
  routingHeader?: string
  passed: boolean
  checks: VerifyCheckItem[]
  replayReport?: ReplayEnvironmentResponse
  autoFixTaskName?: string
  autoFixError?: string
}

export interface ReplayEnvironmentRequest {
  name: string
  namespace?: string
  baseUrl?: string
  previewUrl?: string
  traceContent?: string
  traceFormat?: string
  sequential?: boolean
  compareValues?: boolean
  failOnExtraFields?: boolean
  ignoreFields?: string[]
  filterHosts?: string[]
  stripAuth?: boolean
  stripHeaders?: string[]
  redactQueryParams?: string[]
  redactBodyFields?: string[]
  maxLatencyMs?: number
  autoFix?: boolean
}

/**
 * Helper to execute JSON POST requests against diverge-server REST endpoints with Bearer auth.
 */
async function postJSONEndpoint<TReq, TRes>(url: string, payload: TReq): Promise<TRes> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  }
  const token = getToken()
  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }
  const fullUrl = url.startsWith('http') ? url : `${window.location.origin}${url}`
  const res = await fetch(fullUrl, {
    method: 'POST',
    headers,
    credentials: 'same-origin',
    body: JSON.stringify(payload),
  })
  if (!res.ok) {
    const errText = await res.text()
    try {
      const parsed = JSON.parse(errText) as { error?: unknown }
      if (typeof parsed.error === 'string' && parsed.error.trim()) {
        throw new Error(parsed.error.trim())
      }
    } catch (parseErr) {
      if (parseErr instanceof Error && !(parseErr instanceof SyntaxError)) {
        throw parseErr
      }
    }
    throw new Error(errText.trim() || `Request failed with status ${res.status}`)
  }
  return (await res.json()) as TRes
}

/**
 * Mutation hook for running end-to-end health, readiness, and HTTP probe verification on a preview environment.
 */
export function useVerifyEnvironment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: VerifyEnvironmentRequest) =>
      postJSONEndpoint<VerifyEnvironmentRequest, VerifyEnvironmentResponse>('/api/v1/environments/verify', req),
    onSuccess: (data) => {
      if (data.autoFixTaskName) {
        qc.invalidateQueries({ queryKey: ['agentTasks'] })
      }
    },
  })
}

/**
 * Mutation hook for running deterministic traffic replay and contract diffing on a preview environment.
 */
export function useReplayEnvironment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: ReplayEnvironmentRequest) =>
      postJSONEndpoint<ReplayEnvironmentRequest, ReplayEnvironmentResponse>('/api/v1/environments/replay', req),
    onSuccess: (data) => {
      if (data.autoFixTaskName) {
        qc.invalidateQueries({ queryKey: ['agentTasks'] })
      }
    },
  })
}

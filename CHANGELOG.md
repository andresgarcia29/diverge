# Changelog

## [v0.20.0] - 2026-11-05

### 🚀 Highlights & New Features

#### 🔁 Deterministic Traffic Replay & Protobuf/ConnectRPC/JSON Contract Diffing (`diverge replay` & `diverge_replay`)
- **Multi-Protocol Replay Engine (`pkg/replay/`, `internal/cli/replay.go`, `internal/cli/mcp_replay.go`) (#327)**: Added `diverge replay <env-name> --from <trace.jsonl|trace.har|trace.json>` and the `diverge_replay` MCP tool to replay recorded HTTP, ConnectRPC (`application/connect+proto`, `application/connect+json`), gRPC (`application/grpc`, `application/grpc+proto`), and gRPC-Web (`application/grpc-web+proto`, `application/grpc-web+json`) request traces against baseline and preview environments.
- **Schema-Less Wire & Descriptor-Backed Protobuf Diffing (`pkg/replay/diff.go`) (#327)**: Compares binary Protobuf responses directly at the wire level (`protowire` varint, fixed32/64, length-delimited nested messages, and repeated message element boundaries) without requiring `.proto` files, or resolves human-readable field paths (`dynamicpb`) and map keys when `--proto-descriptor <file.binpb>` and `--proto-message` are provided. Unwraps 5-byte gRPC / Connect streaming / gRPC-Web envelopes and compares HTTP/2 `Grpc-Status` trailers and Connect end-of-stream error frames.
- **JSON Structural Contract Diffing & SSRF-Hardened Transport (`pkg/replay/diff.go`, `pkg/replay/runner.go`) (#327)**: Detects missing fields, type mismatches, array element schema regressions, and trailing JSON stream tokens, while enforcing dial-time SSRF validation (`secureHTTPClient`), redirect target validation, and baseline routing header / W3C `baggage` (`diverge.env`) sanitization.

#### 🤖 Automated Draft PR / MR Creation & `status.prURL` Reporting for `AgentTask` Sandboxes
- **Automated GitHub PR & GitLab MR Creation (`internal/agentrunner/`, `internal/sandbox/`) (#326)**: `diverge-agent-runner` now automatically opens a Draft (or Ready-for-Review) GitHub Pull Request or GitLab Merge Request after committing and pushing an `AgentTask`'s working branch when an SCM token (`divergedev.com/scm-secret` or `DIVERGE_SCM_TOKEN` / `GH_TOKEN` / `GITHUB_TOKEN` / `GITLAB_TOKEN`) is mounted.
- **`AgentTaskStatus.PRURL` Reporting & Dashboard Deep-Links (`api/v1alpha1/agenttask_types.go`, `internal/server/llm_meter.go`, `web/src/pages/AgentTaskDetail.tsx`) (#326)**: Reports `prUrl` via `POST /api/v1/agenttask/meter`, persists `status.prUrl` on the `AgentTask` CR, and surfaces an **Open PR** button and status badge in the Web Dashboard and `diverge task status`.

---

## [v0.19.0] - 2026-11-04

### 🚀 Highlights & New Features

#### 🤖 `AgentTask` Production Readiness, `.diverge/agent.yaml` & `diverge task` CLI
- **Spec-Driven Agent Task CLI (`internal/cli/task*.go`) (#322)**: Added `diverge task init` (scaffolding `.diverge/agent.yaml`), `diverge task create`, `status`, `list`, `logs`, `guide`, `pause`, `resume` (with additive `--budget +15.00` and `--tokens` bumps), and `delete`.
- **Idempotent LLM Metering & Live Steering (`internal/server/llm_meter.go`, `api/v1alpha1/agenttask_types.go`) (#322)**: Added `requestId` deduplication via a bounded 32-entry ring buffer (`status.lastMeterRequestIds`) on `AgentTaskStatus`, `iteration` / `maxIterations` enforcement, wildcard model capability matching (`MatchesAllowedModel`), and live `guidance` + `previewUrl` delivery in `POST /api/v1/agenttask/meter` responses (plus Python `diverge-sdk` and Node.js `@divergedev/sdk` updates).
- **Sandbox Suspension & Linked Preview Hibernation (`internal/controller/agenttask_controller.go`, `internal/sandbox/`) (#322)**: Added `AgentTaskPhasePausing`, `SandboxSuspender` (`spec.operatingMode: Suspended` on `SandboxClaim` or pod scale-down), automatic hibernation of linked preview `Environment`s (`divergedev.com/hibernated: "true"`) on pause, and automatic wake-up + volume-projected Macaroon token rotation on resume.
- **In-Cluster HTTP Port `8080`, `google/ax` Integration & `agent-runner` Release Publishing (`charts/diverge/`, `Dockerfile.agent-runner`, `.github/workflows/release.yml`, `internal/agentrunner/`) (#322)**: Exposed in-cluster HTTP port `8080` (`http-compat`) on `diverge-server`, wired `DIVERGE_MCP_URL` and `DIVERGE_METER_URL` into sandbox pods, packaged `diverge-agent-runner` (`ghcr.io/divergedev/agent-runner`) with `google/ax` workspace and MCP wiring, and added multi-arch (`linux/amd64`, `linux/arm64`) publishing, Cosign signing, and per-platform SPDX SBOM attachment for `ghcr.io/divergedev/agent-runner` in the release pipeline.
- **`PreviewGroup` Diagnostics, Multi-Service `diverge verify`, Docs Schema Validation & Security Hardening (`pkg/doctor/`, `internal/cli/verify.go`, `internal/cli/validate_test.go`, `docs/`, `Dockerfile`) (#323, #324)**: Extended `diverge doctor` and `diverge verify` to inspect child `Environment`s, pods, and all exposed service URLs belonging to a `PreviewGroup`, added automated JSON Schema validation across all `.diverge.yaml` blocks in `README.md` and `docs/`, and upgraded builder images (`golang:1.26.10`) and `github.com/go-jose/go-jose/v4` (`v4.1.6`) to resolve `CVE-2026-111943` and `CVE-2026-116587`.

---

## [v0.18.0] - 2026-10-17

### 🚀 Highlights & New Features

#### 🗄️ PostgreSQL Schema Foreign-Key, Sequence & View Cloning
- **Topological FK & View Cloning (`internal/database/schemaprovider/schema.go`) (#320)**: Upgraded `schema` database seeding (`seedSource: public` or `schema:<name>`) to clone foreign-key constraints (`pg_constraint`), synchronize `SERIAL` / `IDENTITY` sequence values (`setval`), and recreate non-materialized views (`pg_views`) with schema-qualified references rewrite in topological order.

#### 🪣 S3 / GCS / MinIO Object Storage Key-Prefix Isolation
- **Object Storage Async Provisioner (`internal/async/storage.go`) (#320)**: Added `s3`, `gcs`, and `minio` protocols to `AsyncRouteSpec`, provisioning deterministic per-preview object storage prefixes (`DIVERGE_STORAGE_PREFIX=preview/<env>/`) and optional S3-compatible prefix cleanup on teardown.
- **Cross-Language SDK Helpers (#320)**: Added `ResolveStorageKey` (Go `pkg/sdk/http`), `resolveStorageKey` (Node.js `@divergedev/sdk`), and `resolve_storage_key` (Python `diverge-sdk`).

#### 🌐 Gateway API `parentRef` Auto-Discovery & Wildcard TLS / ExternalDNS Polish
- **Gateway Auto-Discovery & Annotations (`internal/routing/gateway.go`, `charts/diverge/`) (#320)**: Auto-discovers `Gateway` resources when `parentRef` is omitted (`divergedev.com/default-gateway` or single namespace `Gateway`), propagates `external-dns.alpha.kubernetes.io/hostname` and `divergedev.com/tls-secret` annotations onto `HTTPRoute`s and `GRPCRoute`s, and adds `gateway.tls` + `gateway.externalDns` Helm values.

#### ✅ `diverge verify` Verification Suite, GitHub Step Summary & Stack Auto-Detection
- **`diverge verify` CLI & GitHub Actions Integration (`internal/cli/verify.go`, `action.yml`, `scripts/ci-preview.sh`) (#320)**: Unified preview verification CLI combining `doctor` workload checks, routed HTTP readiness probes, `--max-latency` SLO assertions, `--body-contains` checks, `--compare-baseline` parity checks, `--github-summary` (`$GITHUB_STEP_SUMMARY` Markdown scorecards), and `::error` workflow annotations.
- **Auto-Doctor on CI Timeout & Stack Auto-Detection (`scripts/ci-preview.sh`, `internal/cli/init.go`)**: Automatically runs `diverge doctor` when `kubectl wait` times out in CI, and auto-detects PostgreSQL, Redis/Valkey, S3/GCS/MinIO, Kafka, and Temporal dependencies during `diverge init --config`.

---

## [v0.17.0] - 2026-10-17

### 🚀 Highlights & New Features

#### 💬 Bidirectional SCM Bot Interaction & Runtime Steering
- **Interactive Agent Steering & PR/MR Feedback (#319)**: Added bidirectional GitHub PR and GitLab MR comment steering, runtime guidance injection for running `AgentTask` sandboxes, and interactive status updates.

#### 🛡️ Baseline Drift Detection, Redis/Valkey Isolation & FinOps Namespace Quotas
- **Baseline Drift Detection (`internal/controller/drift.go`) (#317)**: Computes deterministic SHA-256 fingerprints across baseline `Deployment`s, `ConfigMap`s, and `Secret`s (`BaselineDriftDetected` condition and `divergedev.com/drift-policy: warn | sync`).
- **Redis / Valkey Key-Prefix Isolation (`internal/async/redis.go`) (#317)**: Added `redis` and `valkey` `AsyncRouteSpec` provisioners (`DIVERGE_REDIS_PREFIX=preview:<env>:`) with non-blocking RESP `SCAN` + `UNLINK` teardown and SDK `ResolveRedisKey` helpers.
- **Namespace FinOps Quotas (`internal/controller/quotas.go`) (#317)**: Enforces `divergedev.com/max-active-environments`, `divergedev.com/max-cnpg-clusters`, and `divergedev.com/max-hourly-cost-usd` with `hibernate-oldest` and `reject` policies.
- **100% Pure Apache-2.0 Open-Source Unification (#317)**: Removed all commercial license gates across CLI, controller, and server features.

---

## [v0.16.0] - 2026-10-16

### 🚀 Highlights & New Features
- **Cross-Namespace Gateway `ReferenceGrant`s, Schema Seeding, `diverge mcp setup`, & `diverge top` (#314, #315, #316)**: Added automatic Gateway API `ReferenceGrant` reconciliation, CloudNativePG (`cnpg`) database branching, `diverge top` terminal dashboard, and one-command `diverge mcp setup`.

---

## [v0.15.0] - 2026-10-12

### 🚀 Highlights & New Features

#### 🚩 Unleash Enterprise Feature Flag Provider
- **Unleash Admin API v4.2+ Provider (`pkg/features/unleash.go`, `pkg/features/unleash_client.go`) (#309)**: Native `unleash` feature flag provider (`spec.features.provider: unleash`) supporting environment-scoped `flexibleRollout` strategies (`sortOrder: 0`, `title: "diverge-<namespace>-<env>"`, `diverge.environment IN [<env>]`), `"false"` override `NOT_IN` exclusion constraints on existing strategies, strategy-level variant overrides (`json`, `csv`, and `string` payloads), and automatic environment toggle activation (`POST .../environments/{env}/on`).
- **4-Tier Credential Resolution & SSRF Hardening (#309)**: Resolves credentials from `spec.features.connectionRef` Secret, default `unleash-credentials` Secret, `DIVERGE_UNLEASH_*` environment variables, or in-cluster default (`https://unleash.default.svc.cluster.local:4242`), with Secret-supplied URL token isolation (`resolveCredentialsForClient`) and dial-time SSRF IP pinning (`newUnleashHTTPClient` blocking RFC 1918, loopback, link-local, CGNAT `100.64.0.0/10`, and Alibaba Cloud metadata `100.100.100.200/32`).
- **Lifecycle Reconciliation & Dashboard Integration (#309)**: Cleans up stale strategies when keys are removed from `spec.features.overrides`, scans all project features during `Teardown` when overrides were cleared prior to deletion, and links directly to the Unleash Admin UI in the Web Dashboard `FlagsTab`.

#### 📦 Native Kubernetes Pod (`k8s-pod`) Sandbox Provider & Automatic Fallback
- **Zero-CRD `PodSandboxProvider` (`internal/sandbox/pod.go`, `pod_register.go`) (#308)**: Added native `pod` (`k8s-pod`) sandbox provider (`DIVERGE_SANDBOX_PROVIDER=pod`) that provisions hardened Kubernetes Pods (`automountServiceAccountToken: false`, `runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`, `capabilities.drop: ["ALL"]`, `/workspace` `emptyDir`) with full `Exec` and `StreamLogs` support.
- **Automatic Fallback from `AgentSandboxProvider` (#308)**: When `extensions.agents.x-k8s.io` `SandboxClaim` CRDs are not installed on the cluster, `AgentSandboxProvider` automatically falls back to `PodSandboxProvider` (`divergedev.com/sandbox-backend: pod`).

#### 🌐 Node.js (`@divergedev/sdk`) & Python (`diverge-sdk`) SDK Parity
- **Live LLM Usage Metering (`meterLLMUsage` / `meter_llm_usage`) (#308)**: Added client helpers in both Node.js and Python SDKs for reporting LLM token and USD spend to `POST /api/v1/agenttask/meter`, raising typed `DivergeBudgetExceededError` (`429`) and `DivergeModelNotAllowedError` (`403`) exceptions when task guardrails trip.
- **Kafka & Temporal Context Propagation (#308)**: Added `injectKafkaHeaders`, `extractKafkaContext`, `resolvePreviewTopic`, `injectTemporalHeaders`, and `extractTemporalContext` (and Python snake_case equivalents) across `@divergedev/sdk` and `diverge-sdk`.

#### 🖥️ Interactive Web Dashboard & Quality Gates
- **Interactive Feature Flag Override Editor (#308)**: Added inline override creation, editing, and deletion in `FlagsTab.tsx` backed by `EnvironmentService.UpdateEnvironment`.
- **`PreviewGroup` & `AgentTask` Linked Environment Navigation (#308)**: Added tabbed `Overview`/`Services`/`YAML` navigation with child `Environment` deep-links in `PreviewGroupDetail.tsx` and a linked preview `Environment` status card in `AgentTaskDetail.tsx`.
- **Zero `.testignore` `TODO` Exemptions & VS Code Extension CI (#308, #309)**: Eliminated all remaining `TODO` exemptions in `.testignore`, added `vscode-extension` CI verification, and expanded Hegel property-based test suites (`pkg/features/unleash_property_test.go`).

---

## [v0.14.0] - 2026-10-12

### 🚀 Highlights & New Features

#### 🤖 Autonomous Agent Sandboxes (`AgentTask`), Macaroon Auth & Live LLM Metering
- **`AgentTask` CRD & Reconciler (#290, #295, #298, #299)**: Provision isolated Kubernetes sandboxes (`gVisor` / `Kata` / standard runtime classes) with egress `NetworkPolicy` isolation (blocking cloud metadata `169.254.169.254` / `fd00:ec2::254`, Kubernetes API server, and RFC 1918 private CIDRs), `ResourceQuota`, and `LimitRange` enforcement.
- **Macaroon Capability Tokens (`pkg/auth`)**: Mint HMAC-SHA256 chained Macaroon tokens with caveats for `task_id`, `namespace`, `repo`, `branch`, `max_cost_usd`, `max_tokens`, and `allowed_models`, backed by `diverge-agenttask-tokens` O(1) index Secret revocation.
- **Live LLM Cost & Token Metering (`POST /api/v1/agenttask/meter`) (#301)**: Real-time token and USD spend reporting endpoint on `diverge-server` that validates `allowed_models`, accumulates usage with Kubernetes optimistic concurrency (`RetryOnConflict`), and automatically pauses the `AgentTask` (`spec.suspended = true`, `429 Too Many Requests`) and revokes its Macaroon token when budget thresholds are exceeded.
- **Automatic Linked Preview Environments (#301)**: `AgentTask` resources with `divergedev.com/preview-enabled: "true"` automatically provision and own a linked `Environment` (`agent-preview-<task>`), surfacing `divergedev.com/preview-url` and `PreviewEnvironmentReady` status conditions.

#### 🔌 Built-In Model Context Protocol (MCP) Server
- **Streamable HTTP & Stdio MCP Server (`diverge mcp`, `/mcp`) (#294, #296)**: Exposes 16 tools across `EnvironmentService`, `PreviewGroupService`, `AgentTaskService`, and diagnostic workflows (`diverge_fetch_errors`, `diverge_inspect_preview`, `diverge_run_test`) for Claude Code, Cursor, and Windsurf agents.

#### 💬 Interactive GitHub PR & GitLab MR Slash Commands
- **Webhook Slash Commands (#301)**: Comment `/diverge preview`, `/diverge fix <objective>`, `/diverge extend-ttl [duration]`, `/diverge teardown`, `/diverge pause`, or `/diverge resume` directly on GitHub Pull Requests (`issue_comment`) or GitLab Merge Requests (`Note Hook`), gated to `OWNER`, `MEMBER`, and `COLLABORATOR` roles.

#### ⚡ Turnkey CI/CD Integrations: GitHub Action & GitLab CI/CD Component
- **Composite GitHub Action (`divergedev/diverge@v1` / `action.yml`) (#304)**: One-step `validate`, `diff`, `deploy`, and `teardown` automation with Playwright/Cypress-ready outputs (`preview-name`, `changed-services`, `changed-count`, `skipped`, `routing-header`, `routing-header-json`, `preview-url`).
- **GitLab CI/CD Catalog Component (`templates/preview/template.yml`, `ci/gitlab/.gitlab-ci-component.yml`) (#304)**: Automatic `diverge diff` delta detection, `PreviewGroup` deployment, dynamic GitLab `environment.on_stop` teardown, and dotenv-safe `preview.env` reports (`DIVERGE_ROUTING_HEADER_B64`, `DIVERGE_CHANGED_SERVICES_CSV`).

#### 🛠️ Developer Experience, Protobuf Config Schema & Observability
- **Protobuf-Driven `.diverge.yaml` JSON Schema (#302)**: Defined `DivergeConfig` in `api/proto/diverge/v1alpha1/config.proto` and generated Draft-07 JSON Schema (`schemas/diverge.schema.json`, `diverge validate --print-schema`) for IDE autocomplete and validation.
- **Interactive `diverge init`, `diverge trace`, & Shell Completions (#302)**: Added `diverge init` (auto-detecting services from `docker-compose.yml`, `Chart.yaml`, and `Dockerfile`s), `diverge trace` L7 hop-by-hop routing diagnostics, and `bash`/`zsh`/`fish`/`powershell` completions (`diverge completion`).
- **Stable `diverge doctor` Diagnostic Codes (#302)**: Every cluster health check now emits a stable diagnostic code (`DIV-DOC-001` through `DIV-DOC-009`) and remediation command in text and `--output json` modes.
- **Scale-to-Zero Wake-Up Splash Page & Cost Metrics (#301, #302)**: `activator-proxy` now supports `--wake-splash` (`202 Accepted` auto-refreshing HTML splash screen polling `GET /-/wakeup-status`), and the controller exports `diverge_environment_estimated_hourly_cost_usd` alongside bundled Grafana dashboards and Prometheus rules.
- **Full-Stack Reference Monorepo & Visual Assets (#303, #304)**: Added runnable 3-service reference monorepo (`examples/fullstack-demo` with `storefront-web`, `checkout-api`, `fulfillment-worker`, and `e2e-verify.sh`), architecture SVGs (`docs/assets/`), and VHS terminal tapes (`docs/demos/`).

#### 🛡️ Security Hardening & Property-Based Testing (Hegel PBT)
- **Security & Concurrency Hardening (#300, #301)**: Cross-namespace reverse tunnel isolation, URL fetcher DNS-rebinding/SSRF protection (`DialContext` IP pinning), hop-by-hop `Connection` header stripping protection in `diverge-proxy` and `activator-proxy`, durable schema-provider role passwords across restarts, and auto-provisioned `diverge-db-env-<env>` Secrets.
- **Comprehensive Hegel PBT Suite (#301)**: Added property-based testing across `pkg/auth`, `pkg/topology`, `pkg/license`, `pkg/secrets`, `internal/git`, `internal/config`, `internal/controller`, `internal/proxy`, `internal/server`, `internal/webhook`, `internal/metrics`, and `api/v1alpha1`.

---

## [v0.11.0] - 2026-09-11

### ⚠️ Critical Migration Notice: CRD API Group Transition to `divergedev.com`
The CRD API group, labels, annotations, and RBAC bindings have migrated from `diverge.io` to `divergedev.com` (e.g. `divergedev.com/v1alpha1`).
- Upgrading clusters must install the new CRD manifests: `charts/diverge/crds/divergedev.com_*.yaml`.
- Existing `Environment` and `PreviewGroup` custom resources under `diverge.io/v1alpha1` should be updated to `divergedev.com/v1alpha1`.

### ⚠️ Breaking: Unleash Feature Provider Removed
The `unleash` feature flag provider has been removed. Use the **OpenFeature provider** or migrate to `flipt` / `flagsmith`.

**If you have existing Environments with `spec.features.provider: unleash`:**
1. Update each Environment CR to use `configmap`, `flipt`, or `flagsmith`.
2. Manually remove any orphaned strategy constraints from your Unleash instance
   (strategies with `contextName: "divergeEnvironment"` created by Diverge).
3. When CRD validation ratcheting is enabled (default since Kubernetes v1.33),
   unchanged `unleash` values may persist across updates until the provider
   is explicitly changed to a valid value.

### 🚀 Highlights & New Features

#### 🚩 Enterprise Feature Flag Providers & OpenFeature Integration
- **Flagsmith Provider**: Full enterprise integration supporting identity and trait targeting (`pkg/features/flagsmith`).
- **Flipt Provider**: Dual-tier secret resolution and evaluation provider (`pkg/features/flipt`).
- **OpenFeature Hook**: Generic OpenFeature evaluation hook injecting Diverge preview context into flag evaluations (`pkg/sdk/openfeature`).
- **Dashboard Feature Flags Tab**: Dedicated Feature Flags view in the web dashboard for inspecting active provider state and evaluation rules per environment.

#### 🩺 Cluster Doctor & Preview Verification Suite
- **`diverge doctor` Diagnoser**: Automated health check CLI diagnosing cluster connectivity, CRD installation, DNS resolution, and ingress routes.
- **Preview Verification Suite**: Comprehensive automated test harness validating end-to-end routing integrity, header propagation, and container health across preview environments.

#### 💻 Unified VS Code & OpenCode Extension
- Released unified editor extension (`editors/vscode`) providing preview environment lifecycle controls, active session status bars, and direct terminal launching inside IDEs.

#### 👥 Multi-User Conflict Detection & Session Leasing
- Advanced session leasing and route conflict detection for `diverge dev` (Pro), preventing concurrent developers on shared clusters from overwriting routes.

#### 🗄️ Database Migration Isolation with Atlas
- Support for standalone Atlas migration jobs with production-grade local bundling, lifecycle gating, and CLI support.

#### 🌐 Advanced Routing, IPv6, and Gateway API Conformance
- Support for multi-provider routing composition, IPv6 CIDRs, and direct ingress gateway routing.
- Added comprehensive Kubernetes Gateway API conformance test suite.

#### 📦 Cross-Language Context Propagation SDKs
- Published context propagation and header injection SDKs across multiple languages:
  - Go: `pkg/sdk/http`, `pkg/sdk/grpc`, `pkg/sdk/connect`, `pkg/sdk/kafka`
  - Node.js: `@divergedev/sdk` in `pkg/sdk/node`
  - Python: `diverge-sdk` in `pkg/sdk/python`

#### 🔐 Dynamic TokenSource & OpenBao Integration
- Introduced dynamic `TokenSource` with automatic credential reloading, OpenBao support, and in-cluster `KubeTokenSource`.

### 🛠️ Bug Fixes & Hardening

- **HTTP/2 h2c Tunnel Transport (#281)**: Enabled plaintext h2c protocol negotiation on unencrypted server connections, resolving HTTP 505 errors when running behind ingress proxies or development clusters without TLS.
- **Unbounded Tunnel Stream Timeouts (#282)**: Removed `ReadTimeout` limitation on long-lived bidirectional reverse tunnel streams.
- **Tunnel Service Resolution on kube-dns (#283)**: Fixed `Endpoints` sweeping, label selectors, and transition handling for CoreDNS/kube-dns clusters.
- **Browser Authentication Redirects (#284)**: Redirects unauthenticated browser sessions to the login endpoint with strict URL sanitization to prevent open-redirect vulnerabilities, while preserving HTTP 401 for API clients.
- **CRD `local` Provider Support (#286)**: Added `local` to `EnvironmentSource.Provider` enum and made `Project` optional, enabling `diverge dev` to natively create valid `PreviewGroup` resources.
- **Graceful CLI Cleanup (#285)**: Ensures `diverge dev` terminates cleanly and removes local artifacts even if the cluster runs in a degraded or restricted mode.
- **Flaky Load Test Stabilization (#286)**: Stabilized baseline comparisons in CLI load test smoke tests against CI runner thread scheduling jitter.

---

## [v0.4.0] - 2026-08-16

### 🚀 ConnectRPC Server Foundation

Diverge now includes a stateless ConnectRPC API server, giving developers access to preview environments without needing direct Kubernetes credentials.

#### Highlights
- **ConnectRPC Server**: Full CRUD API for Environments and PreviewGroups over HTTP/2 + gRPC
- **OIDC Authentication**: Zitadel-compatible OIDC auth with K8s TokenReview fallback
- **Streaming**: Real-time watch events and multi-pod log streaming with 4-hour max duration
- **CLI Login**: `diverge login --server <url> --token <token>` with secure credential storage
- **Context Switching**: `diverge context` for managing multiple server connections
- **Dual-Mode Client**: CLI works with direct K8s access OR via ConnectRPC server
- **Helm Chart**: `server.enabled: true` deploys the API server alongside the controller

#### Security
- Auth interceptor covers both unary and streaming RPCs
- RBAC via SubjectAccessReview on all operations
- Error sanitization prevents K8s internal state leakage
- Atomic config file saves prevent credential corruption
- Callback server binds to localhost only

#### Infrastructure
- Property-based tests for CRD types using pgregory.net/rapid
- Bounded broadcaster with 64-event ring buffer
- Stream concurrency limits (max 100)
- CI: buf lint/breaking checks, generated file staleness detection
- Proto stability enforcement with field number locking

### Previous Releases
See [GitHub Releases](https://github.com/divergedev/diverge/releases) for v0.1.0–v0.3.0.

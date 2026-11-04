# Diverge AI Agent Context (AGENTS.md)

This document provides context and guidelines for AI coding assistants and agents working on the Diverge codebase.

## Project Overview

Diverge is an open-source (100% Apache-2.0) Kubernetes operator, environment-as-a-service platform, and autonomous AI agent sandbox engine. Core capabilities include delta deployments (`PreviewGroup` & `Environment`), scale-to-zero hibernation (`activator-proxy`, KEDA, and idle hibernation annotations), service topology resolution, async/storage routing (Kafka, Temporal, Redis/Valkey, S3/GCS/MinIO, Webhook), configurable database provisioning (`schema` with FK/view cloning, `cnpg` CloudNativePG, Ariga Atlas), OpenFeature flags (`flipt`, `unleash`, `flagd`), header-based and subdomain L7 routing, autonomous `AgentTask` sandboxes (`agent-sandbox`, `pod` / `k8s-pod`) with attenuated Macaroon capability tokens and idempotent LLM metering (`POST /api/v1/agenttask/meter`), a ConnectRPC API server with OIDC/TokenReview/Macaroon authentication and embedded React Web UI, and a built-in Model Context Protocol (`diverge mcp` & `POST /mcp`) server.

## Architecture

The project consists of six main binaries:
1. **Controller (`cmd/controller`)**: Kubernetes operator built with `controller-runtime`. Watches `Environment`, `PreviewGroup`, and `AgentTask` CRDs, provisions resources (routing, databases, sandboxes, ArgoCD/Direct deployments, async/storage infra, OpenFeature flags), and enforces FinOps namespace quotas and baseline drift detection.
2. **Server (`cmd/server`)**: ConnectRPC API server (`:8443` HTTPS and `:8080` in-cluster HTTP) with OIDC + K8s TokenReview + Macaroon triple auth, real-time SSE/streaming, embedded React Web Dashboard, HTTP `/mcp` server, `POST /api/v1/agenttask/meter` LLM metering endpoint, and reverse developer tunnels.
3. **Proxy (`cmd/proxy`)**: L7 reverse proxy for header-based, cookie (`?_diverge_env=`), and subdomain routing plus `<diverge-toolbar>` preview banner injection.
4. **Activator Proxy (`cmd/activator-proxy`)**: Scale-to-zero reverse proxy that tracks pod readiness via shared informers, wakes scaled-to-zero deployments on incoming requests, and serves an optional `202 Accepted` auto-refreshing browser wake-up splash screen (`--wake-splash`).
5. **Agent Runner (`cmd/agent-runner`)**: Reference autonomous coding agent entrypoint (`ghcr.io/divergedev/agent-runner`) that executes metered build-verify-fix iterations inside `AgentTask` sandboxes.
6. **CLI (`cmd/diverge`)**: Command-line interface built with `cobra` for managing environments and agent tasks (`init`, `create`, `curl`, `verify`, `top`, `list`, `delete`, `dev`, `diff`, `doctor`, `route`, `graph`, `mcp`, `preview`, `task`, `validate`, `status`, `completion`, etc.).

Slim builds are available with build tags: `no_knative`, `no_schema`, `no_temporal`, `no_kafka`.

## Key Packages

- `cmd/`: Entrypoints for the six binaries (`controller`, `server`, `proxy`, `activator-proxy`, `agent-runner`, `diverge`).
- `internal/cli/`: Implementation of the Cobra CLI commands (`task`, `verify`, `top`, `mcp`, `doctor`, `init`, `preview`, `dev`, etc.).
- `internal/controller/`: K8s operator reconciliation loops (`EnvironmentReconciler`, `PreviewGroupReconciler`, `AgentTaskReconciler`, drift detection, quota enforcement).
- `internal/sandbox/`: Sandbox providers (`AgentSandboxProvider` for `agents.x-k8s.io/v1alpha1`, `PodSandboxProvider` for zero-CRD hardened native pods, `NoopSandboxProvider`).
- `internal/agentrunner/`: Autonomous agent runner loop, metering client, and workspace git orchestration.
- `internal/server/`: ConnectRPC server, auth, embedded dashboard, streaming, `llm_meter.go`, and reverse tunnels.
- `internal/server/auth/`: OIDC, GitHub, TokenReview, and Macaroon authentication with `CompositeProvider`.
- `internal/server/dashboard/`: Embedded React dashboard (Vite + TypeScript).
- `internal/notifier/`: GitLab/GitHub MR/PR comment notifications, `SCMTaskNotifier`, and commit status reporting.
- `internal/webhook/`: Webhook handlers for GitLab/GitHub events and `/diverge` slash commands (`preview`, `fix`, `guide`, `budget`, `status`, `help`, `pause`, `resume`, `extend-ttl`, `teardown`).
- `internal/proxy/` & `internal/activator/`: L7 routing proxy and scale-to-zero activator proxy.
- `internal/routing/`: Gateway API (`HTTPRoute` + `GRPCRoute` + `ReferenceGrant`), Istio, Cilium, Linkerd routers.
- `internal/argocd/`: ArgoCD `Application` generation (Helm + Kustomize).
- `internal/database/`: Database provisioning (`schemaprovider` with FK/sequence/view cloning, `cnpg` CloudNativePG, `atlas` migrations).
- `internal/deployer/`: Deployer interface, `DirectDeployer`, `ArgoCDDeployer`, `KnativeDeployer`, KEDA `ScaledObject`/`HTTPScaledObject` generation.
- `internal/config/`: `.diverge.yaml` and `.diverge/agent.yaml` parsing with strict validation (`KnownFields(true)`).
- `internal/changeset/`: Git diff → changed service detection for `diverge diff`.
- `internal/async/`: Async & storage isolation providers (`kafka`, `temporal`, `redis`/`valkey`, `storage` for S3/GCS/MinIO, `webhook`).
- `internal/metrics/`: Prometheus metrics and FinOps hourly cost estimator (`cost.go`).
- `pkg/auth/`: HMAC-SHA256 chained Macaroon capability tokens and Kubernetes Secret token store (`diverge-agenttask-tokens`).
- `pkg/doctor/`: Root-cause diagnostic engine (`DIV-101`–`DIV-110`).
- `pkg/features/`: OpenFeature flag providers (`flipt`, `unleash`, `flagd`, `configmap`).
- `pkg/topology/`: Service graph resolution (static, Gateway API, Prometheus discovery).
- `pkg/sdk/`: Context propagation & LLM metering SDKs (`http`, `grpc`, `connect`, `kafka`, `openfeature`, `temporal`, `node`, `python`).
- `api/v1alpha1/`: CRD types (`Environment`, `PreviewGroup`, `AgentTask`) with OpenAPI & admission webhook validation.
- `proto/diverge/v1alpha1/`: Protobuf definitions.
- `gen/`: Generated protobuf-go and ConnectRPC stubs.
- `web/`: React dashboard (Vite + TypeScript). Proto files are gitignored — run `buf generate --template buf.gen.web.yaml`.
- `charts/diverge/`: Official Helm chart.

## Build System

- **Nix**: We use Nix for managing development environments (`flake.nix`). **All terminal commands must be run inside the Nix dev shell by prefixing with `nix develop -c`.** This includes `git commit` (for pre-commit hooks).
- **Make**: Standard targets include `make install` (install CRDs), `make run` (run operator locally), `make test`, `make build`, `make generate` and `make manifests` (controller-gen), and `make proto` (generate protobuf/ConnectRPC stubs).
- **Proto**: Uses `buf` for protobuf generation and `protojson` bridge (`internal/server/convert.go`) for CRD-to-Proto mapping. Run `make proto` after editing `.proto` files.
- **Go**: Version 1.26.
- **Dashboard**: `cd web && npm ci && npm run build`. Generated proto TS files require `buf generate --template buf.gen.web.yaml` first.

## Test Patterns

- We use `testify/assert` and `testify/require` for assertions.
- We favor **table-driven tests** for comprehensive coverage.
- We use `httptest` for testing HTTP servers/clients.
- We use Property-Based Testing (PBT) with `hegel.dev/go/hegel` (`*_property_test.go`).
- Config YAML tests **must** include `version: "1"` (strict decoding with `KnownFields(true)`).
- Config YAML tags use **snake_case** for most fields (`tag_template`, `header_key`, `baseline_namespace`) but **camelCase** for newer fields (`dependsOn`, `asyncRoutes`; `tagTemplate` is NOT valid — use `tag_template`).

## Conventions

- **Linting**: Code must pass `golangci-lint run` and `go vet`. `errcheck` requires all `fmt.Fprintf`/`fmt.Fprintln` return values to be checked.
- **Formatting**: `.go` files must use tabs (editorconfig enforced). Run `gofmt -w` before committing.
- **Commits**: Use conventional commit messages (`feat: ...`, `fix: ...`, `docs: ...`). Do **NOT** use `--no-verify` with git commit.
- **Hooks**: We use `lefthook` for pre-commit (go-vet, go-fmt, editorconfig, gitleaks) and pre-push (check-test-files, check-generated-stale, golangci-lint, go-test) hooks.
- **Test files**: Every `.go` source file **must** have a corresponding `_test.go` file (enforced by pre-push hook).
- **Idiomatic Go**: Write standard, idiomatic Go code with proper error handling.
- **Race safety**: CI runs `-race` tests. Use mutex-protected getters, not direct field access.

## Security Conventions

- Sentinel errors (e.g., `ErrEnvironmentNotFound`)
- Context timeouts on all external calls (e.g., 10s on git commands)
- Constant-time secret comparison for webhooks
- HeaderKey validation against RFC 7230
- SHA validation (hex-only regex) for commit status URLs
- Path traversal prevention in notifier API paths
- Label key/value validation using `k8s.io/apimachinery/pkg/util/validation`
- SQL injection prevention via regex-gated schema names (no parameterized DDL)
- Strict YAML unmarshaling (DisallowUnknownFields / `KnownFields(true)`)
- DeepCopy baseline before status mutations
- OIDC: CompositeProvider tries OIDCProvider → GitHubProvider → TokenReviewProvider in order
- Zitadel OIDC: roles returned as `map[string]interface{}` — keys are role names

## CI/CD

- **GitHub Actions**: Workflows in `.github/workflows/` — `ci.yml` (build+test, lint, file-hygiene, helm-lint, check-generated, test-guardrails), `e2e.yml` (dual-cluster k3d tests), `release.yml` (GoReleaser on `v*` tags).
- **GoReleaser**: Builds 6 binaries (`controller`, `server`, `proxy`, `activator-proxy`, `agent-runner`, `diverge` CLI) × 2 OS × 2 arch + slim variants. Dashboard built via `before.hooks` (buf generate + npm build). Docker images pushed to `ghcr.io/divergedev/diverge` and `ghcr.io/divergedev/agent-runner`.
- **Pre-push guards**: `check-test-files` (every .go needs _test.go), `check-generated-stale` (runs make generate + git diff), `golangci-lint`, `go-test`.

## Supported Protocols

- **HTTP/1.1 and HTTP/2** — via Gateway API HTTPRoute
- **gRPC** — via Gateway API GRPCRoute (`protocol: grpc` in service config)
- **WebSocket** — via HTTPRoute with `spec.serviceConfig.webSocket.enabled`, path matching, and configurable timeouts

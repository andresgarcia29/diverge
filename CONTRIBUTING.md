# Contributing to Diverge

Thank you for contributing to **Diverge**! This guide covers setting up your development environment (with either **GitHub Codespaces / VS Code Dev Containers** or **Nix**), running tests, and submitting pull requests.

---

## 1. Development Environment Setup

### Option A: 1-Click Dev Container / GitHub Codespaces (No Nix Required)
Open this repository in **GitHub Codespaces** or VS Code with the **Dev Containers** extension (`.devcontainer/devcontainer.json`). It pre-installs Go 1.26, `kubectl`, `helm`, `kind`, `buf`, Node.js 22, and Python 3.12.

### Option B: Nix Flake (Recommended for Local macOS / Linux)
Diverge provides a reproducible `flake.nix` dev shell that installs all Go, Kubernetes, Protobuf, and linting tools and configures `lefthook` git hooks automatically:

```bash
nix develop
```

To run any command inside the Nix shell without entering it interactively:

```bash
nix develop -c make test
```

---

## 2. Repository Structure

| Path | Description |
| :--- | :--- |
| `api/v1alpha1/` | Kubernetes CRD Go types (`Environment`, `PreviewGroup`, `AgentTask`) |
| `proto/` | Protobuf / ConnectRPC service definitions (`diverge/v1alpha1`) |
| `cmd/controller/` | `diverge-controller` Kubernetes operator entrypoint |
| `cmd/server/` | `diverge-server` ConnectRPC API server + embedded Web UI + HTTP tunnel server |
| `cmd/proxy/` | `diverge-proxy` L7 header-routing reverse proxy |
| `cmd/activator-proxy/` | `activator-proxy` scale-to-zero wake-up proxy |
| `cmd/agent-runner/` | `diverge-agent-runner` autonomous coding sandbox entrypoint |
| `cmd/diverge/` | `diverge` CLI + MCP server (`diverge mcp`) |
| `internal/` | Controller reconcilers, routing/DB/deployer/sandbox providers, server handlers |
| `pkg/auth/` | Macaroon capability tokens & Kubernetes Secret token store |
| `pkg/sdk/` | Context propagation SDKs (`http`, `grpc`, `connect`, `kafka`, `openfeature`, `temporal`, `node`, `python`) |
| `charts/diverge/` | Official Helm chart |
| `web/` | React + TypeScript + Vite Web Dashboard |
| `test/e2e/` | Kind + Envoy Gateway end-to-end test suite |

---

## 3. Common Development Commands

```bash
# Run unit tests
nix develop -c make test

# Run envtest integration tests (controller + server; requires KUBEBUILDER_ASSETS from setup-envtest)
nix develop -c make test-integration

# Regenerate CRDs, DeepCopy, and Protobuf code
nix develop -c make generate manifests proto

# Lint Go code and Helm chart
nix develop -c golangci-lint run
nix develop -c ./scripts/test-helm.sh

# Run full Kind E2E suite locally
nix develop -c make e2e-setup
nix develop -c make e2e-run
nix develop -c make e2e-teardown
```

---

## 4. Testing & Guardrail Conventions

1. **Unit & Property-Based Testing (PBT)**:
   - Every new `.go` file must have a corresponding `_test.go` file (enforced by `./scripts/check-test-coverage.sh`).
   - Core packages use table-driven tests (`stretchr/testify`) and property-based tests via `hegel.dev/go/hegel` (`*_property_test.go`).
2. **Conventional Commits**:
   - Commit messages must follow Conventional Commits (`feat(scope): ...`, `fix(scope): ...`, `docs: ...`), enforced by `lefthook` `commit-msg`.
   - Do **not** bypass pre-commit hooks with `--no-verify`.

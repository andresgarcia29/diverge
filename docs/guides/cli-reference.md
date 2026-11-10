# CLI Reference

The `diverge` CLI manages your local development environments and interacts with the Diverge cluster controller.

## Commands

| Command | Description |
|---------|-------------|
| `diverge completion [bash\|zsh\|fish\|powershell]` | Generate shell completion scripts with live `Environment` and `AgentTask` tab-completion |
| `diverge create` | Create an environment from the current branch |
| `diverge curl <env> [path]` | Send an HTTP request routed to a preview environment (`--compare-baseline` for side-by-side diff) |
| `diverge delete <name>` | Delete an environment |
| `diverge dev` | Route cluster traffic for a service to your local machine |
| `diverge dev sessions` | List active developer sessions across services |
| `diverge dev intercept <service>` | Intercept a service in a preview group |
| `diverge dev release <service>` | Stop intercepting a service |
| `diverge diff` | Detect which services changed relative to a base branch |
| `diverge doctor [env-name]` | Run root-cause diagnostics (`DIV-101`–`DIV-110`) across workloads, migrations, and `AgentTask`s |
| `diverge env export` | Export environment variables for a service |
| `diverge graph show` | Display the service topology graph |
| `diverge graph validate` | Validate topology for cycles, orphans, and unreachable services |
| `diverge init` | Scaffold `.diverge.yaml` (`--config`) or bootstrap a local playground (`--provider auto\|kind\|k3d\|existing`) |
| `diverge list` | List all environments in the cluster |
| `diverge logs [env-name]` | Stream logs from a preview environment |
| `diverge mcp` | Run MCP server over stdio for AI agent integration |
| `diverge mcp setup` | Configure MCP server (`diverge mcp`) for Cursor, Claude Code, and Windsurf (`--client all\|cursor\|claude\|windsurf`) |
| `diverge open <name>` | Open the environment URL in browser (`--magic` for one-click `?_diverge_env=` cookie link) |
| `diverge plugins` | Manage plugins |
| `diverge preview create` | Create a preview group from current branch |
| `diverge preview status <name>` | Show status of a preview group |
| `diverge preview delete <name>` | Delete a preview group |
| `diverge preview watch <name>` | Watch a preview group until Ready/Failed |
| `diverge providers list` | List all registered providers |
| `diverge record [env] --target <url>` | Capture and sanitize live HTTP, ConnectRPC, gRPC-Web, and binary Protobuf-over-HTTP traffic into a replayable `.jsonl` trace |
| `diverge replay <env> --from <trace>` | Replay recorded HTTP, ConnectRPC, and Protobuf/gRPC traces against baseline and preview (`--auto-fix` for self-healing) |
| `diverge replay sanitize --from <trace>` | Sanitize a `.har`, `.json`, or `.jsonl` trace offline by stripping credentials and redacting query/JSON body fields |
| `diverge route <service>` | Simulate request routing to a service through the topology |
| `diverge status` | Show active preview environments and groups |
| `diverge task` | Manage spec-driven autonomous AI developer tasks (`init`, `create`, `status`, `list`, `logs`, `guide`, `pause`, `resume`, `delete`) |
| `diverge top` | Live terminal dashboard (`watch`) of environments, FinOps burn rate, hibernation/drift state, and `AgentTask`s (`-o table\|json\|yaml`) |
| `diverge validate` | Validate `.diverge.yaml` against JSON Schema or print the embedded schema (`--print-schema`) |
| `diverge verify <env> [path]` | Run unified verification suite (`doctor` workload checks, routed HTTP readiness, latency SLOs, baseline parity, `$GITHUB_STEP_SUMMARY`) |
| `diverge version` | Print version info |

## Detailed Command Usage

### `diverge dev`

Route cluster traffic for a service to your local machine.

**Flags:**
- `--service` — Service name (default: auto-detect)
- `--port` — Local port (default: 8080)
- `--endpoint` — Local endpoint IP (default: tailscale ip -4)
- `--env-output` — How to handle env vars: `inject` (in-memory) or `file` (.env.diverge)
- `--devspace` — Generate a devspace.yaml template
- `--proxy-port` — Port for the local proxy (default: 19001)
- `--no-proxy` — Disables the local proxy
- `--proxy-mode` — Proxy mode, either `path` or `host` (default: path)
- `--server` — Diverge server address for the tunnel (default: auto-detect via port-forward)
- `--token` — Bearer token for the Diverge server (default: `$DIVERGE_TOKEN`, then the kubeconfig credential)
- `--on-conflict` — Conflict resolution policy: `warn` (default), `block`, or `allow`
- `--force` — Force start dev session, overriding any active collision

The server authenticates tunnel connections by Kubernetes TokenReview, so the
token must be accepted for the server's `--audiences` (default:
`diverge-server`). The kubeconfig fallback works where that credential is such
a token; a provider-issued kubeconfig credential (GKE, EKS) needs an explicit
`--token`:

```bash
kubectl create token my-sa --audience diverge-server
```

### `diverge dev sessions`

List all active developer sessions and heartbeat leases across services in the cluster.

**Usage:** `diverge dev sessions [flags]` (Alias: `diverge dev list`)

**Flags:**
- `-n, --namespace` — Kubernetes namespace (default: from kubeconfig)

For full details on multi-user development, see the [Multi-User Dev Guide](multi-user-dev.md).

### `diverge logs`

Stream logs from a preview environment.

**Flags:**
- `--service` — Filter to specific service
- `--follow` / `-f` — Follow log output
- `--tail` — Number of lines to show

### `diverge env export`

Export environment variables for a service.

**Supported output formats:**
- `dotenv` (default)
- `json`
- `shell`

### `diverge mcp`

Run a Model Context Protocol (MCP) server over `stdio` for AI agent integration (Claude Desktop, Cursor, VS Code).

**Flags:**
- `--server-url` — Diverge API server URL (defaults to `DIVERGE_SERVER_URL` env var)
- `--allow-destructive` — Enable destructive tools (`delete`)

See the [MCP Server Guide](mcp-server.md) for full setup instructions and editor configurations.

### `diverge diff`

Detect which services changed relative to a base branch.

**Flags:**
- `--config` — Path to `.diverge.yaml`
- `--base` — Base ref to compare against (default: `main`)
- `--output` — Output format: `text` (default), `json`

### `diverge route`

Simulate request routing to a service through the topology.

**Usage:** `diverge route <service> [flags]`

**Flags:**
- `--config` — Path to `.diverge.yaml`
- `--gateway` — Filter to specific gateway
- `--header` — Custom header name (default: `x-diverge-env`)
- `--output` — Output format: `text` (default), `mermaid`, `dot`, `json`
- `--live` — Use Prometheus for live topology

### `diverge graph show`

Display the service topology graph.

**Flags:**
- `--config` — Path to `.diverge.yaml`
- `--service` — Show ingress paths for a specific service
- `--gateway` — Filter to specific gateway
- `--output` — Output format: `text` (default), `mermaid`, `dot`, `json`

### `diverge init`

Initialize a `.diverge.yaml` repository configuration file or bootstrap a complete local Kubernetes development playground (Gateway API CRDs, Envoy Gateway, Diverge CRDs + Operator, optional KEDA, and sample `echo-server`).

**Usage:** `diverge init [flags]`

**Flags:**
- `-c, --config` — Generate a starter `.diverge.yaml` configuration file in the current directory and exit (without creating a cluster)
- `--provider` — Cluster provider to bootstrap: `auto` (default: selects `k3d` when available, then `kind`, and otherwise defaults to `k3d`), `kind`, `k3d`, or `existing` (installs Diverge into your active or `--context` `kubectl` cluster)
- `--cluster-name` — Local Kind or k3d cluster name (default: `diverge-dev`)
- `--install-gateway` — Install Envoy Gateway (default: `true`; setting `--install-gateway=false` skips the Envoy Gateway Helm chart installation while still applying Gateway API CRDs)
- `--install-keda` — Install KEDA and KEDA HTTP Add-on for scale-to-zero (default: `false`)
- `--install-sample-app` — Deploy the `echo-server` baseline sample deployment (default: `true`)
- `--dry-run` — Print the bootstrap commands without executing them

**Examples:**
```bash
# Scaffold a starter .diverge.yaml in your repository
diverge init --config

# Bootstrap a local playground using Kind
diverge init --provider kind --cluster-name diverge-local

# Bootstrap using k3d (or auto-detect kind/k3d)
diverge init --provider auto

# Install Diverge into an existing Kubernetes cluster context
diverge init --provider existing
```

### `diverge validate`

Validate `.diverge.yaml` (local file or remote HTTP/HTTPS URL) against the embedded JSON Schema, or export the JSON Schema for IDE YAML language server integration.

**Usage:** `diverge validate [flags]`

**Flags:**
- `-c, --config` — Path or URL to configuration file (default: `.diverge.yaml`)
- `-o, --output` — Output format: `text` (default) or `json`
- `--print-schema` — Print the embedded `.diverge.yaml` JSON Schema (`diverge-config.schema.json`) to stdout and exit

**Examples:**
```bash
# Validate .diverge.yaml in current directory
diverge validate

# Export JSON Schema for IDE autocompletion (VS Code / JetBrains / Neovim yaml-language-server)
diverge validate --print-schema > .diverge.schema.json
```

### `diverge completion`

Generate shell autocompletion scripts for `bash`, `zsh`, `fish`, and `powershell`. Includes **dynamic cluster-aware tab-completion** that queries the active namespace for live `Environment` names (`diverge status <TAB>`, `diverge logs <TAB>`, `diverge open <TAB>`, `diverge delete <TAB>`, `diverge doctor <TAB>`) and `AgentTask` names (`diverge task <subcommand> <TAB>`).

**Usage:** `diverge completion [bash|zsh|fish|powershell]`

**Examples:**
```bash
# Zsh (load in current session or add to ~/.zshrc)
source <(diverge completion zsh)

# Bash
source <(diverge completion bash)

# Fish
diverge completion fish | source

# PowerShell
diverge completion powershell | Out-String | Invoke-Expression
```

### `diverge doctor`

Run automated root-cause diagnostics across `Environment` CRs, database migration jobs, `AgentTask` sandboxes, and Kubernetes pods. Emits structured diagnostic error codes (`DIV-101` through `DIV-110`) with direct links to the [Troubleshooting Playbook](troubleshooting.md#diagnostic-error-codes-div-101--div-110).

**Usage:** `diverge doctor [environment] [flags]`

**Flags:**
- `--json` — Output machine-readable JSON diagnostic report (including `code` and `doc_url` fields per issue)

### `diverge curl`

Send an HTTP request routed to a preview `Environment` or `PreviewGroup` (automatically injecting `x-diverge-env` / custom routing headers and W3C `baggage: diverge.env=<env>`), with optional side-by-side baseline comparison.

**Usage:** `diverge curl <env-name> [path] [flags]`

**Flags:**
- `-X, --method` — HTTP method (default: `GET`)
- `-H, --header` — Additional HTTP headers (`Key: Value`, repeatable)
- `-d, --data` — HTTP request body payload
- `--base-url` — Override target base URL (defaults to `status.url`, `spec.routing.externalURL`, or `http://localhost:8080`)
- `--compare-baseline` — Send an unrouted baseline request before the routed preview request and display status code, latency, and line-by-line body diffs
- `--timeout` — Request timeout (default: `30s`)

**Examples:**
```bash
# Send a preview-routed GET request to /api/checkout
diverge curl pr-42 /api/checkout

# Compare preview response and latency side-by-side against baseline
diverge curl pr-42 /api/checkout --compare-baseline
```

### `diverge open`

Open a preview `Environment` or `PreviewGroup` URL in your default browser (or print it to stdout). Resolves an `Environment` first and automatically falls back to a `PreviewGroup` of the same name (`spec.routing.externalURL` or the first child service URL that has a URL set).

**Usage:** `diverge open <env-or-previewgroup-name> [flags]`

**Flags:**
- `-m, --magic` — Append `?_diverge_env=<headerValue>` so `diverge-proxy` sets the `diverge_env` sticky routing cookie on first visit
- `-p, --no-browser` — Print the resolved URL to stdout without launching a browser (ideal for CI logs, remote SSH sessions, or piping to `pbcopy`)

### `diverge mcp setup`

Automatically write or merge `diverge mcp` server configuration into AI coding assistant config files (`Cursor`, `Claude Code`, and `Windsurf Cascade`) using atomic file replacement that preserves existing MCP server entries.

**Usage:** `diverge mcp setup [flags]`

**Flags:**
- `--client` — Target AI client: `all` (default: writes `.cursor/mcp.json`, `.mcp.json`, and `~/.codeium/windsurf/mcp_config.json`), `cursor`, `claude`, or `windsurf`
- `--server-url` — Diverge API server URL to embed in the MCP config
- `--allow-destructive` — Include `--allow-destructive` in the configured `diverge mcp` arguments
- `--dir` — Target workspace directory (default: `.`)

**Examples:**
```bash
# Configure Cursor, Claude Code, and Windsurf in one command
diverge mcp setup

# Configure only Cursor with a custom server URL
diverge mcp setup --client cursor --server-url https://diverge.internal.example.com
```

### `diverge top`

Live terminal dashboard (alias: `diverge watch`) displaying fleet FinOps hourly and monthly burn rates, active vs. hibernated (`💤 hibernated`) and baseline-drifted (`⚠️ drift`) environment states, `PreviewGroup` service counts, TTL countdowns, and `AgentTask` LLM token/cost spend.

**Usage:** `diverge top [flags]` (Alias: `diverge watch`)

**Flags:**
- `--interval` — Refresh interval (default: `2s`)
- `--once` — Render a single snapshot to stdout and exit without clearing the screen (ideal for CI logs or scripts)
- `-o, --output` — Output format: `table` (default), `json`, or `yaml` (structured formats imply `--once`)
- `-A, --all-namespaces` — Query `Environment`, `PreviewGroup`, and `AgentTask` resources across all namespaces

**Examples:**
```bash
# Launch interactive live-refreshing dashboard
diverge top

# Print a single fleet FinOps & status snapshot across all namespaces
diverge top --once -A

# Export machine-readable fleet snapshot as JSON
diverge top -o json -A
```

### `diverge verify`

Run a unified verification suite against a preview `Environment` or `PreviewGroup` combining Kubernetes workload diagnostics (`doctor`), routed HTTP readiness probes (with automatic `x-diverge-env` and W3C `baggage` injection), latency SLO assertions, response body substring checks, and optional baseline parity comparisons. When targeting a `PreviewGroup` with multiple exposed service URLs in `status.services` (and `--base-url` / `spec.routing.externalUrl` is not overriding it), `diverge verify` automatically probes every service endpoint and reports per-service checks (`http-status:<service>:<path>`, `latency-slo:<service>:<path>`, `body-contains:<service>:<path>`, `baseline-parity:<service>:<path>`).

**Usage:** `diverge verify <env-name> [path-or-url] [flags]`

A positional `path-or-url` replaces all `--path` values. Use either the positional path or `--path` flags, not both.

**Flags:**
- `--path` — HTTP path(s) to probe (repeatable, default: `["/"]`; ignored if a positional `path-or-url` argument is provided)
- `--expect-status` — Expected HTTP status code (default: `0`, accepting any final `2xx`/`3xx` status `< 400` after following redirects)
- `--max-latency` — Maximum acceptable preview response latency (e.g., `500ms`)
- `--body-contains` — Substring that must appear in the preview HTTP response body
- `--compare-baseline` — Probe baseline (without preview routing headers) and verify HTTP status code parity
- `--fail-on-status-diff` — Fail verification if baseline and preview HTTP status codes differ when `--compare-baseline` is enabled (default: `true`)
- `--skip-doctor` — Skip Kubernetes workload health diagnostics (`doctor`)
- `--github-summary` — Write a Markdown verification scorecard table to `$GITHUB_STEP_SUMMARY` and emit `::error::` workflow annotations on failure
- `--json` — Output machine-readable JSON verification report
- `--base-url` — Override the base URL for HTTP verification
- `--auto-fix` — Automatically create or update a self-healing `AgentTask` (`autofix-verify-<env>`) when verification fails

**Examples:**
```bash
# Verify cluster workload health, /healthz endpoint, baseline status parity, and a 500ms latency SLO
diverge verify pr-42 --path /healthz --compare-baseline --max-latency 500ms

# Verify in GitHub Actions and publish a step summary scorecard
diverge verify pr-42 --path /healthz --compare-baseline --github-summary
```

### `diverge record`

Start a local HTTP/1.1 + HTTP/2 cleartext (`h2c`) reverse-proxy recorder that forwards incoming HTTP, ConnectRPC, gRPC, gRPC-Web, and binary Protobuf-over-HTTP requests (including upstream HTTP/2 trailers) to an upstream target URL (or a Diverge preview environment's gateway URL with automatic routing header injection) and writes a deterministic `.jsonl` trace for `diverge replay`.

> [!WARNING]
> **Redaction limits before saving or committing traces**: By default (`--strip-auth=true`), `diverge record` strips `Authorization`, `Proxy-Authorization`, `Cookie`, and `X-Api-Key` headers, URL userinfo, and default auth query tokens. **JSON request body fields are not redacted by default** — you must explicitly pass `--redact-body-field <field>` (e.g., `--redact-body-field password,cvv`) to scrub sensitive JSON fields, and binary request bodies (`application/proto`, `application/octet-stream`) are captured as base64 and cannot be field-redacted. Verify these limits before saving or committing recorded traces.

**Usage:** `diverge record [env-name] --target <upstream-url> --output <trace.jsonl> [flags]`

**Flags:**
- `-t, --target` — Upstream HTTP target URL to proxy and record (optional when `[env-name]` is provided)
- `-l, --listen` — Local address for the recording reverse proxy (default: `127.0.0.1:8095`)
- `-o, --output` — Output JSONL trace file path, or `-` for stdout (default: `trace.jsonl`)
- `--strip-auth` — Strip `Authorization`, `Cookie`, `X-Api-Key` headers, URL userinfo, and default auth query tokens (default: `true`)
- `--strip-header` — Additional request header name to strip before recording (repeatable)
- `--redact-query` — Additional query parameter name to redact with `[REDACTED]` (repeatable)
- `--redact-body-field` — JSON request body field name or dot-path to redact with `[REDACTED]` (repeatable, e.g., `password,$.user.ssn`)
- `--auto-chain` — Automatically extract `id`/`uuid` fields from `2xx` `POST`/`PUT` JSON responses and parameterize subsequent request paths with `{{stepN_id}}`
- `--max-requests` — Stop recording automatically after capturing `N` requests (`0` = unlimited)
- `-d, --duration` — Stop recording automatically after this duration (e.g., `30s`)

**Examples:**
```bash
# Record 20 requests against staging and scrub auth + sensitive JSON body fields
diverge record --target https://staging.example.com --output ./traces/checkout.jsonl --max-requests 20 --redact-body-field password,cvv

# Record traffic routed to preview environment pr-42 for 30 seconds
diverge record pr-42 --output ./traces/pr-42.jsonl --duration 30s
```

### `diverge replay`

Replay a deterministic request trace (`.jsonl`, `.har`, or `.json`) against both baseline (unrouted) and a candidate preview `Environment` or `PreviewGroup` (with `x-diverge-env` and W3C `baggage` injected). Compares HTTP status code parity, per-request and p50/p95 latency, JSON structural schemas, and binary Protobuf / gRPC / ConnectRPC wire tags (or typed descriptors via `--proto-descriptor`). Automatically strips recorded `diverge_env` sticky-routing cookies from both baseline and preview requests so multi-cookie headers preserve non-routing cookies without contaminating baseline responses.

**Usage:** `diverge replay <env-name> --from <trace.jsonl|trace.har|trace.json> [flags]`

**Subcommands:**
- `diverge replay sanitize --from <trace.har|trace.jsonl> [--output <clean.jsonl>]` — Convert and scrub a trace file offline (`--strip-auth`, `--strip-header`, `--redact-query`, `--redact-body-field`, `--filter-host`) into sanitized JSONL.

**Flags:**
- `-f, --from` — Path to request trace file (`.jsonl`, `.har`, `.json`, or `-` for stdin)
- `--base-url` — Override baseline gateway URL
- `--preview-url` — Optional separate preview base URL (for subdomain-routed environments)
- `--header-key` — Override routing header key (default: `x-diverge-env`)
- `--header-value` — Override routing header value (default: environment name)
- `-c, --concurrency` — Number of concurrent replay workers (default: `4`)
- `--sequential` — Execute trace entries strictly sequentially in trace order (auto-enabled when entries use `extract` or `{{var}}` chaining)
- `--timeout` — Per-request HTTP timeout (default: `30s`)
- `--max-latency` — Maximum acceptable preview response latency per request (e.g., `500ms`)
- `--compare-values` — Compare exact scalar values in addition to JSON/Protobuf field types and wire schema
- `--fail-on-extra-fields` — Treat newly added JSON keys or Protobuf tags in preview as breaking
- `--fail-on-status-diff` — Fail if baseline and preview HTTP status codes differ (default: `true`)
- `--fail-on-contract-diff` — Fail if breaking JSON or Protobuf contract diffs are detected (default: `true`)
- `--ignore-field` — Field name or path to ignore during diffing (repeatable, e.g., `--ignore-field request_id`)
- `--filter-host` — Retain only trace entries matching the given hostname or wildcard pattern (repeatable, e.g., `--filter-host api.example.com --filter-host '*.example.com'`)
- `--strip-header` — Request header name to strip before replaying (repeatable, e.g., `--strip-header X-Internal-Token`)
- `--strip-auth` — Strip `Authorization`, `Proxy-Authorization`, `Cookie`, and `X-Api-Key` headers, URL userinfo, and default auth query tokens before replaying
- `--redact-query` — Additional query parameter name to redact with `[REDACTED]` before replaying (repeatable)
- `--redact-body-field` — JSON request body field name or dot-path to redact with `[REDACTED]` before replaying (repeatable)
- `--proto-descriptor` — Optional path to compiled Protobuf `FileDescriptorSet` (`.binpb`) for schema-aware decoding
- `--proto-message` — Default fully-qualified Protobuf message type for response decoding
- `--auto-fix` — Automatically create or update a self-healing `AgentTask` (`autofix-replay-<env>`) when replay detects regressions
- `--json` — Output replay report as JSON
- `--github-summary` — Write Markdown replay report to `$GITHUB_STEP_SUMMARY` (or stdout, or stderr when `--json` is set)

**Examples:**
```bash
# Replay a JSONL trace against preview environment pr-42
diverge replay pr-42 --from ./traces/checkout.jsonl

# Replay a browser/Playwright HAR file, filtering to api.example.com and stripping captured auth headers
diverge replay pr-42 --from ./recording.har --filter-host api.example.com --strip-auth --github-summary

# Replay binary Protobuf/gRPC traffic with a compiled FileDescriptorSet (.binpb)
diverge replay pr-42 --from ./grpc-trace.jsonl --proto-descriptor ./descriptors.binpb --proto-message acme.v1.ChargeResponse
```

### `diverge task`

Create and manage spec-driven autonomous AI developer tasks (`AgentTask` CRDs) in isolated Kubernetes sandboxes.

**Subcommands:**
- `diverge task init [--force]` — Scaffold `.diverge/agent.yaml` in your repository
- `diverge task create "<objective>"` — Create an `AgentTask` (`--name`, `--repo`, `--base-branch`, `--budget`, `--iterations`, `--capabilities`, `--pool`, `--template`, `--dry-run`, `-o/--output text|yaml|json`; default: `text`)
- `diverge task list` — List `AgentTask` resources in the namespace
- `diverge task status <name>` — Show phase (`Pending`, `Provisioning`, `Active`, `Pausing`, `Paused`, `Completed`, `Failed`), iteration progress, token/USD spend, and linked preview URL
- `diverge task logs <name> [-f]` — Stream live execution logs from the sandbox pod
- `diverge task guide <name> "<guidance>"` — Send real-time steering instructions (`divergedev.com/guidance`) delivered on the next metering heartbeat
- `diverge task pause <name>` — Pause an active `AgentTask` (`spec.suspended: true`), revoke its Macaroon token, suspend its sandbox (`operatingMode: Suspended`), and hibernate its linked preview environment
- `diverge task resume <name> [--budget +10.00] [--tokens 500000]` — Resume a paused `AgentTask` with optional additive budget bumps and mint a fresh Macaroon capability token
- `diverge task delete <name>` — Delete an `AgentTask` and tear down its sandbox and linked preview

**Examples:**
```bash
# Initialize .diverge/agent.yaml
diverge task init

# Launch an autonomous task with a $5.00 budget
diverge task create "Fix N+1 query in /api/v1/checkout" --budget 5.00 --base-branch main

# Steer a running task and resume with an additive budget bump
diverge task guide fix-checkout "Keep v1 response fields backward-compatible"
diverge task resume fix-checkout --budget +10.00 --tokens 500000
```


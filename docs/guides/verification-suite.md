# Preview & Verification Suite Guide

Diverge provides a complete **Full-Lifecycle Preview & Verification Suite** to benchmark microservice performance, prevent visual regressions, diagnose cluster anomalies with AI, and manage environments directly within your editor (VS Code, Cursor, and OpenCode).

---

## 1. Targeted Ephemeral Load Testing (`diverge loadtest`)

Diverge isolates preview environments using header-based routing (`x-diverge-routing-key: <preview-id>`). `diverge loadtest` allows you to benchmark candidate workloads in the cluster under realistic traffic conditions and compare latency against baseline staging/production.

### Quick Start
```bash
# Benchmark a preview environment at 50 RPS for 10 seconds
diverge loadtest https://api.preview.example.com --preview pr-42 --rps 50 --duration 10s

# Compare candidate preview against baseline without routing headers
diverge loadtest https://api.example.com --routing-key pr-42 --baseline
```

### CI/CD Quality Gates
You can enforce strict latency and error SLA thresholds in CI/CD pipelines:
```bash
# Fail CI if candidate p99 latency exceeds 150ms or is >15% slower than baseline
diverge loadtest https://api.example.com \
  --routing-key pr-42 \
  --baseline \
  --max-p99 150ms \
  --fail-on-latency-increase 15.0 \
  --json
```

### Output Sample
```text
📊 DIVERGE LOAD TEST RESULTS
=====================================================
Target URL:    https://api.example.com
Routing Key:   pr-42
Test Duration: 10s
Total Requests:500 (50.0 req/s)

Status Breakdown:
  2xx Success:  500

Latency Percentiles:
  Metric   Baseline   Candidate   Delta
  ------   --------   ---------   -----
  Min      3.20ms     3.40ms      +0.20ms
  Mean     6.80ms     7.10ms      +0.30ms
  p50      5.40ms     5.60ms      +0.20ms
  p90      9.10ms     9.80ms      +0.70ms
  p95      11.20ms    12.00ms     +0.80ms
  p99      14.50ms    15.80ms     +1.30ms (+9.0%)
  Max      22.10ms    24.00ms     +1.90ms

=====================================================
✅ Outcome: PASSED quality thresholds
=====================================================
```

---

## 2. Visual Regression Testing (`diverge test visual`)

Compare screenshots of baseline staging/production against preview candidates pixel-by-pixel to catch inadvertent UI regressions, css styling bugs, or layout shifts.

### Quick Start
```bash
# Compare screenshots and generate interactive visual report
diverge test visual --baseline ./staging.png --candidate ./preview.png -o .diverge/visual

# Enforce quality gate (max 0.1% drift allowed)
diverge test visual --baseline ./staging.png --candidate ./preview.png --max-diff-percent 0.1
```

### Generated Artifacts
When `-o <dir>` is supplied, Diverge outputs:
- `visual-report.html`: Self-contained interactive report with visual diff slider and magenta highlight overlays.
- `diff.png`: High-resolution PNG with changed regions rendered in vibrant magenta.
- `summary.md`: Markdown report table formatted for GitHub PR comments.

---

## 3. Workload & Cluster Diagnostician (`diverge doctor`)

When an environment or dev session encounters an issue, `diverge doctor` inspects pod statuses, init containers, container exit codes, database migration jobs, and HTTPRoutes.

### Quick Start
```bash
# Diagnose all workloads in current namespace
diverge doctor

# Diagnose a specific preview environment
diverge doctor pr-42 -n default

# Machine-readable output for AI agents
diverge doctor pr-42 --json
```

### Diagnosed Failure Modes
- **CrashLoopBackOff**: Extracts container logs and highlights unhandled panics and exceptions.
- **OOMKilled**: Detects Out Of Memory terminations and recommends memory limit adjustments in `diverge.yaml`.
- **ImagePullBackOff**: Verifies image repository, tag existence, and imagePullSecrets.
- **CreateContainerConfigError**: Identifies missing ConfigMaps or Secrets.
- **Database Migration Failure**: Detects failed schema setup jobs.

---

## 4. Deterministic Traffic Replay & Contract Diffing (`diverge replay`)

Replay recorded multi-endpoint HTTP, ConnectRPC, and Protobuf/gRPC request traces (`.jsonl`, `.har`, or `.json`) against both baseline (unrouted) and a candidate preview environment (with `x-diverge-env` and W3C `baggage` injected). `diverge replay` compares status code parity, p50/p95 latency, JSON structural schemas, and binary Protobuf wire tags (or typed `.binpb` descriptors). Because `diverge replay` executes each recorded request against the baseline and attempts the preview request whenever it reads the baseline response without a transport/body-read error (regardless of baseline HTTP status or gRPC status), always target safe environments and filter state-changing requests (`POST`, `PUT`, `PATCH`, `DELETE`, or mutating RPCs) from traces before replaying. In addition, recorded `diverge_env` sticky-routing cookies are not stripped by the replay runner and can route requests from any supported trace format (`.jsonl`, `.har`, or `.json`) to a preview even when `--base-url` is omitted—always remove `diverge_env` routing cookies from all traces and verify that the baseline URL serves the baseline before replaying.

### Quick Start
```bash
# Replay a JSONL request trace against preview environment pr-42
diverge replay pr-42 --from ./traces/checkout.jsonl

# Replay a filtered single-service HAR archive and emit a GitHub Step Summary
# (writes Markdown to $GITHUB_STEP_SUMMARY, or stdout, or stderr when --json is set;
# remove any recorded diverge_env routing cookies, strip or replace captured credentials/sensitive data,
# and filter out third-party hosts before using --base-url)
diverge replay pr-42 --from ./recording.har --base-url https://staging.example.com --github-summary

# Replay binary Protobuf/gRPC traffic with schema-aware descriptor decoding
diverge replay pr-42 --from ./grpc-trace.jsonl --proto-descriptor ./descriptors.binpb --proto-message acme.v1.ChargeResponse
```

---

## 5. VS Code, Cursor & OpenCode Extension

The Diverge IDE extension integrates preview management directly into your editor activity bar.

### Features
- **Status Bar Widget**: `$(rocket) Diverge: Ready` indicator with quick-picker actions.
- **Environments View**: Lists all active preview environments, endpoints, routing keys, and status icons.
- **Dev Sessions View**: Real-time view of active multi-user session leases and locks (PR #273).
- **Verification Suite View**: One-click actions to run `diverge loadtest`, `diverge test visual`, and `diverge doctor`.
- **CodeLens in `diverge.yaml`**: Inline `▶ Run Load Test` and `🩺 Diagnose with Doctor` buttons above service definitions.

### OpenCode Agent Integration
OpenCode AI agents can invoke Diverge tools directly via the native MCP server configured in `opencode.json`:
```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "diverge": {
      "type": "local",
      "command": ["diverge", "mcp"],
      "enabled": true
    }
  }
}
```
Available Agent Tools:
- `diverge_replay`
- `diverge_loadtest`
- `diverge_doctor`
- `diverge_create_environment`
- `diverge_wait_for_ready`
- `diverge_fetch_errors`

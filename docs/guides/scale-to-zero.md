# Scale-to-Zero & Activator Proxy Guide

Idle preview environments typically sit unused for 90%+ of a pull request's lifetime (overnight, between code reviews, or while waiting on CI). Diverge's **Scale-to-Zero** architecture scales idle preview pods to `0` replicas and wakes them transparently on the first incoming HTTP/gRPC request.

---

## 1. How It Works

```mermaid
sequenceDiagram
    participant Client as Browser / CI
    participant Act as diverge-activator-proxy
    participant K8s as Kubernetes API (Shared Informer)
    participant Pod as Preview Pod (payments-pr-42)

    Client->>Act: GET /api/checkout (x-preview-env: pr-42)
    Act->>K8s: Check Pod readiness (in-memory informer)
    alt Pod is scaled to 0
        Act->>K8s: Scale Deployment payments-pr-42 -> 1 replica
        Act->>K8s: Wait for Pod Ready (hold connection)
        K8s-->>Act: Pod Ready (10.244.1.18:8080)
    end
    Act->>Pod: Forward request + X-Preview-Env: pr-42
    Pod-->>Client: 200 OK
```

Diverge supports two scale-to-zero mechanisms:
1. **Built-in `activator-proxy`** (`activatorProxy.enabled: true`) — Uses a Kubernetes shared informer to track pod readiness in-memory, holds the HTTP request until the pod passes readiness probes, and injects `X-Preview-Env`.
2. **KEDA HTTP Add-on (`HTTPScaledObject`) & Event-Driven `ScaledObject`** — Reconciles a KEDA `HTTPScaledObject` per `Environment` when `spec.serviceConfig.keda.enabled: true`, as well as KEDA `ScaledObject` triggers for `temporal` (task queue backlog) and `kafka` (consumer group lag) async workers (`pollingInterval`, `targetQueueSize`, `cooldownPeriod`).

---

## 2. Cost Savings Calculator

| Concurrent Open PRs | Avg Services Changed / PR | Pod Memory / Service | Always-On RAM | With Delta Deploy + Scale-to-Zero (15m idle timeout) | Monthly Cloud Savings (est.) |
| :--- | :--- | :--- | :--- | :--- | :--- |
| 25 PRs | 2 of 15 services | 512 MiB | ~192 GiB (full stack) | **~4 GiB active avg** | **~95% reduction** |
| 50 PRs | 3 of 20 services | 1 GiB | ~1,000 GiB (full stack) | **~18 GiB active avg** | **~97% reduction** |
| 100 PRs | 3 of 25 services | 1 GiB | ~2,500 GiB (full stack) | **~35 GiB active avg** | **~98% reduction** |

---

## 3. Enabling Scale-to-Zero in Helm & Environment CRs

Enable the activator proxy in your Helm values:

```yaml
activatorProxy:
  enabled: true
  replicaCount: 2
```

And configure KEDA scale-to-zero on your `Environment` via `spec.serviceConfig.keda`:

```yaml
apiVersion: divergedev.com/v1alpha1
kind: Environment
metadata:
  name: payments-pr-42
spec:
  serviceName: payments-api
  image: ghcr.io/acme/payments:pr-42
  serviceConfig:
    keda:
      enabled: true
      cooldownPeriod: 300
      minReplicas: 0
      maxReplicas: 5
```

---

## 4. Preview Wake-Up Splash Page (`activator-proxy`)

For browser-facing preview URLs, holding a cold-start connection open for 10–30 seconds can feel unresponsive. `activator-proxy` includes an optional **Preview Wake-Up Splash Page** mode:

- **Enabling Globally**: Pass `--wake-splash` (`--wake-splash=true`) to `activator-proxy`.
- **Enabling Per-Request**: Send the request header `X-Diverge-Wake-Splash: true` on a browser `GET` request (`Accept: text/html`).
- **Behavior**:
  1. When the target pod is scaled to zero, `activator-proxy` triggers an asynchronous wake-up in the background and immediately returns `202 Accepted` (`Retry-After: 2`) with a self-contained HTML splash page (`"Waking up preview environment..."`).
  2. The splash page polls `GET /-/wakeup-status` every `1.5s`, which returns `{"ready": false}` while scaling and `{"ready": true}` (`200 OK`) as soon as a ready pod IP is available in the shared informer cache.
  3. Once `{"ready": true}` is returned, the browser automatically reloads the page and `activator-proxy` forwards the request directly to the ready pod.

---

## 5. Annotation-Based Idle Hibernation

In addition to KEDA `HTTPScaledObject` scaling, `diverge-controller` supports annotation-driven idle hibernation directly on `Environment` resources:

| Annotation | Example | Description |
| :--- | :--- | :--- |
| `divergedev.com/hibernate-after` | `"30m"` | Idle duration (parsed via `time.ParseDuration`) after which the environment's preview Deployment is scaled down to `0` replicas (`divergedev.com/hibernated: "true"`). |
| `divergedev.com/last-traffic-at` | `"2026-10-12T01:30:00Z"` | RFC 3339 timestamp of the most recent request (updated by proxies/gateways or annotations). Falls back to `status.createdAt` when unset. |

---

## 6. Namespace Quotas & FinOps LRU Auto-Hibernation

Platform teams can enforce namespace-level concurrency and FinOps hourly spend limits by annotating the target Kubernetes `Namespace`. When a new preview `Environment` would exceed the namespace's active environment limit or hourly budget cap, `diverge-controller` automatically hibernates the oldest / least-recently-used (`divergedev.com/last-traffic-at`) active environment so CI pipelines never fail due to stale previews:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: staging
  annotations:
    divergedev.com/max-active-environments: "10"
    divergedev.com/max-cnpg-clusters: "3"
    divergedev.com/max-hourly-cost-usd: "1.50"
    divergedev.com/quota-exceed-policy: "hibernate-oldest" # or "reject"
```

| Namespace Annotation | Default | Description |
| :--- | :--- | :--- |
| `divergedev.com/max-active-environments` | *(unlimited)* | Maximum number of non-hibernated active `Environment`s allowed in the namespace simultaneously. |
| `divergedev.com/max-cnpg-clusters` | *(unlimited)* | Maximum number of dedicated CloudNativePG (`cnpg` / `fresh` / `snapshot`) database clusters allowed in the namespace. |
| `divergedev.com/max-hourly-cost-usd` | *(unlimited)* | Maximum combined estimated hourly compute + database spend (`$USD/hr`) across active `Environment`s in the namespace. |
| `divergedev.com/quota-exceed-policy` | `"hibernate-oldest"` | `"hibernate-oldest"` (evicts the least-recently-used active `Environment` to `divergedev.com/hibernated: "true"`, emitting a `QuotaPreempted` event) or `"reject"` (sets `QuotaReady=False` with reason `NamespaceQuotaExceeded`). |

---

## 7. Baseline Drift Detection & Auto-Refresh

In delta preview mode (`spec.deploy.mode: delta`), `diverge-controller` watches baseline `Deployment`s (`divergedev.com/role: baseline`) and referenced `ConfigMap`s / `Secret`s, computing a deterministic SHA-256 fingerprint (`divergedev.com/baseline-fingerprint`):

| Environment Annotation | Values | Behavior |
| :--- | :--- | :--- |
| `divergedev.com/drift-policy` | `"warn"` (default) | Sets condition `BaselineDriftDetected=True` (`Reason: BaselineUpdated`), emits a `BaselineDriftDetected` Warning event, and surfaces `⚠️ drift` in `diverge top` while keeping the preview running. |
| `divergedev.com/drift-policy` | `"sync"` | Automatically re-runs `Deployer.Deploy`, injects the updated `divergedev.com/baseline-fingerprint` into the preview pod template to trigger a clean rolling restart, and sets `BaselineDriftDetected=False` (`Reason: BaselineSynced`). |
| `divergedev.com/drift-policy` | `"ignore"` | Disables baseline drift evaluation for the environment. |

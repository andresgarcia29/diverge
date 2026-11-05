# Diverge ConnectRPC API Server Guide

The Diverge API server ([`cmd/server/main.go`](file:///Users/ab/code/divergedev/diverge/cmd/server/main.go)) provides a high-performance, Kubernetes-native ConnectRPC API for managing Diverge custom resources (`Environment`, `PreviewGroup`, `AgentTask`). Built with [ConnectRPC](https://connectrpc.com/), it delivers seamless interoperability across HTTP/1.1 JSON, HTTP/2, gRPC, and gRPC-Web without requiring an external proxy or transcoder.

---

## 5-Minute Quickstart

Get a local Diverge API server up and running on a [Kind](https://kind.sigs.k8s.io/) cluster in under five minutes.

### 1. Create a Kind Cluster

```bash
kind create cluster --name diverge-dev
```

### 2. Install CRDs and Deploy Diverge with Server Enabled

Apply the CustomResourceDefinitions and install the Diverge Helm chart with the API server activated:

```bash
# Apply Diverge CRDs
kubectl apply -f https://raw.githubusercontent.com/divergedev/diverge/main/config/crd/bases/divergedev.com_environments.yaml
kubectl apply -f https://raw.githubusercontent.com/divergedev/diverge/main/config/crd/bases/divergedev.com_previewgroups.yaml
kubectl apply -f https://raw.githubusercontent.com/divergedev/diverge/main/config/crd/bases/divergedev.com_agenttasks.yaml

# Install Diverge with the API server enabled
helm upgrade --install diverge charts/diverge \
  --namespace diverge-system \
  --create-namespace \
  --set server.enabled=true

# Wait for the server deployment to become ready
kubectl rollout status deployment/diverge-server -n diverge-system --timeout=60s
```

### 3. Create a ServiceAccount and Generate an Auth Token

The API server authenticates requests via Kubernetes `TokenReview` against the `diverge-server` audience:

```bash
# Create a test ServiceAccount with cluster permissions for local testing
kubectl create serviceaccount diverge-admin -n default
kubectl create clusterrolebinding diverge-admin-binding \
  --clusterrole=cluster-admin \
  --serviceaccount=default:diverge-admin

# Mint a 1-hour ServiceAccount token scoped to the diverge-server audience
export TOKEN=$(kubectl create token diverge-admin -n default --duration=1h --audience=diverge-server)
```

### 4. Port-Forward the API Server

Forward local port `8443` to the [`diverge-server`](file:///Users/ab/code/divergedev/diverge/charts/diverge/templates/server-service.yaml) service:

```bash
kubectl port-forward -n diverge-system svc/diverge-server 8443:8443 &
export SERVER_URL="http://localhost:8443"
```

### 5. Make Your First API Call (`ListEnvironments`)

Call the unary [`ListEnvironments`](file:///Users/ab/code/divergedev/diverge/api/proto/diverge/v1alpha1/environment.proto#L204) endpoint via `curl`:

```bash
curl -s -X POST "$SERVER_URL/diverge.v1alpha1.EnvironmentService/ListEnvironments" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"namespace": "default", "page_size": 10}' | jq .
```

### 6. Start a Real-Time Watch Stream (`WatchEnvironments`)

Stream live environment lifecycle events using HTTP/1.1 chunked transfer:

```bash
curl -N -X POST "$SERVER_URL/diverge.v1alpha1.EnvironmentService/WatchEnvironments" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"namespace": "default"}'
```

---

## Architecture Overview

```
                      +------------------------------------------+
                      |         Clients (curl, JS, Go)           |
                      +------------------------------------------+
                                           |  HTTP/1.1, HTTP/2, gRPC
                                           v
+-----------------------------------------------------------------------------------+
| diverge-server                                                                    |
|                                                                                   |
|  +-------------------------+  TokenReview   +----------------------------------+  |
|  | Auth Middleware (LRU)   | -------------> | Kubernetes TokenReview API       |  |
|  +-------------------------+                +----------------------------------+  |
|               | (Authenticated User)                                              |
|  +-------------------------+  SAR Check     +----------------------------------+  |
|  | SubjectAccessReview     | -------------> | Kubernetes SAR API               |  |
|  +-------------------------+                +----------------------------------+  |
|               | (Authorized)                                                      |
|  +-------------------------+  Direct Read   +----------------------------------+  |
|  | Uncached CRD Client     | -------------> | Kubernetes API Server (etcd)     |  |
|  +-------------------------+  Direct Write  +----------------------------------+  |
|               ^                                                                   |
|  +-------------------------+  Informers     +----------------------------------+  |
|  | Informer Broadcaster    | <------------- | Environment / CRD Watch Events   |  |
|  +-------------------------+                +----------------------------------+  |
+-----------------------------------------------------------------------------------+
```

### Stateless CRD Facade Pattern

The Diverge server ([`internal/server/server.go`](file:///Users/ab/code/divergedev/diverge/internal/server/server.go)) maintains no external database. It operates as a stateless facade over Kubernetes Custom Resources:
- **Direct Uncached Client**: Uses an uncached `controller-runtime` [`client.New`](file:///Users/ab/code/divergedev/diverge/cmd/server/main.go#L108) client to guarantee read-your-writes consistency and eliminate cache lag.
- **Single Source of Truth**: All state resides directly in `Environment` and `PreviewGroup` Custom Resources in etcd.
- **Informer Broadcasters**: Multiplexes watch events ([`WatchEnvironments`](file:///Users/ab/code/divergedev/diverge/internal/server/environment.go#L327)) across active streams via lock-free broadcasters ([`streaming.InformerManager`](file:///Users/ab/code/divergedev/diverge/internal/server/streaming/informer.go)).

### Multi-Protocol Support

A single server port (`8443`) supports:
- **HTTP/1.1 + JSON**: REST-like JSON POST endpoints for `curl` and simple HTTP clients.
- **HTTP/2**: High-throughput multiplexed RPCs with bidirectional streaming.
- **gRPC & gRPC-Web**: Standard Protocol Buffers for backend services and browser SPAs without sidecar proxies.

### Security Model

1. **Authentication**: Handled by [`auth.Middleware`](file:///Users/ab/code/divergedev/diverge/internal/server/auth/middleware.go) using Kubernetes `TokenReview` ([`auth.TokenReviewProvider`](file:///Users/ab/code/divergedev/diverge/internal/server/auth/provider.go)) with LRU caching.
2. **Authorization**: Evaluates Namespace-scoped RBAC via Kubernetes `SubjectAccessReview` ([`AuthorizeAction`](file:///Users/ab/code/divergedev/diverge/internal/server/authz.go)).
3. **CORS Support**: Configurable origin whitelist and preflight cache duration for browser apps.
4. **Stream Quotas**: [`StreamLimiter`](file:///Users/ab/code/divergedev/diverge/internal/server/stream_limiter.go) enforces global and per-user active stream limits.
5. **Error Sanitization & Auditing**: Internal cluster errors are sanitized via [`SanitizeK8sError`](file:///Users/ab/code/divergedev/diverge/internal/server/errors.go), and events are recorded by [`AuditLogger`](file:///Users/ab/code/divergedev/diverge/internal/server/audit.go).

---

## Configuration Reference

### Command-Line Flags

The server binary ([`cmd/server/main.go`](file:///Users/ab/code/divergedev/diverge/cmd/server/main.go)) supports the following startup flags:

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `--addr` | `string` | `:8443` | Main ConnectRPC listen address. |
| `--metrics-addr` | `string` | `:9090` | Prometheus `/metrics` and `/healthz` listen address. |
| `--token-cache-ttl` | `duration` | `5s` | LRU cache TTL for `TokenReview` authentications. |
| `--max-streams` | `int` | `1000` | Maximum concurrent active streams globally. |
| `--max-streams-per-user` | `int` | `50` | Maximum concurrent active streams per authenticated user. |
| `--shutdown-timeout` | `duration` | `25s` | Graceful shutdown timeout for draining connections. |
| `--audiences` | `string` | `diverge-server` | Comma-separated list of valid token audiences. |
| `--cors-allowed-origins` | `string` | `*` | Comma-separated list of allowed CORS origins. |
| `--cors-max-age` | `int` | `86400` | CORS preflight cache duration in seconds. |
| `--tls-cert-file` | `string` | `""` | Optional path to TLS certificate file. |
| `--tls-key-file` | `string` | `""` | Optional path to TLS private key file. |
| `--secure-cookies` | `string` | `auto` | Set the `Secure` flag on session cookies: `auto` (when server terminates TLS, or OIDC redirect URL is `https`), `true`, or `false`. |

### Helm Values Mapping

Configure the server in [`charts/diverge/values.yaml`](file:///Users/ab/code/divergedev/diverge/charts/diverge/values.yaml):

```yaml
server:
  enabled: true
  replicaCount: 1
  service:
    type: ClusterIP
    port: 8443
  auth:
    tokenCacheTTL: 5s
    audiences:
      - diverge-server
    maxStreams: 1000
  cors:
    allowedOrigins: "https://dashboard.example.com"
    maxAge: 86400
  rbac:
    clusterWidePodAccess: true # Grants cluster-wide pods/log access for log streaming
    targetNamespaces: []       # Restrict pods/log access when clusterWidePodAccess is false
  metrics:
    serviceMonitor:
      enabled: false
      interval: 30s
```

---

## Client Examples

### 1. curl

#### Unary RPCs

```bash
# List environments with pagination
curl -s -X POST "$SERVER_URL/diverge.v1alpha1.EnvironmentService/ListEnvironments" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"namespace": "default", "page_size": 5}' | jq .

# Get a single environment
curl -s -X POST "$SERVER_URL/diverge.v1alpha1.EnvironmentService/GetEnvironment" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"namespace": "default", "name": "preview-mr-42"}' | jq .
```

#### Streaming RPCs

```bash
# Watch environment lifecycle events
curl -N -X POST "$SERVER_URL/diverge.v1alpha1.EnvironmentService/WatchEnvironments" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"namespace": "default"}'

# Stream live container logs for a preview service
curl -N -X POST "$SERVER_URL/diverge.v1alpha1.EnvironmentService/StreamLogs" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"namespace": "default", "environment_name": "preview-mr-42", "service_name": "api", "follow": true, "tail_lines": 100}'
```

#### AgentTask Live LLM Metering (`POST /api/v1/agenttask/meter`)

Sandbox agents and LLM gateways report incremental token and USD spend (with an idempotent `requestId` and an `iteration` counter) to `POST /api/v1/agenttask/meter` using the task's Macaroon capability token (`requestId` is the sole deduplication key; a distinct `requestId` with the same `iteration` is counted as a new call):

```bash
curl -s -X POST "$SERVER_URL/api/v1/agenttask/meter" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $MACAROON_TOKEN" \
  -d '{"requestId": "iter-1", "iteration": 1, "model": "claude-3-7-sonnet", "tokensUsed": 1500, "costUSD": 0.0225}' | jq .
```

- **`200 OK`**: Returns `{"allowed": true, "suspended": false, "totalTokensUsed": 1500, "totalCostUSD": 0.0225, "iteration": 1, "maxIterations": 5, "guidance": "...", "previewUrl": "https://..."}`. Recent `requestId` values are deduplicated while present in a bounded 32-entry ring buffer (`status.lastMeterRequestIds`) on the `AgentTask` so network retries across server replicas do not double-charge tokens or USD spend (if a retry arrives after 32 newer distinct requests have evicted that `requestId`, it will be counted as a new request).
- **`403 Forbidden`**: Returned when `allowed_models` (`spec.capabilities`) is configured on the token/task and `model` is omitted or not permitted (supporting exact or prefix wildcards like `claude-*`).
- **`429 Too Many Requests`**: Returned when cumulative token/USD usage exceeds the effective budget (`spec.budgetUSD` / `spec.budgetTokens` when positive, or the Macaroon's `max_cost_usd` / `max_tokens` caveats as fallback) with `"reason": "budget exceeded"`, or when `iteration > spec.maxIterations` (and the effective USD/token budgets have not already been exceeded) with `"reason": "max iterations exceeded"`. If a single request crosses both a budget limit and `spec.maxIterations`, `"reason": "budget exceeded"` takes precedence. In either case, `diverge-server` automatically transitions the `AgentTask` to `Paused`, sets `spec.suspended = true`, and revokes the token in `diverge-agenttask-tokens`.

### 2. TypeScript (`@connectrpc/connect-web`)

```typescript
import { createPromiseClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { EnvironmentService } from "./gen/diverge/v1alpha1/environment_connect";

const token = process.env.DIVERGE_TOKEN ?? "";

// Create transport with auth interceptor
const transport = createConnectTransport({
  baseUrl: "https://diverge.example.com",
  interceptors: [
    (next) => async (req) => {
      req.header.set("Authorization", `Bearer ${token}`);
      return await next(req);
    },
  ],
});

const client = createPromiseClient(EnvironmentService, transport);

async function main() {
  // Unary call: List environments
  const response = await client.listEnvironments({ namespace: "default", pageSize: 10 });
  for (const env of response.environments) {
    console.log(`Env: ${env.name} | Status: ${env.status?.phase}`);
  }

  // Server streaming: Watch real-time changes
  for await (const event of client.watchEnvironments({ namespace: "default" })) {
    console.log(`[Event ${event.type}] Env: ${event.environment?.name} (RV: ${event.resourceVersion})`);
  }
}

main().catch(console.error);
```

### 3. Go (`connectrpc.com/connect`)

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"connectrpc.com/connect"
	pb "github.com/divergedev/diverge/api/gen/diverge/v1alpha1"
	"github.com/divergedev/diverge/api/gen/diverge/v1alpha1/divergev1alpha1connect"
)

type bearerAuthInterceptor struct{ token string }

func (i *bearerAuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+i.token)
		return next(ctx, req)
	}
}

func (i *bearerAuthInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+i.token)
		return conn
	}
}

func (i *bearerAuthInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func main() {
	token := "YOUR_BEARER_TOKEN"
	client := divergev1alpha1connect.NewEnvironmentServiceClient(
		http.DefaultClient,
		"http://localhost:8443",
		connect.WithInterceptors(&bearerAuthInterceptor{token: token}),
	)

	ctx := context.Background()

	// 1. Unary RPC: List Environments
	listResp, err := client.ListEnvironments(ctx, connect.NewRequest(&pb.ListEnvironmentsRequest{
		Namespace: "default",
		PageSize:  10,
	}))
	if err != nil {
		log.Fatalf("ListEnvironments failed: %v", err)
	}
	for _, env := range listResp.Msg.Environments {
		fmt.Printf("Env: %s | Phase: %s | RV: %s\n", env.Name, env.Status.Phase, env.ResourceVersion)
	}

	// 2. Server Streaming RPC: Watch Environments
	stream, err := client.WatchEnvironments(ctx, connect.NewRequest(&pb.WatchEnvironmentsRequest{
		Namespace: "default",
	}))
	if err != nil {
		log.Fatalf("WatchEnvironments failed: %v", err)
	}
	for stream.Receive() {
		msg := stream.Msg()
		fmt.Printf("Watch Event [%v]: %s (RV: %s)\n", msg.Type, msg.Environment.Name, msg.ResourceVersion)
	}
	if err := stream.Err(); err != nil {
		log.Fatalf("Watch stream terminated: %v", err)
	}
}
```

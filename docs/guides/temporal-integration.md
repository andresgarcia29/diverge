# Temporal Integration Guide

Diverge provides a ContextPropagator for Temporal to route workflow and activity execution to preview environments.

## How it Works

The propagator intercepts Temporal headers and dynamically routes task execution to the correct environment. Workers started in a preview environment will automatically use environment-scoped task queues.

## 1. Install the SDK

The Diverge Temporal SDK is published as a standalone module to prevent pulling Temporal dependencies into your main application if you aren't using them.

```bash
go get github.com/divergedev/diverge/pkg/sdk/temporal
```

## 2. Quick Setup

The easiest way to integrate Temporal with Diverge is using the configuration helpers:

```go
import (
	"log"

	divergetemporal "github.com/divergedev/diverge/pkg/sdk/temporal"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
)

// Client setup
var clientOpts client.Options
divergetemporal.ConfigureClient(&clientOpts)
c, err := client.Dial(clientOpts)
if err != nil {
    log.Fatalln("Unable to create Temporal client", err)
}
defer c.Close()

// Worker setup
w := worker.New(c, divergetemporal.TaskQueue("orders"), worker.Options{
    Interceptors: []interceptor.WorkerInterceptor{
        divergetemporal.NewConfiguredInterceptor(),
    },
})
```

## 3. Manual Registration (Advanced)

If you need more control, you can register the components manually:

```go
import (
	"log"
	"os"
	
	divergetemporal "github.com/divergedev/diverge/pkg/sdk/temporal"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func main() {
	// The propagator will explicitly use this EnvName to override incoming headers.
	env := os.Getenv("DIVERGE_ENV")

	propagator := &divergetemporal.Propagator{
		EnvName: env,
	}

	c, err := client.Dial(client.Options{
		HostPort:          "localhost:7233",
		ContextPropagators: []workflow.ContextPropagator{propagator},
		// HeadersProvider attaches the correct x-diverge-env header to new workflows
		HeadersProvider: divergetemporal.HeadersProvider{EnvName: env},
	})
	if err != nil {
		log.Fatalln("Unable to create client", err)
	}
	defer c.Close()
	
	// The WorkerInterceptor intercepts activity execution to inject the headers into the Go context
	workerOpts := worker.Options{
		Interceptors: []interceptor.WorkerInterceptor{
			divergetemporal.NewWorkerInterceptor(divergetemporal.WithEnvName(env)),
		},
	}
	w := worker.New(c, divergetemporal.TaskQueue("orders"), workerOpts)
}
```

## Task Queue Isolation

Because Temporal workflows use task queues rather than HTTP routes, preview environment workers must listen on a specific task queue to avoid picking up production tasks (and vice versa). `divergetemporal.TaskQueue()` automatically appends the environment name if running in a preview environment.

### Global task queues (shared/background)

If you have specific workflows that should not be isolated per environment (e.g., a background maintenance job), you can mark the task queue as global:

```go
divergetemporal.TaskQueue("orders")                        // → orders-pr-42 in preview
divergetemporal.TaskQueue("billing", divergetemporal.Global()) // → billing (always)
```

## Local dev with `diverge dev`

When using `diverge dev`, the `DIVERGE_ENV` environment variable is automatically set. Workers started locally will poll the correct preview-scoped task queue.

## Security

> [!WARNING]
> Your API gateway / ingress MUST strip the `x-diverge-env` header from external requests.
> In production, the propagator trusts incoming headers. If an external user injects
> `x-diverge-env: attacker-preview`, production workers could route traffic to the
> attacker's preview environment.

In preview mode (when `DIVERGE_ENV` is set), the Propagator, HeadersProvider, and WorkerInterceptor all **overwrite** the `x-diverge-env` header injected into the workflow context. This guarantees that untrusted user input cannot forge headers and break out of the preview sandbox environment or perform cross-environment spoofing.

## What propagates automatically

| Mechanism | Propagated? | Notes |
|:---|:---:|:---|
| Workflow → Activity | ✅ | Via ContextPropagator |
| Workflow → Child Workflow | ✅ | Via ContextPropagator |
| Continue-as-new | ✅ | Via ContextPropagator |
| Signals | ❌ | Signal payloads don't carry headers |
| Queries | ❌ | Queries don't carry headers |
| Cron schedules | ⚠️ | First execution only, if started with env context |
| External workflow start | ⚠️ | Only if HeadersProvider is configured |

## Multi-language note

For non-Go languages, implement a ContextPropagator in your language's Temporal SDK that reads/writes the `x-diverge-env` header. The header key and serialization format (Temporal protobuf Payload) are standard across all Temporal SDKs.

## Scale-to-Zero

Diverge supports scaling Temporal workers to zero replicas using the native KEDA `temporal` trigger (KEDA v2.17+). When no tasks are pending on the preview task queue, workers scale down to zero. When a workflow dispatches a task, KEDA detects the backlog and scales the worker from 0 → 1.

Configure per-service in the `keda` block:

```yaml
services:
  - name: payments-worker
    asyncRoutes:
      - protocol: temporal
        target: payments-tasks
    keda:
      minReplicas: 0       # Enable scale-to-zero
      maxReplicas: 5
      targetQueueSize: 5   # Tasks per replica
```

**Cold start:** Expect 15–60s latency (KEDA polling + pod scheduling + worker registration). Set `StartToCloseTimeout` ≥ 30s for preview workflows.

See the [Scale-to-Zero & Activator Proxy guide](scale-to-zero.md) for complete details.

# Provider Registry Guide

Diverge uses a powerful self-registration pattern powered by a generic `Registry[T]` to manage its integrations dynamically. This allows the core controller to remain uncoupled from specific implementations of routers, deployers, databases, and notifiers.

## How It Works

The core of the system is the `pkg/registry/Registry[T]` type. It acts as a thread-safe, named-factory store for a single provider kind.

The lifecycle consists of three steps:
1. **Definition**: A global `Registry[T]` instance is defined for each provider kind (e.g., `routing.Providers`).
2. **Registration**: Provider packages call `Providers.Register()` inside an `init()` block (typically located in `*_register.go` files).
3. **Creation**: At runtime, `main.go` uses `Providers.Create(name, deps)` to instantiate the desired provider dynamically without a switch statement.

## Adding a Custom Provider

Adding a custom provider is incredibly straightforward. It requires creating just one file and makes **zero changes to `main.go`**.

For example, let's say you want to add a new `Linkerd` router. You would create a file like `internal/routing/linkerd_register.go`:

```go
// internal/routing/linkerd_register.go
package routing

import "github.com/divergedev/diverge/pkg/registry"

func init() {
    Providers.Register("linkerd", registry.Provider[Router]{
        Create: func(deps registry.Deps) (Router, error) {
            return &LinkerdRouter{Client: deps.Client}, nil
        },
        Description: "Linkerd SMI routing",
    })
}
```

Then, implement the `LinkerdRouter` struct matching the `Router` interface in `internal/routing/linkerd.go`.

Because it registers itself in `init()`, the new `linkerd` router is immediately available to be passed as `--routing-provider=linkerd` when you start the Diverge controller!

## Available Provider Types

- **Routing** (`routing.Router`) — `gateway`, `istio`, `noop`
- **Deployer** (`deployer.Deployer`) — `direct`, `argocd`, `knative`, `noop`
- **Database** (`database.DatabaseProvider`) — `schema`, `cnpg`, `neon`, `cockroach`, `webhook`, `noop`
- **Sandbox** (`sandbox.SandboxProvider`) — `agent-sandbox`, `pod` (`k8s-pod` with automatic fallback), `noop`
- **Feature Flags** (`features.FeatureFlagProvider`) — `configmap`, `flipt`, `flagsmith`, `unleash`, `noop`
- **Async & Storage** (`async.Provisioner`) — `kafka`, `temporal`, `redis`/`valkey`, `s3`/`gcs`/`minio`, `webhook`, `CompositeProvisioner`
- **Notifier** (`notifier.Notifier`) — `github`, `gitlab`, `noop`
- **Testing** (`testing.TestRunner`) — Integration and visual regression test runners

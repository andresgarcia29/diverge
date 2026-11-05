# Configuration Reference

This document provides a comprehensive reference for configuring Diverge, including the `.diverge.yaml` repository file, the `Environment` CRD, Helm values, and environment variables.

## `.diverge.yaml` Reference

The `.diverge.yaml` file is placed in the root of your application repository. It tells the Changeset Detector how to map modified code paths to deployable services.

| Field | Type | Default | Description |
|---|---|---|---|
| `version` | string | `"1"` | The schema version of the configuration file. |
| `services` | map[string]Service | `{}` | A map where the key is the service name and the value contains the configuration for that service. |
| `services.<name>.paths` | list[string] | `[]` | Glob patterns defining which file paths trigger a redeployment of this service. |
| `services.<name>.image.repository` | string | `""` | Container image repository for this service. |
| `services.<name>.dependsOn` | list[string] | `[]` | List of services that this service calls at runtime. Used for topology graph resolution. |
| `services.<name>.entrypoint` | bool | `false` | Marks this service as an ingress gateway / entrypoint for request path resolution. |
| `topology` | TopologyConfig | `{}` | Configuration for dynamic topology discovery and service mesh integration. |
| `topology.prometheus` | PrometheusConfig | `{}` | Prometheus discovery configuration for inferring runtime service dependencies. |

### Topology Configuration

Diverge can dynamically discover service topology from Prometheus metrics emitted by service meshes (such as Istio, Linkerd, or Cilium) or Kubernetes Gateway API resources.

```yaml
topology:
  prometheus:
    address: http://prometheus.monitoring:9090
    # Authentication (pick one)
    tokenEnv: PROMETHEUS_TOKEN        # read bearer token from env var
    tokenFile: /var/run/secrets/token  # or read from file
    # TLS
    caBundle: /etc/ssl/certs/ca.pem
    insecureTLS: false
    # Cache & Polling
    cacheTTL: 5m       # how long cached topology is fresh
    pollInterval: 30s  # background refresh interval
```

| Field | Type | Default | Description |
|---|---|---|---|
| `topology.prometheus.address` | string | `""` | URL of the Prometheus server endpoint. |
| `topology.prometheus.tokenEnv` | string | `""` | Environment variable name containing bearer token for authentication. |
| `topology.prometheus.tokenFile` | string | `""` | Path to a file containing bearer token for authentication. |
| `topology.prometheus.caBundle` | string | `""` | Path to custom CA certificate bundle for TLS verification. |
| `topology.prometheus.insecureTLS` | bool | `false` | Skip TLS certificate verification when connecting to Prometheus. |
| `topology.prometheus.cacheTTL` | string | `5m` | Duration string indicating how long cached topology is considered fresh. |
| `topology.prometheus.pollInterval` | string | `30s` | Duration string for background topology polling. |

### Example

```yaml
version: "1"
services:
  gateway:
    entrypoint: true
    paths:
      - services/gateway/**
    image:
      repository: ghcr.io/acme/gateway
    dependsOn:
      - auth-service
      - payment-service
  auth-service:
    paths:
      - services/auth/**
    image:
      repository: ghcr.io/acme/auth-service
  payment-service:
    paths:
      - services/payment/**
    image:
      repository: ghcr.io/acme/payment-service
    dependsOn:
      - ledger-service
  ledger-service:
    paths:
      - services/ledger/**
    image:
      repository: ghcr.io/acme/ledger-service

topology:
  prometheus:
    address: http://prometheus.monitoring:9090
    tokenEnv: PROMETHEUS_TOKEN
    caBundle: /etc/ssl/certs/ca.pem
    insecureTLS: false
    cacheTTL: 5m
    pollInterval: 30s
```

## Environment CRD Reference

The `Environment` Custom Resource represents a single preview environment in Kubernetes.

### `EnvironmentSpec`

| Field | Description |
|---|---|
| `source.provider` | The VCS provider (e.g., `gitlab`, `github`). |
| `source.project` | The path or ID of the project in the VCS. |
| `source.mr` | The Merge Request / Pull Request ID. |
| `source.branch` | The source branch name. |
| `deploy.mode` | Deployment strategy: `delta` (only changed services) or `full` (all services). |
| `deploy.changedServices` | List of service names detected as changed by the Changeset Detector. |
| `deploy.baselineRef` | The reference environment to fall back to (e.g., `staging`). |
| `routing.mode` | The routing strategy: `header`, `namespace`, or `subdomain`. |
| `routing.headerKey` | The HTTP header key used for routing (e.g., `x-diverge-env`). |
| `routing.headerValue` | The value of the HTTP header indicating this specific environment. |
| `routing.externalUrl` | The generated external URL for the environment (populated dynamically). |
| `database.mode` | Database provisioning strategy: `shared`, `schema`, `snapshot`, `fresh`. |
| `database.connectionRef` | Reference to a Secret containing the database connection string. |
| `database.seedSource` | Path or reference to database seeding scripts. |
| `database.migrationJob` | Configuration for a Kubernetes Job to run migrations against the provisioned DB. |
| `features.provider` | Feature flag provider: `configmap` (default), `flipt`, `flagsmith`, `unleash`, `noop`, `none`. |
| `features.overrides` | Map of key-value flag overrides for this environment. |
| `features.connectionRef` | Reference to a Secret containing remote provider endpoint and auth tokens. |
| `lifecycle.ttl` | Time-to-Live duration string (e.g., `72h`). Environment is deleted after this duration. |
| `lifecycle.cleanupOnMerge` | Boolean. If true, the environment is destroyed when the MR is merged. |
| `serviceConfig` | Configuration for a single preview pod in multi-repo mode. |

### `FeatureSpec`

Diverge includes an OpenFeature-compatible feature flags engine. For details and SDK examples, see the [Feature Flags Guide](guides/feature-flags.md).

| Field | Description |
|---|---|
| `provider` | Provider implementation: `configmap` (default, in-cluster flagd format), `flipt` (remote ephemeral namespaces), `flagsmith` (remote identity-based), `unleash` (context-constrained preview strategies), `noop` / `none` (disabled). |
| `overrides` | Map of flag key to value string. Values `"true"` and `"false"` map to boolean flags; numeric and string values map to variant flags. |
| `connectionRef` | Name of Kubernetes Secret containing provider connection details (e.g., `url`, `adminToken`, `clientToken`). Dual-tier resolution checks `env.Namespace` first, falling back to `diverge-system`. |

### `MigrationJobSpec`

| Field | Description |
|---|---|
| `image` | **Required**. Container image to use for the migration job. |
| `args` | Command arguments to run in the container. |
| `envFrom` | List of Secret references to inject as environment variables. |
| `timeoutSeconds` | Timeout for the migration job. Default is 120. |

### `ServicePreviewConfig`

| Field | Description |
|---|---|
| `serviceName` | **Required**. The Kubernetes Service name to shadow. |
| `namespace` | The namespace where the baseline service runs. |
| `port` | **Required**. The container port the service listens on. |
| `image` | **Required**. The container image for the preview pod. |
| `imagePullPolicy` | Overrides the container image pull policy. Default is `IfNotPresent`. |
| `parentRef` | The Gateway API parentRef name. Default is `diverge-gateway`. |
| `headerKey` | Overrides the routing header key. Default is `x-diverge-env`. |
| `pathPrefix` | Scopes the preview HTTPRoute to a specific path prefix. |
| `databaseEnvKey` | The environment variable name for the database connection URL. Default is `DATABASE_URL`. |
| `env` | Additional environment variables for the preview container (list of EnvVar). |

### `EnvironmentStatus`

| Field | Description |
|---|---|
| `phase` | Current lifecycle phase: `Pending`, `Deploying`, `Running`, `Failed`, `Terminating`. |
| `url` | The primary accessible URL for the environment. |
| `services` | List of services currently actively deployed in this environment. |
| `databaseStatus` | Status of the database provisioning step. |
| `featureConfigMap` | Name of the generated Kubernetes ConfigMap containing flagd definitions (when using `configmap` provider). |
| `featureEnvVars` | Key-value map of environment variables injected into preview workloads for zero-code OpenFeature SDK configuration. |
| `createdAt` | Timestamp when the environment was created. |
| `expiresAt` | Timestamp when the environment will be deleted (if TTL is set). |

## Resource Annotations (`Environment` & `AgentTask`)

### `Environment` Annotations
| Annotation | Example | Description |
|---|---|---|
| `divergedev.com/hibernate-after` | `"30m"` | Idle duration after which `diverge-controller` scales the environment's preview Deployment to `0` replicas (`Phase: Hibernating`). |
| `divergedev.com/last-traffic-at` | `"2026-10-12T01:30:00Z"` | RFC 3339 timestamp of the last observed HTTP/gRPC traffic (falls back to `status.createdAt` when unset). |
| `divergedev.com/pr-number` | `"42"` | Pull Request / Merge Request number associated with the environment. |

### `AgentTask` Preview Annotations
When `spec.repository.url` is set on an `AgentTask`, the controller automatically provisions a linked preview `Environment` (`agent-preview-<taskName>`):

| Annotation | Direction | Default | Description |
|---|---|---|---|
| `divergedev.com/preview-enabled` | Input | `"true"` (when `repository.url` is set) | Set to `"false"` to disable automatic linked preview provisioning, or `"true"` to explicitly enable. |
| `divergedev.com/preview-service` | Input | `<task.name>` | Target Kubernetes Service name for the linked `Environment`. |
| `divergedev.com/preview-image` | Input | `nginx:alpine` (port `80`) | Container image deployed in the linked preview `Environment`. |
| `divergedev.com/preview-branch` | Input | `workingBranch` / `baseBranch` | Git branch tracked by the linked `Environment`. |
| `divergedev.com/preview-environment` | Output | — | Populated by `diverge-controller` with the linked `Environment` name (`agent-preview-<taskName>`). |
| `divergedev.com/preview-url` | Output | — | Populated by `diverge-controller` with the live preview URL once ready. |

## MR Label Overrides

Diverge can read labels applied to a Merge Request and override the default deployment configurations. This allows developers to request specific infrastructure directly from GitLab/GitHub.

Examples of label mappings:
- `diverge:db-snapshot` -> Overrides `database.mode` to `snapshot`.
- `diverge:deploy-full` -> Overrides `deploy.mode` to `full`.
- `diverge:ttl-never` -> Removes the TTL, preventing automatic expiry.

*(Note: Label mapping configuration is typically defined in the controller's Helm values).*

## Helm Chart Values

When deploying Diverge via Helm, you can customize its behavior using `values.yaml`.

Key parameters:

| Key | Default | Description |
|---|---|---|
| `webhook.enabled` | `true` | Enable the VCS webhook listener. |
| `webhook.secretName` | `""` | K8s Secret containing the webhook verification token. |
| `controller.replicaCount` | `1` | Number of controller replicas. |
| `proxy.enabled` | `true` | Enable the Diverge Proxy for advanced routing. |
| `proxy.replicaCount` | `1` | Number of proxy replicas. |
| `routing.defaultMode` | `header` | Default routing mode if not specified. |
| `database.defaultMode` | `shared` | Default database mode if not specified. |

## CLI & Binary Flags Configuration

Diverge binaries can be configured via CLI flags:

### Controller (`diverge-controller`)
| Flag | Default | Description |
|---|---|---|
| `--deploy-provider` | `noop` | Deployment provider (`direct`, `argocd`, `knative`, `noop`). |
| `--routing-provider` | `gateway` | Routing provider (`gateway`, `istio`, `composite`, `noop`, or comma-separated list like `gateway,istio`). |
| `--database-provider` | `none` | Database provider (`schema`, `none`). |
| `--notifier-provider` | `noop` | Notification provider (`github`, `gitlab`, `noop`). |

### Proxy (`diverge-proxy`) & Activator (`activator-proxy`)
| Binary | Flag | Default | Description |
|---|---|---|---|
| `diverge-proxy` | `--hide-environment-list` | `false` | Hide the active environments list on the 404 landing page to prevent environment enumeration. |
| `activator-proxy` | `--wake-splash` | `false` | Serve an auto-refreshing `202 Accepted` HTML wake-up splash page (`GET /-/wakeup-status`) while waking scaled-to-zero revisions (also triggered per request via `X-Diverge-Wake-Splash: true`). |

### Provider-Specific Flags
Because providers self-register, some flags are tied to specific providers (registered in their `*_register.go` files):
- **Direct Deployer**: `--manifest-source-type` (`configmap`, `url`, `serviceconfig`).
- **ArgoCD Deployer**: `--argo-namespace`, `--argo-repo-url`.
- **GitLab Notifier**: `--gitlab-token`, `--gitlab-url`.

## CLI Commands

### Env Export
The `diverge env export` command allows you to export environment variables from a baseline pod. This is useful for running your local preview environments seamlessly.

```bash
diverge env export --service payments --format dotenv > .env.preview
```
Supported formats: `dotenv`, `json`, `shell`.

## Environment Variables

The Diverge controller can be configured via environment variables (usually injected via the Helm chart):

- `DIVERGE_WEBHOOK_SECRET`: The secret token used to validate incoming webhooks from GitLab/GitHub.
- `DIVERGE_LOG_LEVEL`: Logging verbosity (`debug`, `info`, `warn`, `error`).
- `DIVERGE_DEFAULT_BASELINE`: The default baseline environment name (e.g., `staging`).

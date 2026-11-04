# Async Routing

Async routing allows you to route non-HTTP workloads such as Kafka consumers and Temporal workflows to your preview environments.

## How it works
The `diverge dev` command provisions the requested async routing resources (e.g. creating a dedicated Kafka consumer group or a Temporal task queue) and polls `Environment.status.conditions` until the `AsyncRoutingReady` condition becomes true.

## Injected Environment Variables
Diverge automatically provisions and injects specific environment variables into your preview pods to support async routing:

### Temporal
- `TEMPORAL_TASK_QUEUE`: The provisioned task queue for your preview environment.
- `TEMPORAL_NAMESPACE`: The Temporal namespace.

### Kafka
- `KAFKA_TOPIC`: The isolated topic for preview messages.
- `KAFKA_CONSUMER_GROUP`: The preview consumer group.
- `KAFKA_BROKERS`: The Kafka brokers string.

### Redis / Valkey
When no custom `envVarMapping` is set on the route, Diverge injects:
- `DIVERGE_REDIS_PREFIX`: The preview key prefix (`preview:<env>:` for the `default` or empty target, or `preview:<env>:<target>:` for a named target).
- `REDIS_KEY_PREFIX`: Alias for `DIVERGE_REDIS_PREFIX`.
- `REDIS_NAMESPACE`: The prefix without the trailing colon (`preview:<env>` or `preview:<env>:<target>`).
- `REDIS_ADDR`: The resolved Redis / Valkey `host:port` (injected when an address is configured on the controller or environment).

If custom `envVarMapping` entries are specified on the route, `{{ .ResolvedTarget }}` expands to the resolved key prefix (`preview:<env>:` or `preview:<env>:<target>:`).

These variables are merged into your pod environment. There are blocked environment variables that cannot be overridden by async routing: `KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT`, `HOME`, `PATH`, and `KUBECONFIG`.

## SDK Integration

The `pkg/sdk` module provides helpers to extract and propagate the preview environment context.

### Context
Use `sdk.GetEnv(ctx)` to retrieve the current environment from a context.

### Header Customization
By default, the SDK looks for the `x-diverge-env` header. You can customize this by setting the `DIVERGE_HEADER_KEY` environment variable.

### Kafka Headers
To inject the preview environment context into outgoing Kafka messages, use:
```go
func InjectHeaders(headers []Header, envName string) []Header
```

### Temporal Propagator
Temporal contexts can be propagated to workflows using the Temporal propagator:
```go
func NewContextPropagator() *ContextPropagator
```

### Redis / Valkey Key Resolution
To resolve preview-isolated Redis / Valkey keys (`preview:<env>:<key>` in preview environments, or unmodified `<key>` in baseline), use:
```go
key := sdk.ResolveRedisKey(ctx, "cart:user-42")
```
(Equivalent helpers are available in Node.js via `resolveRedisKey` in `@divergedev/sdk` and Python via `resolve_redis_key` in `diverge-sdk`.)

### S3 / GCS / MinIO Object Storage Key Resolution
When `protocol: s3`, `protocol: gcs`, or `protocol: minio` is configured in `asyncRoutes`, Diverge injects `DIVERGE_STORAGE_PREFIX` (`previews/<namespace>/<env>/` or `previews/<namespace>/<env>/<target>/`), `S3_KEY_PREFIX`, `AWS_S3_PREFIX`, and `GCS_KEY_PREFIX`, and purges the preview prefix upon environment teardown. Resolve object keys in application code using:
```go
objectKey := sdk.ResolveStorageKey(ctx, "avatars/user-1.png")
```
(Equivalent helpers are available in Node.js via `resolveStorageKey` in `@divergedev/sdk` and Python via `resolve_storage_key` in `diverge-sdk`.)

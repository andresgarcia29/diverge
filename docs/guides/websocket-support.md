# WebSocket Support

Diverge supports proxying WebSocket connections to your preview environments.

## Overview

WebSocket connections (`ws://` and `wss://`) are long-lived connections that start as HTTP requests and upgrade. To support WebSockets properly, Gateway API routes must be configured to:
1. Preserve the `x-diverge-env` routing header during the initial HTTP upgrade request.
2. Extend or disable timeouts so the connection is not dropped prematurely.

## Configuration

You can enable WebSocket support on your `Environment` Custom Resource via `spec.serviceConfig.websocket`:

```yaml
apiVersion: divergedev.com/v1alpha1
kind: Environment
metadata:
  name: chat-pr-42
spec:
  serviceConfig:
    serviceName: my-websocket-svc
    image: ghcr.io/acme/my-websocket-svc:pr-42
    port: 8080
    websocket:
      enabled: true
      # Optional: Restrict WebSocket configuration to a specific path prefix
      path: /ws
      # Optional: Set a specific timeout (defaults to "0s" which means no timeout)
      timeout: 3600s
```

### Options

- `enabled` (bool): Whether to enable WebSocket timeout rules.
- `path` (string): The path prefix to apply the WebSocket rules to. If omitted, the rules apply to all paths.
- `timeout` (string): The Gateway API request timeout for the route. For WebSockets, `0s` is recommended to disable the timeout.

## How it works

When `websocket.enabled` is set to `true`, the Diverge controller creates a specific rule in the generated Gateway API `HTTPRoute` for your service. This rule includes a `timeouts: { request: 0s }` setting (or your custom timeout) to allow long-lived connections. If `websocket.path` is provided, a separate high-priority rule is generated just for that path.

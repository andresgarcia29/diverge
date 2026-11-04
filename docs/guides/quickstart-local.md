# 2-Minute Local Quickstart (Kind + Helm)

Spin up a complete local Diverge environment — including `diverge-controller`, `diverge-server` (ConnectRPC + Web UI), Envoy Gateway, and an isolated `PreviewGroup` + `AgentTask` — on your laptop in under 2 minutes.

---

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/)
- [Kind](https://kind.sigs.k8s.io/) (`v0.29+`)
- [kubectl](https://kubernetes.io/docs/tasks/tools/) (`v1.30+`)
- [Helm](https://helm.sh/) (`v3.14+` or `v4+`)
- [Diverge CLI](../../README.md#get-started)

---

## Step 1: Boot Local Cluster & Install Diverge

### Option A: One-Command Bootstrap (`diverge init`)

Use `diverge init` to automatically create a local cluster, install Gateway API CRDs + Envoy Gateway, apply the Diverge CRDs, deploy the Diverge operator, and launch a sample baseline app:

```bash
# Bootstrap with Kind (or use --provider k3d / --provider auto)
diverge init --provider kind --cluster-name diverge-local

# Optional: scaffold a starter .diverge.yaml in your repository
diverge init --config
```

### Option B: Manual Step-by-Step (Kind + Helm)

If you prefer to run each Kubernetes and Helm command manually:

```bash
# 1. Create a Kind cluster
kind create cluster --name diverge-local

# 2. Install Envoy Gateway (Gateway API CRDs + GRPCRoute support)
helm install eg oci://docker.io/envoyproxy/gateway-helm \
  --version v1.3.0 -n envoy-gateway-system --create-namespace --wait

# 3. Apply CRDs and install Diverge Operator + API Server + Dashboard
kubectl apply -f charts/diverge/crds/
helm upgrade --install diverge ./charts/diverge \
  --namespace diverge-system --create-namespace \
  --set server.enabled=true \
  --set server.dashboard.enabled=true \
  --set sandbox.enabled=true \
  --set sandbox.provider=noop \
  --wait
```

> [!TIP]
> If you only need CRD + controller reconciliation (without Envoy Gateway data-plane pods), you can run `make e2e-setup` from a clone of this repository.

---

## Step 2: Create Your First Preview Environment

Apply a sample `PreviewGroup` managing two services (`payments-api` on a PR image and `gateway` on baseline):

```bash
kubectl apply -f - <<'EOF'
apiVersion: divergedev.com/v1alpha1
kind: PreviewGroup
metadata:
  name: pr-101
spec:
  lifecycle:
    ttl: 2h
  source:
    provider: github
    project: acme/payments
    branch: feat/instant-payouts
    mr: 101
  routing:
    headerKey: x-preview-env
    headerValue: pr-101
  services:
    - name: payments-api
      namespace: default
      image: hashicorp/http-echo:0.2.3
      mode: image
      port: 5678
    - name: gateway
      namespace: default
      mode: baseline
EOF
```

Check the reconciled status and child `Environment` CRs:

```bash
kubectl get previewgroups
kubectl get environments -l divergedev.com/preview-group=pr-101
```

---

## Step 3: Route Browser Traffic, Open the Web Dashboard & Connect MCP

### One-Click Browser Cookie Switching (`?_diverge_env=<env>`)

You can test header-routed preview environments directly in any browser—**without installing a browser extension**:

1. Open your gateway or `diverge-proxy` URL with the `?_diverge_env=pr-101` query parameter:
   ```text
   http://localhost:8080/?_diverge_env=pr-101
   ```
2. `diverge-proxy` automatically sets an `HttpOnly` session cookie (`Set-Cookie: _diverge_env=pr-101; Path=/; SameSite=Lax`) and routes all subsequent page loads, static assets, `fetch`/XHR requests, and WebSockets to `pr-101` (and injects the floating preview environment switcher if `previewBanner.enabled=true`).
3. To return to the shared baseline environment at any time, visit `http://localhost:8080/?_diverge_env=reset`.

### Web Dashboard, Doctor & MCP Server

Port-forward `diverge-server` to access the Web UI and ConnectRPC / MCP API:

```bash
kubectl port-forward -n diverge-system svc/diverge-server 8443:8443
```

- **Web Dashboard**: Open `http://localhost:8443` in your browser.
- **CLI Health & Verification Check**:
  ```bash
  diverge doctor
  diverge verify pr-101 --base-url http://localhost:8080 --compare-baseline
  ```
- **MCP Server (for Claude Code / Cursor / VS Code)**:
  ```bash
  diverge mcp --server-url http://localhost:8443
  ```

---

## Step 4: Tear Down

```bash
# If created with Kind:
kind delete cluster --name diverge-dev

# If created with k3d (or --provider auto when k3d is installed):
k3d cluster delete diverge-dev
```

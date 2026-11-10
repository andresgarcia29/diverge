#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
echo '=== Helm lint ==='
helm lint charts/diverge/
echo '=== Helm template (defaults) ==='
helm template diverge charts/diverge/ > /dev/null
echo '=== Helm template (all features) ==='
helm template diverge charts/diverge/ \
  --set database.provider=schema \
  --set deploy.provider=direct \
  --set prometheus.alerts.enabled=true \
  --set certManager.enabled=true \
  --set rbac.namespaceScoped=true \
  --set 'rbac.managedNamespaces={ns1,ns2}' \
  > /dev/null
echo '=== Helm template (namespace scoped RBAC) ==='
output=$(helm template diverge charts/diverge/ --set rbac.namespaceScoped=true --set 'rbac.managedNamespaces={test-ns}')
if echo "$output" | grep -qE '^kind: ClusterRole$'; then
  echo 'FAIL: ClusterRole found when namespaceScoped=true'
  exit 1
fi
if ! echo "$output" | grep -A4 '^kind: Role$' | grep -q 'namespace: test-ns'; then
  echo 'FAIL: Role not in expected namespace'
  exit 1
fi
if ! echo "$output" | grep -A4 '^kind: RoleBinding$' | grep -q 'namespace: test-ns'; then
  echo 'FAIL: RoleBinding not in expected namespace'
  exit 1
fi
echo '=== Helm template (server secureCookies flag) ==='
server_output=$(helm template diverge charts/diverge/ --set server.enabled=true --set server.auth.secureCookies=true)
if ! echo "$server_output" | grep -q -- '--secure-cookies=true'; then
  echo 'FAIL: --secure-cookies=true not found in server deployment args'
  exit 1
fi

echo '=== Helm template (all subcomponents: server, proxy, activatorProxy) ==='
all_components=$(helm template diverge charts/diverge/ \
  --set server.enabled=true \
  --set proxy.enabled=true \
  --set activatorProxy.enabled=true \
  --set activatorProxy.activatorUrl=http://activator.knative-serving:8012 \
  --set activatorProxy.targetSelector="app=test")

# Verify no dangling image tag like 'image: "...:"'
if echo "$all_components" | grep -E 'image:\s*".*:[[:space:]]*"'; then
  echo 'FAIL: Dangling image tag detected in rendered templates'
  exit 1
fi

echo '=== Helm template (image tag precedence) ==='
# When image.tag is overridden, controller, server, and proxy should all use that tag if component tags are not set
custom_tag=$(helm template diverge charts/diverge/ \
  --set image.tag=custom-v1 \
  --set server.enabled=true \
  --set proxy.enabled=true)

controller_matches=$(echo "$custom_tag" | grep -c 'image: "ghcr.io/divergedev/diverge:custom-v1"')
if [ "$controller_matches" -ne 3 ]; then
  echo "FAIL: Expected 3 containers with image: ghcr.io/divergedev/diverge:custom-v1, found $controller_matches"
  exit 1
fi

echo '=== Helm template (comma-separated routingProvider) ==='
multi_routing=$(helm template diverge charts/diverge/ --set 'routingProvider=gateway\,istio')
if ! echo "$multi_routing" | grep -q -- '--routing-provider=gateway,istio'; then
  echo 'FAIL: --routing-provider=gateway,istio not found in controller deployment args'
  exit 1
fi

echo '=== Helm template (Grafana dashboards & Prometheus alerts) ==='
observability_output=$(helm template diverge charts/diverge/ \
  --set grafana.dashboards.enabled=true \
  --set prometheus.alerts.enabled=true)
prometheus_rule_count=$(echo "$observability_output" | grep -c '^kind: PrometheusRule$' || true)
if [ "$prometheus_rule_count" -ne 1 ]; then
  echo "FAIL: Expected exactly 1 PrometheusRule resource, found $prometheus_rule_count"
  exit 1
fi
python3 -m json.tool charts/diverge/dashboards/diverge-overview.json > /dev/null
if grep -qE '(\[5m\{|\[1h\{|sum\{)' charts/diverge/dashboards/diverge-overview.json; then
  echo 'FAIL: Malformed PromQL selector detected in diverge-overview.json'
  exit 1
fi

echo '=== Helm template (ValidatingWebhookConfiguration & ValidatingAdmissionPolicy) ==='
admission_output=$(helm template diverge charts/diverge/ \
  --set webhook.validatingWebhook.enabled=true \
  --set webhook.admissionPolicy.enabled=true \
  --set certManager.enabled=true)
if ! echo "$admission_output" | grep -qE '^kind: ValidatingWebhookConfiguration$'; then
  echo 'FAIL: ValidatingWebhookConfiguration not found when webhook.validatingWebhook.enabled=true'
  exit 1
fi
if ! echo "$admission_output" | grep -qE '^kind: ValidatingAdmissionPolicy$'; then
  echo 'FAIL: ValidatingAdmissionPolicy not found when webhook.admissionPolicy.enabled=true'
  exit 1
fi
if ! echo "$admission_output" | grep -qE '^kind: ValidatingAdmissionPolicyBinding$'; then
  echo 'FAIL: ValidatingAdmissionPolicyBinding not found when webhook.admissionPolicy.enabled=true'
  exit 1
fi
if helm template diverge charts/diverge/ --set webhook.validatingWebhook.enabled=true --set certManager.enabled=false > /dev/null 2>&1; then
  echo 'FAIL: Expected helm template to fail when validatingWebhook.enabled=true and certManager.enabled=false without caBundle'
  exit 1
fi
cabundle_output=$(helm template diverge charts/diverge/ \
  --set webhook.validatingWebhook.enabled=true \
  --set certManager.enabled=false \
  --set webhook.validatingWebhook.caBundle=dGVzdC1jYQ==)
if ! echo "$cabundle_output" | grep -q 'caBundle: "dGVzdC1jYQ=="'; then
  echo 'FAIL: Expected caBundle to be rendered in ValidatingWebhookConfiguration'
  exit 1
fi

echo '=== Helm template (server status subresource RBAC, sandbox token Secret gating, and port 8080) ==='
server_default=$(helm template diverge charts/diverge/ --set server.enabled=true)
for subres in "environments/status" "previewgroups/status" "agenttasks/status"; do
  if ! grep -q "\"$subres\"" <<< "$server_default"; then
    echo "FAIL: Expected $subres in server RBAC rules"
    exit 1
  fi
done
if ! grep -q 'name: http-compat' <<< "$server_default"; then
  echo 'FAIL: Expected http-compat port (8080) in server Service'
  exit 1
fi
if ! grep -q 'diverge-agenttask-tokens' <<< "$server_default"; then
  echo 'FAIL: Expected diverge-agenttask-tokens Secret rule when sandbox.enabled=true'
  exit 1
fi
server_no_sandbox=$(helm template diverge charts/diverge/ --set server.enabled=true --set sandbox.enabled=false)
if grep -q 'diverge-agenttask-tokens' <<< "$server_no_sandbox"; then
  echo 'FAIL: Expected diverge-agenttask-tokens Secret rule to be omitted when sandbox.enabled=false'
  exit 1
fi

echo '=== Helm template (notifier, extraArgs, extraEnv) ==='
notifier_output=$(helm template diverge charts/diverge/ \
  --set notifier.provider=gitlab \
  --set notifier.baseUrl=https://gitlab.example.com \
  --set notifier.tokenSecretRef.name=diverge-gitlab \
  --set defaultNamespace=previews \
  --set 'extraArgs={--zap-log-level=debug}' \
  --set 'extraEnv[0].name=HTTPS_PROXY' \
  --set 'extraEnv[0].value=http://proxy:3128')
for expected in '--notifier-provider=gitlab' '--default-namespace=previews' '"--zap-log-level=debug"' \
  'value: "https://gitlab.example.com"' 'name: diverge-gitlab' 'name: HTTPS_PROXY'; do
  if ! grep -qF -- "$expected" <<< "$notifier_output"; then
    echo "FAIL: Expected '$expected' in controller deployment"
    exit 1
  fi
done
if helm template diverge charts/diverge/ | grep -q 'DIVERGE_NOTIFIER_TOKEN'; then
  echo 'FAIL: DIVERGE_NOTIFIER_TOKEN rendered without notifier.tokenSecretRef.name'
  exit 1
fi

echo '✅ All Helm tests passed'



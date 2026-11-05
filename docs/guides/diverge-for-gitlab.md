# Diverge for GitLab

Complete guide to using Diverge with GitLab — from setup to production.

## Prerequisites
- Kubernetes cluster with Diverge installed
- GitLab (SaaS or Self-Hosted)
- Container registry (GitLab Container Registry recommended)

## 1. Install Diverge
Install the Diverge controller using the official OCI Helm chart, passing in your GitLab-specific values:
```bash
helm install diverge oci://ghcr.io/divergedev/charts/diverge \
  --namespace diverge-system \
  --create-namespace \
  -f gitlab-values.yaml
```

## 2. Configure GitLab Webhook
To enable automatic environment creation and interactive MR slash commands, configure a webhook in your GitLab project:
1. Go to your GitLab Project -> **Settings** -> **Webhooks**.
2. URL: `https://diverge.yourdomain.com/gitlab-webhook` (replace with your Diverge ingress endpoint).
3. Secret Token: Your configured webhook secret token.
4. Trigger events: Check **Merge request events** and **Comments** (`Note Hook` on Merge Requests).
5. Click **Add webhook**.

For **PreviewGroups** support (multi-service environments), add a second webhook pointing to:
- URL: `https://diverge.yourdomain.com/gitlab-previewgroup-webhook`
*(Note: The `/gitlab-previewgroup-webhook` endpoint is currently only available on the latest main branch, not yet in a stable release.)*

*Note: The webhook endpoint must be accessible from your GitLab instance.*

## 3. Configure Diverge Environment Variables
The Diverge controller requires access to GitLab to post comments and validate webhooks. Ensure the following environment variables are set on the Diverge controller manager container:
- `DIVERGE_NOTIFIER_TOKEN`: GitLab personal access token or project access token (requires `api` scope).
- `DIVERGE_WEBHOOK_SECRET`: Your configured webhook secret token.

## 4. Helm Values for GitLab
To configure Diverge for GitLab integration, apply the following `values.yaml` during installation:

```yaml
notifierProvider: gitlab
```

*Note: The Helm chart does not currently natively map the secrets to the required environment variables. You will need to patch the `diverge-controller-manager` deployment to include `DIVERGE_NOTIFIER_TOKEN` and `DIVERGE_WEBHOOK_SECRET` in its `env` list.*

## 5. Single Service Setup
For single-service repositories, the `Environment` CRD is deployed when a Merge Request webhook event is received. Ensure your `.diverge.yaml` maps the repository correctly:
```yaml
version: "1"
services:
  frontend:
    paths:
      - apps/web/**
    image:
      repository: registry.example.com/frontend
```
When an MR is opened or updated, Diverge creates an `Environment` matching the MR ID and routes header-based traffic specifically to the updated service.

## 6. Multi-Service Setup (PreviewGroups)
For monorepos with multiple inter-dependent services, the `PreviewGroup` CRD ensures all services build and deploy together for a specific MR.

When the PreviewGroup webhook is triggered:
- Diverge provisions a cohesive environment for the MR.
- MR comments are posted to summarize the collective state and provide the routing header (e.g., `x-preview-env: 123`).
- Teardown of the PreviewGroup happens automatically when the MR is closed or merged.

## 7. Interactive MR Slash Commands

With **Comments** (`Note Hook` on Merge Requests) enabled on your GitLab webhook (or **Issue comments** (`issue_comment`) on GitHub, restricted to repository `OWNER`, `MEMBER`, or `COLLABORATOR` roles), developers can trigger preview and agent actions directly from Merge Request comments:

| Slash Command | Action |
| :--- | :--- |
| `/diverge preview` | Provisions or reconciles an `Environment` (`mr-<iid>`) for the Merge Request branch. |
| `/diverge fix <objective>` | Creates or updates an autonomous `AgentTask` (with an automatic linked preview environment) targeting the MR branch. |
| `/diverge extend-ttl [duration]` | Extends the TTL (`spec.lifecycle.ttl`) of the MR's `Environment` and `PreviewGroup` resources (defaults to `24h` when `[duration]` is omitted). |
| `/diverge teardown` | Immediately deletes all `Environment` and `PreviewGroup` resources tied to the MR. |
| `/diverge pause` | Suspends active `AgentTask` execution for the MR (`spec.suspended = true`). |
| `/diverge resume` | Resumes paused `AgentTask` execution for the MR (`spec.suspended = false`). |

Example MR comment commands:
```text
/diverge preview
/diverge fix Fix N+1 query in user settings endpoint
/diverge extend-ttl 48h
/diverge pause
/diverge resume
/diverge teardown
```

## 8. GitLab CI/CD Pipeline
Include the official Diverge GitLab CI/CD Component (`templates/preview/template.yml`) to automatically detect changed microservices via `diverge diff`, apply a `PreviewGroup` CR, and export dotenv-safe variables in `preview.env` (`DIVERGE_PREVIEW_NAME`, `DIVERGE_SKIPPED`, `DIVERGE_ROUTING_HEADER_B64`, `DIVERGE_PREVIEW_URL`) to downstream test jobs:

```yaml
include:
  - component: $CI_SERVER_FQDN/divergedev/diverge/preview@v0.19.0
    inputs:
      diverge_registry: "$CI_REGISTRY_IMAGE"
      diverge_base_url: "https://staging.example.com"

stages:
  - build
  - deploy
  - test

build-preview:
  stage: build
  image: docker:24.0.5
  services:
    - docker:24.0.5-dind
  script:
    - docker login -u $CI_REGISTRY_USER -p $CI_REGISTRY_PASSWORD $CI_REGISTRY
    - docker build -t $CI_REGISTRY_IMAGE/frontend:${CI_COMMIT_SHA:0:12} .
    - docker push $CI_REGISTRY_IMAGE/frontend:${CI_COMMIT_SHA:0:12}
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event" && $CI_MERGE_REQUEST_EVENT_TYPE != "merge_train"

e2e-preview:
  stage: test
  needs:
    - job: diverge:preview
      optional: true
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event" && $CI_MERGE_REQUEST_EVENT_TYPE != "merge_train"
  script:
    - if [ "${DIVERGE_SKIPPED:-false}" = "true" ]; then exit 0; fi
    - EXTRA_HTTP_HEADERS="$(echo "$DIVERGE_ROUTING_HEADER_B64" | base64 -d)" npx playwright test
```
Refer to [`ci/gitlab/README.md`](../../ci/gitlab/README.md) and [`docs/guides/ci-cd-workflows.md`](ci-cd-workflows.md) for full input and output documentation.

## 9. Self-Hosted GitLab
If you are running a self-hosted GitLab instance, ensure the following are configured:
- **Custom URL configuration**: You must pass the `--gitlab-url` flag to the controller manager deployment args. The base URL handling for the standard notifier is currently unconfigurable via env var or Helm value.
- **Internal CA certificates**: If your instance uses self-signed certs, mount your CA bundle into the controller pod.
- **Private registry authentication**: Ensure Diverge has the correct image pull secrets for your internal container registry.
- **Network considerations**: The webhook delivery must be able to reach your Kubernetes cluster from the GitLab instance. You may need to configure local network access in GitLab's Admin Area (`Settings` > `Network` > `Outbound requests`).

## 10. GitLab vs GitHub Feature Parity
| Feature | GitHub | GitLab |
|---------|--------|--------|
| Environment MR comments | ✅ | ✅ |
| PreviewGroup MR comments | ✅ | ✅ |
| Interactive `/diverge` slash commands | ✅ (`issue_comment`) | ✅ (`Note Hook`) |
| Webhook signature validation | ✅ HMAC-SHA256 | ✅ Token |
| CI/CD integration | GitHub Actions | GitLab CI/CD |
| Container registry | ghcr.io | registry.gitlab.com |

## 11. Troubleshooting
- **Webhook 404** → Check that your webhook URL endpoint is correct and accessible.
- **No MR comments** → Verify your `DIVERGE_NOTIFIER_TOKEN` permissions; it requires the `api` scope to post notes.
- **Self-hosted cert errors** → You likely need to mount your custom CA bundle in the controller.

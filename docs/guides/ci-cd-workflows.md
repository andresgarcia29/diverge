# CI/CD Workflows (GitHub Actions & GitLab CI)

Drop-in pipeline templates that detect changed services with `diverge diff`, deploy a delta `PreviewGroup` for only the modified microservices, wait for readiness, and run end-to-end tests against the isolated preview environment.

---

## 1. `.diverge.yaml` Service Mapping

Place `.diverge.yaml` at the root of your repository so `diverge diff` maps file paths to Kubernetes services:

```yaml
version: "1"
services:
  payments-api:
    paths:
      - services/payments/**
    image:
      repository: ghcr.io/acme/payments-api
  checkout-web:
    paths:
      - apps/checkout/**
    image:
      repository: ghcr.io/acme/checkout-web
  ledger-worker:
    paths:
      - services/ledger/**
    image:
      repository: ghcr.io/acme/ledger-worker
```

---

## 2. GitHub Actions Workflow (`.github/workflows/preview.yml`)

### Turnkey Composite Action (Recommended)

Use the official `divergedev/diverge@v0.21.0` GitHub Action to validate `.diverge.yaml`, run `diverge diff`, deploy a delta `PreviewGroup`, verify readiness and latency SLOs (`verify: "true"`), replay recorded traffic traces (`replay: "true"`), and expose routing headers to Playwright or Cypress:

```yaml
name: Preview Environment

on:
  pull_request:
    types: [opened, synchronize, reopened, closed]

jobs:
  preview:
    if: github.event.action != 'closed'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Deploy Diverge Delta Preview
        id: diverge
        uses: divergedev/diverge@v0.21.0
        with:
          action: deploy
          kubeconfig: ${{ secrets.PREVIEW_KUBECONFIG }}
          image-registry: ghcr.io/acme
          base-url: https://staging.example.com
          wait: "true"
          verify: "true"
          verify-path: "/healthz"
          verify-max-latency: "2s"

      - name: Run Playwright E2E Against Preview
        if: steps.diverge.outputs.skipped != 'true'
        run: npx playwright test
        env:
          PREVIEW_URL: ${{ steps.diverge.outputs.preview-url }}
          EXTRA_HTTP_HEADERS: ${{ steps.diverge.outputs.routing-header-json }}

  cleanup:
    if: github.event.action == 'closed'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: divergedev/diverge@v0.21.0
        with:
          action: teardown
          kubeconfig: ${{ secrets.PREVIEW_KUBECONFIG }}
```

### Action Inputs & Outputs

| Input | Default | Description |
| :--- | :--- | :--- |
| `action` | `deploy` | `validate`, `diff`, `deploy`, `verify`, `replay`, or `teardown` |
| `kubeconfig` | `""` | Raw or base64-encoded kubeconfig for target cluster |
| `image-registry` | `""` | Registry prefix for changed services (e.g. `ghcr.io/acme`) |
| `image-tag` | `""` | Image tag for changed services (when omitted, defaults to resolved `preview-name`, e.g. `pr-<number>` or branch slug) |
| `header-key` | `x-diverge-env` | HTTP routing header key |
| `ttl` | `4h` | Preview environment TTL |
| `base-url` | `""` | Optional gateway base URL to emit `preview-url` (`?_diverge_env=`) |
| `wait` | `false` | Wait for `PreviewGroup` `Ready` condition (automatically runs `diverge doctor` on timeout) |
| `verify` | `false` | Run `diverge verify` after deploy and write scorecard to `$GITHUB_STEP_SUMMARY` |
| `verify-path` | `/` | HTTP path to probe during `verify` |
| `verify-compare-baseline` | `false` | Compare preview HTTP status against baseline during `verify` |
| `verify-max-latency` | `5s` | Maximum allowed HTTP latency during `verify` |
| `replay` | `false` | Run `diverge replay` after deploy and write contract diff scorecard to `$GITHUB_STEP_SUMMARY` |
| `replay-from` | `""` | Path to HAR (`.har`) or JSONL (`.jsonl`) trace file for `diverge replay` |
| `replay-max-latency` | `""` | Maximum allowed preview latency per replayed request (e.g. `500ms`, `2s`) |
| `replay-compare-values` | `false` | Compare exact scalar values during replay instead of structural types |
| `replay-fail-on-extra-fields` | `false` | Fail replay when preview adds new fields not present in baseline |
| `replay-ignore-fields` | `""` | Comma-separated field paths or Protobuf field numbers to ignore during replay |
| `replay-filter-hosts` | `""` | Comma-separated host patterns to include from HAR/JSONL traces during replay |
| `replay-strip-auth` | `false` | Strip recorded `Authorization`, `Proxy-Authorization`, `Cookie`, and `X-Api-Key` headers during replay |
| `replay-proto-descriptor` | `""` | Optional compiled Protobuf `FileDescriptorSet` (`.binpb`) for named-field Protobuf diffs |
| `replay-proto-message` | `""` | Fully-qualified Protobuf response message name when using `replay-proto-descriptor` |

| Output | Example | Description |
| :--- | :--- | :--- |
| `preview-name` | `pr-42` | Resolved `PreviewGroup` name |
| `changed-services` | `["payments-api"]` | JSON array of changed services from `diverge diff` |
| `changed-count` | `1` | Number of changed services detected |
| `skipped` | `false` | `true` when 0 services changed |
| `routing-header` | `x-diverge-env: pr-42` | HTTP header for curl / API tests |
| `routing-header-json` | `{"x-diverge-env":"pr-42"}` | JSON header map for Playwright / Cypress |
| `preview-url` | `https://staging.example.com/?_diverge_env=pr-42` | Zero-extension browser preview link |

### Manual CLI Workflow

If you prefer calling the `diverge` CLI directly without the composite action:

```yaml
      - name: Install Diverge CLI
        run: curl -fsSL https://raw.githubusercontent.com/divergedev/diverge/main/install.sh | sh

      - name: Detect Changed Services & Deploy Delta PreviewGroup
        env:
          KUBECONFIG_DATA: ${{ secrets.PREVIEW_KUBECONFIG }}
        run: |
          mkdir -p ~/.kube && echo "$KUBECONFIG_DATA" | base64 -d > ~/.kube/config
          DIFF_JSON=$(diverge diff --base "origin/${{ github.base_ref }}" --output json)
          mapfile -t SVC_ARGS < <(echo "$DIFF_JSON" | jq -r '.services[]? | "--service=\(.)=ghcr.io/acme/\(.):pr-${{ github.event.number }}"')
          if [ "${#SVC_ARGS[@]}" -eq 0 ]; then
            echo "No service changes detected; skipping preview deployment."
            exit 0
          fi
          diverge preview create \
            --name "pr-${{ github.event.number }}" \
            --mr "${{ github.event.number }}" \
            --ttl 4h \
            "${SVC_ARGS[@]}"
          diverge verify "pr-${{ github.event.number }}" --path /healthz --max-latency 2s --github-summary
```

---

## 3. GitLab CI Pipeline (`.gitlab-ci.yml`)

### Turnkey GitLab CI/CD Component (Recommended)

Include the official component (`templates/preview/template.yml`). It automatically runs `diverge diff` when `diverge_services` is omitted, optionally runs `diverge replay` when `diverge_replay_from` is set, and exports dotenv-safe variables in `preview.env` (`DIVERGE_PREVIEW_NAME`, `DIVERGE_SKIPPED`, `DIVERGE_ROUTING_HEADER_B64`, `DIVERGE_PREVIEW_URL`) to downstream jobs:

```yaml
include:
  - component: $CI_SERVER_FQDN/divergedev/diverge/preview@v0.21.0
    inputs:
      diverge_registry: "$CI_REGISTRY_IMAGE"
      diverge_base_url: "https://staging.example.com"
      diverge_replay_from: "./traces/checkout.jsonl"

e2e_preview:
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

### Manual CLI Pipeline

Ensure your `playwright.config.ts` maps `EXTRA_HTTP_HEADERS` to `use.extraHTTPHeaders` (e.g. `extraHTTPHeaders: process.env.EXTRA_HTTP_HEADERS ? JSON.parse(process.env.EXTRA_HTTP_HEADERS) : undefined`):

```yaml
stages:
  - preview
  - test
  - cleanup

deploy_preview:
  stage: preview
  image: ghcr.io/divergedev/diverge-cli:latest
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event" && $CI_MERGE_REQUEST_EVENT_TYPE != "merge_train"
  artifacts:
    reports:
      dotenv: preview.env
  script:
    - git fetch --depth=50 origin "$CI_MERGE_REQUEST_TARGET_BRANCH_NAME"
    - DIFF_JSON=$(diverge diff --base "origin/$CI_MERGE_REQUEST_TARGET_BRANCH_NAME" --output json)
    - SVC_ARGS=$(echo "$DIFF_JSON" | jq -r '.services[]? | "--service=\(.)=registry.example.com/\(.):mr-'"${CI_MERGE_REQUEST_IID}"'"')
    - |
      if [ -n "$SVC_ARGS" ]; then
        diverge preview create \
          --name "mr-${CI_MERGE_REQUEST_IID}" \
          --mr "${CI_MERGE_REQUEST_IID}" \
          --ttl 4h \
          $SVC_ARGS
        echo "DIVERGE_SKIPPED=false" > preview.env
      else
        echo "No service changes detected; skipping preview deployment."
        echo "DIVERGE_SKIPPED=true" > preview.env
      fi

e2e_preview:
  stage: test
  needs: ["deploy_preview"]
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event" && $CI_MERGE_REQUEST_EVENT_TYPE != "merge_train"
  script:
    - if [ "${DIVERGE_SKIPPED:-false}" = "true" ]; then exit 0; fi
    - npm ci
    - EXTRA_HTTP_HEADERS="{\"x-diverge-env\":\"mr-${CI_MERGE_REQUEST_IID}\"}" npx playwright test

stop_preview:
  stage: cleanup
  image: bitnami/kubectl:latest
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event" && $CI_MERGE_REQUEST_EVENT_TYPE != "merge_train"
      when: manual
      allow_failure: true
  environment:
    name: preview/mr-${CI_MERGE_REQUEST_IID}
    action: stop
  script:
    - kubectl delete previewgroup "mr-${CI_MERGE_REQUEST_IID}" --ignore-not-found
```

---

## 4. Interactive PR / MR Slash Commands

In addition to CI-driven preview deployments, Diverge's webhook handlers execute interactive `/diverge` slash commands posted in GitHub Pull Request and GitLab Merge Request comments.

### Webhook Event Subscriptions
- **GitHub**: Subscribe your webhook (`/github-webhook`) to **Issue comments** (`issue_comment`) in addition to **Pull requests** (`pull_request`). For security, only comments on Pull Requests from repository `OWNER`, `MEMBER`, or `COLLABORATOR` accounts are executed.
- **GitLab**: Enable **Comments** (`Note Hook` on Merge Requests) in addition to **Merge request events** on `/gitlab-webhook`.

### PR / MR Slash Commands Reference

| Slash Command | Target Resource(s) | Description |
| :--- | :--- | :--- |
| `/diverge preview` | `Environment` | Creates or reconciles an `Environment` (`pr-<num>` / `mr-<num>`) for the current PR/MR branch. |
| `/diverge verify [path]` | `Environment`, `PreviewGroup` | Runs Kubernetes workload `doctor` diagnostics and routed HTTP readiness + baseline parity probes against the PR/MR preview environment (defaults to `/` when `[path]` is omitted) and replies with a Markdown scorecard. |
| `/diverge replay [path1,path2,...]` | `Environment`, `PreviewGroup` | Replays inline `GET` probe paths against baseline and the PR/MR preview environment (defaults to `/` when omitted), diffing status codes, latency, and JSON/Protobuf response contracts, and replies with a Markdown scorecard. |
| `/diverge fix <objective>` | `AgentTask` | Spawns or updates an autonomous `AgentTask` (and linked preview environment) targeting the PR/MR branch with `<objective>`. |
| `/diverge extend-ttl [duration]` | `Environment`, `PreviewGroup` | Extends `spec.lifecycle.ttl` for all preview environments and preview groups matching the PR/MR (defaults to `24h` if `[duration]` is omitted, e.g., `/diverge extend-ttl 48h`). |
| `/diverge teardown` | `Environment`, `PreviewGroup` | Immediately deletes all `Environment` and `PreviewGroup` resources associated with the PR/MR. |
| `/diverge pause` | `AgentTask` | Suspends active `AgentTask` runs associated with the PR/MR (`spec.suspended = true`). |
| `/diverge resume` | `AgentTask` | Resumes paused `AgentTask` runs associated with the PR/MR (`spec.suspended = false`). |

### Examples

```text
/diverge preview
/diverge verify /healthz
/diverge replay /api/v1/catalog,/api/v1/healthz
/diverge fix Resolve the flaky checkout integration test and update currency rounding
/diverge extend-ttl 48h
/diverge pause
/diverge resume
/diverge teardown
```

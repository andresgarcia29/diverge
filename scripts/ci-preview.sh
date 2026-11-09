#!/usr/bin/env bash
# scripts/ci-preview.sh
# Portable CI helper for GitHub Actions and GitLab CI preview workflows.
set -euo pipefail

DIVERGE_BIN="${DIVERGE_BIN:-diverge}"
KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"

INPUT_ACTION="${INPUT_ACTION:-${ACTION:-deploy}}"
ACTION="$INPUT_ACTION"
INPUT_CONFIG="${INPUT_CONFIG:-.diverge.yaml}"
INPUT_BASE_REF="${INPUT_BASE_REF:-origin/main}"
INPUT_PREVIEW_NAME="${INPUT_PREVIEW_NAME:-}"
INPUT_PR_NUMBER="${INPUT_PR_NUMBER:-}"
INPUT_BRANCH="${INPUT_BRANCH:-}"
INPUT_SERVICES="${INPUT_SERVICES:-}"
INPUT_IMAGE_REGISTRY="${INPUT_IMAGE_REGISTRY:-}"
INPUT_IMAGE_TAG="${INPUT_IMAGE_TAG:-}"
INPUT_HEADER_KEY="${INPUT_HEADER_KEY:-x-diverge-env}"
INPUT_HEADER_VALUE="${INPUT_HEADER_VALUE:-}"
INPUT_TTL="${INPUT_TTL:-4h}"
INPUT_NAMESPACE="${INPUT_NAMESPACE:-${NAMESPACE:-default}}"
NAMESPACE="$INPUT_NAMESPACE"
INPUT_BASE_URL="${INPUT_BASE_URL:-}"
INPUT_WAIT="${INPUT_WAIT:-false}"
INPUT_WAIT_TIMEOUT="${INPUT_WAIT_TIMEOUT:-180s}"
INPUT_VERIFY="${INPUT_VERIFY:-false}"
INPUT_VERIFY_PATH="${INPUT_VERIFY_PATH:-/}"
INPUT_VERIFY_COMPARE_BASELINE="${INPUT_VERIFY_COMPARE_BASELINE:-false}"
INPUT_VERIFY_MAX_LATENCY="${INPUT_VERIFY_MAX_LATENCY:-5s}"
INPUT_REPLAY="${INPUT_REPLAY:-false}"
INPUT_REPLAY_FROM="${INPUT_REPLAY_FROM:-}"
INPUT_REPLAY_MAX_LATENCY="${INPUT_REPLAY_MAX_LATENCY:-}"
INPUT_REPLAY_COMPARE_VALUES="${INPUT_REPLAY_COMPARE_VALUES:-false}"
INPUT_REPLAY_FAIL_ON_EXTRA_FIELDS="${INPUT_REPLAY_FAIL_ON_EXTRA_FIELDS:-false}"
INPUT_REPLAY_IGNORE_FIELDS="${INPUT_REPLAY_IGNORE_FIELDS:-}"
INPUT_REPLAY_FILTER_HOSTS="${INPUT_REPLAY_FILTER_HOSTS:-}"
INPUT_REPLAY_STRIP_AUTH="${INPUT_REPLAY_STRIP_AUTH:-false}"
INPUT_REPLAY_PROTO_DESCRIPTOR="${INPUT_REPLAY_PROTO_DESCRIPTOR:-}"
INPUT_REPLAY_PROTO_MESSAGE="${INPUT_REPLAY_PROTO_MESSAGE:-}"
INPUT_DRY_RUN="${INPUT_DRY_RUN:-false}"
INPUT_KUBECONFIG="${INPUT_KUBECONFIG:-}"

TEMP_KUBECONFIG=""
cleanup() {
  if [[ -n "$TEMP_KUBECONFIG" && -f "$TEMP_KUBECONFIG" ]]; then
    rm -f "$TEMP_KUBECONFIG"
  fi
}
trap cleanup EXIT

if [[ -n "$INPUT_KUBECONFIG" ]]; then
  TEMP_KUBECONFIG="$(mktemp)"
  chmod 600 "$TEMP_KUBECONFIG"
  if [[ "$INPUT_KUBECONFIG" == *"apiVersion:"* ]]; then
    printf '%s\n' "$INPUT_KUBECONFIG" > "$TEMP_KUBECONFIG"
  else
    printf '%s' "$INPUT_KUBECONFIG" | base64 -d > "$TEMP_KUBECONFIG" 2>/dev/null || \
      printf '%s' "$INPUT_KUBECONFIG" | base64 -D > "$TEMP_KUBECONFIG"
  fi
  export KUBECONFIG="$TEMP_KUBECONFIG"
fi

emit_output() {
  local key="$1"
  local val="$2"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf '%s=%s\n' "$key" "$val" >> "$GITHUB_OUTPUT"
  fi
  if [[ -n "${DIVERGE_DOTENV_FILE:-}" ]]; then
    local upper_key
    upper_key="$(printf '%s' "$key" | tr '[:lower:]-' '[:upper:]_')"
    printf 'DIVERGE_%s=%s\n' "$upper_key" "$val" >> "$DIVERGE_DOTENV_FILE"
  fi
}

resolve_preview_name() {
  if [[ -n "$INPUT_PREVIEW_NAME" ]]; then
    printf '%s' "$INPUT_PREVIEW_NAME"
    return
  fi
  if [[ -n "$INPUT_PR_NUMBER" ]]; then
    printf 'pr-%s' "$INPUT_PR_NUMBER"
    return
  fi
  local branch="${INPUT_BRANCH:-}"
  if [[ -z "$branch" ]]; then
    branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "preview")"
  fi
  local sanitized
  sanitized="$(printf '%s' "$branch" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//')"
  printf '%s' "${sanitized:-preview}"
}

extract_config_services() {
  local cfg="$1"
  [[ -f "$cfg" ]] || return 0
  awk '
    /^services:[[:space:]]*$/ { in_services=1; next }
    /^[^[:space:]#]/ { in_services=0 }
    in_services && /^  [a-zA-Z0-9._-]+:[[:space:]]*$/ {
      line=$0
      sub(/^[[:space:]]+/, "", line)
      sub(/:[[:space:]]*$/, "", line)
      print line
    }
  ' "$cfg"
}

run_verify() {
  local preview_name="$1"
  local header_val="${INPUT_HEADER_VALUE:-$preview_name}"
  local -a verify_args=(
    verify "$preview_name"
    -n "$NAMESPACE"
    --header-key "$INPUT_HEADER_KEY"
    --header-value "$header_val"
    --path "${INPUT_VERIFY_PATH:-/}"
    --max-latency "${INPUT_VERIFY_MAX_LATENCY:-5s}"
  )
  if [[ -n "${INPUT_BASE_URL:-}" ]]; then
    verify_args+=(--base-url "$INPUT_BASE_URL")
  fi
  if [[ "${INPUT_VERIFY_COMPARE_BASELINE:-false}" == "true" ]]; then
    verify_args+=(--compare-baseline)
  fi
  verify_args+=(--github-summary)
  "$DIVERGE_BIN" "${verify_args[@]}"
}

run_replay() {
  local preview_name="$1"
  if [[ -z "${INPUT_REPLAY_FROM:-}" ]]; then
    echo "Error: INPUT_REPLAY_FROM (replay-from) is required when running diverge replay" >&2
    exit 1
  fi
  local header_val="${INPUT_HEADER_VALUE:-$preview_name}"
  local -a replay_args=(
    replay "$preview_name"
    --from "$INPUT_REPLAY_FROM"
    -n "$NAMESPACE"
    --header-key "$INPUT_HEADER_KEY"
    --header-value "$header_val"
  )
  if [[ -n "${INPUT_BASE_URL:-}" ]]; then
    replay_args+=(--base-url "$INPUT_BASE_URL")
  fi
  if [[ -n "${INPUT_REPLAY_MAX_LATENCY:-}" ]]; then
    replay_args+=(--max-latency "$INPUT_REPLAY_MAX_LATENCY")
  fi
  if [[ "${INPUT_REPLAY_COMPARE_VALUES:-false}" == "true" ]]; then
    replay_args+=(--compare-values)
  fi
  if [[ "${INPUT_REPLAY_FAIL_ON_EXTRA_FIELDS:-false}" == "true" ]]; then
    replay_args+=(--fail-on-extra-fields)
  fi
  if [[ "${INPUT_REPLAY_STRIP_AUTH:-false}" == "true" ]]; then
    replay_args+=(--strip-auth)
  fi
  if [[ -n "${INPUT_REPLAY_PROTO_DESCRIPTOR:-}" ]]; then
    replay_args+=(--proto-descriptor "$INPUT_REPLAY_PROTO_DESCRIPTOR")
  fi
  if [[ -n "${INPUT_REPLAY_PROTO_MESSAGE:-}" ]]; then
    replay_args+=(--proto-message "$INPUT_REPLAY_PROTO_MESSAGE")
  fi
  if [[ -n "${INPUT_REPLAY_IGNORE_FIELDS:-}" ]]; then
    while IFS= read -r fld; do
      fld="$(printf '%s' "$fld" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
      [[ -z "$fld" ]] && continue
      replay_args+=(--ignore-field "$fld")
    done < <(printf '%s\n' "$INPUT_REPLAY_IGNORE_FIELDS" | tr ',' '\n')
  fi
  if [[ -n "${INPUT_REPLAY_FILTER_HOSTS:-}" ]]; then
    while IFS= read -r hst; do
      hst="$(printf '%s' "$hst" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
      [[ -z "$hst" ]] && continue
      replay_args+=(--filter-host "$hst")
    done < <(printf '%s\n' "$INPUT_REPLAY_FILTER_HOSTS" | tr ',' '\n')
  fi
  replay_args+=(--github-summary)
  "$DIVERGE_BIN" "${replay_args[@]}"
}

case "$INPUT_ACTION" in
  validate)
    "$DIVERGE_BIN" validate --config "$INPUT_CONFIG" --output json
    ;;

  diff)
    DIFF_JSON="$("$DIVERGE_BIN" diff --config "$INPUT_CONFIG" --base "$INPUT_BASE_REF" --output json)"
    printf '%s\n' "$DIFF_JSON"
    SERVICES_JSON="$(printf '%s' "$DIFF_JSON" | jq -c '.services // []')"
    COUNT="$(printf '%s' "$DIFF_JSON" | jq -r '.count // 0')"
    emit_output "changed-services" "$SERVICES_JSON"
    emit_output "changed-count" "$COUNT"
    ;;

  deploy)
    PREVIEW_NAME="$(resolve_preview_name)"
    HEADER_VALUE="${INPUT_HEADER_VALUE:-$PREVIEW_NAME}"
    ROUTING_HEADER="${INPUT_HEADER_KEY}: ${HEADER_VALUE}"
    ROUTING_HEADER_JSON="{\"${INPUT_HEADER_KEY}\":\"${HEADER_VALUE}\"}"

    declare -a SVC_ARGS=()
    declare -a CHANGED_SERVICES=()

    if [[ -n "$INPUT_SERVICES" ]]; then
      while IFS= read -r entry; do
        entry="$(printf '%s' "$entry" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
        [[ -z "$entry" ]] && continue
        SVC_ARGS+=("--service" "$entry")
        svc_name="${entry%%=*}"
        CHANGED_SERVICES+=("$svc_name")
      done < <(printf '%s\n' "$INPUT_SERVICES" | tr ',' '\n')
      if [[ "${#CHANGED_SERVICES[@]}" -eq 0 ]]; then
        SERVICES_JSON="[]"
      else
        SERVICES_JSON="$(printf '%s\n' "${CHANGED_SERVICES[@]}" | jq -R . | jq -cs .)"
      fi
    else
      DIFF_JSON="$("$DIVERGE_BIN" diff --config "$INPUT_CONFIG" --base "$INPUT_BASE_REF" --output json)"
      SERVICES_JSON="$(printf '%s' "$DIFF_JSON" | jq -c '.services // []')"
      while IFS= read -r svc; do
        [[ -z "$svc" ]] && continue
        CHANGED_SERVICES+=("$svc")
      done < <(printf '%s' "$DIFF_JSON" | jq -r '.services[]?')

      if [[ "${#CHANGED_SERVICES[@]}" -eq 0 ]]; then
        echo "No service changes detected relative to ${INPUT_BASE_REF}; skipping preview deployment."
        emit_output "preview-name" "$PREVIEW_NAME"
        emit_output "changed-services" "[]"
        emit_output "changed-count" "0"
        emit_output "skipped" "true"
        emit_output "routing-header" "$ROUTING_HEADER"
        emit_output "routing-header-json" "$ROUTING_HEADER_JSON"
        emit_output "preview-url" ""
        exit 0
      fi

      IMAGE_TAG="${INPUT_IMAGE_TAG:-$PREVIEW_NAME}"
      for svc in "${CHANGED_SERVICES[@]}"; do
        if [[ -n "$INPUT_IMAGE_REGISTRY" ]]; then
          SVC_ARGS+=("--service" "${svc}=${INPUT_IMAGE_REGISTRY%/}/${svc}:${IMAGE_TAG}")
        else
          SVC_ARGS+=("--service" "${svc}=${svc}:${IMAGE_TAG}")
        fi
      done

      while IFS= read -r cfg_svc; do
        [[ -z "$cfg_svc" ]] && continue
        is_changed=0
        for changed_svc in "${CHANGED_SERVICES[@]}"; do
          if [[ "$cfg_svc" == "$changed_svc" ]]; then
            is_changed=1
            break
          fi
        done
        if [[ "$is_changed" -eq 0 ]]; then
          SVC_ARGS+=("--service" "$cfg_svc")
        fi
      done < <(extract_config_services "$INPUT_CONFIG")
    fi

    CREATE_ARGS=(
      preview create
      --name "$PREVIEW_NAME"
      --header-key "$INPUT_HEADER_KEY"
      --header-value "$HEADER_VALUE"
      --ttl "$INPUT_TTL"
      -n "$INPUT_NAMESPACE"
    )
    if [[ "$INPUT_PR_NUMBER" =~ ^[0-9]+$ ]] && [[ "$INPUT_PR_NUMBER" -gt 0 ]]; then
      CREATE_ARGS+=(--mr "$INPUT_PR_NUMBER")
    fi
    if [[ "$INPUT_DRY_RUN" == "true" ]]; then
      CREATE_ARGS+=(--dry-run)
    fi
    CREATE_ARGS+=("${SVC_ARGS[@]}")

    "$DIVERGE_BIN" "${CREATE_ARGS[@]}"

    if [[ ( "$INPUT_WAIT" == "true" || "$INPUT_VERIFY" == "true" || "$INPUT_REPLAY" == "true" ) && "$INPUT_DRY_RUN" != "true" ]]; then
      if ! "$KUBECTL_BIN" wait -n "$INPUT_NAMESPACE" --for=condition=Ready "previewgroup/$PREVIEW_NAME" --timeout="$INPUT_WAIT_TIMEOUT"; then
        "$DIVERGE_BIN" doctor "$PREVIEW_NAME" -n "$NAMESPACE" || true
        exit 1
      fi
      for _ in $(seq 1 15); do
        GEN_PAIR="$("$KUBECTL_BIN" get previewgroup "$PREVIEW_NAME" -n "$INPUT_NAMESPACE" -o jsonpath='{.metadata.generation}:{.status.conditions[?(@.type=="Ready")].observedGeneration}' 2>/dev/null || true)"
        META_GEN="${GEN_PAIR%%:*}"
        OBS_GEN="${GEN_PAIR#*:}"
        if [[ ! "$META_GEN" =~ ^[0-9]+$ ]] || [[ ! "$OBS_GEN" =~ ^[0-9]+$ ]] || [[ "$OBS_GEN" -ge "$META_GEN" ]]; then
          break
        fi
        sleep 2
        if ! "$KUBECTL_BIN" wait -n "$INPUT_NAMESPACE" --for=condition=Ready "previewgroup/$PREVIEW_NAME" --timeout="$INPUT_WAIT_TIMEOUT"; then
          "$DIVERGE_BIN" doctor "$PREVIEW_NAME" -n "$NAMESPACE" || true
          exit 1
        fi
      done
    fi

    PREVIEW_URL=""
    if [[ -n "$INPUT_BASE_URL" ]]; then
      PREVIEW_URL="${INPUT_BASE_URL%/}/?_diverge_env=${HEADER_VALUE}"
    fi

    emit_output "preview-name" "$PREVIEW_NAME"
    emit_output "changed-services" "$SERVICES_JSON"
    emit_output "changed-count" "${#CHANGED_SERVICES[@]}"
    emit_output "skipped" "false"
    emit_output "routing-header" "$ROUTING_HEADER"
    emit_output "routing-header-json" "$ROUTING_HEADER_JSON"
    emit_output "preview-url" "$PREVIEW_URL"

    if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]] && { [[ -w "$GITHUB_STEP_SUMMARY" ]] || [[ -w "$(dirname "$GITHUB_STEP_SUMMARY")" ]]; }; then
      {
        echo "### 🚀 Diverge Preview Environment: \`${PREVIEW_NAME}\`"
        echo ""
        echo "| Field | Value |"
        echo "| :--- | :--- |"
        echo "| **Preview Name** | \`${PREVIEW_NAME}\` |"
        echo "| **Preview URL** | ${PREVIEW_URL:-N/A} |"
        echo "| **Routing Header** | \`${ROUTING_HEADER}\` |"
        echo "| **Changed Services** | \`${SERVICES_JSON}\` |"
        echo ""
      } >> "$GITHUB_STEP_SUMMARY"
    fi

    if [[ "$INPUT_VERIFY" == "true" && "$INPUT_DRY_RUN" != "true" ]]; then
      run_verify "$PREVIEW_NAME"
    fi

    if [[ "$INPUT_REPLAY" == "true" && "$INPUT_DRY_RUN" != "true" ]]; then
      run_replay "$PREVIEW_NAME"
    fi
    ;;

  verify)
    PREVIEW_NAME="$(resolve_preview_name)"
    run_verify "$PREVIEW_NAME"
    emit_output "preview-name" "$PREVIEW_NAME"
    ;;

  replay)
    PREVIEW_NAME="$(resolve_preview_name)"
    run_replay "$PREVIEW_NAME"
    emit_output "preview-name" "$PREVIEW_NAME"
    ;;

  teardown)
    PREVIEW_NAME="$(resolve_preview_name)"
    "$KUBECTL_BIN" delete previewgroup "$PREVIEW_NAME" -n "$INPUT_NAMESPACE" --ignore-not-found
    "$KUBECTL_BIN" delete environment "$PREVIEW_NAME" -n "$INPUT_NAMESPACE" --ignore-not-found
    emit_output "preview-name" "$PREVIEW_NAME"
    ;;

  *)
    echo "Unsupported INPUT_ACTION: ${INPUT_ACTION} (expected validate, diff, deploy, verify, replay, or teardown)" >&2
    exit 1
    ;;
esac

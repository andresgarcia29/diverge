# Model Context Protocol (MCP) Server Guide

The Diverge CLI includes a built-in Model Context Protocol (MCP) server that exposes Diverge preview environment management capabilities directly to AI assistants and IDEs (such as Claude Desktop, Claude Code, Cursor, and VS Code).

Using the MCP server, AI assistants can query active environments, provision new ephemeral previews, extend TTLs, monitor deployment readiness, and diagnose failure logs directly within your development workflow.

---

## 1. Overview

The `diverge mcp` command runs an MCP server over standard input/output (`stdio`) adhering to the [Model Context Protocol](https://modelcontextprotocol.io/) specification.

```text
+-------------------------------------------------------------+
|               AI Assistant / Agent / IDE                   |
|          (Claude Code, Claude Desktop, Cursor, VS Code)     |
+-------------------------------------------------------------+
                              |
                              | MCP (JSON-RPC over stdio)
                              v
+-------------------------------------------------------------+
|                      diverge mcp                            |
|             (stdio MCP Server / proto2mcp)                  |
+-------------------------------------------------------------+
                              |
                              | ConnectRPC (HTTP/1.1 / HTTP/2)
                              v
+-------------------------------------------------------------+
|                    diverge-server                           |
|             (Kubernetes API Facade / CRDs)                  |
+-------------------------------------------------------------+
```

### Tool Naming Convention

All MCP tools are named using the `diverge_<snake_case_method>` pattern derived from Diverge's protobuf service definitions:
- RPC `EnvironmentService.CreateEnvironment` &rarr; Tool `diverge_create_environment`
- RPC `EnvironmentService.GetEnvironment` &rarr; Tool `diverge_get_environment`
- RPC `PreviewGroupService.ListPreviewGroups` &rarr; Tool `diverge_list_preview_groups`

### Security Scoping

The MCP server enforces strict security boundaries:
- **Exposed Services**: Only `EnvironmentService` and `PreviewGroupService` operations are exposed.
- **Excluded Services**: Sensitive internal services such as `AuthService`, `ClusterService`, and `TunnelService` are **never** exposed to AI agents over MCP.
- **Safe by Default**: Destructive operations (`delete`) are disabled by default and require explicit user opt-in via `--allow-destructive`.

### Streaming RPC Handling

Standard MCP tool calls operate using request/response semantics. Streaming RPCs defined on the API server (`WatchEnvironments`, `StreamLogs`, `WatchPreviewGroups`) are automatically skipped during standard tool generation.

To provide streaming-like functionality without unbounded context consumption, Diverge registers dedicated non-streaming helper tools:
- `diverge_wait_for_ready`: Blocks until an environment reaches `Ready` or `Failed`/`Error` phase (up to 5 minutes), eliminating noisy polling loops.
- `diverge_fetch_errors`: Fetches recent `ERROR` and `FATAL` log lines from environment pods, protecting agent context windows from log spam.

---

## 2. Prerequisites

Before connecting your AI editor to the Diverge MCP server, ensure you have:

1. **Diverge CLI** installed (`diverge version`):
   ```bash
   curl -fsSL https://divergedev.com/install.sh -o install.sh && sh install.sh
   ```
2. **Access to a Diverge API Server** (`diverge-server`):
   - **In-cluster / Remote**: An accessible ConnectRPC endpoint (e.g., `https://diverge.example.com` or via ingress).
   - **Local Port-Forward**:
     ```bash
     kubectl port-forward -n diverge-system svc/diverge-server 8080:8443
     ```

---

## 3. Quick Start

Run the MCP server directly from your terminal to verify connectivity:

```bash
diverge mcp --server-url http://localhost:8080
```

You can also specify the server URL using the `DIVERGE_SERVER_URL` environment variable:

```bash
export DIVERGE_SERVER_URL="http://localhost:8080"
diverge mcp
```

The process will listen on `stdio` for JSON-RPC messages from MCP clients.

---

## 4. Tool Categories

The MCP server provides the following tools grouped by service:

### EnvironmentService Tools

| Tool Name | Description |
| :--- | :--- |
| `diverge_create_environment` | Provision a new preview environment with source branch, routing rules, and database configuration. |
| `diverge_get_environment` | Fetch details, conditions, URLs, and phase status of a specific preview environment. |
| `diverge_list_environments` | List environments with optional filters (namespace, branch, phase, label selector) and pagination. |
| `diverge_update_environment` | Update an existing environment spec or update mask. |
| `diverge_delete_environment` | Delete an environment and clean up its resources (*requires `--allow-destructive`*). |
| `diverge_extend_ttl` | Extend the remaining Time-To-Live (TTL) for an active preview environment. |
| `diverge_list_hook_jobs` | List database migration and post-deploy lifecycle hook Jobs for an environment. |
| `diverge_retry_hook` | Re-trigger a failed lifecycle hook Job (e.g., migration or post-deploy task). |

### PreviewGroupService Tools

| Tool Name | Description |
| :--- | :--- |
| `diverge_create_preview_group` | Create a multi-service PreviewGroup CR managing several microservices for an MR/PR. |
| `diverge_get_preview_group` | Retrieve status, member services, endpoints, and conditions for a PreviewGroup. |
| `diverge_list_preview_groups` | List PreviewGroups across namespaces with label selectors and pagination. |
| `diverge_update_preview_group` | Update configuration or service definitions for a PreviewGroup. |
| `diverge_delete_preview_group` | Tear down a PreviewGroup and its child environments (*requires `--allow-destructive`*). |

### Built-in Helper & Diagnostic Tools

| Tool Name | Description |
| :--- | :--- |
| `diverge_wait_for_ready` | Block execution until a target environment reaches `Ready` or `Failed` phase (max 5 minutes). |
| `diverge_fetch_errors` | Retrieve the last error-level (`ERROR`, `FATAL`, `panic`) log lines from an environment to diagnose failures. |
| `diverge_doctor` | Run cluster and environment health checks (GatewayClass, CRDs, RBAC, database connectivity). |
| `diverge_verify` | Run the unified verification suite (Doctor Kubernetes workload diagnostics, routed HTTP readiness status check, optional latency threshold check, and optional baseline status parity). |
| `diverge_loadtest` | Run a bounded HTTP/gRPC load test against a preview environment and return latency/error percentiles. |
| `diverge_replay` | Replay a recorded HTTP, ConnectRPC, or Protobuf/gRPC traffic trace (`.jsonl`, `.har`, `.json`, or inline `requests`) against baseline and a preview environment, returning status parity, p50/p95 latency, JSON schema diffs, and Protobuf wire/descriptor contract diffs. Supports stateful multi-step variable chaining (`extract` rules and `{{var}}` interpolation with isolated baseline/preview variable stores and `sequential` execution), host filtering (`filter_hosts`), header sanitization (`strip_headers`, `strip_auth`), query parameter redaction (`redact_query_params`), and recursive JSON request body field redaction (`redact_body_fields`), and automatically strips recorded `diverge_env` sticky-routing cookies from both baseline and preview requests. **Warning:** `strip_auth` defaults to `false`, so recorded `Authorization` headers, non-routing `Cookie` headers, and unredacted request bodies are forwarded to both baseline and preview unless sanitized—always sanitize traces beforehand (`diverge replay sanitize`) or enable `strip_auth`, `strip_headers`, `redact_query_params`, and `redact_body_fields` before replay. |
| `diverge_visual_test` | Compare baseline and preview PNG screenshots (`baseline_path`/`preview_path` or batch `baseline_dir`/`preview_dir`) and return pixel-diff pass/fail metrics (writing diff PNGs when `diff_output_path` is set and diff data exists). |

### Autonomous AgentTask Tools

| Tool Name | Description |
| :--- | :--- |
| `diverge_create_task` | Spawn a new sandboxed `AgentTask` with repository, branch, model capabilities, and USD/token/iteration budgets. |
| `diverge_get_task` | Retrieve the current phase (`Pending`, `Provisioning`, `Active`, `Pausing`, `Paused`, `Completed`, `Failed`), iteration count, token usage, and sandbox pod status. |
| `diverge_list_tasks` | List `AgentTask` resources in a namespace with optional phase filtering. |
| `diverge_pause_task` | Suspend an active `AgentTask` (`spec.suspended: true`) so the controller transitions it to `Paused` and revokes its Macaroon capability token upon reconciliation. |
| `diverge_resume_task` | Resume a paused `AgentTask` and mint a fresh Macaroon capability token. |
| `diverge_guide_task` | Send steering instructions or updated budget/iteration limits to an `AgentTask` and resume execution. |
| `diverge_delete_task` | Delete an `AgentTask` and tear down its sandbox (*requires `--allow-destructive`*). |

---

## 5. Editor Setup

Configure your AI assistant or editor to launch `diverge mcp` as a stdio tool server.

### Automated 1-Command Setup (`diverge mcp setup`)

Run `diverge mcp setup` in your repository root to automatically write or merge the `diverge` MCP server entry into **Cursor** (`.cursor/mcp.json`), **Claude Code** (`.mcp.json`), and **Windsurf Cascade** (`~/.codeium/windsurf/mcp_config.json`) without clobbering existing MCP servers:

```bash
# Configure all supported AI editors (Cursor, Claude Code, Windsurf)
diverge mcp setup

# Or target a specific client with a custom server URL
diverge mcp setup --client cursor --server-url http://localhost:8080
```

### Claude Code / Claude Desktop

Add the server configuration to `.mcp.json` (workspace) or `~/.claude/claude_desktop_config.json` (or `~/.claude.json`):

```json
{
  "mcpServers": {
    "diverge": {
      "command": "diverge",
      "args": ["mcp", "--server-url", "http://localhost:8080"]
    }
  }
}
```

### Cursor

Create or edit `.cursor/mcp.json` in your workspace root or global Cursor settings:

```json
{
  "mcpServers": {
    "diverge": {
      "command": "diverge",
      "args": ["mcp", "--server-url", "http://localhost:8080"]
    }
  }
}
```

### VS Code

Add the server entry to `.vscode/mcp.json` in your project workspace:

```json
{
  "servers": {
    "diverge": {
      "type": "stdio",
      "command": "diverge",
      "args": ["mcp", "--server-url", "http://localhost:8080"]
    }
  }
}
```

> [!TIP]
> If your Diverge server is running behind a custom domain, set `DIVERGE_SERVER_URL` as an environment variable in your MCP configuration:
> ```json
> {
>   "mcpServers": {
>     "diverge": {
>       "command": "diverge",
>       "args": ["mcp"],
>       "env": {
>         "DIVERGE_SERVER_URL": "https://diverge.internal.example.com"
>       }
>     }
>   }
> }
> ```

---

## 6. Destructive Operations

By default, the CLI `diverge mcp` `stdio` server operates in **safe mode**:
- Destructive tools (`diverge_delete_environment`, `diverge_delete_preview_group`, and `diverge_delete_task`) are **omitted** from the CLI's registered tool registry.
- AI assistants connected over `stdio` cannot accidentally delete active preview environments, tear down running `PreviewGroup`s, or delete running `AgentTask`s unless `--allow-destructive` is set.

### Enabling Destructive Tools on the CLI

To allow your AI assistant to use `diverge_delete_environment`, `diverge_delete_preview_group`, and `diverge_delete_task` over `diverge mcp`, append the `--allow-destructive` flag:

```json
{
  "mcpServers": {
    "diverge": {
      "command": "diverge",
      "args": [
        "mcp",
        "--server-url", "http://localhost:8080",
        "--allow-destructive"
      ]
    }
  }
}
```

> [!WARNING]
> Enabling `--allow-destructive` allows the AI agent to initiate irreversible deletions and cleanup of Kubernetes preview namespaces, databases, and `AgentTask` sandboxes.

---

## 7. Flags Reference

### `diverge mcp` Command Flags

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `--server-url` | `string` | `""` | Diverge API server URL. If empty, falls back to `DIVERGE_SERVER_URL` environment variable. |
| `--allow-destructive` | `bool` | `false` | Enable destructive tools (`diverge_delete_environment`, `diverge_delete_preview_group`, and `diverge_delete_task`). |

### Global Flags

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `--kubeconfig` | `string` | `~/.kube/config` | Path to the Kubernetes kubeconfig file. |
| `-n`, `--namespace` | `string` | `""` | Kubernetes namespace (defaults to active kubeconfig context). |
| `--context` | `string` | `""` | Kubernetes context name. |
| `--no-color` | `bool` | `false` | Disable ANSI color formatting. |

### Environment Variables

| Variable | Description |
| :--- | :--- |
| `DIVERGE_SERVER_URL` | URL of the Diverge API server (used when `--server-url` is omitted). |

---

## 8. Examples

Once configured, you can interact with Diverge naturally through your AI assistant:

### Inspecting Environments
- *"List all preview environments in the `staging` namespace"*
- *"Check the phase and status for environment `preview-mr-42`"*
- *"Show conditions and ingress endpoints for active environments"*

### Creating Previews
- *"Create a preview environment for my feature branch"*
- *"Provision a PreviewGroup for MR 88 containing the `payments-api` service"*

### Diagnosing Issues & Lifecycle
- *"Extend the TTL of environment `preview-mr-42` by 2 hours"*
- *"Wait for environment `preview-mr-42` to become ready, and if it fails, show me the error logs"*
- *"List all hook jobs for environment `preview-mr-42` and retry the database migration if failed"*
- *"Fetch the recent error logs for `payments-api` in the preview environment"*
- *"Run `diverge_visual_test` comparing `/tmp/baseline` and `/tmp/preview` screenshots"*

---

## 9. Official AI Coding Agent Plugin, Skill & Cursor Rules

Diverge ships ready-to-use agent instructions and an official **Google Antigravity Plugin** so Antigravity, Claude Code, Cursor, Windsurf, and Copilot automatically follow the **diff → deploy → wait → verify → diagnose** preview loop:

- **Google Antigravity Plugin (`plugins/diverge/`)**: Bundles `plugin.json`, `mcp_config.json` (`diverge-mcp`), `skills/diverge/SKILL.md`, the `diverge-operator` specialist subagent (`agents/diverge-operator.md`), and `rules/AGENTS.md`. Automatically discovered in workspaces via `.agents/plugins.json`, or installable globally via `cp -R plugins/diverge ~/.gemini/config/plugins/diverge`.
- **Antigravity IDE & VS Code Extension (`editors/vscode/`)**: Provides the Diverge sidebar (`Environments`, `Dev Session Leases`, `Verification Suite`), 1-click Magic Link launcher (`diverge.openMagicLink`), and baseline diff (`diverge.compareBaseline`) for Antigravity IDE, VS Code, and Cursor.
- **Standalone Agent Skill (`skills/diverge/SKILL.md`)**: Install into `~/.claude/skills/diverge/SKILL.md` or `.agents/skills/diverge/SKILL.md`.
- **Cursor Rules (`.cursor/rules/diverge.mdc`)**: Automatically loaded by Cursor when working in repositories configured with `.diverge.yaml`.


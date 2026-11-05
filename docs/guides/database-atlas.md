# Database Schema Management with Ariga Atlas

Diverge provides first-class support for [Ariga Atlas](https://atlasgo.io) to manage database schemas across ephemeral preview environments. With automated local directory bundling, isolated PostgreSQL schema tenants, and lifecycle deployment gating, developers can test database migrations and schema changes with zero operational friction.

---

## Key Features

- **Zero-Friction Local Bundling**: Point Diverge at your local `./migrations` directory or `./schema.sql` file. The CLI bundles, content-hashes, and securely uploads files into immutable Kubernetes ConfigMaps.
- **Dual Execution Engines**:
  - **Job Engine (`engine: job`)**: Standalone Kubernetes Job running the official `arigaio/atlas` container. No CRDs or operator installation required.
  - **Operator Engine (`engine: operator`)**: Native integration with the Kubernetes [Atlas Operator](https://atlasgo.io/integrations/kubernetes/operator) via `AtlasMigration` and `AtlasSchema` custom resources.
- **Dual Schema Paradigms**:
  - **Versioned Migrations (`mode: versioned`)**: Apply ordered SQL migration scripts (`atlas migrate apply`).
  - **Declarative Schemas (`mode: declarative`)**: Align database state with a target schema definition (`atlas schema apply`).
- **Strict Lifecycle Gating**: With `blocking: true` (default), environments enter the `Migrating` phase with a pulsing status badge. Workload application pods are scheduled **only after** migrations report `Succeeded`. If migrations fail, the environment transitions to `Failed` and halts deployment to protect against broken database states.
- **Ephemeral Isolation & Clean Teardown**: Each preview environment operates in its own isolated PostgreSQL schema (`search_path=preview_<env>,public`) with automatic `public` schema write-protection (`REVOKE CREATE ON SCHEMA public`, `REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public`) and optional `spec.database.seedSource` cloning (`public`, `schema:<name>`, or `configmap:<name>[/<key>]`) that clones tables, foreign keys, sequences, and views in topological order. When `spec.database.connectionRef` is omitted in `schema` mode, the controller auto-provisions an owned Secret named `diverge-db-env-<env>-<hash8>` containing `DATABASE_URL` (scoped to `diverge_preview_<schema>` with `search_path=preview_<env>,public`) and `DIVERGE_PREVIEW_SCHEMA` (`preview_<env>`), which `ServiceConfigFetcher` automatically injects into preview pods and Atlas migration jobs. Deleting the preview environment drops the entire schema cascade, deleting all tables and Atlas migration tracking records cleanly.

---

## Architecture Overview

```
┌────────────────────────────────────────────────────────────────────────┐
│                          DIVERGE CLI                                   │
│  diverge create / diverge preview create                               │
│                                                                        │
│  1. Resolves .diverge.yaml (dir: ./migrations or schema: ./schema.sql) │
│     or CLI flags (--atlas-dir, --atlas-schema, --atlas-mode)           │
│  2. Validates security boundary (symlinks, path traversal, whitelist)  │
│  3. Bundles local files into ConfigMap: atlas-<env>-mig-<hash8>        │
│  4. Injects ConfigMap into Environment.spec.database.atlas             │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │ Creates ConfigMap & Environment CR
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│                     DIVERGE CONTROLLER                                 │
│                                                                        │
│  1. Provisions per-preview PostgreSQL schema (preview_<env>) &         │
│     auto-creates Secret diverge-db-env-<env>-<hash8>                   │
│     (DATABASE_URL + DIVERGE_PREVIEW_SCHEMA) when connectionRef omitted │
│  2. Attaches OwnerReference to ConfigMap & Secret for auto-cleanup     │
│  3. Dispatches Atlas Job / CR with DATABASE_URL (search_path set)      │
│  4. Lifecycle Gating: Sets Phase = "Migrating", MigrationStatus = "Running"│
│  5. Requeues and WAITS for Job Completion before Deploying Workloads   │
│  6. Sets Phase = "Running", MigrationStatus = "Succeeded"              │
│  7. Teardown: DROP SCHEMA CASCADE drops all tables + atlas revisions   │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │ Status Streaming
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│                     DIVERGE WEB DASHBOARD                              │
│                                                                        │
│  - StatusBadge: Renders pulsing amber "Migrating" badge                │
│  - Environment overview: Displays migration hook status and messages   │
└────────────────────────────────────────────────────────────────────────┘
```

---

## Quickstart

### 1. Versioned Migrations from Local Directory

Configure `.diverge.yaml` in your project root:

```yaml
version: 1
database:
  mode: schema
  connection_ref: database-credentials
  atlas:
    mode: versioned
    engine: job
    dir: ./migrations
    blocking: true
```

Create a preview environment:

```bash
diverge create --branch feature/user-profiles
```

The CLI will output:
```
📦 Bundled 4 migration files from migrations into ConfigMap atlas-feat-user-profiles-mig-a1b2c3d4 (12 KB)
🚀 Created Environment feat-user-profiles
```

### 2. Declarative Schema from File

```yaml
version: 1
database:
  mode: schema
  connection_ref: database-credentials
  atlas:
    mode: declarative
    engine: job
    schema: ./schema.sql
    policy:
      destructive: allow
```

Apply with the CLI:

```bash
diverge create --branch feature/new-schema
```

---

## CLI Flags

You can override or define Atlas settings on the command line for both `diverge create` and `diverge preview create`:

| Flag | Default | Description |
|------|---------|-------------|
| `--atlas-mode` | `versioned` | Migration mode: `versioned` or `declarative` |
| `--atlas-engine` | `job` | Execution engine: `job` (standalone K8s Job) or `operator` |
| `--atlas-dir` | `""` | Path to local directory containing `.sql` migrations and `atlas.sum` |
| `--atlas-schema` | `""` | Path to local declarative schema file (`schema.sql` or `schema.hcl`) |
| `--atlas-configmap` | `""` | Name of pre-existing Kubernetes ConfigMap to mount |
| `--atlas-image` | `arigaio/atlas:latest` | Custom container image for Atlas Job runner |
| `--atlas-blocking` | `true` | Wait for migrations to succeed before scheduling workloads |
| `--atlas-destructive` | `error` | Destructive policy: `error`, `warn`, or `allow` |
| `--atlas-args` | `""` | Additional comma-separated CLI flags for `atlas` |

### Example: Multi-Service Preview Group

```bash
diverge preview create \
  --name pr-128 \
  --atlas-mode versioned \
  --atlas-engine job \
  --atlas-dir ./db/migrations \
  --atlas-blocking=true \
  --service api=registry.example.com/api:v2 \
  --service web=registry.example.com/web:v2
```

---

## Security & Protection Guardrails

When bundling local directories or schema files, the Diverge CLI enforces strict defense-in-depth checks:

1. **Path Traversal Defenses**: All paths are resolved with `filepath.Clean` and evaluated against symlinks via `filepath.EvalSymlinks`. Any directory or symlink pointing outside the workspace repository root is rejected immediately.
2. **File Whitelist**: Only schema-relevant files are bundled (`.sql`, `.hcl`, `.sum`, `.json`, `.yaml`, `.yml`).
3. **Secret & Sensitive File Exclusion**: Files matching `.env*`, `*.key`, `*.pem`, `*.crt`, or residing in `.git/` are automatically excluded.
4. **Payload Ceiling**: A strict **800 KiB** ceiling protects against Kubernetes etcd object limits.
5. **Garbage Collection**: The controller attaches an `OwnerReference` to Diverge-managed ConfigMaps (`divergedev.com/managed-by: diverge`), ensuring Kubernetes automatically deletes them when the parent `Environment` is removed.

---

## Lifecycle Gating & Web Dashboard

When an environment reconciles with an Atlas migration:

1. **Phase: `Migrating`**: The environment status reflects `Phase: "Migrating"` and `MigrationStatus: "Running"`.
2. **Web UI Pulsing Badge**: The Diverge web dashboard renders a pulsing amber `Migrating` badge.
3. **Workload Protection**: The controller halts pod scheduling while migrations run, requeuing every 3 seconds.
4. **On Success**: Status updates to `MigrationStatus: "Succeeded"`, condition `MigrationReady: True` is emitted, and workload deployments proceed to `Running`.
5. **On Failure**: If migrations fail (e.g. invalid SQL syntax, lock timeout), status transitions to `Phase: "Failed"` with `MigrationStatus: "Failed"`, preventing broken applications from running against an unmigrated database.

---

## Framework Integration Recipes

### Prisma (Node.js / TypeScript)

Use Atlas to diff your Prisma schema against the baseline:

```bash
# Generate migration from Prisma schema
npx prisma migrate diff \
  --from-empty \
  --to-schema-datamodel prisma/schema.prisma \
  --script > migrations/$(date +%Y%m%d%H%M%S)_init.sql

# Generate atlas.sum
atlas migrate hash --dir file://migrations
```

In `.diverge.yaml`:
```yaml
database:
  atlas:
    mode: versioned
    dir: ./migrations
```

### Drizzle ORM

Generate SQL migrations using Drizzle Kit:

```bash
npx drizzle-kit generate
atlas migrate hash --dir file://drizzle
```

In `.diverge.yaml`:
```yaml
database:
  atlas:
    mode: versioned
    dir: ./drizzle
```

### SQLAlchemy / Alembic (Python)

If you have Alembic migrations, you can run them with a migration job or export your target declarative schema:

```bash
# Dump Alembic target schema to SQL
python -m myapp.dump_schema > schema.sql
```

In `.diverge.yaml`:
```yaml
database:
  atlas:
    mode: declarative
    schema: ./schema.sql
    policy:
      destructive: allow
```

---

## CloudNativePG (`cnpg` / `fresh` / `snapshot`) In-Cluster Branching

In addition to lightweight PostgreSQL schema isolation (`mode: schema`), Diverge includes a native **CloudNativePG** (`postgresql.cnpg.io/v1` `Cluster`) database provider (`internal/database/cnpg`) for full cluster-level isolation inside Kubernetes:

- **Fresh Cluster (`mode: fresh` or annotation `divergedev.com/database-provider: cnpg`)**: Provisions an ephemeral single-instance CloudNativePG `Cluster` (`diverge-pg-<env>`) bootstrapped via `initdb` (`database: app`, `owner: app`).
- **Cloned Snapshot Cluster (`mode: snapshot`)**: When `spec.database.seedSource` (or annotation `divergedev.com/cnpg-source-cluster` / `DIVERGE_CNPG_SOURCE_CLUSTER`) references an in-cluster baseline CNPG cluster, Diverge provisions `diverge-pg-<env>` using `pg_basebackup` streaming clone (with configurable `divergedev.com/cnpg-source-namespace` and `divergedev.com/cnpg-source-secret`), or via `recovery` when `divergedev.com/cnpg-bootstrap-mode: recovery` and an object-store backup path (`divergedev.com/cnpg-barman-destination-path`) are configured.
- **Automatic Secret Wiring & Teardown**: Once the CNPG `Cluster` reports healthy, Diverge reads the generated `<cluster>-app` Secret URI and injects `DATABASE_URL` and `DIVERGE_CNPG_CLUSTER` into `diverge-db-env-<env>`, and automatically deletes the `Cluster` when the preview `Environment` is torn down.


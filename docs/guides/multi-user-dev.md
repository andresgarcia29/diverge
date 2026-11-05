# Multi-User Dev & Conflict Detection

In teams sharing development or staging clusters, multiple engineers frequently develop microservices simultaneously. If two engineers run `diverge dev` on the same service concurrently (e.g. `order-api`), conflicting database migrations, shared cache mutations, or overlapping async message queues can cause collisions.

Diverge provides built-in **multi-user conflict detection** and distributed session tracking to give teams real-time visibility into who is working on what, prevent accidental collisions, and safely coordinate concurrent work.

```
┌──────────────────────────────────────────────────────────────┐
│                     Kubernetes Namespace                     │
│                                                              │
│  ConfigMap: diverge-dev-session-order-api                    │
│  Labels:                                                     │
│    divergedev.com/managed-by: diverge                        │
│    divergedev.com/service: order-api                         │
│    divergedev.com/developer: alice                           │
│  Data:                                                       │
│    session: {                                                │
│      "service": "order-api",                                 │
│      "developer": "alice",                                   │
│      "hostname": "alices-mac.local",                         │
│      "branch": "feat/checkout",                              │
│      "heartbeat": "2026-09-07T04:00:00Z"                     │
│    }                                                         │
└──────────────────────────────┬───────────────────────────────┘
                               │
            ┌──────────────────┴──────────────────┐
            │                                     │
      [Alice CLI]                           [Bob CLI]
   Running diverge dev                   Running diverge dev
   Renews heartbeat every 20s            Detects collision!
   Deletes ConfigMap on exit             Applies policy (warn/block/force)
```

---

## Conflict Resolution Policies

When you run `diverge dev --service <name>`, Diverge checks whether another developer currently holds an active development session for that service.

You can configure conflict resolution via CLI flags or team-wide defaults:

### 1. `warn` (Default)
Logs an informative, high-visibility warning banner detailing the active developer, their hostname, branch, session age, and last heartbeat, but proceeds with session startup:

```bash
diverge dev --service order-api --on-conflict=warn
```

Terminal output:
```text
┌────────────────────────────────────────────────────────────────────────┐
│ ⚠️  WARNING: Another developer is currently working on this service!    │
├────────────────────────────────────────────────────────────────────────┤
│  Service:        order-api                                             │
│  Active User:    alice                                                 │
│  Machine / Host: alices-macbook.local                                  │
│  Branch:         feat/checkout                                         │
│  Preview ID:     feat/checkout                                         │
│  Session Age:    12m                                                   │
│  Last Heartbeat: 8s ago                                                │
├────────────────────────────────────────────────────────────────────────┤
│ Policy: Warn only (--on-conflict=warn). Proceeding with session.       │
│ ⚠️  Note: Concurrent mutations may collide on shared baseline state.   │
└────────────────────────────────────────────────────────────────────────┘
```

### 2. `block` (Strict Team Locking)
Prevents collisions by exiting immediately if another developer holds an active session:

```bash
diverge dev --service order-api --on-conflict=block
```

Terminal output:
```text
┌────────────────────────────────────────────────────────────────────────┐
│ ❌ CONFLICT: Service is locked by another developer!                    │
├────────────────────────────────────────────────────────────────────────┤
│  Service:        order-api                                             │
│  Active User:    alice                                                 │
│  Machine / Host: alices-macbook.local                                  │
│  Branch:         feat/checkout                                         │
│  Preview ID:     feat/checkout                                         │
│  Session Age:    12m                                                   │
│  Last Heartbeat: 8s ago                                                │
├────────────────────────────────────────────────────────────────────────┤
│ Action: Dev session blocked.                                           │
│ To override and take over the lock, run with:                          │
│   diverge dev --service order-api --force                              │
└────────────────────────────────────────────────────────────────────────┘
Error: dev session blocked: conflict: service "order-api" is currently locked by developer "alice"
```

### 3. `force` / `allow` (Last-Writer-Wins)
Overrides any existing lock and takes over the session:

```bash
diverge dev --service order-api --force
```

---

## Configuring Team-Wide Defaults (`.diverge.yaml`)

Teams can enforce strict locking or warning policies across all engineers by committing default settings in `.diverge.yaml`:

```yaml
version: "1"

services:
  order-api:
    paths: ["services/order-api/**"]
    image:
      repository: ghcr.io/acme/order-api
  payments:
    paths: ["services/payments/**"]
    image:
      repository: ghcr.io/acme/payments

defaults:
  dev:
    on_conflict: block  # Enforce locking across the team
```

---

## Inspecting Active Dev Sessions

To see all services currently being actively developed across the cluster or namespace, run:

```bash
diverge dev sessions
```

*(Alias: `diverge dev list`)*

Output:
```text
SERVICE      DEVELOPER   BRANCH          HOST                   HEARTBEAT   STATUS
order-api    alice       feat/checkout   alices-macbook.local   12s ago     Active
payments     bob         fix/discounts   bobs-thinkpad          4s ago      Active
inventory    carol       chore/cleanup   carol-desktop          140s ago    Stale
```

---

## Heartbeats & Stale Session Reclamation

- **Heartbeat Loop**: The active CLI automatically renews its lease heartbeat every 20 seconds.
- **Automatic Teardown**: Exiting the CLI (or `Ctrl+C` / `SIGTERM`) safely deletes the session ConfigMap.
- **Stale Expiry**: If a laptop goes to sleep, closes unexpectedly, or loses network connection, sessions with no heartbeat for **> 90 seconds** are marked `Stale` and automatically reclaimed by subsequent developers without blocking.


# Multiple replicas

Comporta can run as several replicas that share every budget, rate limit, concurrency slot, pause and loop guard count. Postgres holds the durable journal, ledger, usage history and credentials; Redis holds the projection on which admissions are decided. A single instance keeps using SQLite and none of this applies to it.

## Why replicas needed shared stores

Before this release, limits lived in the memory of one process. Two processes with their own SQLite directories would each count only their own traffic, so two replicas would each grant the full budget: a daily budget of 10 USD became 20. That is why a replica count above 1 with SQLite still aborts at start:

```
SQLite storage does not support multiple replicas; configure redis and postgres
```

`deployment.replicas` is a declaration. A single instance keeps its directory lock, but it cannot detect a copy started by hand with another data directory; do not do that.

## What is shared and what is not

| Shared through the stores | Local to each replica |
|---|---|
| Budgets, hard caps, reservations, rate windows, concurrency | Response cache content (never leaves the process) |
| Pauses, user deletions, loop guard counts and episodes | Request records waiting to be materialized |
| App keys, admin token and panel sessions, authentication throttling | Telemetry counters (`comporta_requests_total` and others) |
| Usage history, events, usage cursors | The event bus feeding the local exporters |

Request and cost counters are per replica and can be summed. Cluster gauges (`comporta_cluster_mode`, `comporta_outbox_pending`, `comporta_outbox_oldest_seconds`) report the same state on every replica: aggregate them with `max`, never `sum`. Budget gauges are not exposed in distributed mode.

## Requirements

- Postgres 16 or later, reached with `sslmode=verify-full` unless it is on loopback. The suite runs on Postgres 17.12.
- Redis 7.2 or later, standalone, with `appendonly yes`, `appendfsync always` and `maxmemory-policy noeviction`. The gateway reads these at start and refuses to serve without them, because an evicted or lost reservation would reopen budget. TLS is mandatory unless Redis is on loopback. Sentinel and Cluster are not supported. The suite runs on Redis 7.4.12.
- Two shared secrets of exactly 32 random bytes each, the same on every replica: `hmac_key_file` makes user pseudonyms agree, `cursor_key_file` makes usage cursors valid on any replica. They must differ. Changing `hmac_key_file` makes the cluster refuse to start, because history would no longer match its users.

Redis and Postgres are single points of unavailability: the gateway survives the loss of a replica, not of a store. High availability of the stores is outside this release.

## Configuration

```yaml
deployment:
  mode: distributed
  replicas: 2
  cluster_id: gateway-prod        # stable, [a-z0-9-]{1,40}
  config_revision: 1              # raise it to roll out a change
  hmac_key_file: /run/secrets/identity/hmac.key
  cursor_key_file: /run/secrets/identity/cursor.key
storage:
  backend: postgres
  postgres:
    dsn_file: /run/secrets/postgres/dsn   # or dsn: ${POSTGRES_DSN}; never a literal
    max_connections: 10                   # per replica
  redis:
    address: redis.internal:6379
    username: comporta
    password_file: /run/secrets/redis/password
    tls: {enabled: true, ca_file: /run/secrets/redis/ca.crt}
```

[`examples/distributed.yaml`](../examples/distributed.yaml) is a complete file. Changing the mode, the backend or the stores requires a restart. `comporta config validate` checks the structure without reading secrets; `serve` resolves them and checks the connections, the durability settings and the shared identity before opening any listener.

## First start

One database and one Redis namespace belong to one cluster. Start from empty stores:

```sh
comporta storage migrate --config comporta.yaml        # optional: serve also migrates, under a lock
comporta admin reset-token --config comporta.yaml      # prints the admin token once
comporta keys create support-bot --config comporta.yaml
comporta serve --config comporta.yaml                  # on every replica
```

Migrations are applied once even when replicas start together, and a database with a newer or edited migration stops the start without changing anything. To keep schema privileges away from the runtime user, run `storage migrate` as a separate job with a privileged user and give the replicas a user without DDL rights.

`keys` commands write to Postgres in distributed mode and never touch `keys.json`. A key created or revoked on one replica takes effect on all of them within about one second. `admin reset-token` works while replicas run: the new token applies within a second and every panel session ends. The admin panel works on every replica with the shared session: each process follows a shared watermark of requests and events once per second, so a write on another replica refreshes open screens, and limit state, which lives in Postgres and Redis, is shown as not verifiable instead of a local copy.

## Configuration changes

Every replica reports its `config_revision`. A replica that starts with a higher revision activates it for the cluster; replicas still on the previous one become not ready and stop admitting until they get the new file. Two different files with the same revision are refused. To roll back, deploy the old content with a higher revision. Requests already admitted keep the rules they were admitted under.

## How the budget guarantee holds

Every admission takes a shared lock in Postgres, holds the app's lock, reads the app's state from Redis, decides with the same rules as a single instance and applies the reservation only if the app did not change meanwhile; then the journal and the reservation commit in Postgres. Nothing is sent upstream before that commit. A settlement commits in Postgres first and is projected in Redis afterwards; until then the reservation keeps holding the money, and the next admission of the app applies the pending settlement before deciding.

The overshoot guarantee is the one of a single instance, under the same conditions: `fail_closed`, healthy stores, and an upstream that reports no more usage than the estimate. A lost reply, a crash or a partition can hold more budget than needed for a while; none of them frees budget without proof.

## Failures

| Situation | Behavior |
|---|---|
| A replica dies | Its lease expires after 15 seconds. Another replica settles what it admitted, at the reserved value when no result was recorded, frees its slots and materializes its journal as recovered usage. Nothing is sent again. |
| A replica loses its lease | It stops admitting and exits; its work is recovered by another replica. |
| Redis unreachable, `fail_closed` app | HTTP 503 `limits_unavailable`, nothing sent. |
| Redis unreachable, `fail_open` app | The whole cluster switches to degraded mode: admissions are journaled in Postgres as degraded, without budget headers, and a `limit_state_unavailable` event is recorded. Budgets and hard caps can be exceeded in this mode. Pauses, deletions, keys and the authentication throttle keep working, since they live in Postgres. |
| Redis back, or restarted, or its data lost | The projection is rebuilt from the Postgres ledger in a new epoch under an exclusive lock; admissions wait or get 503 meanwhile. Degraded spend is applied before the cluster returns to normal and `limit_state_recovered` is recorded. Loop guard history only lived in Redis, so generations are held back with 503 for the longest similar window or cooldown, at least one minute. |
| Postgres unreachable | New admissions fail with 503 `usage_storage_unavailable` right away, because nothing is sent without a durable journal. Completions of requests already admitted wait in memory, up to 50,000 per replica, with their reservations held; a crash meanwhile is recovered at the reserved value. This is stricter than queueing new traffic in memory, on purpose. |
| Pause or resume | Applied under an exclusive lock: once the command returns, no replica admits a request of the paused app. Requests already admitted finish. |
| User deletion | Requests of the user are held back on every replica, each replica empties the response cache of the affected apps and flushes its usage queue, and only then is the history deleted. App totals stay. A failure after the history was deleted answers 503 `operation_pending` and is finished in the background; retrying is safe. |

`comporta storage status` prints the schema version, the cluster mode, the revision, the live replicas and the pending projections, without secrets. `comporta storage recover` rebuilds the projection on demand; it never deletes history or resets a balance.

## Kubernetes

The chart in [`deploy/helm`](../deploy/helm) installs at least two replicas against stores you operate:

```sh
helm install comporta ./deploy/helm -f examples/kubernetes/values.yaml
```

Create the Secrets first, as the comments in [`examples/kubernetes/values.yaml`](../examples/kubernetes/values.yaml) show; the chart only references them and never renders a secret value. They are mounted read-only as files, without `subPath`, so a rotation reaches the pods. Then run `storage migrate`, `admin reset-token` and `keys create` once, from a job or a workstation that reaches the stores; replicas without an admin token refuse to start and are restarted until it exists.

What the chart sets:

- `RollingUpdate` with `maxUnavailable: 0` and `maxSurge: 1`, a `PodDisruptionBudget` of one unavailable pod and topology spread across nodes.
- UID and GID 65532, `runAsNonRoot`, read-only root filesystem, no privilege escalation, every capability dropped, `RuntimeDefault` seccomp, no service account token. `/tmp` is a memory volume; there is no persistent volume.
- Separate `ClusterIP` Services for the proxy and the admin listener, and no Ingress. Reach the admin API with `kubectl port-forward` or from the peers in `networkPolicy.adminFrom`.
- A default-deny `NetworkPolicy` that opens the proxy to `networkPolicy.proxyFrom`, the admin listener to `networkPolicy.adminFrom`, DNS, and the egress you list. NetworkPolicy matches addresses, not domain names: list the CIDRs of your providers and keep them current, or send provider traffic through an egress gateway.
- Probes on the admin port with a fixed `Host`: `/healthz` for liveness, which never depends on the stores, and `/readyz` for readiness, which reports `not_ready` while the lease, the revision or the cluster state do not allow admissions. On shutdown a replica reports not ready first, keeps serving for five seconds, drains for up to ten and has 45 seconds in total.

## Moving an existing installation

There is no automatic migration of a SQLite history, `keys.json` or `hmac.key` into Postgres. A distributed cluster starts empty: its budgets do not continue the old ones. Switch at a moment when every budget you care about renews, or accept that the new cluster starts counting from zero; stop the old instance, create new keys and a new identity secret, and point the apps at the new gateway. A migration without downtime is not part of this release.

## Measured results

The suite runs on Linux in CI (job "Distributed") and on a Windows workstation with Docker Desktop:

- Two `serve` processes, 500 simultaneous generations against a 0.002 USD daily budget, several users, uneven split: no run spent more than the budget plus the largest reservation.
- A pause sent to one replica refused the next request on the other in about 40 ms.
- One admission plus its settlement on one app took about 31 ms on the workstation (tmpfs-backed containers). Admissions of one app are serialized by its lock, so the throughput of a single app is bounded by that latency; different apps proceed in parallel. These numbers are not those of a single instance and depend on the stores' latency and fsync cost.

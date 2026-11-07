# Comporta

Open-source LLM gateway, in development.

**Current state:** this repository delivers the configuration runtime, the app key lifecycle and the request proxy. `comporta serve` opens the proxy and admin listeners and forwards SDK traffic to the configured providers, with authentication, streaming, hot reload and a built-in fake provider.

**Not implemented yet:** limits, budgets, policies (including `max_tokens` caps and `allowed_models` enforcement on calls), metering and persistence, end-user identity handling, fallback, cache, loop guard, sensitive data detection, alerts, telemetry and the admin panel. Their settings are validated and made available **without taking effect**, so do not rely on them as protection. Usage is measured per request but not stored yet.

## Commands

```sh
comporta config validate [path]                    # structure and references, no secrets needed
comporta config validate [path] --resolve-secrets  # also reads the environment, .env and secret files
comporta keys create <app> [--config path]         # creates a key and prints it once
comporta keys rotate <app> [--config path]         # adds a key, keeping the others valid
comporta keys list [--config path]                 # lists keys without revealing them
comporta keys revoke <key-id> [--config path]      # revokes a key
comporta serve [--config path]                     # runs the proxy and admin listeners until interrupted
comporta serve --reset-user-history                # recovery from a lost hmac.key, see End users
comporta fake-provider [--port 9090] [--bind ip] [--scenario name]  # local imitation of both provider APIs
```

`config validate` exits 0 when the file is valid and 1 with a list of errors otherwise. Results go to stdout and diagnostics to stderr. The structural mode is meant for CI: it checks the format of secret references, but it does not prove that a secret exists or that a provider works.

The path comes from the positional argument or `--config`, and defaults to `comporta.yaml` in the working directory. Giving both with different files is an error.

## Getting started

```sh
go build -o comporta ./cmd/comporta
cp examples/comporta.yaml comporta.yaml
echo 'ANTHROPIC_API_KEY=your-key' > .env
./comporta keys create my-app
./comporta serve
```

Then point an existing SDK at the gateway. Nothing else in the application changes:

```python
client = anthropic.Anthropic(base_url="http://localhost:8080", api_key="cmp_...")
client = openai.OpenAI(base_url="http://localhost:8080/v1", api_key="cmp_...")
```

[`examples/comporta.yaml`](examples/comporta.yaml) is minimal and [`examples/comporta-full.yaml`](examples/comporta-full.yaml) shows every field. Prices in the examples are illustrative. The published JSON Schema, [`schema/comporta.schema.json`](schema/comporta.schema.json), gives editors autocomplete and validation, and documents each default. Relational rules, such as model references and hard caps, are enforced only by `config validate`.

## Configuration format

The file is one YAML document of at most 1,000,000 bytes. Unknown fields, duplicate keys, anchors, aliases, merge keys, explicit tags and `null` are rejected.

- **Timezone:** the default is the system timezone when it has an IANA name. Windows zone names are not IANA names, so on Windows set `timezone` in the file or `TZ` in the environment.
- **Money:** plain decimals with at most six decimal places, converted exactly. Scientific notation and quoted amounts are rejected.
- **Hard caps:** with `on_limit: downgrade`, an omitted dimension defaults to 120% of its budget, rounded up. With `on_limit: warn`, an omitted dimension has no hard ceiling.

## Secrets

Secrets are never written in the file. A literal value in any field named `api_key` makes startup and reload fail.

```yaml
api_key: ${ANTHROPIC_API_KEY}   # the reference must fill the whole value
api_key_file: /run/secrets/key  # or a file; use only one of the two
```

- Variables come from the process environment and from a `.env` next to the configuration file. The process environment wins, even when a variable is set to empty. A missing or empty variable is an error.
- `.env` is read once at startup. Changing it requires a restart.
- Relative secret paths are resolved from the configuration file's directory. Files may be at most 64 KiB, and one final line break is removed. Symlinks are followed.
- Alert channel URLs, signing secrets and tokens follow the same rules, with the fields `url`/`url_file`, `signing_secret`/`signing_secret_file` and `token`/`token_file`.

Logs, status and change events show secrets only as `[REDACTED]`. At startup, a warning is logged when the configuration file or data directory is readable by other OS users. Comporta never changes permissions by itself.

## Hot reload

Comporta watches the file's directory, so editors that save atomically keep working, on Windows too. A valid change is active within 2 seconds on a local disk, after a 500 ms debounce. A periodic check recovers events the operating system dropped, but this timing is not guaranteed on remote volumes.

```text
config reloaded: 3 apps, 2 changes (bot-discord.budget.usd_per_day 2.00 → 5.00)
config reload rejected: apps.bot-discord.on_limit: must be one of block, downgrade, warn (previous configuration still active)
```

- **Invalid edits** are rejected as a whole and the previous configuration keeps running.
- **Restart-only fields:** `data_dir` and the `bind` and `port` of both listeners. An edit that changes one of them is rejected entirely, including its other changes.
- **Deleted file:** the previous configuration stays active, a warning is logged every 60 seconds and the file is picked up again when it returns.
- **Secret rotation:** changing only a secret file does not trigger a reload. Save the configuration file, even unchanged, to reread secret files.
- **At startup** there is no previous configuration, so any failure aborts.

## App keys

Apps authenticate with keys that Comporta generates. A key is shown once, when it is created, and only its SHA-256 hash is stored, so a lost key cannot be recovered: revoke it and create another. [ADR 0001](docs/adr/0001-app-key-hashing.md) explains why plain SHA-256 is the right hash here.

```text
$ comporta keys create my-app
Key for my-app (shown only once): cmp_<43 base62 characters>
Store it in the app's environment. Key ID: k_<32 hex characters>

$ comporta keys list
KEY ID  APP     STATUS  CREATED AT            LAST USED AT
k_<id>  my-app  active  2026-11-06T12:00:00Z  never
```

These commands read the configuration without resolving secrets and write directly to the data directory, so they need no provider credentials and work with or without a running service. They exit 0 on success and 1 otherwise. An app name must be defined in the configuration.

- **Provisioning:** `serve` refuses to start without `keys.json` and never creates it, so run `keys create` first. A store that exists with no active key is valid, and every credential is rejected.
- **Rotation:** each app can have 2 active keys. `keys rotate` adds the second one. Move the app to it, then `keys revoke` the old one. Rotation never revokes anything by itself.
- **Revocation:** a running service applies it within 2 seconds on a local disk, without a restart. Operations already authenticated are not interrupted. Revoked records stay in the file, which holds at most 10,000 records, so that a later attempt reports the key as revoked.
- **Recovery:** while `keys.json` is missing, invalid or too open, authentication is blocked with 503 regardless of `fail_mode`, and resumes when the file is valid again. Comporta never recreates a damaged file: restore it from a backup, or remove it and create new keys.
- **Last used:** the time is saved every 5 seconds and on shutdown, so it is approximate and a crash can lose the latest update.

### Files

`keys.json`, `keys.lock`, `hmac.key`, `hmac.lock` and a data directory created by these commands are private to the user running Comporta: modes 0600 and 0700 on Linux and macOS, and a protected ACL limited to that user, SYSTEM and Administrators on Windows, which is equivalent in effect but is not POSIX 0600. An existing file with broader access, or a link in its place, is refused rather than fixed. An existing data directory is left as it is. Writes are atomic and coordinated by an operating system lock, which requires a local disk: network volumes are not supported.

### Authentication limits

After 20 failed attempts from one address within 60 seconds, that address gets 429 with `Retry-After` for 5 minutes, even with a valid key. This protection is held in memory per process and a restart clears it. It uses only the address of the connection and ignores `Forwarded`, `X-Forwarded-For` and `X-Real-IP`, so all clients behind one reverse proxy share a single counter and can block each other.

On Anthropic routes a key may come in `x-api-key` or as a bearer token, because SDK versions differ in which one they send for the same `api_key`. OpenAI routes take a bearer token only. Sending both, or repeating either, is refused.

## Proxy

The proxy listens on `127.0.0.1:8080` and the admin listener on `127.0.0.1:8081`. They are separate: no admin route exists on the proxy, and no conversation route on the admin listener. Both refuse a `Host` outside `localhost`, `127.0.0.1`, `[::1]` and `listeners.allowed_hosts` with 421, which protects against DNS rebinding. Forwarded headers are never trusted.

| Endpoint | Format | Notes |
|---|---|---|
| `POST /v1/messages` | Anthropic | Normal and streaming |
| `POST /v1/messages/count_tokens` | Anthropic | Advisory count, never treated as consumption |
| `POST /v1/chat/completions` | OpenAI, also used for Ollama | Normal and streaming |
| `GET /v1/models` | Chosen by the client | Local list, see below |

Every other path or method returns 404 `endpoint not allowed by Comporta`, without forwarding anything, so an app cannot use your provider key for files, batches or admin APIs. The admin listener has only `GET /healthz`, which answers `{"status":"ok"}` and says nothing about providers.

**What reaches the provider:** the body exactly as received, byte for byte, including unknown fields, with two changes. End-user fields are replaced by a pseudonym, as [End users](#end-users) describes. On OpenAI-format streaming, `stream_options.include_usage` is set to `true` so that usage can be measured, and if the client did not ask for it, the extra usage chunk is removed before delivery. Only `content-type`, `anthropic-version` and `anthropic-beta` are forwarded; every other client header is dropped, and the app key is replaced by the provider key. The destination comes only from the configuration. Redirects are never followed, proxy environment variables are ignored, and a remote provider name that resolves to a private, loopback or link-local address is refused. A base URL may end in `/v1` or not; the same path results.

**Errors** use the format of the route and carry a `comporta_code`. Provider error messages are never passed on, because they can echo prompts: status and category are preserved and the message is rebuilt. Inside a stream already started, the failure is an error event followed by closing the stream, so a truncated stream never looks like a success.

| `comporta_code` | Status | Meaning |
|---|---|---|
| `endpoint_not_allowed` | 404 | Route or method outside the list |
| `host_not_allowed` | 421 | `Host` not allowed |
| `invalid_request` | 400 | Invalid JSON, types, duplicate keys or query |
| `user_required`, `user_invalid`, `user_email_not_allowed`, `user_document_not_allowed` | 400 | End-user identifier missing or refused, see [End users](#end-users) |
| `model_unknown`, `provider_format_mismatch` | 400 | Model not declared, or declared in another API format |
| `unsupported_media_type` | 415 | Body is not uncompressed JSON |
| `request_too_large` | 413 | Body over `max_body_bytes` |
| `provider_unreachable`, `provider_redirect`, `invalid_upstream_response` | 502 | Provider could not be used |
| `provider_timeout` | 504 | A deadline expired |
| `provider_rate_limited`, `provider_overloaded`, `provider_error` | Provider's | Provider refused or failed |
| `internal_error` | 500 | A panic, limited to that request and logged without content |

**Limits:** request body 10 MB, response headers from the provider 60 s, non-streaming requests 300 s in total including reading the body, stream silence 60 s, all configurable under `listeners`. Fixed: 64 MB per non-streaming response, 10 MB per stream event, JSON nesting of 128 levels, 64 KiB of request headers. On shutdown, requests get 10 s to finish and are then canceled, so a long stream can end when the service stops.

**Duplicate JSON keys are refused** anywhere in the body, because the gateway and the provider could otherwise read different values. Each client connection carries one request, which is what lets a reload change the header timeout, so clients open a new connection per request. Timeouts and allowed hosts apply to new requests after a reload; requests in flight keep the configuration and identity they started with. Connections to providers use HTTP/1.1.

### Model catalog

`GET /v1/models` lists the app's `allowed_models` in the format of the client, without calling a provider, so it costs nothing and says nothing about availability. `created` and `created_at` hold fixed values because the configuration has no release dates. The Anthropic format supports `limit`, `after_id` and `before_id`. This list is only a listing: until policies exist, an app can still call any declared model.

### Usage and privacy

Each provider call produces one internal result with counters, timing, categories and the user's pseudonym, never content, raw user identifiers or raw errors. It is not stored anywhere yet. A counter the provider did not report stays unknown instead of becoming zero, and tokens are never estimated. When a client disconnects, the provider call is canceled within 1 s and only what was already observed is recorded, so the usage of an interrupted stream can be lower than what the provider bills.

Prompts and responses stay in memory for the duration of the request and are not logged, and no option enables logging them. A privacy test sends marked prompts and identifiers through the gateway, including error paths, and fails if a marker appears in logs, answers built by the gateway, results or generated files. It covers those destinations today; [`tests/privacy/coverage.json`](tests/privacy/coverage.json) lists the ones that do not exist yet (database, metrics, traces and alert payloads), which join the test when they are built.

### End users

To attribute requests to the people using your app, send an opaque ID. In most SDKs that is one line, and it is the only code change later per-user limits will need:

```python
client = anthropic.Anthropic(base_url="http://127.0.0.1:8080", api_key=key, default_headers={"X-Comporta-User": user.id})
client = openai.OpenAI(base_url="http://127.0.0.1:8080/v1", api_key=key, default_headers={"X-Comporta-User": user.id})
```

The ID is read from `X-Comporta-User`, then `metadata.user_id`, then `user`. The first one present is used, on every format, and a present but empty value is refused instead of falling through to the next. The header goes no further than the gateway.

**What is accepted:** 1 to 128 characters of `A-Z a-z 0-9 _ . : -`, compared exactly: no trimming and no change of case. IDs shaped like an email address, or a valid CPF or CNPJ (numeric or alphanumeric, with or without the usual mask, check digits verified), are refused. A long ID that merely contains such a number is fine, and an opaque ID that happens to form a valid document is refused too. Only these fields are checked: an ID your app copies into a message or into another metadata field is content, and stays as it is.

| `comporta_code` | Message |
|---|---|
| `user_required` | `this app requires a user ID (X-Comporta-User header)` |
| `user_invalid` | `user ID must be 1-128 characters of [A-Za-z0-9_.:-]` |
| `user_email_not_allowed` | `user ID looks like an email address; send an opaque ID instead` |
| `user_document_not_allowed` | `user ID looks like a CPF or CNPJ; send an opaque ID instead` |

**Without an ID:** an app with `require_user: true` gets `user_required`, read from the configuration in force when the request arrived. Otherwise the request counts as the app's anonymous user, kept apart from a user whose ID is literally `anonymous`. `GET /v1/models` never asks for a user.

**What the provider sees:** a pseudonym of 32 hexadecimal characters, HMAC-SHA256 truncated to 16 bytes, stable across restarts and app key rotation. The same person in two apps gets two different pseudonyms, so renaming an app also breaks that app's continuity. Every known identity field in the body is replaced, including ones that lost the precedence, and the native field is added when missing: `metadata.user_id` on `/v1/messages`, which makes a `metadata` that is not an object a 400, and `user` on `/v1/chat/completions`. `count_tokens` replaces what is present and adds nothing. The raw ID is never logged or stored.

**What this does not prove:** Comporta authenticates the app, not its users. The app decides which ID goes with which person and can send any ID it likes, so a pseudonym is only as trustworthy as the app that sent it.

### The identity key

`serve` loads `hmac.key` from the data directory before opening any port, and creates it on the first run. It holds 32 random bytes and follows the same [file protections](#files) as the app keys. A file of any other size or with broader access stops the service: Comporta never repairs or replaces it. There is no configuration for it, and changes to the file only apply after a restart.

**Back it up together with the data directory, and keep it private.** Anyone with the key and a list of candidate IDs can recognize pseudonyms. Replacing or losing the key breaks the continuity of per-user history, because the same user gets a new pseudonym, and there is no migration. To rotate it, stop the service and plan for that break. App keys are independent, so rotating them changes nothing here.

When the key is missing, Comporta only creates one if it can prove that nothing depends on the old one: the directory must hold nothing besides `keys.json`, `keys.lock`, `hmac.lock` and its own temporary files. Anything else stops the service with `hmac.key not found but usage data exists; restore the key or start with --reset-user-history`, even an unrelated file, because it cannot tell. Restore the key, or point `data_dir` at a new directory and move the app keys there.

`--reset-user-history` exists for that recovery, but Comporta does not store usage yet, so it has nothing it could safely reset: with other files present it refuses with `user history reset requires usage storage support; restore hmac.key or use a new data directory`. It never deletes files, and with a valid key in place it changes nothing. When usage storage arrives, a reset will remove what links data to users and keep each app's spending, so it cannot reopen a budget.

## Fake provider

`comporta fake-provider` imitates both APIs with synthetic, deterministic responses, for tests without keys or cost. It listens on `127.0.0.1:9090` by default. The scenario is chosen when it starts, and no request can change it: `success`, `rate-limit` (429), `overload` (529), `header-timeout`, `slow-stream` and `disconnect-midstream`.

```yaml
providers:
  fake:
    api_format: anthropic
    base_url: http://127.0.0.1:9090
    models: [fake-claude]
```

## Container

The image is distroless and runs as a non-root user. `compose.yaml` sets a read-only root filesystem, drops all capabilities, allows writes only to the `/data` volume and publishes both ports on `127.0.0.1` only. Inside the container the service binds `0.0.0.0`, as [`examples/comporta-container.yaml`](examples/comporta-container.yaml) shows.

```sh
export ANTHROPIC_API_KEY=your-key   # or put it in a .env file next to compose.yaml
docker compose build
docker compose run --rm comporta keys create my-app --config /etc/comporta/comporta.yaml
docker compose up -d
curl http://127.0.0.1:8081/healthz
```

App keys and `hmac.key` live in the volume and survive recreating the container, so back up the volume as a whole. `COMPORTA_CONFIG` selects another configuration file, which is mounted read-only. The image has no shell or curl, so check it from the host. Reaching the service from another machine means publishing the proxy port elsewhere and adding its name to `allowed_hosts`; never publish the admin port that way.

## Development

The repository uses [Task](https://taskfile.dev). `task --list` shows the available tasks, and `task check` runs formatting, static checks, tests, example validation and the vulnerability and secret scanners. The race detector needs a C toolchain: `task test-race`. `task test-privacy` runs the privacy test, `task test-sdk` runs the official Python SDKs at the versions pinned in `tests/sdk/requirements.txt`, and `task test-container` builds the image and verifies its restrictions; none of them needs provider credentials. A test that adds a prompt builds it with `internal/testkit`, or the marked `prompt` helper in the SDK suite, and new destinations for data go in `tests/privacy/coverage.json` in the same change.

# Comporta

Open-source LLM gateway, in development.

**Current state:** this repository delivers the configuration runtime and the app key lifecycle. `comporta serve` loads and validates the configuration and the key store, resolves secrets and applies saved changes without a restart, but it **opens no network ports and proxies no traffic yet**. Authentication exists as a tested internal module that nothing exposes over the network: a key cannot be used against a running service until the proxy lands. The proxy, limits, policies, metering, alerts, telemetry and the admin panel are not implemented, so settings for them are validated and made available without taking effect.

## Commands

```sh
comporta config validate [path]                    # structure and references, no secrets needed
comporta config validate [path] --resolve-secrets  # also reads the environment, .env and secret files
comporta keys create <app> [--config path]         # creates a key and prints it once
comporta keys rotate <app> [--config path]         # adds a key, keeping the others valid
comporta keys list [--config path]                 # lists keys without revealing them
comporta keys revoke <key-id> [--config path]      # revokes a key
comporta serve [--config path]                     # keeps configuration and keys active until interrupted
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

`keys.json`, `keys.lock` and a data directory created by these commands are private to the user running Comporta: modes 0600 and 0700 on Linux and macOS, and a protected ACL limited to that user, SYSTEM and Administrators on Windows, which is equivalent in effect but is not POSIX 0600. An existing file with broader access, or a link in its place, is refused rather than fixed. An existing data directory is left as it is. Writes are atomic and coordinated by an operating system lock, which requires a local disk: network volumes are not supported.

### Authentication limits

After 20 failed attempts from one address within 60 seconds, that address gets 429 with `Retry-After` for 5 minutes, even with a valid key. This protection is held in memory per process and a restart clears it. It uses only the address of the connection and ignores `Forwarded`, `X-Forwarded-For` and `X-Real-IP`, so all clients behind one reverse proxy share a single counter and can block each other.

## Development

The repository uses [Task](https://taskfile.dev). `task --list` shows the available tasks, and `task check` runs formatting, static checks, tests, example validation and the vulnerability and secret scanners. The race detector needs a C toolchain: `task test-race`.

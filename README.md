# Comporta

Open-source LLM gateway, in development.

**Current state:** this repository delivers the configuration runtime only. `comporta serve` loads and validates the configuration, resolves its secrets and applies saved changes without a restart, but it **opens no network ports and proxies no traffic yet**. The proxy, authentication, limits, policies, metering, alerts, telemetry and the admin panel are not implemented, so settings for them are validated and made available without taking effect.

## Commands

```sh
comporta config validate [path]                    # structure and references, no secrets needed
comporta config validate [path] --resolve-secrets  # also reads the environment, .env and secret files
comporta serve [--config path]                     # keeps the configuration active until interrupted
```

`config validate` exits 0 when the file is valid and 1 with a list of errors otherwise. Results go to stdout and diagnostics to stderr. The structural mode is meant for CI: it checks the format of secret references, but it does not prove that a secret exists or that a provider works.

The path comes from the positional argument or `--config`, and defaults to `comporta.yaml` in the working directory. Giving both with different files is an error.

## Getting started

```sh
go build -o comporta ./cmd/comporta
cp examples/comporta.yaml comporta.yaml
echo 'ANTHROPIC_API_KEY=your-key' > .env
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

## Development

The repository uses [Task](https://taskfile.dev). `task --list` shows the available tasks, and `task check` runs formatting, static checks, tests, example validation and the vulnerability and secret scanners. The race detector needs a C toolchain: `task test-race`.

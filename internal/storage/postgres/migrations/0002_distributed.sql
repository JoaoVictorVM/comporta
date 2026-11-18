CREATE TABLE cluster_state (
 cluster_id text PRIMARY KEY CHECK (cluster_id ~ '^[a-z0-9-]{1,40}$'),
 epoch text NOT NULL CHECK (epoch ~ '^[0-9a-f]{32}$'),
 mode text NOT NULL CHECK (mode IN ('normal','degraded','recovering')),
 config_revision bigint NOT NULL CHECK (config_revision > 0),
 config_fingerprint text NOT NULL CHECK (config_fingerprint ~ '^[0-9a-f]{64}$'),
 identity_fingerprint text NOT NULL CHECK (identity_fingerprint ~ '^[0-9a-f]{64}$'),
 redis_run_id text,
 control_revision bigint NOT NULL DEFAULT 0 CHECK (control_revision >= 0),
 degraded_since bigint,
 updated_at bigint NOT NULL
);

CREATE TABLE replica_leases (
 instance_id text PRIMARY KEY CHECK (instance_id ~ '^[0-9a-f]{32}$'),
 fence bigint NOT NULL CHECK (fence > 0),
 lease_until bigint NOT NULL,
 config_revision bigint NOT NULL CHECK (config_revision > 0),
 cache_ack_revision bigint NOT NULL DEFAULT 0 CHECK (cache_ack_revision >= 0),
 state text NOT NULL CHECK (state IN ('active','draining','fenced'))
);
CREATE INDEX ix_leases_expiry ON replica_leases (lease_until, state);
CREATE SEQUENCE replica_fence;

ALTER TABLE metering_journal ADD COLUMN owner_id text NOT NULL DEFAULT '' CHECK (owner_id = '' OR owner_id ~ '^[0-9a-f]{32}$');
ALTER TABLE metering_journal ADD COLUMN owner_fence bigint NOT NULL DEFAULT 0 CHECK (owner_fence >= 0);
CREATE INDEX ix_journal_owner ON metering_journal (owner_id, state);

ALTER TABLE request_operations ADD COLUMN owner_id text NOT NULL DEFAULT '' CHECK (owner_id = '' OR owner_id ~ '^[0-9a-f]{32}$');
ALTER TABLE request_operations ADD COLUMN owner_fence bigint NOT NULL DEFAULT 0 CHECK (owner_fence >= 0);
ALTER TABLE request_operations ADD COLUMN cluster_epoch text NOT NULL DEFAULT '';
ALTER TABLE request_operations ADD COLUMN config_revision bigint NOT NULL DEFAULT 0 CHECK (config_revision >= 0);

CREATE TABLE coordination_outbox (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 operation_id text NOT NULL CHECK (length(operation_id) BETWEEN 1 AND 80),
 transition text NOT NULL CHECK (transition IN ('settle','release','forget','pause')),
 app_name text NOT NULL DEFAULT '',
 payload jsonb NOT NULL,
 created_at bigint NOT NULL,
 delivered_at bigint,
 claimed_until bigint,
 claim_token text,
 UNIQUE (operation_id, transition)
);
CREATE INDEX ix_outbox_pending ON coordination_outbox (created_at, id) WHERE delivered_at IS NULL;
CREATE INDEX ix_outbox_app ON coordination_outbox (app_name) WHERE delivered_at IS NULL;

CREATE TABLE app_keys (
 key_id text PRIMARY KEY CHECK (length(key_id) BETWEEN 1 AND 64),
 app_name text NOT NULL CHECK (app_name ~ '^[a-z0-9-]{1,40}$'),
 digest bytea NOT NULL UNIQUE CHECK (length(digest) = 32),
 prefix text NOT NULL,
 created_at bigint NOT NULL,
 revoked_at bigint,
 last_used_at bigint
);
CREATE INDEX ix_keys_app ON app_keys (app_name, revoked_at);

CREATE TABLE admin_credentials (
 id integer PRIMARY KEY CHECK (id = 1),
 digest bytea NOT NULL CHECK (length(digest) = 32),
 epoch bigint NOT NULL CHECK (epoch > 0),
 updated_at bigint NOT NULL
);

CREATE TABLE admin_sessions (
 digest bytea PRIMARY KEY CHECK (length(digest) = 32),
 credential_epoch bigint NOT NULL,
 expires_at bigint NOT NULL
);
CREATE INDEX ix_sessions_expiry ON admin_sessions (expires_at);

CREATE TABLE auth_windows (
 surface text NOT NULL CHECK (surface IN ('proxy','admin')),
 origin_digest bytea NOT NULL CHECK (length(origin_digest) = 32),
 failures jsonb NOT NULL,
 blocked_until bigint,
 expires_at bigint NOT NULL,
 PRIMARY KEY (surface, origin_digest)
);
CREATE INDEX ix_auth_expiry ON auth_windows (expires_at);

CREATE TABLE deletion_jobs (
 job_id text PRIMARY KEY CHECK (job_id ~ '^[0-9a-f]{32}$'),
 phase text NOT NULL CHECK (phase IN ('preparing','committed','cleaning','done')),
 revision bigint NOT NULL CHECK (revision > 0),
 subjects jsonb,
 apps jsonb NOT NULL,
 created_at bigint NOT NULL,
 updated_at bigint NOT NULL,
 receipt jsonb,
 CHECK ((phase = 'done' AND subjects IS NULL AND receipt IS NOT NULL) OR (phase <> 'done' AND subjects IS NOT NULL))
);
CREATE INDEX ix_deletion_phase ON deletion_jobs (phase, updated_at);

CREATE TABLE cache_epochs (
 app_name text PRIMARY KEY CHECK (app_name ~ '^[a-z0-9-]{1,40}$'),
 revision bigint NOT NULL CHECK (revision >= 0),
 updated_at bigint NOT NULL
);

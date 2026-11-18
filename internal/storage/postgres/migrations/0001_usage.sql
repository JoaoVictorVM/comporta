CREATE TABLE requests (
 request_id text NOT NULL CHECK (request_id ~ '^[0-9a-f]{32}$'),
 attempt_index integer NOT NULL CHECK (attempt_index >= 0),
 started_at bigint NOT NULL,
 finished_at bigint NOT NULL CHECK (finished_at >= started_at),
 app_name text NOT NULL CHECK (app_name ~ '^[a-z0-9-]{1,40}$'),
 user_hash text, anonymous boolean,
 operation text NOT NULL CHECK (operation IN ('generation','count_tokens')),
 provider text, model_requested text, model_used text,
 upstream_status integer CHECK (upstream_status BETWEEN 100 AND 599),
 downstream_status integer CHECK (downstream_status BETWEEN 100 AND 599),
 outcome text NOT NULL CHECK (outcome IN ('ok','blocked','cached','fallback','downgraded','loop_cut')),
 error_class text, error_code text,
 input_tokens bigint CHECK (input_tokens >= 0), output_tokens bigint CHECK (output_tokens >= 0),
 cache_write_tokens bigint CHECK (cache_write_tokens >= 0), cache_read_tokens bigint CHECK (cache_read_tokens >= 0),
 raw_input_total bigint CHECK (raw_input_total >= 0), counted_input_tokens bigint CHECK (counted_input_tokens >= 0),
 cache_write_5m_tokens bigint CHECK (cache_write_5m_tokens >= 0), cache_write_1h_tokens bigint CHECK (cache_write_1h_tokens >= 0),
 cost_microusd bigint NOT NULL CHECK (cost_microusd >= 0),
 reserved_cost_microusd bigint NOT NULL CHECK (reserved_cost_microusd >= 0),
 usage_missing boolean NOT NULL, recovered boolean NOT NULL, estimate_exceeded boolean NOT NULL,
 request_count integer NOT NULL CHECK (request_count IN (0,1)),
 latency_us bigint NOT NULL CHECK (latency_us >= 0),
 upstream_ttfb_us bigint CHECK (upstream_ttfb_us >= 0), client_ttfb_us bigint CHECK (client_ttfb_us >= 0),
 usage_day text NOT NULL CHECK (usage_day ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$'),
 timezone text NOT NULL CHECK (length(timezone) BETWEEN 1 AND 64),
 price_quote_json jsonb NOT NULL,
 record_json jsonb NOT NULL,
 PRIMARY KEY (request_id, attempt_index),
 CHECK ((user_hash IS NULL AND anonymous IS NULL) OR (user_hash ~ '^[0-9a-f]{32}$' AND anonymous IS NOT NULL))
);
CREATE INDEX ix_requests_app_time ON requests (app_name, started_at, request_id, attempt_index);
CREATE INDEX ix_requests_user_time ON requests (app_name, user_hash, anonymous, started_at);
CREATE INDEX ix_requests_retention ON requests (finished_at);
CREATE UNIQUE INDEX ix_request_terminal ON requests (request_id) WHERE request_count = 1;

CREATE TABLE usage_daily (
 day text NOT NULL CHECK (day ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$'),
 timezone text NOT NULL,
 app_name text NOT NULL,
 scope text NOT NULL CHECK (scope IN ('app','user')),
 subject text NOT NULL,
 anonymous boolean NOT NULL,
 requests bigint NOT NULL CHECK (requests >= 0), attempts bigint NOT NULL CHECK (attempts >= 0),
 input_tokens bigint NOT NULL CHECK (input_tokens >= 0), output_tokens bigint NOT NULL CHECK (output_tokens >= 0),
 cache_write_tokens bigint NOT NULL CHECK (cache_write_tokens >= 0), cache_read_tokens bigint NOT NULL CHECK (cache_read_tokens >= 0),
 usage_missing_count bigint NOT NULL CHECK (usage_missing_count >= 0), recovered_count bigint NOT NULL CHECK (recovered_count >= 0),
 cost_microusd bigint NOT NULL CHECK (cost_microusd >= 0),
 PRIMARY KEY (day, timezone, app_name, scope, subject, anonymous),
 CHECK ((scope = 'app' AND subject = '' AND NOT anonymous) OR (scope = 'user' AND subject ~ '^[0-9a-f]{32}$'))
);
CREATE INDEX ix_daily_user ON usage_daily (app_name, scope, subject, day);
CREATE INDEX ix_daily_period ON usage_daily (scope, day, app_name);

CREATE TABLE metering_journal (
 request_id text NOT NULL CHECK (request_id ~ '^[0-9a-f]{32}$'),
 attempt_index integer NOT NULL CHECK (attempt_index >= 0),
 admitted_at bigint NOT NULL,
 state text NOT NULL CHECK (state IN ('admitted','completed')),
 metadata_json jsonb NOT NULL,
 quote_json jsonb NOT NULL,
 result_json jsonb,
 PRIMARY KEY (request_id, attempt_index),
 CHECK ((state = 'admitted' AND result_json IS NULL) OR (state = 'completed' AND result_json IS NOT NULL))
);
CREATE INDEX ix_journal_admitted ON metering_journal (admitted_at);

CREATE TABLE events (
 event_id text PRIMARY KEY CHECK (event_id ~ '^[0-9a-f]{32}$'),
 type text NOT NULL CHECK (type IN ('request_completed','blocked','cached','fallback','downgraded','warned','loop_cut','config_changed','config_reload_failed','paused','sensitive_data_detected','limit_state_unavailable','limit_state_recovered')),
 occurred_at bigint NOT NULL,
 request_id text CHECK (request_id IS NULL OR request_id ~ '^[0-9a-f]{32}$'),
 attempt_index integer CHECK (attempt_index >= 0),
 app_name text, user_hash text, anonymous boolean,
 payload_json jsonb NOT NULL
);
CREATE INDEX ix_events_time ON events (occurred_at, event_id);
CREATE INDEX ix_events_app_type ON events (app_name, type, occurred_at);
CREATE INDEX ix_events_user ON events (app_name, user_hash, anonymous);

CREATE TABLE config_changes (
 event_id text PRIMARY KEY REFERENCES events (event_id) ON DELETE CASCADE,
 generation bigint NOT NULL,
 result text NOT NULL CHECK (result IN ('initial','applied','unchanged','rejected')),
 changes_json jsonb NOT NULL
);

CREATE TABLE budget_state (
 app_name text NOT NULL, scope text NOT NULL CHECK (scope IN ('app','user')), subject text NOT NULL, anonymous boolean NOT NULL,
 dimension text NOT NULL CHECK (dimension IN ('usd_per_day','usd_per_month','tokens_per_day','tokens_per_month')),
 window_start bigint NOT NULL, window_end bigint NOT NULL CHECK (window_end > window_start),
 spent bigint NOT NULL DEFAULT 0 CHECK (spent >= 0), reserved bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0),
 updated_at bigint NOT NULL,
 PRIMARY KEY (app_name, scope, subject, anonymous, dimension, window_start, window_end),
 CHECK ((scope = 'app' AND subject = '' AND NOT anonymous) OR (scope = 'user' AND subject ~ '^[0-9a-f]{32}$'))
);
CREATE INDEX ix_budget_state_window ON budget_state (window_end, app_name);

CREATE TABLE runtime_state (
 namespace text NOT NULL CHECK (namespace IN ('pause_global','pause_app','admin','identity','limit_zone')),
 key text NOT NULL,
 value_json jsonb NOT NULL,
 updated_at bigint NOT NULL,
 PRIMARY KEY (namespace, key)
);

CREATE TABLE limit_reservations (
 request_id text NOT NULL CHECK (request_id ~ '^[0-9a-f]{32}$'),
 attempt_index integer NOT NULL CHECK (attempt_index >= 1),
 app_name text NOT NULL CHECK (app_name ~ '^[a-z0-9-]{1,40}$'),
 subject text NOT NULL CHECK (subject = '' OR subject ~ '^[0-9a-f]{32}$'),
 anonymous boolean NOT NULL,
 state text NOT NULL CHECK (state IN ('reserved','settled','released')),
 admitted_at bigint NOT NULL, finished_at bigint,
 quote_json jsonb NOT NULL,
 settlement_json jsonb,
 PRIMARY KEY (request_id, attempt_index),
 CHECK ((state = 'reserved' AND finished_at IS NULL AND settlement_json IS NULL) OR (state <> 'reserved' AND finished_at >= admitted_at AND settlement_json IS NOT NULL))
);
CREATE INDEX ix_limit_reservations_subject ON limit_reservations (app_name, subject, anonymous, state);
CREATE INDEX ix_limit_reservations_state ON limit_reservations (state);

CREATE TABLE limit_reservation_items (
 request_id text NOT NULL, attempt_index integer NOT NULL, item_index integer NOT NULL CHECK (item_index >= 0),
 scope text NOT NULL CHECK (scope IN ('app','user')), subject text NOT NULL, anonymous boolean NOT NULL,
 dimension text NOT NULL CHECK (dimension IN ('usd_per_day','usd_per_month','tokens_per_day','tokens_per_month')),
 window_start bigint NOT NULL, window_end bigint NOT NULL CHECK (window_end > window_start),
 timezone text NOT NULL CHECK (length(timezone) BETWEEN 1 AND 64),
 reserved bigint NOT NULL CHECK (reserved >= 0), settled bigint CHECK (settled IS NULL OR settled >= 0),
 PRIMARY KEY (request_id, attempt_index, item_index),
 FOREIGN KEY (request_id, attempt_index) REFERENCES limit_reservations (request_id, attempt_index) ON DELETE RESTRICT,
 CHECK ((scope = 'app' AND subject = '' AND NOT anonymous) OR (scope = 'user' AND subject ~ '^[0-9a-f]{32}$'))
);

CREATE TABLE limit_ledger (
 seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 request_id text NOT NULL CHECK (request_id ~ '^[0-9a-f]{32}$'),
 attempt_index integer NOT NULL CHECK (attempt_index >= 1),
 transition text NOT NULL CHECK (transition IN ('admit','settle','release')),
 occurred_at bigint NOT NULL,
 payload_json jsonb NOT NULL,
 UNIQUE (request_id, attempt_index, transition)
);

CREATE TABLE limit_rate_events (
 request_id text PRIMARY KEY CHECK (request_id ~ '^[0-9a-f]{32}$'),
 app_name text NOT NULL CHECK (app_name ~ '^[a-z0-9-]{1,40}$'),
 subject text NOT NULL CHECK (subject = '' OR subject ~ '^[0-9a-f]{32}$'),
 anonymous boolean NOT NULL,
 admitted_at bigint NOT NULL,
 estimated_tokens bigint NOT NULL CHECK (estimated_tokens >= 0),
 actual_tokens bigint CHECK (actual_tokens IS NULL OR actual_tokens >= 0),
 active boolean NOT NULL
);
CREATE INDEX ix_limit_rate_app ON limit_rate_events (app_name, admitted_at);
CREATE INDEX ix_limit_rate_user ON limit_rate_events (app_name, subject, anonymous, admitted_at);

CREATE TABLE limit_attempt_rate_events (
 request_id text NOT NULL CHECK (request_id ~ '^[0-9a-f]{32}$'),
 attempt_index integer NOT NULL CHECK (attempt_index BETWEEN 2 AND 5),
 app_name text NOT NULL CHECK (app_name ~ '^[a-z0-9-]{1,40}$'),
 subject text NOT NULL CHECK (subject = '' OR subject ~ '^[0-9a-f]{32}$'),
 anonymous boolean NOT NULL,
 admitted_at bigint NOT NULL,
 estimated_tokens bigint NOT NULL CHECK (estimated_tokens >= 0),
 actual_tokens bigint CHECK (actual_tokens IS NULL OR actual_tokens >= 0),
 active boolean NOT NULL,
 PRIMARY KEY (request_id, attempt_index)
);
CREATE INDEX ix_attempt_rate_app ON limit_attempt_rate_events (app_name, admitted_at);
CREATE INDEX ix_attempt_rate_user ON limit_attempt_rate_events (app_name, subject, anonymous, admitted_at);

CREATE TABLE limit_checkpoints (
 id integer PRIMARY KEY CHECK (id = 1),
 covered_seq bigint NOT NULL CHECK (covered_seq >= 0),
 created_at bigint NOT NULL,
 civil_watermark bigint NOT NULL,
 metadata_json jsonb NOT NULL
);

CREATE TABLE request_operations (
 request_id text PRIMARY KEY CHECK (request_id ~ '^[0-9a-f]{32}$'),
 app_name text NOT NULL CHECK (app_name ~ '^[a-z0-9-]{1,40}$'),
 subject text NOT NULL CHECK (subject = '' OR subject ~ '^[0-9a-f]{32}$'),
 anonymous boolean NOT NULL,
 generation bigint NOT NULL CHECK (generation >= 0),
 started_at bigint NOT NULL,
 state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','closed')),
 last_attempt_index integer NOT NULL CHECK (last_attempt_index BETWEEN 1 AND 5),
 terminal_attempt_index integer,
 finished_at bigint,
 terminal_status integer CHECK (terminal_status IS NULL OR terminal_status BETWEEN 100 AND 599),
 terminal_code text NOT NULL DEFAULT '' CHECK (terminal_code ~ '^[a-z0-9_]{0,64}$'),
 materialized boolean NOT NULL DEFAULT false,
 CHECK ((state = 'active' AND terminal_attempt_index IS NULL AND finished_at IS NULL AND NOT materialized) OR
        (state = 'closed' AND terminal_attempt_index BETWEEN 1 AND last_attempt_index AND finished_at >= started_at))
);
CREATE INDEX ix_operations_subject ON request_operations (app_name, subject, anonymous, state);
CREATE INDEX ix_operations_recovery ON request_operations (state, materialized);
CREATE INDEX ix_operations_retention ON request_operations (finished_at);

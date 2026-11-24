CREATE TABLE alert_inputs (
 event_id text PRIMARY KEY REFERENCES events (event_id) ON DELETE CASCADE,
 event_type text NOT NULL CHECK (event_type IN ('request_completed','blocked','fallback','loop_cut','config_changed','config_reload_failed','limit_state_unavailable')),
 enqueued_at bigint NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','claimed','processed')),
 claimed_until bigint,
 claim_token text CHECK (claim_token IS NULL OR claim_token ~ '^[0-9a-f]{32}$'),
 processed_at bigint,
 CHECK ((state = 'claimed') = (claim_token IS NOT NULL AND claimed_until IS NOT NULL)),
 CHECK ((state = 'processed') = (processed_at IS NOT NULL))
);
CREATE INDEX ix_alert_inputs_pending ON alert_inputs (state, enqueued_at, event_id);
CREATE INDEX ix_alert_inputs_lease ON alert_inputs (claimed_until);

CREATE TABLE alert_occurrences (
 occurrence_id text PRIMARY KEY CHECK (occurrence_id ~ '^[0-9a-f]{32}$'),
 dedupe_key text NOT NULL UNIQUE CHECK (octet_length(dedupe_key) BETWEEN 1 AND 512),
 trigger text NOT NULL CHECK (trigger IN ('budget_threshold','hard_cap','anomaly','loop_cut','fallback_burst','config_reload_failed','fail_open')),
 occurred_at bigint NOT NULL,
 app_name text CHECK (app_name IS NULL OR length(app_name) BETWEEN 1 AND 40),
 scope text NOT NULL CHECK (scope IN ('global','app','user')),
 user_hash text, anonymous boolean,
 dimension text CHECK (dimension IS NULL OR dimension IN ('usd_per_day','usd_per_month','tokens_per_day','tokens_per_month')),
 window_start bigint, window_end bigint,
 threshold_percent integer CHECK (threshold_percent IS NULL OR threshold_percent BETWEEN 1 AND 100),
 -- text, not jsonb: the generic webhook signs these exact bytes.
 payload_json text NOT NULL CHECK (octet_length(payload_json) <= 16384 AND payload_json::jsonb IS NOT NULL),
 created_at bigint NOT NULL,
 CHECK ((user_hash IS NULL AND anonymous IS NULL) OR (user_hash ~ '^[0-9a-f]{32}$' AND anonymous IS NOT NULL)),
 CHECK (window_start IS NULL OR window_end IS NULL OR window_end > window_start),
 CHECK ((trigger = 'config_reload_failed' AND scope = 'global' AND app_name IS NULL) OR (trigger <> 'config_reload_failed' AND app_name IS NOT NULL)),
 CHECK ((trigger = 'budget_threshold') = (threshold_percent IS NOT NULL)),
 CHECK (trigger NOT IN ('budget_threshold','hard_cap') OR (dimension IS NOT NULL AND window_end IS NOT NULL))
);
CREATE INDEX ix_alert_occurrences_trigger ON alert_occurrences (trigger, occurred_at);
CREATE INDEX ix_alert_occurrences_app ON alert_occurrences (app_name, occurred_at);
CREATE INDEX ix_alert_occurrences_window ON alert_occurrences (window_end);
CREATE INDEX ix_alert_occurrences_created ON alert_occurrences (created_at);

CREATE TABLE alert_deliveries (
 delivery_id text PRIMARY KEY CHECK (delivery_id ~ '^[0-9a-f]{32}$'),
 occurrence_id text NOT NULL REFERENCES alert_occurrences (occurrence_id) ON DELETE RESTRICT,
 channel_name text NOT NULL CHECK (length(channel_name) BETWEEN 1 AND 40),
 channel_type text NOT NULL CHECK (channel_type IN ('webhook','discord','slack','ntfy')),
 state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','claimed','delivered','failed','canceled')),
 attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 4),
 next_attempt_at bigint NOT NULL,
 claimed_until bigint,
 claim_token text CHECK (claim_token IS NULL OR claim_token ~ '^[0-9a-f]{32}$'),
 last_status integer CHECK (last_status IS NULL OR last_status BETWEEN 100 AND 599),
 last_error text CHECK (last_error IS NULL OR last_error IN ('success','timeout','network','rate_limited','server_error','client_error','invalid_response','channel_changed')),
 delivered_at bigint,
 finished_at bigint,
 created_at bigint NOT NULL,
 UNIQUE (occurrence_id, channel_name),
 CHECK ((state = 'claimed') = (claim_token IS NOT NULL AND claimed_until IS NOT NULL)),
 CHECK ((state = 'delivered') = (delivered_at IS NOT NULL)),
 CHECK ((state IN ('delivered','failed','canceled')) = (finished_at IS NOT NULL))
);
CREATE INDEX ix_alert_deliveries_due ON alert_deliveries (state, next_attempt_at, created_at);
CREATE INDEX ix_alert_deliveries_lease ON alert_deliveries (claimed_until);
CREATE INDEX ix_alert_deliveries_channel ON alert_deliveries (channel_name, created_at);
CREATE INDEX ix_alert_deliveries_finished ON alert_deliveries (finished_at);

CREATE TABLE usage_5m (
 bucket_start bigint NOT NULL CHECK (bucket_start % 300000000 = 0),
 app_name text NOT NULL CHECK (length(app_name) BETWEEN 1 AND 40),
 cost_microusd bigint NOT NULL DEFAULT 0 CHECK (cost_microusd >= 0),
 updated_at bigint NOT NULL,
 PRIMARY KEY (bucket_start, app_name)
);
CREATE INDEX ix_usage_5m_app ON usage_5m (app_name, bucket_start);
INSERT INTO usage_5m (bucket_start, app_name, cost_microusd, updated_at)
 SELECT (finished_at / 300000000) * 300000000, app_name, sum(cost_microusd), (extract(epoch FROM clock_timestamp()) * 1000000)::bigint
 FROM requests WHERE cost_microusd > 0 GROUP BY 1, 2;

CREATE FUNCTION comporta_alert_input() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO alert_inputs (event_id, event_type, enqueued_at, state)
 VALUES (NEW.event_id, NEW.type, (extract(epoch FROM clock_timestamp()) * 1000000)::bigint, 'pending')
 ON CONFLICT DO NOTHING;
 RETURN NULL;
END $$;
CREATE TRIGGER tr_alert_inputs AFTER INSERT ON events FOR EACH ROW
 WHEN (NEW.type IN ('request_completed','blocked','fallback','loop_cut','config_changed','config_reload_failed','limit_state_unavailable'))
 EXECUTE FUNCTION comporta_alert_input();

CREATE FUNCTION comporta_usage_5m() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO usage_5m (bucket_start, app_name, cost_microusd, updated_at)
 VALUES ((NEW.finished_at / 300000000) * 300000000, NEW.app_name, NEW.cost_microusd, (extract(epoch FROM clock_timestamp()) * 1000000)::bigint)
 ON CONFLICT (bucket_start, app_name) DO UPDATE SET cost_microusd = usage_5m.cost_microusd + excluded.cost_microusd, updated_at = excluded.updated_at;
 RETURN NULL;
END $$;
CREATE TRIGGER tr_usage_5m AFTER INSERT ON requests FOR EACH ROW
 WHEN (NEW.cost_microusd > 0)
 EXECUTE FUNCTION comporta_usage_5m();

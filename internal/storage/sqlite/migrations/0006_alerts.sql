CREATE TABLE alert_inputs (
 event_id TEXT PRIMARY KEY REFERENCES events(event_id) ON DELETE CASCADE,
 event_type TEXT NOT NULL CHECK(event_type IN('request_completed','blocked','fallback','loop_cut','config_changed','config_reload_failed','limit_state_unavailable')),
 enqueued_at INTEGER NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN('pending','claimed','processed')),
 claimed_until INTEGER,
 claim_token TEXT CHECK(claim_token IS NULL OR (length(claim_token)=32 AND claim_token NOT GLOB '*[^0-9a-f]*')),
 processed_at INTEGER,
 CHECK((state='claimed') = (claim_token IS NOT NULL AND claimed_until IS NOT NULL)),
 CHECK((state='processed') = (processed_at IS NOT NULL))
) STRICT;
CREATE INDEX ix_alert_inputs_pending ON alert_inputs(state,enqueued_at,event_id);
CREATE INDEX ix_alert_inputs_lease ON alert_inputs(claimed_until);
CREATE TABLE alert_occurrences (
 occurrence_id TEXT PRIMARY KEY CHECK(length(occurrence_id)=32 AND occurrence_id NOT GLOB '*[^0-9a-f]*'),
 dedupe_key TEXT NOT NULL UNIQUE CHECK(length(CAST(dedupe_key AS BLOB)) BETWEEN 1 AND 512),
 trigger TEXT NOT NULL CHECK(trigger IN('budget_threshold','hard_cap','anomaly','loop_cut','fallback_burst','config_reload_failed','fail_open')),
 occurred_at INTEGER NOT NULL,
 app_name TEXT CHECK(app_name IS NULL OR length(app_name) BETWEEN 1 AND 40),
 scope TEXT NOT NULL CHECK(scope IN('global','app','user')),
 user_hash TEXT, anonymous INTEGER CHECK(anonymous IN(0,1)),
 dimension TEXT CHECK(dimension IS NULL OR dimension IN('usd_per_day','usd_per_month','tokens_per_day','tokens_per_month')),
 window_start INTEGER, window_end INTEGER,
 threshold_percent INTEGER CHECK(threshold_percent IS NULL OR threshold_percent BETWEEN 1 AND 100),
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json) AND length(CAST(payload_json AS BLOB))<=16384),
 created_at INTEGER NOT NULL,
 CHECK((user_hash IS NULL AND anonymous IS NULL) OR (length(user_hash)=32 AND user_hash NOT GLOB '*[^0-9a-f]*' AND anonymous IS NOT NULL)),
 CHECK(window_start IS NULL OR window_end IS NULL OR window_end>window_start),
 CHECK((trigger IN('config_reload_failed') AND scope='global' AND app_name IS NULL) OR (trigger NOT IN('config_reload_failed') AND app_name IS NOT NULL)),
 CHECK((trigger='budget_threshold') = (threshold_percent IS NOT NULL)),
 CHECK(trigger NOT IN('budget_threshold','hard_cap') OR (dimension IS NOT NULL AND window_end IS NOT NULL))
) STRICT;
CREATE INDEX ix_alert_occurrences_trigger ON alert_occurrences(trigger,occurred_at);
CREATE INDEX ix_alert_occurrences_app ON alert_occurrences(app_name,occurred_at);
CREATE INDEX ix_alert_occurrences_window ON alert_occurrences(window_end);
CREATE INDEX ix_alert_occurrences_created ON alert_occurrences(created_at);
CREATE TABLE alert_deliveries (
 delivery_id TEXT PRIMARY KEY CHECK(length(delivery_id)=32 AND delivery_id NOT GLOB '*[^0-9a-f]*'),
 occurrence_id TEXT NOT NULL REFERENCES alert_occurrences(occurrence_id) ON DELETE RESTRICT,
 channel_name TEXT NOT NULL CHECK(length(channel_name) BETWEEN 1 AND 40),
 channel_type TEXT NOT NULL CHECK(channel_type IN('webhook','discord','slack','ntfy')),
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN('pending','claimed','delivered','failed','canceled')),
 attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count BETWEEN 0 AND 4),
 next_attempt_at INTEGER NOT NULL,
 claimed_until INTEGER,
 claim_token TEXT CHECK(claim_token IS NULL OR (length(claim_token)=32 AND claim_token NOT GLOB '*[^0-9a-f]*')),
 last_status INTEGER CHECK(last_status IS NULL OR last_status BETWEEN 100 AND 599),
 last_error TEXT CHECK(last_error IS NULL OR last_error IN('success','timeout','network','rate_limited','server_error','client_error','invalid_response','channel_changed')),
 delivered_at INTEGER,
 finished_at INTEGER,
 created_at INTEGER NOT NULL,
 UNIQUE(occurrence_id,channel_name),
 CHECK((state='claimed') = (claim_token IS NOT NULL AND claimed_until IS NOT NULL)),
 CHECK((state='delivered') = (delivered_at IS NOT NULL)),
 CHECK((state IN('delivered','failed','canceled')) = (finished_at IS NOT NULL))
) STRICT;
CREATE INDEX ix_alert_deliveries_due ON alert_deliveries(state,next_attempt_at,created_at);
CREATE INDEX ix_alert_deliveries_lease ON alert_deliveries(claimed_until);
CREATE INDEX ix_alert_deliveries_channel ON alert_deliveries(channel_name,created_at);
CREATE INDEX ix_alert_deliveries_finished ON alert_deliveries(finished_at);
CREATE TABLE usage_5m (
 bucket_start INTEGER NOT NULL CHECK(bucket_start % 300000000 = 0),
 app_name TEXT NOT NULL CHECK(length(app_name) BETWEEN 1 AND 40),
 cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK(cost_microusd>=0),
 updated_at INTEGER NOT NULL,
 PRIMARY KEY(bucket_start,app_name)
) STRICT;
CREATE INDEX ix_usage_5m_app ON usage_5m(app_name,bucket_start);
INSERT INTO usage_5m(bucket_start,app_name,cost_microusd,updated_at)
 SELECT (finished_at/300000000)*300000000, app_name, sum(cost_microusd), CAST((julianday('now')-2440587.5)*86400000000 AS INTEGER)
 FROM requests WHERE cost_microusd>0 GROUP BY 1,2;
CREATE TRIGGER tr_alert_inputs AFTER INSERT ON events
 WHEN NEW.type IN('request_completed','blocked','fallback','loop_cut','config_changed','config_reload_failed','limit_state_unavailable')
BEGIN
 INSERT OR IGNORE INTO alert_inputs(event_id,event_type,enqueued_at,state) VALUES(NEW.event_id,NEW.type,CAST((julianday('now')-2440587.5)*86400000000 AS INTEGER),'pending');
END;
CREATE TRIGGER tr_usage_5m AFTER INSERT ON requests
 WHEN NEW.cost_microusd>0
BEGIN
 INSERT INTO usage_5m(bucket_start,app_name,cost_microusd,updated_at) VALUES((NEW.finished_at/300000000)*300000000,NEW.app_name,NEW.cost_microusd,CAST((julianday('now')-2440587.5)*86400000000 AS INTEGER))
 ON CONFLICT(bucket_start,app_name) DO UPDATE SET cost_microusd=cost_microusd+excluded.cost_microusd,updated_at=excluded.updated_at;
END;

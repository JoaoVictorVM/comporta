CREATE TABLE events_v7 (
 event_id TEXT PRIMARY KEY CHECK(length(event_id)=32 AND event_id NOT GLOB '*[^0-9a-f]*'),
 type TEXT NOT NULL CHECK(type IN('request_completed','blocked','cached','fallback','downgraded','warned','loop_cut','config_changed','config_reload_failed','paused','sensitive_data_detected','limit_state_unavailable','limit_state_recovered','translation_skipped')),
 occurred_at INTEGER NOT NULL, request_id TEXT CHECK(request_id IS NULL OR length(request_id)=32), attempt_index INTEGER CHECK(attempt_index>=0), app_name TEXT, user_hash TEXT, anonymous INTEGER CHECK(anonymous IN(0,1)),
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json))
) STRICT;
INSERT INTO events_v7(event_id,type,occurred_at,request_id,attempt_index,app_name,user_hash,anonymous,payload_json) SELECT event_id,type,occurred_at,request_id,attempt_index,app_name,user_hash,anonymous,payload_json FROM events;
CREATE TABLE config_changes_v7 (event_id TEXT PRIMARY KEY REFERENCES events_v7(event_id) ON DELETE CASCADE, generation INTEGER NOT NULL, result TEXT NOT NULL CHECK(result IN('initial','applied','unchanged','rejected')), changes_json TEXT NOT NULL CHECK(json_valid(changes_json))) STRICT;
INSERT INTO config_changes_v7(event_id,generation,result,changes_json) SELECT event_id,generation,result,changes_json FROM config_changes;
CREATE TABLE alert_inputs_v7 (
 event_id TEXT PRIMARY KEY REFERENCES events_v7(event_id) ON DELETE CASCADE,
 event_type TEXT NOT NULL CHECK(event_type IN('request_completed','blocked','fallback','loop_cut','config_changed','config_reload_failed','limit_state_unavailable')),
 enqueued_at INTEGER NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN('pending','claimed','processed')),
 claimed_until INTEGER,
 claim_token TEXT CHECK(claim_token IS NULL OR (length(claim_token)=32 AND claim_token NOT GLOB '*[^0-9a-f]*')),
 processed_at INTEGER,
 CHECK((state='claimed') = (claim_token IS NOT NULL AND claimed_until IS NOT NULL)),
 CHECK((state='processed') = (processed_at IS NOT NULL))
) STRICT;
INSERT INTO alert_inputs_v7(event_id,event_type,enqueued_at,state,claimed_until,claim_token,processed_at) SELECT event_id,event_type,enqueued_at,state,claimed_until,claim_token,processed_at FROM alert_inputs;
CREATE TEMP TABLE migration_counts AS SELECT
 (SELECT count(*) FROM events) - (SELECT count(*) FROM events_v7) +
 (SELECT count(*) FROM config_changes) - (SELECT count(*) FROM config_changes_v7) +
 (SELECT count(*) FROM alert_inputs) - (SELECT count(*) FROM alert_inputs_v7) AS missing;
CREATE TEMP TRIGGER migration_counts_guard BEFORE DELETE ON migration_counts BEGIN SELECT RAISE(ABORT,'row counts differ') WHERE OLD.missing<>0; END;
DELETE FROM migration_counts;
DROP TABLE migration_counts;
DROP TABLE alert_inputs;
DROP TABLE config_changes;
DROP TABLE events;
ALTER TABLE events_v7 RENAME TO events;
ALTER TABLE config_changes_v7 RENAME TO config_changes;
ALTER TABLE alert_inputs_v7 RENAME TO alert_inputs;
CREATE INDEX ix_events_time ON events(occurred_at,event_id);
CREATE INDEX ix_events_app_type ON events(app_name,type,occurred_at);
CREATE INDEX ix_events_user ON events(app_name,user_hash,anonymous);
CREATE INDEX ix_alert_inputs_pending ON alert_inputs(state,enqueued_at,event_id);
CREATE INDEX ix_alert_inputs_lease ON alert_inputs(claimed_until);
CREATE TRIGGER tr_alert_inputs AFTER INSERT ON events
 WHEN NEW.type IN('request_completed','blocked','fallback','loop_cut','config_changed','config_reload_failed','limit_state_unavailable')
BEGIN
 INSERT OR IGNORE INTO alert_inputs(event_id,event_type,enqueued_at,state) VALUES(NEW.event_id,NEW.type,CAST((julianday('now')-2440587.5)*86400000000 AS INTEGER),'pending');
END;

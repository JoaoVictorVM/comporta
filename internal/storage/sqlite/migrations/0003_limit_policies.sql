CREATE TABLE events_v3 (
 event_id TEXT PRIMARY KEY CHECK(length(event_id)=32 AND event_id NOT GLOB '*[^0-9a-f]*'),
 type TEXT NOT NULL CHECK(type IN('request_completed','blocked','cached','fallback','downgraded','warned','loop_cut','config_changed','config_reload_failed','paused','sensitive_data_detected','limit_state_unavailable','limit_state_recovered')),
 occurred_at INTEGER NOT NULL, request_id TEXT CHECK(request_id IS NULL OR length(request_id)=32), attempt_index INTEGER CHECK(attempt_index>=0), app_name TEXT, user_hash TEXT, anonymous INTEGER CHECK(anonymous IN(0,1)),
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json))
) STRICT;
INSERT INTO events_v3(event_id,type,occurred_at,request_id,attempt_index,app_name,user_hash,anonymous,payload_json) SELECT event_id,type,occurred_at,request_id,attempt_index,app_name,user_hash,anonymous,payload_json FROM events;
CREATE TABLE config_changes_v3 (event_id TEXT PRIMARY KEY REFERENCES events_v3(event_id) ON DELETE CASCADE, generation INTEGER NOT NULL, result TEXT NOT NULL CHECK(result IN('initial','applied','unchanged','rejected')), changes_json TEXT NOT NULL CHECK(json_valid(changes_json))) STRICT;
INSERT INTO config_changes_v3(event_id,generation,result,changes_json) SELECT event_id,generation,result,changes_json FROM config_changes;
DROP TABLE config_changes;
DROP TABLE events;
ALTER TABLE events_v3 RENAME TO events;
ALTER TABLE config_changes_v3 RENAME TO config_changes;
CREATE INDEX ix_events_time ON events(occurred_at,event_id);
CREATE INDEX ix_events_app_type ON events(app_name,type,occurred_at);
CREATE INDEX ix_events_user ON events(app_name,user_hash,anonymous);

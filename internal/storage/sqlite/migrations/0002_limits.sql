CREATE TABLE limit_reservations (
 request_id TEXT NOT NULL CHECK(length(request_id)=32 AND request_id NOT GLOB '*[^0-9a-f]*'),
 attempt_index INTEGER NOT NULL CHECK(attempt_index>=1),
 app_name TEXT NOT NULL CHECK(length(app_name) BETWEEN 1 AND 40),
 subject TEXT NOT NULL CHECK(subject='' OR (length(subject)=32 AND subject NOT GLOB '*[^0-9a-f]*')),
 anonymous INTEGER NOT NULL CHECK(anonymous IN(0,1)),
 state TEXT NOT NULL CHECK(state IN('reserved','settled','released')),
 admitted_at INTEGER NOT NULL, finished_at INTEGER,
 quote_json TEXT NOT NULL CHECK(json_valid(quote_json)),
 settlement_json TEXT CHECK(settlement_json IS NULL OR json_valid(settlement_json)),
 PRIMARY KEY(request_id,attempt_index),
 CHECK((state='reserved' AND finished_at IS NULL AND settlement_json IS NULL) OR (state<>'reserved' AND finished_at>=admitted_at AND settlement_json IS NOT NULL))
) STRICT;
CREATE INDEX ix_limit_reservations_subject ON limit_reservations(app_name,subject,anonymous,state);
CREATE INDEX ix_limit_reservations_state ON limit_reservations(state);
CREATE TABLE limit_reservation_items (
 request_id TEXT NOT NULL, attempt_index INTEGER NOT NULL, item_index INTEGER NOT NULL CHECK(item_index>=0),
 scope TEXT NOT NULL CHECK(scope IN('app','user')), subject TEXT NOT NULL, anonymous INTEGER NOT NULL CHECK(anonymous IN(0,1)),
 dimension TEXT NOT NULL CHECK(dimension IN('usd_per_day','usd_per_month','tokens_per_day','tokens_per_month')),
 window_start INTEGER NOT NULL, window_end INTEGER NOT NULL CHECK(window_end>window_start),
 timezone TEXT NOT NULL CHECK(length(timezone) BETWEEN 1 AND 64),
 reserved INTEGER NOT NULL CHECK(reserved>=0), settled INTEGER CHECK(settled IS NULL OR settled>=0),
 PRIMARY KEY(request_id,attempt_index,item_index),
 FOREIGN KEY(request_id,attempt_index) REFERENCES limit_reservations(request_id,attempt_index) ON DELETE RESTRICT,
 CHECK((scope='app' AND subject='' AND anonymous=0) OR (scope='user' AND length(subject)=32 AND subject NOT GLOB '*[^0-9a-f]*'))
) STRICT;
CREATE TABLE limit_ledger (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 request_id TEXT NOT NULL CHECK(length(request_id)=32 AND request_id NOT GLOB '*[^0-9a-f]*'),
 attempt_index INTEGER NOT NULL CHECK(attempt_index>=1),
 transition TEXT NOT NULL CHECK(transition IN('admit','settle','release')),
 occurred_at INTEGER NOT NULL,
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json)),
 UNIQUE(request_id,attempt_index,transition)
) STRICT;
CREATE TABLE limit_rate_events (
 request_id TEXT PRIMARY KEY CHECK(length(request_id)=32 AND request_id NOT GLOB '*[^0-9a-f]*'),
 app_name TEXT NOT NULL CHECK(length(app_name) BETWEEN 1 AND 40),
 subject TEXT NOT NULL CHECK(subject='' OR (length(subject)=32 AND subject NOT GLOB '*[^0-9a-f]*')),
 anonymous INTEGER NOT NULL CHECK(anonymous IN(0,1)),
 admitted_at INTEGER NOT NULL,
 estimated_tokens INTEGER NOT NULL CHECK(estimated_tokens>=0),
 actual_tokens INTEGER CHECK(actual_tokens IS NULL OR actual_tokens>=0),
 active INTEGER NOT NULL CHECK(active IN(0,1))
) STRICT;
CREATE INDEX ix_limit_rate_app ON limit_rate_events(app_name,admitted_at);
CREATE INDEX ix_limit_rate_user ON limit_rate_events(app_name,subject,anonymous,admitted_at);
CREATE TABLE limit_checkpoints (
 id INTEGER PRIMARY KEY CHECK(id=1),
 covered_seq INTEGER NOT NULL CHECK(covered_seq>=0),
 created_at INTEGER NOT NULL,
 civil_watermark INTEGER NOT NULL,
 metadata_json TEXT NOT NULL CHECK(json_valid(metadata_json))
) STRICT;
CREATE INDEX ix_budget_state_window ON budget_state(window_end,app_name);
CREATE TABLE events_v2 (
 event_id TEXT PRIMARY KEY CHECK(length(event_id)=32 AND event_id NOT GLOB '*[^0-9a-f]*'),
 type TEXT NOT NULL CHECK(type IN('request_completed','blocked','cached','fallback','downgraded','loop_cut','config_changed','config_reload_failed','paused','sensitive_data_detected','limit_state_unavailable','limit_state_recovered')),
 occurred_at INTEGER NOT NULL, request_id TEXT CHECK(request_id IS NULL OR length(request_id)=32), attempt_index INTEGER CHECK(attempt_index>=0), app_name TEXT, user_hash TEXT, anonymous INTEGER CHECK(anonymous IN(0,1)),
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json))
) STRICT;
INSERT INTO events_v2(event_id,type,occurred_at,request_id,attempt_index,app_name,user_hash,anonymous,payload_json) SELECT event_id,type,occurred_at,request_id,attempt_index,app_name,user_hash,anonymous,payload_json FROM events;
CREATE TABLE config_changes_v2 (event_id TEXT PRIMARY KEY REFERENCES events_v2(event_id) ON DELETE CASCADE, generation INTEGER NOT NULL, result TEXT NOT NULL CHECK(result IN('initial','applied','unchanged','rejected')), changes_json TEXT NOT NULL CHECK(json_valid(changes_json))) STRICT;
INSERT INTO config_changes_v2(event_id,generation,result,changes_json) SELECT event_id,generation,result,changes_json FROM config_changes;
DROP TABLE config_changes;
DROP TABLE events;
ALTER TABLE events_v2 RENAME TO events;
ALTER TABLE config_changes_v2 RENAME TO config_changes;
CREATE INDEX ix_events_time ON events(occurred_at,event_id);
CREATE INDEX ix_events_app_type ON events(app_name,type,occurred_at);
CREATE INDEX ix_events_user ON events(app_name,user_hash,anonymous);

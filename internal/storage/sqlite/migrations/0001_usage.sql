CREATE TABLE requests (
 request_id TEXT NOT NULL CHECK(length(request_id)=32 AND request_id NOT GLOB '*[^0-9a-f]*'),
 attempt_index INTEGER NOT NULL CHECK(attempt_index>=0),
 started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL CHECK(finished_at>=started_at),
 app_name TEXT NOT NULL, user_hash TEXT, anonymous INTEGER CHECK(anonymous IN(0,1)),
 operation TEXT NOT NULL CHECK(operation IN('generation','count_tokens')),
 provider TEXT, model_requested TEXT, model_used TEXT,
 upstream_status INTEGER CHECK(upstream_status BETWEEN 100 AND 599), downstream_status INTEGER CHECK(downstream_status BETWEEN 100 AND 599),
 outcome TEXT NOT NULL CHECK(outcome IN('ok','blocked','cached','fallback','downgraded','loop_cut')),
 error_class TEXT, error_code TEXT,
 input_tokens INTEGER CHECK(input_tokens>=0), output_tokens INTEGER CHECK(output_tokens>=0),
 cache_write_tokens INTEGER CHECK(cache_write_tokens>=0), cache_read_tokens INTEGER CHECK(cache_read_tokens>=0),
 raw_input_total INTEGER CHECK(raw_input_total>=0), counted_input_tokens INTEGER CHECK(counted_input_tokens>=0),
 cache_write_5m_tokens INTEGER CHECK(cache_write_5m_tokens>=0), cache_write_1h_tokens INTEGER CHECK(cache_write_1h_tokens>=0),
 cost_microusd INTEGER NOT NULL CHECK(cost_microusd>=0), reserved_cost_microusd INTEGER NOT NULL CHECK(reserved_cost_microusd>=0),
 usage_missing INTEGER NOT NULL CHECK(usage_missing IN(0,1)), recovered INTEGER NOT NULL CHECK(recovered IN(0,1)), estimate_exceeded INTEGER NOT NULL CHECK(estimate_exceeded IN(0,1)),
 request_count INTEGER NOT NULL CHECK(request_count IN(0,1)), latency_us INTEGER NOT NULL CHECK(latency_us>=0),
 upstream_ttfb_us INTEGER CHECK(upstream_ttfb_us>=0), client_ttfb_us INTEGER CHECK(client_ttfb_us>=0),
 usage_day TEXT NOT NULL, timezone TEXT NOT NULL, price_quote_json TEXT NOT NULL CHECK(json_valid(price_quote_json)),
 record_json TEXT NOT NULL CHECK(json_valid(record_json)),
 PRIMARY KEY(request_id,attempt_index),
 CHECK((user_hash IS NULL AND anonymous IS NULL) OR (length(user_hash)=32 AND user_hash NOT GLOB '*[^0-9a-f]*' AND anonymous IS NOT NULL))
) STRICT;
CREATE INDEX ix_requests_app_time ON requests(app_name,started_at,request_id);
CREATE INDEX ix_requests_user_time ON requests(app_name,user_hash,anonymous,started_at);
CREATE INDEX ix_requests_retention ON requests(finished_at);
CREATE UNIQUE INDEX ix_request_terminal ON requests(request_id) WHERE request_count=1;
CREATE TABLE usage_daily (
 day TEXT NOT NULL, timezone TEXT NOT NULL, app_name TEXT NOT NULL,
 scope TEXT NOT NULL CHECK(scope IN('app','user')), subject TEXT NOT NULL, anonymous INTEGER NOT NULL CHECK(anonymous IN(0,1)),
 requests INTEGER NOT NULL CHECK(requests>=0), attempts INTEGER NOT NULL CHECK(attempts>=0),
 input_tokens INTEGER NOT NULL CHECK(input_tokens>=0), output_tokens INTEGER NOT NULL CHECK(output_tokens>=0),
 cache_write_tokens INTEGER NOT NULL CHECK(cache_write_tokens>=0), cache_read_tokens INTEGER NOT NULL CHECK(cache_read_tokens>=0),
 usage_missing_count INTEGER NOT NULL CHECK(usage_missing_count>=0), recovered_count INTEGER NOT NULL CHECK(recovered_count>=0),
 cost_microusd INTEGER NOT NULL CHECK(cost_microusd>=0),
 PRIMARY KEY(day,timezone,app_name,scope,subject,anonymous),
 CHECK((scope='app' AND subject='' AND anonymous=0) OR (scope='user' AND length(subject)=32 AND subject NOT GLOB '*[^0-9a-f]*'))
) STRICT;
CREATE INDEX ix_daily_user ON usage_daily(app_name,scope,subject,day);
CREATE INDEX ix_daily_period ON usage_daily(scope,day,app_name);
CREATE TABLE metering_journal (
 request_id TEXT NOT NULL CHECK(length(request_id)=32 AND request_id NOT GLOB '*[^0-9a-f]*'), attempt_index INTEGER NOT NULL CHECK(attempt_index>=0), admitted_at INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN('admitted','completed')),
 metadata_json TEXT NOT NULL CHECK(json_valid(metadata_json)), quote_json TEXT NOT NULL CHECK(json_valid(quote_json)), result_json TEXT,
 PRIMARY KEY(request_id,attempt_index),
 CHECK((state='admitted' AND result_json IS NULL) OR (state='completed' AND result_json IS NOT NULL AND json_valid(result_json)))
) STRICT;
CREATE INDEX ix_journal_admitted ON metering_journal(admitted_at);
CREATE TABLE events (
 event_id TEXT PRIMARY KEY CHECK(length(event_id)=32 AND event_id NOT GLOB '*[^0-9a-f]*'),
 type TEXT NOT NULL CHECK(type IN('request_completed','blocked','cached','fallback','downgraded','loop_cut','config_changed','config_reload_failed','paused','sensitive_data_detected')),
 occurred_at INTEGER NOT NULL, request_id TEXT CHECK(request_id IS NULL OR length(request_id)=32), attempt_index INTEGER CHECK(attempt_index>=0), app_name TEXT, user_hash TEXT, anonymous INTEGER CHECK(anonymous IN(0,1)),
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json))
) STRICT;
CREATE INDEX ix_events_time ON events(occurred_at,event_id);
CREATE INDEX ix_events_app_type ON events(app_name,type,occurred_at);
CREATE INDEX ix_events_user ON events(app_name,user_hash,anonymous);
CREATE TABLE config_changes (event_id TEXT PRIMARY KEY REFERENCES events(event_id) ON DELETE CASCADE, generation INTEGER NOT NULL, result TEXT NOT NULL CHECK(result IN('initial','applied','unchanged','rejected')), changes_json TEXT NOT NULL CHECK(json_valid(changes_json))) STRICT;
CREATE TABLE budget_state (
 app_name TEXT NOT NULL, scope TEXT NOT NULL CHECK(scope IN('app','user')), subject TEXT NOT NULL, anonymous INTEGER NOT NULL CHECK(anonymous IN(0,1)), dimension TEXT NOT NULL,
 window_start INTEGER NOT NULL, window_end INTEGER NOT NULL CHECK(window_end>window_start), spent INTEGER NOT NULL DEFAULT 0 CHECK(spent>=0), reserved INTEGER NOT NULL DEFAULT 0 CHECK(reserved>=0), updated_at INTEGER NOT NULL,
 PRIMARY KEY(app_name,scope,subject,anonymous,dimension,window_start,window_end),
 CHECK((scope='app' AND subject='' AND anonymous=0) OR (scope='user' AND length(subject)=32 AND subject NOT GLOB '*[^0-9a-f]*'))
) STRICT;
CREATE TABLE runtime_state (namespace TEXT NOT NULL, key TEXT NOT NULL, value_json TEXT NOT NULL CHECK(json_valid(value_json)), updated_at INTEGER NOT NULL, PRIMARY KEY(namespace,key)) STRICT;

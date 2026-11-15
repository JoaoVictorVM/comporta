CREATE TABLE request_operations (
 request_id TEXT PRIMARY KEY CHECK(length(request_id)=32 AND request_id NOT GLOB '*[^0-9a-f]*'),
 app_name TEXT NOT NULL CHECK(length(app_name) BETWEEN 1 AND 40),
 subject TEXT NOT NULL CHECK(subject='' OR (length(subject)=32 AND subject NOT GLOB '*[^0-9a-f]*')),
 anonymous INTEGER NOT NULL CHECK(anonymous IN(0,1)),
 generation INTEGER NOT NULL CHECK(generation>=0),
 started_at INTEGER NOT NULL,
 state TEXT NOT NULL DEFAULT 'active' CHECK(state IN('active','closed')),
 last_attempt_index INTEGER NOT NULL CHECK(last_attempt_index BETWEEN 1 AND 5),
 terminal_attempt_index INTEGER,
 finished_at INTEGER,
 terminal_status INTEGER CHECK(terminal_status IS NULL OR terminal_status BETWEEN 100 AND 599),
 terminal_code TEXT NOT NULL DEFAULT '' CHECK(length(terminal_code)<=64 AND terminal_code NOT GLOB '*[^a-z0-9_]*'),
 materialized INTEGER NOT NULL DEFAULT 0 CHECK(materialized IN(0,1)),
 CHECK((state='active' AND terminal_attempt_index IS NULL AND finished_at IS NULL AND materialized=0) OR
       (state='closed' AND terminal_attempt_index BETWEEN 1 AND last_attempt_index AND finished_at>=started_at))
) STRICT;
CREATE INDEX ix_operations_subject ON request_operations(app_name,subject,anonymous,state);
CREATE INDEX ix_operations_recovery ON request_operations(state,materialized);
CREATE INDEX ix_operations_retention ON request_operations(finished_at);
CREATE TABLE limit_attempt_rate_events (
 request_id TEXT NOT NULL CHECK(length(request_id)=32 AND request_id NOT GLOB '*[^0-9a-f]*'),
 attempt_index INTEGER NOT NULL CHECK(attempt_index BETWEEN 2 AND 5),
 app_name TEXT NOT NULL CHECK(length(app_name) BETWEEN 1 AND 40),
 subject TEXT NOT NULL CHECK(subject='' OR (length(subject)=32 AND subject NOT GLOB '*[^0-9a-f]*')),
 anonymous INTEGER NOT NULL CHECK(anonymous IN(0,1)),
 admitted_at INTEGER NOT NULL,
 estimated_tokens INTEGER NOT NULL CHECK(estimated_tokens>=0),
 actual_tokens INTEGER CHECK(actual_tokens IS NULL OR actual_tokens>=0),
 active INTEGER NOT NULL CHECK(active IN(0,1)),
 PRIMARY KEY(request_id,attempt_index)
) STRICT;
CREATE INDEX ix_attempt_rate_app ON limit_attempt_rate_events(app_name,admitted_at);
CREATE INDEX ix_attempt_rate_user ON limit_attempt_rate_events(app_name,subject,anonymous,admitted_at);

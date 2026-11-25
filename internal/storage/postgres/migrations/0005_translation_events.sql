ALTER TABLE events DROP CONSTRAINT events_type_check;
ALTER TABLE events ADD CONSTRAINT events_type_check CHECK (type IN ('request_completed','blocked','cached','fallback','downgraded','warned','loop_cut','config_changed','config_reload_failed','paused','sensitive_data_detected','limit_state_unavailable','limit_state_recovered','translation_skipped'));

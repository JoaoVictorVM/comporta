ALTER TABLE requests ADD COLUMN cache_saved_microusd INTEGER NOT NULL DEFAULT 0 CHECK(cache_saved_microusd>=0);
ALTER TABLE requests ADD COLUMN downgrade_saved_microusd INTEGER NOT NULL DEFAULT 0 CHECK(downgrade_saved_microusd>=0);
ALTER TABLE usage_daily ADD COLUMN cache_saved_microusd INTEGER NOT NULL DEFAULT 0 CHECK(cache_saved_microusd>=0);
ALTER TABLE usage_daily ADD COLUMN downgrade_saved_microusd INTEGER NOT NULL DEFAULT 0 CHECK(downgrade_saved_microusd>=0);
CREATE INDEX ix_requests_time ON requests(started_at,request_id,attempt_index);
CREATE INDEX ix_requests_outcome_time ON requests(outcome,started_at,request_id,attempt_index);
CREATE INDEX ix_requests_model_time ON requests(model_used,started_at,request_id,attempt_index);
CREATE INDEX ix_requests_provider_time ON requests(provider,started_at);

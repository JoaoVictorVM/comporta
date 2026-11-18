-- Reads everything one admission of one app needs, at one instant of the
-- shared clock. Settled rate entries older than the sliding window are purged
-- in bounded batches; active ones stay until their settlement.
local meta, pause, version, b_app, b_user, r_app, r_user, c_app, c_user, zone =
  KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], KEYS[7], KEYS[8], KEYS[9], KEYS[10]
local epoch, app = ARGV[1], ARGV[2]

if redis.call("HGET", meta, "epoch") ~= epoch then return {"fenced"} end
local state = redis.call("HGET", meta, "state")
if state ~= "normal" then return {"fenced"} end

local t = redis.call("TIME")
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
-- The watermark keeps the shared clock from moving backwards after a step of
-- the Redis host clock. It is persisted once per second, so a plain read does
-- not cost a write: a regression smaller than that is absorbed by the window.
local mark = tonumber(redis.call("HGET", meta, "watermark") or "0")
if now < mark then
  now = mark
elseif now >= mark + 1000 then
  redis.call("HSET", meta, "watermark", tostring(now))
end

local function purge(key)
  local fields = redis.call("HGETALL", key)
  local removed = 0
  for k = 1, #fields, 2 do
    local entry = fields[k + 1]
    local stamp, _, active = entry:match("^(%d+)|(%d+)|(%d)$")
    if active == "0" and tonumber(stamp) <= now - 60000 then
      redis.call("HDEL", key, fields[k])
      removed = removed + 1
      if removed >= 1000 then break end
    end
  end
end
purge(r_app)
purge(r_user)

local paused = "0"
if redis.call("HGET", pause, "global") == "1" or redis.call("HGET", pause, "app:" .. app) == "1" then paused = "1" end

return {
  "ok", tostring(now), redis.call("GET", version) or "0", paused,
  redis.call("HGETALL", b_app), redis.call("HGETALL", b_user),
  redis.call("HGETALL", r_app), redis.call("HGETALL", r_user),
  tostring(redis.call("HLEN", c_app)), tostring(redis.call("HLEN", c_user)),
  redis.call("HGETALL", zone),
}

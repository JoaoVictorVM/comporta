-- Applies one admission planned by a replica. The plan was computed on a read
-- of the same app at version ARGV[2]; any change since then refuses with
-- "stale" and nothing is written. Every argument is validated before the first
-- write, because Lua cannot roll back a write when a later command fails.
local meta, pause, version, b_app, b_user, r_app, r_user, c_app, c_user, zone, receipt, owned, subjects, pending =
  KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], KEYS[7], KEYS[8], KEYS[9], KEYS[10], KEYS[11], KEYS[12], KEYS[13], KEYS[14]
local epoch, expected, digest, app, slot, request, attempt, at, tokens, owner, user =
  ARGV[1], ARGV[2], ARGV[3], ARGV[4], ARGV[5], ARGV[6], ARGV[7], ARGV[8], ARGV[9], ARGV[10], ARGV[11]

if redis.call("HGET", meta, "epoch") ~= epoch or redis.call("HGET", meta, "state") ~= "normal" then
  return {"fenced"}
end
local previous = redis.call("HGET", receipt, "admit")
if previous then
  if previous == digest then return {"dup", redis.call("GET", version) or "0"} end
  return {"conflict"}
end
if redis.call("HGET", pause, "global") == "1" or redis.call("HGET", pause, "app:" .. app) == "1" then
  return {"paused"}
end
if (redis.call("GET", version) or "0") ~= expected then
  return {"stale"}
end
if not valid_nonneg(at) or not valid_nonneg(tokens) or (slot ~= "0" and slot ~= "1") then
  return {"invalid"}
end

local n = tonumber(ARGV[12])
local writes = {}
local i = 13
for _ = 1, n do
  local scope, prefix, amount = ARGV[i], ARGV[i + 1], ARGV[i + 2]
  if not valid_nonneg(amount) then return {"invalid"} end
  local key = scope == "a" and b_app or b_user
  local next = apply(current(key, prefix .. "|r"), amount)
  if not next then return {"overflow"} end
  writes[#writes + 1] = {key, prefix .. "|r", next}
  i = i + 3
end
local has_zone = ARGV[i]
local zone_active, zone_pending, zone_at = ARGV[i + 1], ARGV[i + 2], ARGV[i + 3]
i = i + 4
local seeds = tonumber(ARGV[i])
i = i + 1
local seed_writes = {}
for _ = 1, seeds do
  local scope, prefix, spent, reserved, unknown = ARGV[i], ARGV[i + 1], ARGV[i + 2], ARGV[i + 3], ARGV[i + 4]
  if not valid_nonneg(spent) or not valid_nonneg(reserved) then return {"invalid"} end
  local key = scope == "a" and b_app or b_user
  seed_writes[#seed_writes + 1] = {key, prefix, spent, reserved, unknown}
  i = i + 5
end

-- Validated: from here on every command is a plain write that cannot fail on
-- the values.
for _, s in ipairs(seed_writes) do
  redis.call("HSET", s[1], s[2] .. "|s", s[3], s[2] .. "|r", s[4])
  if s[5] == "1" then redis.call("HSET", s[1], s[2] .. "|u", "1") end
end
for _, w in ipairs(writes) do
  redis.call("HSET", w[1], w[2], w[3])
end
local entry = at .. "|" .. tokens .. "|1"
redis.call("HSET", r_app, request .. "|" .. attempt, entry)
redis.call("HSET", r_user, request .. "|" .. attempt, entry)
if slot == "1" then
  redis.call("HSET", c_app, request, owner)
  redis.call("HSET", c_user, request, owner)
  redis.call("SADD", owned, app .. "|" .. user .. "|" .. request)
end
if has_zone == "1" then
  redis.call("HSET", zone, "active", zone_active, "pending", zone_pending, "activate_at", zone_at)
end
redis.call("SADD", subjects, user)
redis.call("HSET", receipt, "admit", digest, "app", app, "user", user)
redis.call("ZADD", pending, at, request .. "|" .. attempt)
return {"ok", tostring(redis.call("INCR", version))}

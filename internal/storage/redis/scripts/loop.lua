-- Shared loop guard for one subject. Arrivals are counted in sorted sets of
-- request IDs scored by the shared clock; signatures arrive already wrapped in
-- an HMAC, so the store never sees a digest a dictionary could reverse. An
-- episode is frozen in a hash: every retry during its cooldown, on any
-- replica, gets the same event identifier.
local meta, subject, volume, signature, subjects = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5]
local epoch, request, event, policy = ARGV[1], ARGV[2], ARGV[3], ARGV[4]
local similar, window, rpm, cooldown = tonumber(ARGV[5]), tonumber(ARGV[6]), tonumber(ARGV[7]), tonumber(ARGV[8])
local has_sig, member, max_subjects, ttl = ARGV[9], ARGV[10], tonumber(ARGV[11]), tonumber(ARGV[12])

if redis.call("HGET", meta, "epoch") ~= epoch or redis.call("HGET", meta, "state") ~= "normal" then
  return {"fenced"}
end
local t = redis.call("TIME")
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
-- After a rebuild the loop history is lost; every generation is held back
-- until the longest window or cooldown that history could have covered ends.
local quarantine = tonumber(redis.call("HGET", meta, "loop_quarantine") or "0")
if now < quarantine then
  return {"quarantine", tostring(quarantine - now)}
end

local until_ms = tonumber(redis.call("HGET", subject, "until") or "0")
if now < until_ms then
  local ep = redis.call("HMGET", subject, "event", "at", "request", "reason", "count", "limit", "window", "cooldown")
  return {"cut", "0", tostring(until_ms - now), ep[1], ep[2], ep[3], ep[4], ep[5], ep[6], ep[7], ep[8]}
end

-- A change of thresholds discards the history counted under the old ones.
if redis.call("HGET", subject, "policy") ~= policy then
  redis.call("DEL", volume)
  redis.call("HSET", subject, "policy", policy)
end
if redis.call("ZSCORE", subjects, subject) == false and redis.call("ZCARD", subjects) >= max_subjects then
  redis.call("ZREMRANGEBYSCORE", subjects, "-inf", tostring(now - ttl))
  if redis.call("ZCARD", subjects) >= max_subjects then return {"capacity"} end
end
redis.call("ZREMRANGEBYSCORE", volume, "-inf", tostring(now - 60000))
if has_sig == "1" then redis.call("ZREMRANGEBYSCORE", signature, "-inf", tostring(now - window)) end

local reason, count, limit, win = nil, 0, 0, 0
if has_sig == "1" and redis.call("ZCARD", signature) >= similar then
  reason, count, limit, win = "similar_requests", similar + 1, similar, window
elseif redis.call("ZCARD", volume) >= rpm then
  reason, count, limit, win = "request_rate", rpm + 1, rpm, 60000
end
if reason then
  redis.call("DEL", volume)
  if has_sig == "1" then redis.call("DEL", signature) end
  redis.call("HSET", subject, "event", event, "at", tostring(now), "request", request, "reason", reason,
    "count", tostring(count), "limit", tostring(limit), "window", tostring(win), "cooldown", tostring(cooldown), "until", tostring(now + cooldown))
  redis.call("PEXPIRE", subject, cooldown + ttl)
  redis.call("ZADD", subjects, now, subject)
  return {"cut", "1", tostring(cooldown), event, tostring(now), request, reason, tostring(count), tostring(limit), tostring(win), tostring(cooldown)}
end

redis.call("ZADD", volume, now, request)
redis.call("PEXPIRE", volume, 60000 + ttl)
if has_sig == "1" then
  redis.call("ZADD", signature, now, member)
  redis.call("PEXPIRE", signature, window + ttl)
end
redis.call("PEXPIRE", subject, math.max(window, 60000, cooldown) + ttl)
redis.call("ZADD", subjects, now, subject)
return {"ok"}

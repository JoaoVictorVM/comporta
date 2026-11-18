-- Control operations on the projection: concurrency slots, pause projection,
-- rollback of an admission whose SQL commit is proven absent, and the epoch
-- marker written by a bootstrap or a rebuild.
local op = ARGV[1]

if op == "slot" then
  -- KEYS: c_app, c_user, owned, version. ARGV: op, request, entry.
  local removed = redis.call("HDEL", KEYS[1], ARGV[2]) + redis.call("HDEL", KEYS[2], ARGV[2])
  redis.call("SREM", KEYS[3], ARGV[3])
  if removed > 0 then redis.call("INCR", KEYS[4]) end
  return {"ok"}
end

if op == "pause" then
  -- KEYS: meta, pause. ARGV: op, epoch, revision, global, apps...
  if redis.call("HGET", KEYS[1], "epoch") ~= ARGV[2] then return {"fenced"} end
  local current = tonumber(redis.call("HGET", KEYS[2], "revision") or "-1")
  if tonumber(ARGV[3]) < current then return {"older"} end
  redis.call("DEL", KEYS[2])
  local fields = {"revision", ARGV[3], "global", ARGV[4]}
  for i = 5, #ARGV do
    fields[#fields + 1] = "app:" .. ARGV[i]
    fields[#fields + 1] = "1"
  end
  redis.call("HSET", KEYS[2], unpack(fields))
  return {"ok"}
end

if op == "rollback" then
  -- KEYS: meta, version, b_app, b_user, r_app, r_user, c_app, c_user, owned, receipt, pending.
  -- ARGV: op, epoch, digest, request, attempt, entry, n, items(scope, prefix, amount).
  if redis.call("HGET", KEYS[1], "epoch") ~= ARGV[2] then return {"fenced"} end
  local receipt = KEYS[10]
  if redis.call("HGET", receipt, "admit") ~= ARGV[3] or redis.call("HGET", receipt, "end") then return {"skip"} end
  local n = tonumber(ARGV[7])
  local writes = {}
  local i = 8
  for _ = 1, n do
    local key = ARGV[i] == "a" and KEYS[3] or KEYS[4]
    local r = apply(current(key, ARGV[i + 1] .. "|r"), "-" .. ARGV[i + 2])
    if ARGV[i + 2] == "0" then r = current(key, ARGV[i + 1] .. "|r") end
    if not r then return {"integrity"} end
    writes[#writes + 1] = {key, ARGV[i + 1] .. "|r", r}
    i = i + 3
  end
  for _, w in ipairs(writes) do redis.call("HSET", w[1], w[2], w[3]) end
  local field = ARGV[4] .. "|" .. ARGV[5]
  redis.call("HDEL", KEYS[5], field)
  redis.call("HDEL", KEYS[6], field)
  redis.call("HDEL", KEYS[7], ARGV[4])
  redis.call("HDEL", KEYS[8], ARGV[4])
  redis.call("SREM", KEYS[9], ARGV[6])
  redis.call("HSET", receipt, "end", "rollback:" .. ARGV[3])
  redis.call("ZREM", KEYS[11], field)
  redis.call("INCR", KEYS[2])
  return {"ok"}
end

if op == "confirm" then
  -- KEYS: pending, receipt. ARGV: op, member. The plan, which names the
  -- user, is only kept while the commit is unconfirmed.
  redis.call("ZREM", KEYS[1], ARGV[2])
  redis.call("HDEL", KEYS[2], "plan")
  return {"ok"}
end

if op == "publish" then
  -- KEYS: meta. ARGV: op, epoch, revision, state. A new namespace becomes usable
  -- only through this single write, after every key of it was filled.
  redis.call("HSET", KEYS[1], "protocol", "1", "epoch", ARGV[2], "revision", ARGV[3], "state", ARGV[4])
  return {"ok"}
end

if op == "fence" then
  -- KEYS: meta. ARGV: op, epoch. Closes an epoch so no replica admits on it.
  if redis.call("HGET", KEYS[1], "epoch") == ARGV[2] then
    redis.call("HSET", KEYS[1], "state", "closed")
  end
  return {"ok"}
end

return {"invalid"}

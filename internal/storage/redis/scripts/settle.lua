-- Projects a settlement or release already committed in Postgres. A repeat with
-- the same digest is a confirmation; a settlement of an admission this epoch
-- never saw refuses with "unknown" instead of subtracting a reservation that
-- does not exist here.
local meta, version, b_app, b_user, r_app, r_user, receipt, pending =
  KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], KEYS[7], KEYS[8]
local epoch, transition, digest, request, attempt, rate_tokens = ARGV[1], ARGV[2], ARGV[3], ARGV[4], ARGV[5], ARGV[6]

if redis.call("HGET", meta, "epoch") ~= epoch or redis.call("HGET", meta, "state") ~= "normal" then
  return {"fenced"}
end
if transition ~= "settle" and transition ~= "release" then return {"invalid"} end
local done = redis.call("HGET", receipt, "end")
if done then
  if done == transition .. ":" .. digest then return {"dup"} end
  return {"conflict"}
end
if not redis.call("HGET", receipt, "admit") then
  return {"unknown"}
end
if rate_tokens ~= "" and not valid_nonneg(rate_tokens) then return {"invalid"} end

local n = tonumber(ARGV[7])
local writes = {}
local i = 8
for _ = 1, n do
  local scope, prefix, reserved, settled = ARGV[i], ARGV[i + 1], ARGV[i + 2], ARGV[i + 3]
  if not valid_nonneg(reserved) or not valid_nonneg(settled) then return {"invalid"} end
  local key = scope == "a" and b_app or b_user
  local r = apply(current(key, prefix .. "|r"), "-" .. reserved)
  local s = apply(current(key, prefix .. "|s"), settled)
  if reserved == "0" then r = current(key, prefix .. "|r") end
  if r and not current(key, prefix .. "|s") then r = nil end
  if not r then
    -- A reservation smaller than the one being ended is an integrity failure:
    -- the projection is rebuilt from the ledger rather than clamped.
    return {"integrity"}
  end
  if not s then return {"overflow"} end
  writes[#writes + 1] = {key, prefix, r, s}
  i = i + 4
end
for _, w in ipairs(writes) do
  redis.call("HSET", w[1], w[2] .. "|r", w[3], w[2] .. "|s", w[4])
end
if rate_tokens ~= "" then
  local field = request .. "|" .. attempt
  for _, key in ipairs({r_app, r_user}) do
    local entry = redis.call("HGET", key, field)
    if entry then
      local stamp = entry:match("^(%d+)|")
      redis.call("HSET", key, field, stamp .. "|" .. rate_tokens .. "|0")
    end
  end
end
redis.call("HSET", receipt, "end", transition .. ":" .. digest)
redis.call("ZREM", pending, request .. "|" .. attempt)
redis.call("INCR", version)
return {"ok"}

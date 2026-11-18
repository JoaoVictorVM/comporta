-- Exact decimal arithmetic on int64 values carried as strings. Lua numbers are
-- doubles, so a financial value never becomes a number here: it stays a string
-- of at most 19 digits, compared and added digit by digit.

local MAXINT = "9223372036854775807"

local function valid_nonneg(s)
  return type(s) == "string" and #s >= 1 and #s <= 19 and s:match("^%d+$") ~= nil and (#s == 1 or s:sub(1, 1) ~= "0")
end

local function valid_signed(s)
  if type(s) ~= "string" then return false end
  if s:sub(1, 1) == "-" then return s ~= "-0" and valid_nonneg(s:sub(2)) end
  return valid_nonneg(s)
end

local function cmp(a, b)
  if #a ~= #b then return #a < #b and -1 or 1 end
  if a == b then return 0 end
  return a < b and -1 or 1
end

local function fits(s)
  return #s < 19 or (#s == 19 and cmp(s, MAXINT) <= 0)
end

local function strip(s)
  local t = s:gsub("^0+", "")
  if t == "" then return "0" end
  return t
end

local function add(a, b)
  local out, carry = {}, 0
  local i, j = #a, #b
  while i > 0 or j > 0 or carry > 0 do
    local da = i > 0 and (a:byte(i) - 48) or 0
    local db = j > 0 and (b:byte(j) - 48) or 0
    local s = da + db + carry
    out[#out + 1] = string.char(48 + s % 10)
    carry = math.floor(s / 10)
    i, j = i - 1, j - 1
  end
  return strip(string.reverse(table.concat(out)))
end

-- sub requires a >= b.
local function sub(a, b)
  local out, borrow = {}, 0
  local i, j = #a, #b
  while i > 0 do
    local da = (a:byte(i) - 48) - borrow
    local db = j > 0 and (b:byte(j) - 48) or 0
    if da < db then da, borrow = da + 10, 1 else borrow = 0 end
    out[#out + 1] = string.char(48 + da - db)
    i, j = i - 1, j - 1
  end
  return strip(string.reverse(table.concat(out)))
end

-- apply adds a signed delta to a non-negative value. It returns nil when the
-- result would be negative or exceed int64, so the caller refuses before any
-- write.
local function apply(value, delta)
  if value == nil then return nil end
  if delta:sub(1, 1) == "-" then
    local d = delta:sub(2)
    if cmp(value, d) < 0 then return nil end
    return sub(value, d)
  end
  local r = add(value, delta)
  if not fits(r) then return nil end
  return r
end

-- current returns nil for a stored value that is not a valid integer, which
-- every caller treats as an integrity failure before writing.
local function current(key, field)
  local v = redis.call("HGET", key, field)
  if not v then return "0" end
  if not valid_nonneg(v) then return nil end
  return v
end

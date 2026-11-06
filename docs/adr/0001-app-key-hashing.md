# ADR 0001: SHA-256 for app keys

## Status

Accepted.

## Context

Comporta stores the keys that apps use to authenticate. If `keys.json` leaks, the attacker must not be able to recover usable keys from it. Verification runs on every request.

The usual advice for stored credentials is a slow, salted function such as bcrypt or Argon2. That advice addresses a specific weakness: passwords are chosen by people, so an attacker can test likely candidates. A slow function makes each guess expensive and a salt makes each guess specific to one record.

## Decision

Keys are 256 random bits from the operating system's secure source, generated only by Comporta. `keys.json` stores the SHA-256 of the complete key, in hexadecimal, with no salt. Verification compares digests with `crypto/subtle.ConstantTimeCompare` across every record, without stopping at a match.

## Why this is enough

There is no dictionary to attack. A guess is a uniformly random 256-bit value, so finding a key from its hash means searching a space of 2^256, which is out of reach at any hash rate. A slow function multiplies the cost of each guess by a constant, and a constant is irrelevant against that number.

A salt defends against precomputed tables and against users sharing a password. Neither applies: no table can cover the space, and two generated keys do not coincide.

This reasoning holds only because operators cannot choose a key. Comporta has no option to import one. A feature that accepted human-chosen keys would have to revisit this decision.

## Alternatives

- **bcrypt or Argon2:** adds tens of milliseconds of CPU per comparison, on every request, without any gain for random keys. With a per-record salt the service could not find a record from its hash, so it would have to try every record or embed a lookup identifier in the key, and the cost per request becomes a denial of service risk. bcrypt also truncates its input at 72 bytes.
- **HMAC with a server secret:** would protect a leaked file even against low-entropy keys, which these are not. It adds a second secret to provision, rotate and back up, and losing it invalidates every key.
- **Encrypting keys so they can be shown again:** a leak of the file and the encryption key would expose working credentials. Showing a key once avoids holding anything reversible.

## Consequences

- A lost key cannot be recovered. The operator revokes it and creates another, and the two active keys per app allow this without downtime.
- Verification costs microseconds, so it adds no meaningful latency to a request.
- A leaked `keys.json` does not expose usable keys, but it does expose key IDs, app names and usage times, which is why the file stays private to the service user.
- The comparison is constant time over the secret bytes. Total time still grows with the number of records, and that number is not secret.

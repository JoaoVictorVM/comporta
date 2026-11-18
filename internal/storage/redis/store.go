// Package redis keeps the shared limit projection of a distributed deployment:
// budgets, reservations, sliding windows, concurrency slots, pauses and loop
// signatures. Decisions are applied by embedded Lua scripts on one standalone
// server, so every key of an admission is on the same node.
package redis

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

//go:embed scripts/integers.lua
var integersLua string

//go:embed scripts/read.lua
var readLua string

//go:embed scripts/admit.lua
var admitLua string

//go:embed scripts/settle.lua
var settleLua string

//go:embed scripts/control.lua
var controlLua string

// Failures of the projection, as fixed categories.
var (
	// ErrUnavailable is a connection failure or timeout: the projection may be
	// intact, and fail modes apply.
	ErrUnavailable = errors.New("shared limit state unavailable")
	// ErrFenced means the epoch this process uses is no longer the published
	// one, or the server is not the one the cluster recorded.
	ErrFenced = errors.New("shared limit state epoch fenced")
	// ErrIntegrity is a script refusal on values or a partial write. It is never
	// eligible for fail_open: the projection is rebuilt from the ledger.
	ErrIntegrity = errors.New("shared limit state integrity failure")
	// ErrServerConfig means the server does not keep reservations durably.
	ErrServerConfig = errors.New("redis must run with appendonly yes, appendfsync always and maxmemory-policy noeviction")
)

// Options configures a Store. Password and the CA come already resolved.
type Options struct {
	Address    string
	Username   string
	Password   string
	Database   int
	TLS        bool
	CAFile     string
	ServerName string
	PoolSize   int
}

// Store is one process's client.
type Store struct {
	client  *goredis.Client
	scripts struct {
		read, admit, settle, control *goredis.Script
	}
	expected atomic.Pointer[string]
	changed  atomic.Bool

	mu   sync.Mutex
	keys Keys
}

func script(body string) *goredis.Script { return goredis.NewScript(integersLua + "\n" + body) }

// Open connects and verifies durability settings. Retries are disabled: a
// mutation is never re-sent by the client after a reconnect that skipped the
// identity check, only by the coordinator with the same idempotency key.
func Open(ctx context.Context, o Options) (*Store, error) {
	s := &Store{}
	options := &goredis.Options{
		Addr: o.Address, Username: o.Username, Password: o.Password, DB: o.Database,
		MaxRetries: -1, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		PoolSize: max(o.PoolSize, 16), ContextTimeoutEnabled: true, DisableIdentity: true,
		OnConnect: s.handshake,
	}
	if o.TLS {
		host, _, _ := net.SplitHostPort(o.Address)
		config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
		if o.ServerName != "" {
			config.ServerName = o.ServerName
		}
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, errors.New("storage.redis.tls.ca_file is unreadable")
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("storage.redis.tls.ca_file holds no certificate")
			}
			config.RootCAs = pool
		}
		options.TLSConfig = config
	}
	s.client = goredis.NewClient(options)
	s.scripts.read = script(readLua)
	s.scripts.admit = script(admitLua)
	s.scripts.settle = script(settleLua)
	s.scripts.control = script(controlLua)
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.client.Ping(call).Err(); err != nil {
		s.client.Close()
		return nil, ErrUnavailable
	}
	return s, nil
}

func (s *Store) Close() error { return s.client.Close() }

// handshake runs on every new or reopened connection, before it carries any
// script: a server whose run identifier differs from the recorded one marks
// the projection as possibly lost and refuses the connection.
func (s *Store) handshake(ctx context.Context, cn *goredis.Conn) error {
	expected := s.expected.Load()
	if expected == nil {
		return nil
	}
	info, err := cn.Info(ctx, "server").Result()
	if err != nil {
		return err
	}
	if runID(info) != *expected {
		// The connection stays usable for identity checks, but every script
		// refuses until the coordinator decided between following the epoch
		// and rebuilding.
		s.changed.Store(true)
	}
	return nil
}

func (s *Store) guard() error {
	if s.changed.Load() {
		return ErrFenced
	}
	return nil
}

func runID(info string) string {
	for line := range strings.SplitSeq(info, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); ok {
			return value
		}
	}
	return ""
}

// RunID reads the identifier of the server process. It changes on every
// restart, which is how a lost or reloaded projection is detected.
func (s *Store) RunID(ctx context.Context) (string, error) {
	// A read is safe to repeat: the second try replaces a pooled connection the
	// restart of the server closed.
	info, err := s.client.Info(ctx, "server").Result()
	if err != nil {
		info, err = s.client.Info(ctx, "server").Result()
	}
	if err != nil {
		return "", ErrUnavailable
	}
	return runID(info), nil
}

// Expect records the run identifier this process trusts.
func (s *Store) Expect(run string) {
	s.changed.Store(false)
	s.expected.Store(&run)
}

// ServerChanged reports whether a reconnection found another server process.
func (s *Store) ServerChanged() bool { return s.changed.Load() }

// UseEpoch selects the namespace of the current epoch.
func (s *Store) UseEpoch(cluster, epoch string) {
	s.mu.Lock()
	s.keys = NewKeys(cluster, epoch)
	s.mu.Unlock()
}

func (s *Store) Keys() Keys {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys
}

func (s *Store) epoch() string {
	k := s.Keys()
	parts := strings.Split(strings.TrimSuffix(k.prefix, ":"), ":")
	return parts[len(parts)-1]
}

// CheckServer verifies that reservations survive a restart and are never
// evicted. INFO is always readable; CONFIG GET may be denied by an ACL, in
// which case appendfsync cannot be confirmed and the start is refused.
func (s *Store) CheckServer(ctx context.Context) error {
	persistence, err := s.client.Info(ctx, "persistence").Result()
	if err != nil {
		return ErrUnavailable
	}
	memory, err := s.client.Info(ctx, "memory").Result()
	if err != nil {
		return ErrUnavailable
	}
	if infoValue(persistence, "aof_enabled") != "1" || infoValue(memory, "maxmemory_policy") != "noeviction" {
		return ErrServerConfig
	}
	fsync, err := s.client.ConfigGet(ctx, "appendfsync").Result()
	if err != nil || fsync["appendfsync"] != "always" {
		return ErrServerConfig
	}
	return nil
}

func infoValue(info, key string) string {
	for line := range strings.SplitSeq(info, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+":"); ok {
			return value
		}
	}
	return ""
}

// classify turns a client error into a category. Script errors are integrity
// failures; network errors and timeouts are unavailability.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrFenced):
		return ErrFenced
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return ErrUnavailable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrUnavailable
	}
	var redisErr goredis.Error
	if errors.As(err, &redisErr) {
		msg := redisErr.Error()
		if strings.HasPrefix(msg, "LOADING") || strings.HasPrefix(msg, "BUSY") || strings.HasPrefix(msg, "MASTERDOWN") || strings.HasPrefix(msg, "NOPERM") {
			return ErrUnavailable
		}
		return ErrIntegrity
	}
	return ErrUnavailable
}

func strs(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		switch x := item.(type) {
		case string:
			out = append(out, x)
		case int64:
			out = append(out, strconv.FormatInt(x, 10))
		}
	}
	return out
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// Snapshot is what one admission reads.
type Snapshot struct {
	State   limits.SharedState
	Version string
	Paused  bool
}

func subjectKeys(app string, user limits.Subject) (string, string) {
	return AppSubject, SubjectKey(user.Hash, user.Anonymous)
}

// Read loads the app and user state of one admission.
func (s *Store) Read(ctx context.Context, app string, user limits.Subject) (Snapshot, error) {
	if err := s.guard(); err != nil {
		return Snapshot{}, err
	}
	var out Snapshot
	k := s.Keys()
	a, u := subjectKeys(app, user)
	keys := []string{k.Meta(), k.Pause(), k.Version(app), k.Budget(app, a), k.Budget(app, u), k.Rate(app, a), k.Rate(app, u), k.Slots(app, a), k.Slots(app, u), k.Zone(app)}
	raw, err := s.scripts.read.Run(ctx, s.client, keys, s.epoch(), app).Result()
	if err != nil {
		return out, classify(err)
	}
	items, _ := raw.([]any)
	if len(items) == 0 {
		return out, ErrIntegrity
	}
	if status, _ := items[0].(string); status != "ok" {
		return out, ErrFenced
	}
	if len(items) != 11 {
		return out, ErrIntegrity
	}
	now, err := strconv.ParseInt(items[1].(string), 10, 64)
	if err != nil {
		return out, ErrIntegrity
	}
	out.Version, out.Paused = items[2].(string), items[3].(string) == "1"
	appSubject := limits.Subject{App: app}
	userSubject := limits.Subject{App: app, Hash: user.Hash, Anonymous: user.Anonymous}
	appBuckets, err := parseBuckets(app, appSubject, strs(items[4]))
	if err != nil {
		return out, ErrIntegrity
	}
	userBuckets, err := parseBuckets(app, userSubject, strs(items[5]))
	if err != nil {
		return out, ErrIntegrity
	}
	appRates, err := parseRates(strs(items[6]))
	if err != nil {
		return out, ErrIntegrity
	}
	userRates, err := parseRates(strs(items[7]))
	if err != nil {
		return out, ErrIntegrity
	}
	concApp, _ := strconv.ParseInt(items[8].(string), 10, 64)
	concUser, _ := strconv.ParseInt(items[9].(string), 10, 64)
	out.State = limits.SharedState{
		Now:        msTime(now),
		Buckets:    append(appBuckets, userBuckets...),
		Rates:      map[limits.Subject][]limits.SharedRate{appSubject: appRates, userSubject: userRates},
		Concurrent: map[limits.Subject]int64{appSubject: concApp, userSubject: concUser},
	}
	if zone := strs(items[10]); len(zone) > 0 {
		z := storage.ZoneState{}
		for i := 0; i+1 < len(zone); i += 2 {
			switch zone[i] {
			case "active":
				z.Active = zone[i+1]
			case "pending":
				z.Pending = zone[i+1]
			case "activate_at":
				z.ActivateAt, _ = strconv.ParseInt(zone[i+1], 10, 64)
			}
		}
		out.State.Zone = &z
	}
	return out, nil
}

// AdmitStatus is the answer of the admission script.
type AdmitStatus string

const (
	AdmitOK        AdmitStatus = "ok"
	AdmitDuplicate AdmitStatus = "dup"
	AdmitStale     AdmitStatus = "stale"
	AdmitPaused    AdmitStatus = "paused"
	AdmitConflict  AdmitStatus = "conflict"
)

// Admission is one plan to apply.
type Admission struct {
	Plan    limits.Plan
	Version string
	Digest  string
	Owner   string
}

func scopeTag(scope string) string {
	if scope == "app" {
		return "a"
	}
	return "u"
}

// Admit applies a plan if the app is still at the version it was read at.
func (s *Store) Admit(ctx context.Context, a Admission) (AdmitStatus, error) {
	if err := s.guard(); err != nil {
		return "", err
	}
	k := s.Keys()
	res := a.Plan.Reservation
	app := res.AppName
	user := SubjectKey(res.Subject, res.Anonymous)
	keys := []string{k.Meta(), k.Pause(), k.Version(app), k.Budget(app, AppSubject), k.Budget(app, user), k.Rate(app, AppSubject), k.Rate(app, user),
		k.Slots(app, AppSubject), k.Slots(app, user), k.Zone(app), k.Receipt(res.RequestID, res.AttemptIndex), k.Owned(a.Owner), k.Subjects(app), k.Pending()}
	slot := "0"
	if a.Plan.Slot {
		slot = "1"
	}
	tokens := int64(0)
	if res.Rate != nil {
		tokens = res.Rate.EstimatedTokens
	}
	args := []any{s.epoch(), a.Version, a.Digest, app, slot, res.RequestID, strconv.Itoa(res.AttemptIndex),
		strconv.FormatInt(res.AdmittedAt.UnixMilli(), 10), strconv.FormatInt(tokens, 10), a.Owner, user, planPayload(res, a.Digest), strconv.Itoa(len(res.Items))}
	for _, item := range res.Items {
		args = append(args, scopeTag(item.Scope), BucketField(item.Dimension, item.WindowStart, item.WindowEnd), strconv.FormatInt(item.Reserved, 10))
	}
	if z := a.Plan.Zone; z != nil {
		args = append(args, "1", z.Active, z.Pending, strconv.FormatInt(z.ActivateAt, 10))
	} else {
		args = append(args, "0", "", "", "0")
	}
	var seeds []limits.SharedBucket
	for _, seed := range a.Plan.Seeds {
		if seed.Subject == "" || (seed.Subject == res.Subject && seed.Anonymous == res.Anonymous) {
			seeds = append(seeds, seed)
		}
	}
	args = append(args, strconv.Itoa(len(seeds)))
	for _, seed := range seeds {
		unknown := "0"
		if seed.Unknown {
			unknown = "1"
		}
		args = append(args, scopeTag(seed.Scope), BucketField(seed.Dimension, seed.WindowStart, seed.WindowEnd),
			strconv.FormatInt(seed.Spent, 10), strconv.FormatInt(seed.Reserved, 10), unknown)
	}
	raw, err := s.scripts.admit.Run(ctx, s.client, keys, args...).Result()
	if err != nil {
		return "", classify(err)
	}
	result := strs(raw)
	if len(result) == 0 {
		return "", ErrIntegrity
	}
	switch status := AdmitStatus(result[0]); status {
	case AdmitOK, AdmitDuplicate, AdmitStale, AdmitPaused, AdmitConflict:
		return status, nil
	case "fenced":
		return "", ErrFenced
	}
	return "", ErrIntegrity
}

// Ending is a settlement or release to project.
type Ending struct {
	Reservation storage.Reservation
	Settlement  storage.Settlement
	Digest      string
}

// SettleStatus is the answer of the settlement script.
type SettleStatus string

const (
	SettleOK      SettleStatus = "ok"
	SettleDup     SettleStatus = "dup"
	SettleUnknown SettleStatus = "unknown"
)

// Settle projects an ending already durable in Postgres.
func (s *Store) Settle(ctx context.Context, e Ending) (SettleStatus, error) {
	if err := s.guard(); err != nil {
		return "", err
	}
	k := s.Keys()
	res, st := e.Reservation, e.Settlement
	if len(st.Settled) != len(res.Items) {
		return "", storage.ErrInvalid
	}
	app := res.AppName
	user := SubjectKey(res.Subject, res.Anonymous)
	keys := []string{k.Meta(), k.Version(app), k.Budget(app, AppSubject), k.Budget(app, user), k.Rate(app, AppSubject), k.Rate(app, user),
		k.Receipt(res.RequestID, res.AttemptIndex), k.Pending()}
	rate := ""
	if st.RateTokens != nil {
		rate = strconv.FormatInt(*st.RateTokens, 10)
	}
	args := []any{s.epoch(), st.Transition, e.Digest, res.RequestID, strconv.Itoa(res.AttemptIndex), rate, strconv.Itoa(len(res.Items))}
	for i, item := range res.Items {
		args = append(args, scopeTag(item.Scope), BucketField(item.Dimension, item.WindowStart, item.WindowEnd),
			strconv.FormatInt(item.Reserved, 10), strconv.FormatInt(st.Settled[i], 10))
	}
	raw, err := s.scripts.settle.Run(ctx, s.client, keys, args...).Result()
	if err != nil {
		return "", classify(err)
	}
	result := strs(raw)
	if len(result) == 0 {
		return "", ErrIntegrity
	}
	switch status := SettleStatus(result[0]); status {
	case SettleOK, SettleDup, SettleUnknown:
		return status, nil
	case "fenced":
		return "", ErrFenced
	}
	return "", ErrIntegrity
}

// SlotEntry names a concurrency slot in its owner's set.
func SlotEntry(app, userKey, request string) string { return app + "|" + userKey + "|" + request }

// ReleaseSlot frees the concurrency slot of a logical request. It is
// idempotent: a second call finds nothing to remove.
func (s *Store) ReleaseSlot(ctx context.Context, owner, app string, user limits.Subject, request string) error {
	k := s.Keys()
	userKey := SubjectKey(user.Hash, user.Anonymous)
	keys := []string{k.Slots(app, AppSubject), k.Slots(app, userKey), k.Owned(owner), k.Version(app)}
	_, err := s.scripts.control.Run(ctx, s.client, keys, "slot", request, SlotEntry(app, userKey, request)).Result()
	return classify(err)
}

// Rollback undoes an admission whose SQL commit is proven absent.
func (s *Store) Rollback(ctx context.Context, owner, digest string, res storage.Reservation) error {
	k := s.Keys()
	app := res.AppName
	user := SubjectKey(res.Subject, res.Anonymous)
	keys := []string{k.Meta(), k.Version(app), k.Budget(app, AppSubject), k.Budget(app, user), k.Rate(app, AppSubject), k.Rate(app, user),
		k.Slots(app, AppSubject), k.Slots(app, user), k.Owned(owner), k.Receipt(res.RequestID, res.AttemptIndex), k.Pending()}
	args := []any{"rollback", s.epoch(), digest, res.RequestID, strconv.Itoa(res.AttemptIndex), SlotEntry(app, user, res.RequestID), strconv.Itoa(len(res.Items))}
	for _, item := range res.Items {
		args = append(args, scopeTag(item.Scope), BucketField(item.Dimension, item.WindowStart, item.WindowEnd), strconv.FormatInt(item.Reserved, 10))
	}
	raw, err := s.scripts.control.Run(ctx, s.client, keys, args...).Result()
	if err != nil {
		return classify(err)
	}
	if result := strs(raw); len(result) == 0 || (result[0] != "ok" && result[0] != "skip") {
		return ErrIntegrity
	}
	return nil
}

// Confirm removes an admission from the orphan watch once its SQL commit is
// confirmed.
func (s *Store) Confirm(ctx context.Context, request string, attempt int) error {
	k := s.Keys()
	_, err := s.scripts.control.Run(ctx, s.client, []string{k.Pending(), k.Receipt(request, attempt)}, "confirm", request+"|"+strconv.Itoa(attempt)).Result()
	return classify(err)
}

// Orphans lists admissions older than the cutoff whose commit was never
// confirmed.
func (s *Store) Orphans(ctx context.Context, before time.Time, limit int64) ([]string, error) {
	k := s.Keys()
	members, err := s.client.ZRangeByScore(ctx, k.Pending(), &goredis.ZRangeBy{Min: "-inf", Max: strconv.FormatInt(before.UnixMilli(), 10), Count: limit}).Result()
	return members, classify(err)
}

// SetPause replaces the pause projection with the durable state of a control
// revision. An older revision never overwrites a newer one.
func (s *Store) SetPause(ctx context.Context, revision int64, global bool, apps []string) error {
	k := s.Keys()
	flag := "0"
	if global {
		flag = "1"
	}
	args := []any{"pause", s.epoch(), strconv.FormatInt(revision, 10), flag}
	for _, app := range apps {
		args = append(args, app)
	}
	raw, err := s.scripts.control.Run(ctx, s.client, []string{k.Meta(), k.Pause()}, args...).Result()
	if err != nil {
		return classify(err)
	}
	if result := strs(raw); len(result) == 0 || (result[0] != "ok" && result[0] != "older") {
		return ErrFenced
	}
	return nil
}

// Publish makes a filled namespace the current one in a single write.
func (s *Store) Publish(ctx context.Context, revision int64) error {
	k := s.Keys()
	_, err := s.scripts.control.Run(ctx, s.client, []string{k.Meta()}, "publish", s.epoch(), strconv.FormatInt(revision, 10), "normal").Result()
	return classify(err)
}

// Fence closes the namespace of an epoch, so no replica still on it admits.
func (s *Store) Fence(ctx context.Context, cluster, epoch string) error {
	k := NewKeys(cluster, epoch)
	_, err := s.scripts.control.Run(ctx, s.client, []string{k.Meta()}, "fence", epoch).Result()
	return classify(err)
}

// Empty reports whether a namespace holds no key, which a bootstrap requires.
func (s *Store) Empty(ctx context.Context, prefix string) (bool, error) {
	keys, _, err := s.client.Scan(ctx, 0, prefix+"*", 10).Result()
	if err != nil {
		return false, ErrUnavailable
	}
	return len(keys) == 0, nil
}

// DropNamespace deletes an old epoch's keys in batches. It never flushes the
// database, which other clusters or epochs may share.
func (s *Store) DropNamespace(ctx context.Context, cluster, epoch string) error {
	prefix := NewKeys(cluster, epoch).Prefix()
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, prefix+"*", 500).Result()
		if err != nil {
			return ErrUnavailable
		}
		if len(keys) > 0 {
			if err := s.client.Unlink(ctx, keys...).Err(); err != nil {
				return ErrUnavailable
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// Client exposes the connection to the rebuild, which writes a namespace
// before it is published.
func (s *Store) Client() *goredis.Client { return s.client }

// SeedApp applies a timezone activation to every subject of an app: the new
// windows are charged with what overlapping old windows hold. It adds deltas
// with HINCRBY, so a settlement projected at the same time is never lost.
func (s *Store) SeedApp(ctx context.Context, app, timezone string, at time.Time, seeded ...string) error {
	k := s.Keys()
	subjects, err := s.client.SMembers(ctx, k.Subjects(app)).Result()
	if err != nil {
		return classify(err)
	}
	pipe := s.client.TxPipeline()
	for _, key := range subjects {
		// The admission script already seeded the subjects it decided on.
		if key == AppSubject || slices.Contains(seeded, key) {
			continue
		}
		hash, anonymous, ok := ParseSubjectKey(key)
		if !ok {
			return ErrIntegrity
		}
		fields, err := s.client.HGetAll(ctx, k.Budget(app, key)).Result()
		if err != nil {
			return classify(err)
		}
		flat := make([]string, 0, len(fields)*2)
		for name, value := range fields {
			flat = append(flat, name, value)
		}
		buckets, err := parseBuckets(app, limits.Subject{App: app, Hash: hash, Anonymous: anonymous}, flat)
		if err != nil {
			return ErrIntegrity
		}
		before := map[string]limits.SharedBucket{}
		for _, b := range buckets {
			before[BucketField(b.Dimension, b.WindowStart, b.WindowEnd)] = b
		}
		for _, seed := range limits.ActivationSeeds(app, timezone, at, buckets) {
			field := BucketField(seed.Dimension, seed.WindowStart, seed.WindowEnd)
			delta := seed.Spent - before[field].Spent
			if delta > 0 {
				pipe.HIncrBy(ctx, k.Budget(app, key), field+"|s", delta)
			}
			if seed.Unknown {
				pipe.HSet(ctx, k.Budget(app, key), field+"|u", "1")
			}
		}
	}
	pipe.Incr(ctx, k.Version(app))
	_, err = pipe.Exec(ctx)
	return classify(err)
}

// planPayload is what the orphan watch needs to undo an admission whose SQL
// commit never happened: the reservation itself, nothing else.
func planPayload(res storage.Reservation, digest string) string {
	data, _ := json.Marshal(struct {
		Reservation storage.Reservation `json:"reservation"`
		Digest      string              `json:"digest"`
	}{res, digest})
	return string(data)
}

// Orphan is an admission applied in Redis whose commit was never confirmed.
type Orphan struct {
	Reservation storage.Reservation
	Digest      string
	Owner       string
}

// LoadOrphan reads the plan kept in the receipt of an unconfirmed admission.
func (s *Store) LoadOrphan(ctx context.Context, member string) (Orphan, bool, error) {
	request, attempt, ok := strings.Cut(member, "|")
	index, err := strconv.Atoi(attempt)
	if !ok || err != nil {
		return Orphan{}, false, ErrIntegrity
	}
	values, err := s.client.HMGet(ctx, s.Keys().Receipt(request, index), "plan", "owner", "end").Result()
	if err != nil {
		return Orphan{}, false, classify(err)
	}
	plan, _ := values[0].(string)
	owner, _ := values[1].(string)
	if values[2] != nil || plan == "" {
		_ = s.Confirm(ctx, request, index)
		return Orphan{}, false, nil
	}
	var payload struct {
		Reservation storage.Reservation `json:"reservation"`
		Digest      string              `json:"digest"`
	}
	if err := json.Unmarshal([]byte(plan), &payload); err != nil {
		return Orphan{}, false, ErrIntegrity
	}
	return Orphan{Reservation: payload.Reservation, Digest: payload.Digest, Owner: owner}, true, nil
}

// ReleaseOwner frees every concurrency slot a dead process held.
func (s *Store) ReleaseOwner(ctx context.Context, owner string) error {
	k := s.Keys()
	entries, err := s.client.SMembers(ctx, k.Owned(owner)).Result()
	if err != nil {
		return classify(err)
	}
	for _, entry := range entries {
		parts := strings.SplitN(entry, "|", 3)
		if len(parts) != 3 {
			continue
		}
		hash, anonymous, ok := ParseSubjectKey(parts[1])
		if !ok {
			continue
		}
		if err := s.ReleaseSlot(ctx, owner, parts[0], limits.Subject{App: parts[0], Hash: hash, Anonymous: anonymous}, parts[2]); err != nil {
			return err
		}
	}
	return classify(s.client.Del(ctx, k.Owned(owner)).Err())
}

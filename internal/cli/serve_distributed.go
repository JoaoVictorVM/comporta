package cli

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/admin"
	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/distributed"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/metering"
	"github.com/JoaoVictorVM/comporta/internal/panel"
	"github.com/JoaoVictorVM/comporta/internal/panel/ui"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/responsecache"
	"github.com/JoaoVictorVM/comporta/internal/runtimecontrol"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/server"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
	redisstore "github.com/JoaoVictorVM/comporta/internal/storage/redis"
	"github.com/JoaoVictorVM/comporta/internal/telemetry"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

// drainNotice gives the balancer time to see /readyz fail before the
// listeners stop accepting.
const drainNotice = 5 * time.Second

// serveDistributed runs one replica of a distributed deployment. Everything
// that must agree across replicas comes from the shared stores or from the
// shared secrets; nothing is generated per pod. Every check runs before the
// listeners open, so a misconfigured replica never takes traffic.
func serveDistributed(ctx context.Context, out streams, logger *slog.Logger, manager *config.Manager, catalog *pricing.Catalog, configEvents *metering.ConfigEvents) error {
	fail := func(message string) error {
		logger.Error(message)
		return errReported
	}
	snapshot := manager.Capture()
	deployment := snapshot.Deployment()
	hmacKey, err := config.ReadSharedKey("deployment.hmac_key_file", deployment.HMACKeyFile)
	if err != nil {
		return fail(err.Error())
	}
	cursorKey, err := config.ReadSharedKey("deployment.cursor_key_file", deployment.CursorKeyFile)
	if err != nil {
		return fail(err.Error())
	}
	identity, err := useridentity.FromSharedKey(hmacKey)
	if err != nil {
		return fail("deployment.hmac_key_file must contain exactly 32 bytes")
	}
	dsn, ok := snapshot.StorageSecret("storage.postgres.dsn")
	if !ok {
		return fail("storage.postgres.dsn is not resolved")
	}
	sql, err := postgres.Open(ctx, postgres.Options{DSN: dsn.Reveal(), MaxConnections: int(snapshot.Storage().Postgres.MaxConnections)})
	if err != nil {
		return fail("shared database unavailable; listeners not opened")
	}
	defer sql.Close()
	if err := sql.Migrate(ctx, 60*time.Second); err != nil {
		return fail(err.Error())
	}
	r := snapshot.Storage().Redis
	redisOptions := redisstore.Options{Address: r.Address, Username: r.Username, Database: int(r.Database), TLS: r.TLS.Enabled, CAFile: r.TLS.CAFile, ServerName: r.TLS.ServerName}
	if secret, ok := snapshot.StorageSecret("storage.redis.password"); ok {
		redisOptions.Password = secret.Reveal()
	}
	redis, err := redisstore.Open(ctx, redisOptions)
	if err != nil {
		return fail("shared Redis unavailable; listeners not opened")
	}
	defer redis.Close()
	if err := redis.CheckServer(ctx); err != nil {
		return fail(err.Error())
	}
	cluster := deployment.ClusterID
	identityOf := func() distributed.Identity {
		s := manager.Capture()
		return distributed.Identity{Revision: s.Deployment().ConfigRevision, ConfigFingerprint: s.DistributedFingerprint(), IdentityFingerprint: identity.Fingerprint()}
	}
	state, bootErr := distributed.Bootstrap(ctx, sql, redis, cluster, identityOf())
	switch {
	case bootErr == nil, errors.Is(bootErr, distributed.ErrRecoveryNeeded):
	default:
		var known = []error{distributed.ErrIdentityMismatch, distributed.ErrRevisionConflict, distributed.ErrRevisionBehind, distributed.ErrNamespaceInUse, postgres.ErrClusterMismatch}
		for _, k := range known {
			if errors.Is(bootErr, k) {
				return fail(bootErr.Error())
			}
		}
		return fail("cluster bootstrap failed; listeners not opened")
	}
	configEvents.Publisher = sql
	var lifecycle *distributed.Lifecycle
	coordinator := distributed.New(distributed.Options{
		Cluster: cluster, SQL: sql, Redis: redis, Logger: logger, Publisher: sql,
		Revision: func() int64 { return manager.Capture().Deployment().ConfigRevision },
		Recover: func(reason string) {
			if lifecycle != nil {
				lifecycle.Signal(reason)
			}
		},
	})
	coordinator.UseEpoch(state.Epoch, state.Mode)
	accounting := metering.New(sql, catalog, logger, func() int64 { return manager.Capture().Storage().RetentionDays })
	control := runtimecontrol.New(coordinator.Pauses())
	cache := responsecache.New(responsecache.Options{MaxBytes: snapshot.Cache().MaxBytes})
	epochs := responsecache.NewEpochTracker()
	lifecycle, err = distributed.Start(ctx, distributed.LifecycleOptions{
		Coordinator: coordinator, SQL: sql, Redis: redis, Cluster: cluster, Identity: identityOf, Logger: logger, Publisher: sql,
		Quarantine: func() time.Duration { return loopQuarantine(manager.Capture()) },
		OnControl: func(ctx context.Context, _ int64) error {
			if err := control.Reload(ctx); err != nil {
				return err
			}
			revisions, err := sql.CacheEpochs(ctx)
			if err != nil {
				return err
			}
			if apps := epochs.Changed(revisions); len(apps) > 0 {
				cache.PurgeApps(apps)
			}
			// Blocked records live only in memory until flushed; a deletion
			// waits for this acknowledgement so none of them can come back.
			return accounting.Flush(ctx)
		},
	})
	if err != nil {
		return fail("replica lease not acquired; listeners not opened")
	}
	coordinator.UseLeases(lifecycle)
	if errors.Is(bootErr, distributed.ErrRecoveryNeeded) {
		if err := coordinator.Rebuild(ctx, loopQuarantine(snapshot)); err != nil {
			return fail("shared limit state could not be rebuilt; listeners not opened")
		}
	}
	if err := accounting.UseLimits(coordinator, sql, manager); err != nil {
		return err
	}
	if err := control.Load(ctx); err != nil {
		return fail("pause state could not be loaded; listeners not opened")
	}
	if revisions, err := sql.CacheEpochs(ctx); err == nil {
		epochs.Changed(revisions)
	}
	keySource, err := keys.NewSharedSource(ctx, sql.Keys(), logger)
	if err != nil {
		return fail("app keys unavailable; listeners not opened")
	}
	credential, err := admin.OpenSharedCredential(ctx, sql)
	if err != nil {
		return fail(admin.ErrNoSharedCredential.Error())
	}
	observer := telemetry.New(telemetry.Options{Source: manager, Logger: logger})
	defer observer.Close()
	accounting.UseTelemetry(observer)
	_ = observer.Register(telemetry.NewClusterCollector(func(ctx context.Context) telemetry.ClusterHealth {
		h := telemetry.ClusterHealth{Mode: coordinator.Mode(), LocalBacklog: coordinator.Backlog(), Ready: lifecycle.Ready()}
		h.PostgresUp = sql.Ping(ctx) == nil
		_, err := redis.RunID(ctx)
		h.RedisUp = err == nil
		h.OutboxAge, h.OutboxPending, _ = sql.OldestPending(ctx)
		return h
	}))
	// Every replica evaluates and delivers alerts from the shared store; claims
	// and unique keys keep them from repeating an alert or a call.
	alertWorker := newAlerts(sql, manager, &sql.Bus, logger, observer)
	loopGuard := distributed.NewLoopGuard(redis, manager, identity.Derive("comporta:loop-signature:v1"), logger)
	loop := &proxy.LoopStage{Guard: loopGuard, Source: manager, Recorder: accounting}
	sensitive := &proxy.SensitiveStage{Scanner: sensitivedata.NewScanner(), Recorder: accounting}
	originKey := identity.Derive("comporta:auth-origin:v1")
	sessions := admin.NewSharedSessions(sql, credential)
	adminGuard := auth.NewSharedGuard(sql.AuthWindows(), "admin", originKey, logger,
		auth.GuardOptions{MaxFailures: admin.GuardMaxFailures, Window: admin.GuardWindow, Cooldown: admin.GuardCooldown})
	adminService := admin.NewService(admin.ServiceOptions{
		Control: control, Source: manager, Identity: identity, History: coordinator.History(),
		Metering: accounting, Limits: sharedLimits{}, Loop: loopGuard, Cache: cache, Usage: sql, Logger: logger,
		CursorKey: cursorKey, FreshPause: true,
	})
	adminHandler := admin.NewHandler(admin.HandlerOptions{
		Credential: credential, Logger: logger, Sessions: sessions, Guard: adminGuard, Service: adminService,
	})
	// Other replicas write too: the hub follows the shared watermark once per
	// second besides the local bus. Limit state lives in the shared stores, so
	// the panel shows it as not verifiable here instead of a local copy.
	live := panel.NewLiveHub(panel.LiveOptions{Bus: &sql.Bus, Config: manager, Watermark: sql.PanelWatermark, Logger: logger})
	panelHandler := panel.NewHandler(panel.HandlerOptions{
		Service: panel.NewService(panel.Options{
			Store: sql, Config: manager, Pause: adminService, Identity: identity, History: coordinator.History(), Logger: logger, CursorKey: cursorKey,
			Alerts: alertReader{store: sql, source: manager},
		}),
		Views: ui.Views{}, Credential: credential, Sessions: sessions, Guard: adminGuard, Live: live, Logger: logger,
	})
	cacheWorker := func(ctx context.Context) error {
		return cache.Run(ctx, func() responsecache.Settings { return responsecache.FromSnapshot(manager.Capture()) })
	}
	eventsWorker := func(ctx context.Context) error { return observer.RunEvents(ctx, &sql.Bus) }
	logger.Info("replica joined the cluster", "config_revision", deployment.ConfigRevision)
	return runGatewayWith(ctx, logger, manager, gatewayParts{
		keys: keySource, keysRun: keySource.Run,
		guard:    auth.NewSharedGuard(sql.AuthWindows(), "proxy", originKey, logger, auth.GuardOptions{}),
		identity: identity, accounting: accounting, gate: control, loop: loop, sensitive: sensitive, cache: cache, observer: observer,
		adminAPI: adminHandler, metrics: adminHandler.Metrics(observer),
		panel: panelHandler, live: live,
		ready:   lifecycle.Ready,
		onDrain: lifecycle.Drain, drainNotice: drainNotice,
		extra: []server.Worker{configEvents.Run, lifecycle.Run, cacheWorker, observer.Run, eventsWorker, alertWorker.Run},
	})
}

// sharedLimits is the limit side of a deletion in distributed mode: the
// shared projection is cleaned by the deletion job itself, after the history
// commits, so here only the history write runs.
type sharedLimits struct{}

func (sharedLimits) ForgetSubjects(_ []storage.SubjectRef, persist func() error) (int, error) {
	return 0, persist()
}

func (sharedLimits) Apps() []string { return nil }

// loopQuarantine is the longest window loop history could still matter in.
func loopQuarantine(snapshot *config.Snapshot) time.Duration {
	longest := time.Minute
	for _, name := range snapshot.AppNames() {
		if app, ok := snapshot.App(name); ok && app.LoopGuard.Enabled {
			longest = max(longest, app.LoopGuard.SimilarWindow, app.LoopGuard.Cooldown)
		}
	}
	return longest
}

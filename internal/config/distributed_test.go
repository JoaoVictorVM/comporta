package config

import (
	"strings"
	"testing"
	"time"
)

const distributedYAML = `deployment:
  mode: distributed
  replicas: 2
  cluster_id: gateway-test
  config_revision: 1
  hmac_key_file: /run/secrets/identity/hmac.key
  cursor_key_file: /run/secrets/identity/cursor.key
storage:
  backend: postgres
  postgres:
    dsn_file: /run/secrets/postgres/dsn
  redis:
    address: redis.database.svc:6379
    username: comporta
    password_file: /run/secrets/redis/password
`

func TestDistributedDefaultsPreserveSingle(t *testing.T) {
	candidate := requireValid(t, baseYAML)
	dep, st := candidate.cfg.Deployment, candidate.cfg.Storage
	if dep.Mode != ModeSingle || dep.Replicas != 1 || dep.Distributed() {
		t.Fatalf("deployment defaults changed: %+v", dep)
	}
	if st.Backend != BackendSQLite || st.RetentionDays != 30 || st.Postgres.MaxConnections != 10 || !st.Redis.TLS.Enabled {
		t.Fatalf("storage defaults changed: %+v", st)
	}
	snapshot := newSnapshot(1, time.Unix(0, 0), candidate)
	if strings.Contains(snapshot.EffectiveYAML(), "postgres") || strings.Contains(snapshot.EffectiveYAML(), "redis") {
		t.Fatal("a single instance must not show distributed backends")
	}
}

func TestSQLiteRejectsReplicas(t *testing.T) {
	_, diags := loadText(t, baseYAML+"deployment:\n  replicas: 2\n", structuralOptions())
	found := false
	for _, d := range diags {
		if d.String() == ErrSQLiteReplicas {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the documented message, got %+v", diags)
	}
	requireValid(t, baseYAML+"deployment:\n  replicas: 1\n")
}

func TestDistributedRequiresBothStores(t *testing.T) {
	requireValid(t, baseYAML+distributedYAML)
	cases := map[string]struct{ old, new, path string }{
		"sqlite backend":   {"backend: postgres", "backend: sqlite", "storage.backend"},
		"no redis":         {"    address: redis.database.svc:6379\n", "", "storage.redis.address"},
		"no postgres":      {"    dsn_file: /run/secrets/postgres/dsn\n", "    max_connections: 4\n", "storage.postgres.dsn"},
		"no cluster":       {"  cluster_id: gateway-test\n", "", "deployment.cluster_id"},
		"no revision":      {"  config_revision: 1\n", "", "deployment.config_revision"},
		"no hmac":          {"  hmac_key_file: /run/secrets/identity/hmac.key\n", "", "deployment.hmac_key_file"},
		"no cursor":        {"  cursor_key_file: /run/secrets/identity/cursor.key\n", "", "deployment.cursor_key_file"},
		"same keys":        {"/run/secrets/identity/cursor.key", "/run/secrets/identity/hmac.key", "deployment.cursor_key_file"},
		"sentinel":         {"redis.database.svc:6379", "a:1,b:2", "storage.redis.address"},
		"credentials":      {"redis.database.svc:6379", "user:pw@redis:6379", "storage.redis.address"},
		"remote plain":     {"    username: comporta\n", "    username: comporta\n    tls: {enabled: false}\n", "storage.redis.tls.enabled"},
		"remote no secret": {"    password_file: /run/secrets/redis/password\n", "", "storage.redis.password"},
		"bad cluster":      {"cluster_id: gateway-test", "cluster_id: Gateway_Test", "deployment.cluster_id"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			requireDiagnostic(t, baseYAML+replaced(t, distributedYAML, c.old, c.new), CodeInvalidValue, c.path)
		})
	}
	requireValid(t, baseYAML+replaced(t, replaced(t, distributedYAML, "redis.database.svc:6379", "127.0.0.1:6379"),
		"    password_file: /run/secrets/redis/password\n", "    tls: {enabled: false}\n"))
	requireDiagnostic(t, baseYAML+"storage:\n  backend: postgres\n", CodeInvalidValue, "storage.backend")
	requireDiagnostic(t, baseYAML+"deployment:\n  cluster_id: lonely\n", CodeInvalidValue, "deployment.cluster_id")
}

func TestDistributedSecretReferences(t *testing.T) {
	requireDiagnostic(t, baseYAML+replaced(t, distributedYAML, "dsn_file: /run/secrets/postgres/dsn", "dsn: postgres://u:p@db/x"), CodeLiteralSecret, "storage.postgres.dsn")
	requireDiagnostic(t, baseYAML+replaced(t, distributedYAML, "password_file: /run/secrets/redis/password", "password: hunter2"), CodeLiteralSecret, "storage.redis.password")
	requireValid(t, baseYAML+replaced(t, distributedYAML, "dsn_file: /run/secrets/postgres/dsn", "dsn: ${POSTGRES_DSN}"))
	requireDiagnostic(t, baseYAML+replaced(t, distributedYAML, "dsn_file: /run/secrets/postgres/dsn", "dsn: ${POSTGRES_DSN}\n    dsn_file: /x"), CodeInvalidValue, "storage.postgres")

	env := NewEnvironment(map[string]string{"ANTHROPIC_API_KEY": "k", "POSTGRES_DSN": "postgres://u:secretpw@db.example.com/x?sslmode=require", "REDIS_PASSWORD": "pw"})
	text := baseYAML + replaced(t, replaced(t, distributedYAML, "dsn_file: /run/secrets/postgres/dsn", "dsn: ${POSTGRES_DSN}"),
		"password_file: /run/secrets/redis/password", "password: ${REDIS_PASSWORD}")
	_, diags := loadText(t, text, Options{SystemTimezone: "UTC", ResolveSecrets: true, Environment: env})
	if !hasDiagnostic(diags, CodeInvalidValue, "storage.postgres.dsn") {
		t.Fatalf("a remote DSN without verify-full must be refused, got %+v", diags)
	}
	for _, d := range diags {
		if strings.Contains(d.Message, "secretpw") || strings.Contains(d.Message, "db.example.com") {
			t.Fatalf("diagnostic echoes the DSN: %q", d.Message)
		}
	}
	env = NewEnvironment(map[string]string{"ANTHROPIC_API_KEY": "k", "POSTGRES_DSN": "postgres://u:secretpw@db.example.com/x?sslmode=verify-full", "REDIS_PASSWORD": "pw"})
	candidate, diags := loadText(t, text, Options{SystemTimezone: "UTC", ResolveSecrets: true, Environment: env})
	if candidate == nil {
		t.Fatalf("verify-full DSN refused: %+v", diags)
	}
	snapshot := newSnapshot(1, time.Unix(0, 0), candidate)
	if secret, ok := snapshot.StorageSecret("storage.postgres.dsn"); !ok || !strings.Contains(secret.Reveal(), "verify-full") {
		t.Fatal("DSN not resolved")
	}
	if strings.Contains(snapshot.EffectiveYAML(), "secretpw") || strings.Contains(snapshot.EffectiveYAML(), "/run/secrets/postgres") {
		t.Fatal("effective configuration exposes a storage secret or its path")
	}
	for _, dsn := range []string{"host=127.0.0.1 user=x dbname=y", "postgres://u@localhost:5432/x"} {
		if problem := CheckPostgresDSN(dsn); problem != "" {
			t.Fatalf("loopback DSN refused: %s", problem)
		}
	}
	if CheckPostgresDSN("host=db.internal sslmode=disable") == "" {
		t.Fatal("remote plain DSN accepted")
	}
}

func TestClusterRevisionFingerprint(t *testing.T) {
	load := func(text string) *Snapshot {
		candidate := requireValid(t, text)
		return newSnapshot(1, time.Unix(0, 0), candidate)
	}
	base := load(baseYAML + distributedYAML)
	same := load(baseYAML + replaced(t, distributedYAML, "replicas: 2", "replicas: 3") + "listeners:\n  proxy: {bind: 0.0.0.0, port: 9000}\ntelemetry:\n  metrics_enabled: false\n")
	if base.DistributedFingerprint() != same.DistributedFingerprint() {
		t.Fatal("bind, replicas and telemetry must not change the fingerprint")
	}
	if len(base.DistributedFingerprint()) != 64 {
		t.Fatal("fingerprint is not a SHA-256 hex digest")
	}
	revision := load(baseYAML + replaced(t, distributedYAML, "config_revision: 1", "config_revision: 2"))
	if base.DistributedFingerprint() != revision.DistributedFingerprint() {
		t.Fatal("the revision is compared apart from the fingerprint")
	}
	changed := load(replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 3.00") + distributedYAML)
	if base.DistributedFingerprint() == changed.DistributedFingerprint() {
		t.Fatal("a budget change must change the fingerprint")
	}
	if base.Deployment().ConfigRevision != 1 || base.Deployment().ClusterID != "gateway-test" {
		t.Fatalf("deployment accessor: %+v", base.Deployment())
	}
}

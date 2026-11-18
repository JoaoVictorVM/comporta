package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ErrSQLiteReplicas is the documented refusal for several replicas on SQLite:
// each process would keep its own counters and multiply every budget.
const ErrSQLiteReplicas = "SQLite storage does not support multiple replicas; configure redis and postgres"

var clusterIDPattern = namePattern

// validateDeployment checks the combinations of deployment mode and storage
// backend. Secrets are only checked for presence here; serve resolves them
// and verifies the connections.
func validateDeployment(cfg *Config, d *decoder, baseDir string) {
	dep, st := &cfg.Deployment, &cfg.Storage
	if dep.Mode == ModeSingle {
		if st.Backend == BackendSQLite && dep.Replicas > 1 {
			position := d.positions["deployment.replicas"]
			d.diags.add(Diagnostic{Code: CodeInvalidValue, Line: position.Line, Column: position.Column, Message: ErrSQLiteReplicas})
		}
		if st.Backend == BackendPostgres {
			d.reportAt(CodeInvalidValue, "storage.backend", "postgres requires deployment.mode distributed")
		}
		for _, field := range []string{"deployment.cluster_id", "deployment.config_revision", "deployment.hmac_key_file", "deployment.cursor_key_file", "storage.postgres", "storage.redis"} {
			if _, declared := d.positions[field]; declared {
				d.reportAt(CodeInvalidValue, field, "only applies to deployment.mode distributed")
			}
		}
		return
	}
	if st.Backend != BackendPostgres {
		d.reportAt(CodeInvalidValue, "storage.backend", "deployment.mode distributed requires postgres")
	}
	if dep.ClusterID == "" {
		d.reportAt(CodeInvalidValue, "deployment.cluster_id", "is required in distributed mode")
	} else if !clusterIDPattern.MatchString(dep.ClusterID) {
		d.reportAt(CodeInvalidValue, "deployment.cluster_id", "must match "+clusterIDPattern.String())
	}
	if dep.ConfigRevision <= 0 {
		d.reportAt(CodeInvalidValue, "deployment.config_revision", "is required in distributed mode")
	}
	for field, value := range map[string]*string{"deployment.hmac_key_file": &dep.HMACKeyFile, "deployment.cursor_key_file": &dep.CursorKeyFile} {
		if *value == "" {
			d.reportAt(CodeInvalidValue, field, "is required in distributed mode")
			continue
		}
		if !filepath.IsAbs(*value) {
			*value = filepath.Join(baseDir, *value)
		}
		*value = filepath.Clean(*value)
	}
	if dep.HMACKeyFile != "" && dep.HMACKeyFile == dep.CursorKeyFile {
		d.reportAt(CodeInvalidValue, "deployment.cursor_key_file", "must differ from hmac_key_file")
	}
	if !st.Postgres.DSN.IsSet() {
		d.reportAt(CodeInvalidValue, "storage.postgres.dsn", "dsn or dsn_file is required in distributed mode")
	}
	validateRedis(&st.Redis, d)
}

func validateRedis(r *RedisStorage, d *decoder) {
	const path = "storage.redis"
	if r.Address == "" {
		d.reportAt(CodeInvalidValue, joinPath(path, "address"), "is required in distributed mode")
		return
	}
	host, port, err := net.SplitHostPort(r.Address)
	if err != nil || host == "" || strings.ContainsAny(r.Address, "@/,;?# ") {
		d.reportAt(CodeInvalidValue, joinPath(path, "address"), "must be one host:port endpoint without credentials; Sentinel and Cluster are not supported")
		return
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		d.reportAt(CodeInvalidValue, joinPath(path, "address"), "port must be between 1 and 65535")
		return
	}
	loopback := isLoopbackHost(host)
	if !r.TLS.Enabled && !loopback {
		d.reportAt(CodeInvalidValue, joinPath(joinPath(path, "tls"), "enabled"), "TLS can only be disabled for a loopback address")
	}
	if !r.Password.IsSet() && !loopback {
		d.reportAt(CodeInvalidValue, joinPath(path, "password"), "password or password_file is required for a non-loopback address")
	}
	if r.TLS.CAFile != "" && !r.TLS.Enabled {
		d.reportAt(CodeInvalidValue, joinPath(joinPath(path, "tls"), "ca_file"), "requires tls.enabled")
	}
}

// isLoopbackHost accepts only addresses that never leave the host, which is
// where plain transport to a test database is tolerated.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && addr.Unmap().IsLoopback()
}

// CheckPostgresDSN verifies a resolved DSN without echoing it: a remote server
// must be reached with full certificate and host name verification. It returns
// a fixed message, never a fragment of the value.
func CheckPostgresDSN(dsn string) string {
	host, mode := "", ""
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil || parsed.Host == "" {
			return "storage.postgres.dsn must be a postgres URL or key=value DSN"
		}
		host, mode = parsed.Hostname(), parsed.Query().Get("sslmode")
	} else {
		for field := range strings.FieldsSeq(dsn) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				return "storage.postgres.dsn must be a postgres URL or key=value DSN"
			}
			switch key {
			case "host":
				host = value
			case "sslmode":
				mode = value
			}
		}
	}
	if host == "" {
		return "storage.postgres.dsn must name a host"
	}
	if mode != "verify-full" && !isLoopbackHost(host) {
		return "storage.postgres.dsn must use sslmode=verify-full for a non-loopback host"
	}
	return ""
}

// DistributedFingerprint is the canonical SHA-256 of every definition that
// changes an admission decision. Bind addresses, telemetry destinations, the
// pod name and secret values are excluded, so equal revisions on different
// pods produce equal fingerprints.
func (s *Snapshot) DistributedFingerprint() string {
	cfg := &s.cfg
	apps := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Apps)) {
		apps[name] = publicApp(cfg.Apps[name])
	}
	providers := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[name]
		entry := map[string]any{"api_format": string(p.APIFormat), "base_url": p.BaseURL, "models": stringsToAny(p.Models), "tls_min_version": p.TLSMinVersion}
		addSecret(entry, "api_key", p.APIKey)
		providers[name] = entry
	}
	pricing := map[string]any{}
	for model, price := range cfg.Pricing {
		pricing[model] = []string{price.Input.String(), price.Output.String(), price.CacheWrite.String(), price.CacheRead.String()}
	}
	tree := map[string]any{
		"timezone": cfg.Timezone, "fail_mode": string(cfg.FailMode), "apps": apps, "providers": providers,
		"pricing": pricing, "cache_max_bytes": cfg.Cache.MaxBytes, "cluster_id": cfg.Deployment.ClusterID,
	}
	// encoding/json sorts map keys, which makes the rendering canonical.
	data, _ := json.Marshal(tree)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

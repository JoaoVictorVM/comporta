package server

import (
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

var echoableHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// NormalizeHost lowercases a Host value and removes a syntactically valid port.
// Anything else that a host could smuggle (userinfo, zones, wildcards, paths) makes
// it malformed, so the later comparison can stay a plain equality.
func NormalizeHost(raw string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, "@/\\?#%* \t,") {
		return "", false
	}
	host := strings.ToLower(raw)
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return "", false
		}
		addr, err := netip.ParseAddr(host[1:end])
		if err != nil || !addr.Is6() {
			return "", false
		}
		if rest := host[end+1:]; rest != "" && (rest[0] != ':' || !validPort(rest[1:])) {
			return "", false
		}
		return host[:end+1], true
	}
	name, port, hasPort := strings.Cut(host, ":")
	if name == "" || (hasPort && !validPort(port)) {
		return "", false
	}
	return name, true
}

func validPort(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	value, err := strconv.Atoi(port)
	return err == nil && value >= 1 && value <= 65535
}

// checkHost returns whether the Host is allowed and, when it is not, the form of
// it that is safe to repeat in the error message. Forwarded headers are never read.
func checkHost(raw string, snapshot *config.Snapshot) (allowed bool, safeHost string) {
	host, ok := NormalizeHost(raw)
	if !ok {
		return false, ""
	}
	if slices.Contains(snapshot.Listeners().AllowedHosts, host) {
		return true, ""
	}
	if echoableHost.MatchString(host) {
		return false, host
	}
	return false, ""
}

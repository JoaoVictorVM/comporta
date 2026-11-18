//go:build kubernetes

// Package kubernetes installs the official chart on a kind cluster: two
// replicas against Redis and Postgres served with TLS, the hardened security
// context, enforced network policies and a rolling restart that keeps the
// shared budget. It needs docker, kind, kubectl and helm; a missing tool fails
// the test instead of skipping it.
package kubernetes

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const (
	clusterName = "comporta-it"
	namespace   = "comporta-it"
	release     = "gw"
	image       = "comporta:kind-it"
)

func run(t *testing.T, stdin string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s\n%s", name, strings.Join(args, " "), err, out.String(), errOut.String())
	}
	return out.String()
}

func kubectl(t *testing.T, args ...string) string {
	return run(t, "", "kubectl", append([]string{"--context", "kind-" + clusterName, "-n", namespace}, args...)...)
}

func apply(t *testing.T, manifest string) {
	run(t, manifest, "kubectl", "--context", "kind-"+clusterName, "-n", namespace, "apply", "-f", "-")
}

// certificates issues a test CA and one server certificate for the stores.
func certificates(t *testing.T) (ca, cert, key []byte) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "comporta-it-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	names := []string{"redis", "postgres"}
	for _, n := range []string{"redis", "postgres"} {
		names = append(names, n+"."+namespace+".svc", n+"."+namespace+".svc.cluster.local")
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "stores"}, DNSNames: names, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	caCert, _ := x509.ParseCertificate(caDER)
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(serverKey)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return fmt.Sprintf("%x", raw)
}

func secretFiles(t *testing.T, name string, files map[string][]byte) {
	dir := t.TempDir()
	args := []string{"create", "secret", "generic", name}
	for k, v := range files {
		path := filepath.Join(dir, k)
		if err := os.WriteFile(path, v, 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--from-file="+k+"="+path)
	}
	kubectl(t, args...)
}

// TestHelmInstallTwoReplicas installs the chart and runs every check on the
// same release, since a kind cluster takes minutes to prepare.
func TestHelmInstallTwoReplicas(t *testing.T) {
	for _, tool := range []string{"docker", "kind", "kubectl", "helm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required for the kind test", tool)
		}
	}
	root, _ := filepath.Abs("../..")
	run(t, "", "kind", "create", "cluster", "--name", clusterName, "--wait", "180s")
	t.Cleanup(func() { _ = exec.Command("kind", "delete", "cluster", "--name", clusterName).Run() })
	run(t, "", "docker", "build", "-t", image, root)
	run(t, "", "kind", "load", "docker-image", image, "--name", clusterName)
	run(t, "", "kubectl", "--context", "kind-"+clusterName, "create", "namespace", namespace)

	ca, cert, key := certificates(t)
	storePassword, redisPassword := randomHex(16), randomHex(16)
	secretFiles(t, "store-tls", map[string][]byte{"ca.crt": ca, "tls.crt": cert, "tls.key": key})
	secretFiles(t, "store-password", map[string][]byte{"password": []byte(storePassword)})
	secretFiles(t, "comporta-redis", map[string][]byte{"password": []byte(redisPassword), "ca.crt": ca})
	dsn := fmt.Sprintf("postgres://postgres:%s@postgres.%s.svc:5432/postgres?sslmode=verify-full&sslrootcert=/run/secrets/postgres/ca.crt", storePassword, namespace)
	secretFiles(t, "comporta-postgres", map[string][]byte{"dsn": []byte(dsn), "ca.crt": ca})
	hmacKey, cursorKey := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(hmacKey)
	_, _ = rand.Read(cursorKey)
	secretFiles(t, "comporta-identity", map[string][]byte{"hmac.key": hmacKey, "cursor.key": cursorKey})
	secretFiles(t, "comporta-providers", map[string][]byte{"fake": []byte("fake-provider-key")})
	for _, f := range []string{"redis.yaml", "postgres.yaml"} {
		data, err := os.ReadFile(filepath.Join("fixtures", f))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, string(data))
	}
	apply(t, fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: fake}
spec:
  selector: {matchLabels: {app: fake}}
  template:
    metadata: {labels: {app: fake, role: provider}}
    spec:
      containers:
        - name: fake
          image: %s
          imagePullPolicy: Never
          args: ["fake-provider", "--bind", "0.0.0.0", "--port", "9090"]
---
apiVersion: v1
kind: Service
metadata: {name: fake}
spec:
  selector: {app: fake}
  ports: [{port: 9090}]
`, image))
	rollout(t, "redis", "180")
	rollout(t, "postgres", "180")
	rollout(t, "fake", "180")
	fakeIP := strings.TrimSpace(kubectl(t, "get", "service", "fake", "-o", "jsonpath={.spec.clusterIP}"))

	values := fmt.Sprintf(`replicas: 2
image: {repository: comporta, tag: kind-it, pullPolicy: Never}
cluster: {id: kind-it, configRevision: 1}
storage:
  redis: {address: "redis.%[1]s.svc:6379", username: default, tls: {enabled: true}}
config:
  timezone: UTC
  providers:
    fake:
      api_format: anthropic
      base_url: http://%[2]s:9090
      api_key_file: /run/secrets/providers/fake
      models: [fake-claude]
  pricing:
    fake-claude: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
  apps:
    bot:
      allowed_models: [fake-claude]
      budget: {usd_per_day: 0.01}
networkPolicy:
  enabled: true
  proxyFrom: [{podSelector: {matchLabels: {access: proxy}}}]
  adminFrom: [{podSelector: {matchLabels: {access: admin}}}]
  egress:
    - to: [{podSelector: {matchLabels: {role: store}}}]
      ports: [{port: 5432, protocol: TCP}, {port: 6379, protocol: TCP}]
    - to: [{podSelector: {matchLabels: {role: provider}}}]
      ports: [{port: 9090, protocol: TCP}]
`, namespace, fakeIP)
	valuesPath := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesPath, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "", "helm", "--kube-context", "kind-"+clusterName, "-n", namespace, "install", release, filepath.Join(root, "deploy", "helm"), "-f", valuesPath)

	// The admin token is created once for the cluster, by a job with the same
	// image, configuration and secrets as the replicas.
	token := bootstrap(t)
	rollout(t, release, "300")

	t.Run("HardenedSecurityContext", func(t *testing.T) { checkSecurityContext(t) })
	t.Run("ProbeHostAllowed", func(t *testing.T) {
		ready := kubectl(t, "get", "deployment", release, "-o", "jsonpath={.status.readyReplicas}")
		if strings.TrimSpace(ready) != "2" {
			t.Fatalf("both replicas must pass their probes, ready=%s", ready)
		}
	})
	appKey := regexp.MustCompile(`(cmp_[A-Za-z0-9]+)`).FindString(job(t, "keys", "keys", "create", "bot"))
	if appKey == "" {
		t.Fatal("no app key")
	}
	t.Run("TrafficAndNetworkPolicyEnforced", func(t *testing.T) { checkTraffic(t, appKey) })
	t.Run("RollingRestartPreservesBudgets", func(t *testing.T) { checkRollingRestart(t, appKey, token) })
}

const curlImage = "curlimages/curl:8.17.0@sha256:935d9100e9ba842cdb060de42472c7ca90cfe9a7c96e4dacb55e79e560b3ff40"

// probe runs curl from a pod with the given labels and returns its output.
func probe(t *testing.T, labels, script string) (string, error) {
	name := "probe-" + randomHex(4)
	cmd := exec.Command("kubectl", "--context", "kind-"+clusterName, "-n", namespace, "run", name, "--rm", "-i", "--restart=Never", "--quiet",
		"--labels="+labels, "--image="+curlImage, "--command", "--", "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func call(key string) string {
	content, _ := json.Marshal(testkit.Prompt("hi"))
	return fmt.Sprintf(`curl -s -m 5 -o /dev/null -w '%%{http_code}' -H 'Host: %[1]s-proxy' -H 'x-api-key: %[2]s' -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' `+
		`-d '{"model":"fake-claude","max_tokens":16,"messages":[{"role":"user","content":%[3]s}]}' http://%[1]s-proxy:8080/v1/messages`, release, key, content)
}

func checkTraffic(t *testing.T, key string) {
	out, err := probe(t, "access=proxy", call(key))
	if err != nil || !strings.Contains(out, "200") {
		t.Fatalf("an allowed client must reach the proxy: %s %v", out, err)
	}
	out, _ = probe(t, "access=none", call(key))
	if strings.Contains(out, "200") {
		t.Fatalf("a client outside proxyFrom must be blocked by the policy: %s", out)
	}
	out, _ = probe(t, "access=proxy", fmt.Sprintf(`curl -s -m 5 -o /dev/null -w '%%{http_code}' -H 'Host: %[1]s-admin' http://%[1]s-admin:8081/healthz`, release))
	if strings.Contains(out, "200") {
		t.Fatalf("the admin listener must not be reachable from proxy clients: %s", out)
	}
}

func checkSecurityContext(t *testing.T) {
	raw := kubectl(t, "get", "pods", "-l", "app.kubernetes.io/instance="+release, "-o", "json")
	var list struct {
		Items []struct {
			Spec struct {
				AutomountServiceAccountToken *bool `json:"automountServiceAccountToken"`
				SecurityContext              struct {
					RunAsNonRoot *bool `json:"runAsNonRoot"`
					RunAsUser    *int  `json:"runAsUser"`
					Seccomp      *struct {
						Type string `json:"type"`
					} `json:"seccompProfile"`
				} `json:"securityContext"`
				Containers []struct {
					SecurityContext struct {
						ReadOnlyRootFilesystem   *bool `json:"readOnlyRootFilesystem"`
						AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation"`
						Capabilities             struct {
							Drop []string `json:"drop"`
						} `json:"capabilities"`
					} `json:"securityContext"`
					VolumeMounts []struct {
						SubPath string `json:"subPath"`
					} `json:"volumeMounts"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list.Items) < 2 {
		t.Fatalf("pods: %v", err)
	}
	for _, pod := range list.Items {
		sc := pod.Spec.SecurityContext
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 65532 || sc.Seccomp == nil || sc.Seccomp.Type != "RuntimeDefault" {
			t.Fatal("pod must run as non-root 65532 with RuntimeDefault seccomp")
		}
		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Fatal("pod must not mount a service account token")
		}
		c := pod.Spec.Containers[0].SecurityContext
		if c.ReadOnlyRootFilesystem == nil || !*c.ReadOnlyRootFilesystem || c.AllowPrivilegeEscalation == nil || *c.AllowPrivilegeEscalation || len(c.Capabilities.Drop) != 1 || c.Capabilities.Drop[0] != "ALL" {
			t.Fatal("container must have a read-only root, no privilege escalation and every capability dropped")
		}
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			if m.SubPath != "" {
				t.Fatal("secrets must be mounted without subPath")
			}
		}
	}
}

func checkRollingRestart(t *testing.T, key, token string) {
	for range 3 {
		if out, err := probe(t, "access=proxy", call(key)); err != nil || !strings.Contains(out, "200") {
			t.Fatalf("call before restart: %s", out)
		}
	}
	before := spent(t, token)
	kubectl(t, "rollout", "restart", "deployment/"+release)
	rollout(t, release, "300")
	after := spent(t, token)
	if after < before || before == "" {
		t.Fatalf("a rolling restart must keep the shared budget: before %q after %q", before, after)
	}
}

// spent reads the app's usage through the admin API, from a client the
// admin policy allows.
func spent(t *testing.T, token string) string {
	script := fmt.Sprintf(`curl -s -m 5 -X POST -H 'Host: %[1]s-admin' -H 'Authorization: Bearer %[2]s' -H 'content-type: application/json' -d '{"app":"bot","since":"7d"}' http://%[1]s-admin:8081/admin/v1/usage/query`, release, token)
	out, err := probe(t, "access=admin", script)
	if err != nil {
		t.Fatalf("usage: %s %v", out, err)
	}
	return regexp.MustCompile(`"cost_microusd":"([0-9]+)"`).FindString(out)
}

// job runs one CLI command with the release's image, configuration and secrets.
func job(t *testing.T, name string, args ...string) string {
	jobName := name + "-" + randomHex(3)
	quoted, _ := json.Marshal(append(args, "--config", "/etc/comporta/comporta.yaml"))
	apply(t, fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata: {name: %[1]s}
spec:
  backoffLimit: 0
  template:
    metadata: {labels: {role: job, app.kubernetes.io/name: comporta-job}}
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: cli
          image: %[2]s
          imagePullPolicy: Never
          args: %[3]s
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          volumeMounts:
            - {name: config, mountPath: /etc/comporta, readOnly: true}
            - {name: identity, mountPath: /run/secrets/identity, readOnly: true}
            - {name: postgres, mountPath: /run/secrets/postgres, readOnly: true}
            - {name: redis, mountPath: /run/secrets/redis, readOnly: true}
            - {name: providers, mountPath: /run/secrets/providers, readOnly: true}
            - {name: tmp, mountPath: /tmp}
      volumes:
        - {name: config, configMap: {name: %[4]s}}
        - {name: identity, secret: {secretName: comporta-identity, defaultMode: 0440}}
        - {name: postgres, secret: {secretName: comporta-postgres, defaultMode: 0440}}
        - {name: redis, secret: {secretName: comporta-redis, defaultMode: 0440}}
        - {name: providers, secret: {secretName: comporta-providers, defaultMode: 0440}}
        - {name: tmp, emptyDir: {medium: Memory}}
`, jobName, image, quoted, release))
	deadline := time.Now().Add(180 * time.Second)
	for {
		status := kubectl(t, "get", "job", jobName, "-o", "jsonpath={.status.succeeded}/{.status.failed}")
		if strings.HasPrefix(status, "1/") {
			return kubectl(t, "logs", "job/"+jobName)
		}
		if strings.HasSuffix(status, "/1") || time.Now().After(deadline) {
			t.Fatalf("job %s did not complete (%s)\n%s", strings.Join(args, " "), status, diagnose("job-name="+jobName))
		}
		time.Sleep(2 * time.Second)
	}
}

// rollout waits for a deployment and explains a failure.
func rollout(t *testing.T, name, seconds string) {
	t.Helper()
	out, err := exec.Command("kubectl", "--context", "kind-"+clusterName, "-n", namespace, "rollout", "status", "deployment/"+name, "--timeout="+seconds+"s").CombinedOutput()
	if err != nil {
		selector := "app=" + name
		if name == release {
			selector = "app.kubernetes.io/instance=" + release
		}
		t.Fatalf("deployment %s not ready: %s\n%s", name, out, diagnose(selector))
	}
}

// diagnose collects what explains a stuck workload: its logs, its pod
// description and the namespace events. Secrets never reach them: the job
// prints only fixed messages and the token, which this test discards.
func diagnose(selector string) string {
	var b strings.Builder
	for _, args := range [][]string{
		{"logs", "-l", selector, "--all-containers", "--tail=50"},
		{"describe", "pods", "-l", selector},
		{"get", "events", "--sort-by=.lastTimestamp"},
		{"get", "pods", "-o", "wide"},
	} {
		out, _ := exec.Command("kubectl", append([]string{"--context", "kind-" + clusterName, "-n", namespace}, args...)...).CombinedOutput()
		b.WriteString("$ kubectl " + strings.Join(args, " ") + "\n" + string(out) + "\n")
	}
	return b.String()
}

// bootstrap migrates the shared database and creates the admin token. The
// jobs reach the stores through a policy of their own.
func bootstrap(t *testing.T) string {
	apply(t, `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: jobs}
spec:
  podSelector: {matchLabels: {role: job}}
  policyTypes: [Egress]
  egress:
    - ports: [{port: 53, protocol: UDP}, {port: 53, protocol: TCP}]
    - to: [{podSelector: {matchLabels: {role: store}}}]
`)
	job(t, "migrate", "storage", "migrate")
	token := regexp.MustCompile(`store it now\): ([0-9a-f]{64})`).FindStringSubmatch(job(t, "token", "admin", "reset-token"))
	if token == nil {
		t.Fatal("no admin token")
	}
	return token[1]
}

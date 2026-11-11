//go:build container

package integration

import (
	"archive/tar"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const containerProject = "comporta-container-test"

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestContainerUsageSurvivesRestartAndCrash(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "comporta.yaml")
	override := filepath.Join(dir, "compose.override.yaml")
	text := `data_dir: /data
timezone: UTC
listeners:
  proxy: {bind: 0.0.0.0}
  admin: {bind: 0.0.0.0}
providers:
  fake:
    api_format: openai
    base_url: http://127.0.0.1:9090/v1
    models: [fake-gpt]
pricing:
  fake-gpt: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
apps:
  my-app: {allowed_models: [fake-gpt]}
`
	// This bind-mounted fixture has no secret values and must be readable by
	// container UID 65532, which differs from the host runner's owner.
	if err := os.WriteFile(config, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	overrideText := `services:
  fake:
    image: comporta:local
    network_mode: service:comporta
    read_only: true
    user: "65532:65532"
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    command: ["fake-provider", "--port", "9090"]
`
	if err := os.WriteFile(override, []byte(overrideText), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPORTA_CONFIG", config)
	t.Setenv("COMPOSE_FILE", filepath.Join(repoRoot(t), "compose.yaml")+string(os.PathListSeparator)+override)
	key := startProvisioned(t)
	// usage asks the running gateway, so it needs the admin token the first
	// start printed. It reaches exec through the environment, not argv.
	token := regexp.MustCompile(`store it now\): ([0-9a-f]{64})`).FindStringSubmatch(compose(t, "logs", "--no-log-prefix", "comporta"))
	if token == nil {
		t.Fatal("the first start did not show the admin token")
	}
	t.Setenv("COMPORTA_ADMIN_TOKEN", token[1])
	var status int
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/chat/completions", strings.NewReader(testkit.OpenAIBody()))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+key)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			status = response.StatusCode
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if status == 200 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status != 200 {
		t.Fatal("fake provider did not answer through container")
	}
	compose(t, "kill", "-s", "SIGKILL", "comporta")
	compose(t, "up", "-d", "comporta")
	waitForHealth(t)
	first := compose(t, "exec", "-T", "-e", "COMPORTA_ADMIN_TOKEN", "comporta", "/comporta", "usage", "--app", "my-app", "--config", "/etc/comporta/comporta.yaml")
	if !strings.Contains(first, "estimated_cost_usd") || strings.Contains(first, "total - 0 0 0.000000") {
		t.Fatal("crash recovery did not preserve usage")
	}
	compose(t, "restart", "comporta")
	waitForHealth(t)
	second := compose(t, "exec", "-T", "-e", "COMPORTA_ADMIN_TOKEN", "comporta", "/comporta", "usage", "--app", "my-app", "--config", "/etc/comporta/comporta.yaml")
	totals := func(s string) string {
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "total ") {
				return line
			}
		}
		return ""
	}
	if totals(first) == "" || totals(first) != totals(second) {
		t.Fatal("restart changed totals")
	}
	for _, name := range []string{"usage.sqlite", "usage.sqlite-wal", "usage.sqlite-shm"} {
		reader := tar.NewReader(bytes.NewReader(copyFromContainer(t, "/data/"+name)))
		header, err := reader.Next()
		if err != nil || header.Mode&0077 != 0 || header.Uid != 65532 {
			t.Fatal("SQLite artifact not private")
		}
	}
	// A copied, checkpointed snapshot also verifies that the history is queryable
	// outside the container without relying solely on CLI text.
	compose(t, "stop", "comporta")
	containerID := strings.TrimSpace(compose(t, "ps", "-aq", "comporta"))
	output := filepath.Join(t.TempDir(), "usage.sqlite")
	if data, err := exec.Command("docker", "cp", containerID+":/data/usage.sqlite", output).CombinedOutput(); err != nil {
		t.Fatalf("copy database: %v %s", err, data)
	}
	// The source owner differs from the host user; the CLI already exercised the
	// private volume. Use the driver's read-only connection for this snapshot.
	db, err := sql.Open("sqlite", output+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var requests int64
	if err = db.QueryRow(`SELECT sum(requests) FROM usage_daily WHERE scope='app' AND app_name='my-app'`).Scan(&requests); err != nil || requests < 1 {
		t.Fatal("copied database has no app totals")
	}
}

func compose(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command("docker", append([]string{"compose", "-p", containerProject}, args...)...)
	command.Dir = repoRoot(t)
	// The credential is synthetic: these tests never reach a provider.
	command.Env = append(os.Environ(), "ANTHROPIC_API_KEY=synthetic-container-key")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func TestComposeLoopbackOnly(t *testing.T) {
	var rendered struct {
		Services map[string]struct {
			Ports []struct {
				HostIP    string `json:"host_ip"`
				Published string `json:"published"`
				Target    int    `json:"target"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(compose(t, "config", "--format", "json")), &rendered); err != nil {
		t.Fatal(err)
	}
	ports := rendered.Services["comporta"].Ports
	if len(ports) != 2 {
		t.Fatalf("ports %+v", ports)
	}
	for _, port := range ports {
		if port.HostIP != "127.0.0.1" {
			t.Errorf("port %d published on %q", port.Target, port.HostIP)
		}
		if port.Published != fmt.Sprint(port.Target) {
			t.Errorf("port %d published as %s", port.Target, port.Published)
		}
	}
}

func waitForHealth(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://127.0.0.1:8081/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the service did not become healthy:\n%s", compose(t, "logs"))
}

func modelsStatus(t *testing.T, key string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Api-Key", key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

// startProvisioned builds the image, creates a key in the volume and starts the
// service, which refuses to run on a volume without a key store.
func startProvisioned(t *testing.T) string {
	t.Helper()
	t.Cleanup(func() { compose(t, "down", "-v", "--remove-orphans") })
	compose(t, "build")
	created := compose(t, "run", "--rm", "--no-deps", "comporta", "keys", "create", "my-app", "--config", "/etc/comporta/comporta.yaml")
	key := regexp.MustCompile(`cmp_[0-9A-Za-z]{43}`).FindString(created)
	if key == "" {
		t.Fatalf("no key printed: %s", created)
	}
	compose(t, "up", "-d")
	waitForHealth(t)
	return key
}

func TestContainerNonRootReadOnlyAndNoCaps(t *testing.T) {
	startProvisioned(t)
	containerID := strings.TrimSpace(compose(t, "ps", "-q", "comporta"))
	output, err := exec.Command("docker", "inspect", containerID).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect: %v\n%s", err, output)
	}
	var inspected []struct {
		Config struct {
			User string
		}
		HostConfig struct {
			ReadonlyRootfs bool
			CapDrop        []string
			SecurityOpt    []string
			PortBindings   map[string][]struct {
				HostIP string
			}
		}
	}
	if err := json.Unmarshal(output, &inspected); err != nil || len(inspected) != 1 {
		t.Fatalf("inspect output: %v", err)
	}
	host := inspected[0].HostConfig
	if inspected[0].Config.User != "65532:65532" {
		t.Errorf("user %q", inspected[0].Config.User)
	}
	if !host.ReadonlyRootfs {
		t.Error("the root filesystem is writable")
	}
	if !slices.Contains(host.CapDrop, "ALL") {
		t.Errorf("capabilities %v", host.CapDrop)
	}
	if !slices.Contains(host.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("security options %v", host.SecurityOpt)
	}
	for port, bindings := range host.PortBindings {
		for _, binding := range bindings {
			if binding.HostIP != "127.0.0.1" {
				t.Errorf("%s bound to %q", port, binding.HostIP)
			}
		}
	}
}

func TestContainerProvisionAndRestart(t *testing.T) {
	key := startProvisioned(t)
	if logs := compose(t, "logs"); strings.Contains(logs, key) || strings.Contains(logs, "synthetic-container-key") {
		t.Fatal("startup logs expose a secret")
	}
	if status := modelsStatus(t, key); status != http.StatusOK {
		t.Fatalf("models status %d", status)
	}
	if status := modelsStatus(t, "cmp_invalid"); status != http.StatusUnauthorized {
		t.Fatalf("invalid key status %d", status)
	}

	compose(t, "down")
	compose(t, "up", "-d")
	waitForHealth(t)
	if status := modelsStatus(t, key); status != http.StatusOK {
		t.Fatalf("the key did not survive recreating the container: %d", status)
	}
}

// copyFromContainer returns what is at a path of the service container as a tar stream,
// which also carries owner and mode. The image has no shell to ask with. Standard output
// is kept apart and never printed, because it can hold the key.
func copyFromContainer(t *testing.T, path string) []byte {
	t.Helper()
	containerID := strings.TrimSpace(compose(t, "ps", "-q", "comporta"))
	output, err := exec.Command("docker", "cp", containerID+":"+path, "-").Output()
	if err != nil {
		t.Fatalf("docker cp %s: %v", path, err)
	}
	return output
}

func readKeyEntry(t *testing.T) (*tar.Header, []byte) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(copyFromContainer(t, "/data/hmac.key")))
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return header, data
}

func TestContainerHMACKeyPersistsAcrossRestart(t *testing.T) {
	key := startProvisioned(t)
	_, first := readKeyEntry(t)
	if len(first) != 32 {
		t.Fatalf("the key has %d bytes", len(first))
	}
	compose(t, "restart")
	waitForHealth(t)
	_, afterRestart := readKeyEntry(t)
	compose(t, "down")
	compose(t, "up", "-d")
	waitForHealth(t)
	_, afterRecreate := readKeyEntry(t)
	// The pseudonym is a function of the key, the app and the ID, so the same bytes in
	// the volume are the same pseudonym for every user.
	if !bytes.Equal(first, afterRestart) || !bytes.Equal(first, afterRecreate) {
		t.Fatal("the identity key changed, which would break every user's history")
	}
	if status := modelsStatus(t, key); status != http.StatusOK {
		t.Fatalf("models status %d", status)
	}
}

func TestContainerIdentityKeyIsPrivate(t *testing.T) {
	startProvisioned(t)
	header, data := readKeyEntry(t)
	if header.Mode&0o077 != 0 || header.Mode&0o600 != 0o600 {
		t.Errorf("mode %o", header.Mode)
	}
	if header.Uid != 65532 {
		t.Errorf("owner %d", header.Uid)
	}
	if logs := compose(t, "logs"); strings.Contains(logs, fmt.Sprintf("%x", data)) {
		t.Error("the logs expose the identity key")
	}
}

func TestContainerPrivacyCanary(t *testing.T) {
	key := startProvisioned(t)
	scanner := testkit.NewScanner(testkit.Needles()...)
	// These requests are refused before any provider call, so a synthetic provider
	// credential is enough and nothing leaves the host.
	for name, user := range map[string]string{"email": testkit.EmailID(), "spaced": testkit.SpacedID(), "document": testkit.DocumentID()} {
		request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/messages", strings.NewReader(testkit.AnthropicBody()))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", key)
		request.Header.Set("X-Comporta-User", user)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, response.StatusCode)
		}
		scanner.Scan("errors", name, answer)
	}
	scanner.Scan("logs", "container", []byte(compose(t, "logs")))
	reader := tar.NewReader(bytes.NewReader(copyFromContainer(t, "/data")))
	files := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		files++
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		scanner.Scan("files", filepath.Base(header.Name), data)
	}
	if files == 0 {
		t.Fatal("the volume held nothing to inspect")
	}
	if err := scanner.Err("errors", "logs", "files"); err != nil {
		t.Fatal(err)
	}
}

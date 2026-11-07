//go:build container

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
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

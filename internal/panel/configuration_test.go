package panel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectedReloadHighlighted(t *testing.T) {
	env := newEnv(t)
	c := env.service.Configuration()
	if !c.Available || c.Rejected || !strings.Contains(c.YAML, "apps:") {
		t.Fatalf("configuration: %+v", c)
	}
	path := filepath.Join(filepath.Dir(env.manager.Capture().DataDir()), "comporta.yaml")
	if err := os.WriteFile(path, []byte("apps: [this is not valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.manager.Reload(context.Background())
	c = env.service.Configuration()
	if !c.Rejected || len(c.Diagnostics) == 0 || c.Generation != 1 || !strings.Contains(c.YAML, "bot:") {
		t.Fatalf("rejected reload: %+v", c)
	}
}

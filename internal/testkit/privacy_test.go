package testkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryConstructorCarriesTheMarker(t *testing.T) {
	for name, build := range Constructors() {
		body := build()
		if !strings.Contains(body, Marker()) {
			t.Errorf("%s has no marker", name)
		}
		if !json.Valid([]byte(body)) {
			t.Errorf("%s is not JSON", name)
		}
		if strings.Contains(body, UserMarker()) {
			t.Errorf("%s mixes the markers", name)
		}
	}
}

func TestMarkersAreDistinctAndValidShape(t *testing.T) {
	if !markerShape.MatchString(Marker()) {
		t.Fatal("marker shape")
	}
	if Marker() == UserMarker() || strings.Contains(UserMarker(), Marker()) {
		t.Fatal("the user marker must be told apart from the prompt marker")
	}
	if !strings.Contains(WithField(OpenAIBody(), `"user":"x"`), `,"user":"x"}`) {
		t.Fatal("WithField")
	}
}

func TestLoadMarkerRefusesMalformedValue(t *testing.T) {
	t.Setenv(MarkerEnv, "short")
	defer func() {
		if recover() == nil {
			t.Fatal("a malformed marker must stop the suite")
		}
	}()
	loadMarker()
}

func TestLoadMarkerUsesTheSharedValue(t *testing.T) {
	const shared = "cmpcanary0123456789abcdef0123456789abcdef"
	t.Setenv(MarkerEnv, shared)
	if got := loadMarker(); got != shared {
		t.Fatalf("got %s", got)
	}
}

// Controls: each kind of destination must detect the needle in each spelling.
func TestScannerDetectsRawAndEscapedForms(t *testing.T) {
	needle := "cmpuserabc@mail.example"
	quoted, err := json.Marshal(needle)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"raw":          "level=info value=" + needle + " end",
		"json encoder": `{"value":` + string(quoted) + `}`,
		"at escaped":   `{"value":"cmpuserabc@mail.example"}`,
		"all escaped":  `{"value":"` + unicodeEscaped(needle, true) + `"}`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			scanner := NewScanner(needle)
			scanner.Scan("logs", "case", []byte(text))
			findings := scanner.Findings()
			if len(findings) == 0 {
				t.Fatal("not detected")
			}
			if err := scanner.Err("logs"); err == nil || strings.Contains(err.Error(), needle) {
				t.Fatalf("the report must fail without printing the needle: %v", err)
			}
		})
	}
}

func TestScannerCleanOutput(t *testing.T) {
	scanner := NewScanner("needle-one")
	scanner.Scan("logs", "case", []byte("nothing of interest"))
	if err := scanner.Err("logs"); err != nil {
		t.Fatal(err)
	}
	if scanner.Inspected("logs") != 1 {
		t.Fatal("inspected count")
	}
}

func TestScannerFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "output.log"), []byte("a needle-one b"), 0o600); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner("needle-one")
	scanner.ScanDir("files", dir)
	findings := scanner.Findings()
	if len(findings) == 0 || findings[0].Channel != "files" || findings[0].Case != "output.log" {
		t.Fatalf("findings %v", findings)
	}
}

func TestScannerEmptyOrUnreadableIsAFailure(t *testing.T) {
	t.Run("empty directory", func(t *testing.T) {
		scanner := NewScanner("needle-one")
		scanner.ScanDir("files", t.TempDir())
		if scanner.Err() == nil {
			t.Fatal("an empty directory must not pass")
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		scanner := NewScanner("needle-one")
		scanner.ScanDir("files", filepath.Join(t.TempDir(), "missing"))
		if scanner.Err() == nil {
			t.Fatal("a missing directory must not pass")
		}
	})
	t.Run("unreadable file", func(t *testing.T) {
		scanner := NewScanner("needle-one")
		scanner.ScanFile("files", filepath.Join(t.TempDir(), "missing.log"))
		if scanner.Err() == nil {
			t.Fatal("an unreadable file must not pass")
		}
	})
	t.Run("channel never inspected", func(t *testing.T) {
		scanner := NewScanner("needle-one")
		scanner.Scan("logs", "case", []byte("clean"))
		if scanner.Err("logs", "errors") == nil {
			t.Fatal("a required channel without outputs must not pass")
		}
	})
}

func TestInventoryRefusesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.json")
	if err := os.WriteFile(path, []byte(`{"constructor":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInventory(path); err == nil {
		t.Fatal("a misspelled field must not be ignored")
	}
}

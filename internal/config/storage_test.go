package config

import "testing"

func TestStorageRetentionBounds(t *testing.T) {
	for _, value := range []string{"0", "366", "-1", "1.5"} {
		if _, err := LoadBytes([]byte(baseYAML+"storage:\n  retention_days: "+value+"\n"), "comporta.yaml", structuralOptions()); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	for _, value := range []string{"1", "30", "365"} {
		if _, err := LoadBytes([]byte(baseYAML+"storage:\n  retention_days: "+value+"\n"), "comporta.yaml", structuralOptions()); err != nil {
			t.Fatal(err)
		}
	}
}

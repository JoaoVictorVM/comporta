package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDotenvRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "FROM_DOTENV=dotenv-value\n")
	writeFile(t, filepath.Join(t.TempDir(), ".env"), "OTHER=never-read\n")

	env, err := CaptureEnvironment(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := env.Lookup("FROM_DOTENV"); !ok || value != "dotenv-value" {
		t.Fatalf("variable from .env beside the file: %q %v", value, ok)
	}
	if _, ok := env.Lookup("OTHER"); ok {
		t.Fatal("only the .env next to the configuration may be read")
	}
}

func TestMissingDotenvIsNormal(t *testing.T) {
	env, err := CaptureEnvironment(t.TempDir(), []string{"A=1", "=C:=C:\\x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := env.Lookup("A"); value != "1" {
		t.Fatalf("process variable: %q", value)
	}
	if _, ok := env.Lookup(""); ok {
		t.Fatal("entries without a name must be skipped")
	}
}

func TestEnvironmentWinsIncludingEmpty(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "SET=dotenv\nEMPTY=dotenv\nONLY=dotenv\n")
	env, err := CaptureEnvironment(dir, []string{"SET=process", "EMPTY="}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"SET": "process", "EMPTY": "", "ONLY": "dotenv"} {
		if got, _ := env.Lookup(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestDotenvSyntaxErrorHidesContent(t *testing.T) {
	dir := t.TempDir()
	const canary = "canary-dotenv-value"
	writeFile(t, filepath.Join(dir, ".env"), canary+"\n")
	_, err := CaptureEnvironment(dir, nil, nil)
	if err == nil {
		t.Fatal("invalid .env must fail")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("error exposes .env content: %v", err)
	}
}

func TestDotenvNotReloaded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "KEY=first\n")
	env, err := CaptureEnvironment(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".env"), "KEY=second\n")
	if value, _ := env.Lookup("KEY"); value != "first" {
		t.Fatalf("captured environment changed: %q", value)
	}
}

func TestEnvironmentNotMutated(t *testing.T) {
	dir := t.TempDir()
	const name = "COMPORTA_TEST_DOTENV_ONLY"
	writeFile(t, filepath.Join(dir, ".env"), name+"=value\n")
	if _, err := CaptureEnvironment(dir, os.Environ(), nil); err != nil {
		t.Fatal(err)
	}
	if _, present := os.LookupEnv(name); present {
		t.Fatal("capturing .env must not change the process environment")
	}
}

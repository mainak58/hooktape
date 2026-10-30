package config

import (
	"os"
	"path/filepath"
	"testing"
)

// tempDir points Dir at a fresh directory. XDG_CONFIG_HOME is set too so the
// default Dir cannot reach a real home directory if a test forgets to use this.
func tempDir(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "hooktape")
	t.Setenv("XDG_CONFIG_HOME", dir)

	old := Dir
	Dir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { Dir = old })

	return dir
}

func TestLoadMissingFile(t *testing.T) {
	tempDir(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c != (Config{}) {
		t.Errorf("got %+v, want the zero value", c)
	}
}

func TestSaveCreatesDirsAndRoundTrips(t *testing.T) {
	dir := tempDir(t)

	if err := Save(Config{LastForward: "http://localhost:3000"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LastForward != "http://localhost:3000" {
		t.Errorf("LastForward = %q", c.LastForward)
	}

	// The rename leaves config.json and nothing else behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Errorf("%d entries, first %q", len(entries), entries[0].Name())
	}
}

func TestSaveOverwrites(t *testing.T) {
	tempDir(t)

	if err := Save(Config{LastForward: "http://old"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Save(Config{LastForward: "http://new"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LastForward != "http://new" {
		t.Errorf("LastForward = %q, want http://new", c.LastForward)
	}
}

func TestLoadBadJSON(t *testing.T) {
	dir := tempDir(t)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(); err == nil {
		t.Error("broken config.json: want an error")
	}
}

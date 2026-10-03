package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestLoad_MissingFileIsEmpty covers the common case: nothing is configured and
// that is not a failure.
func TestLoad_MissingFileIsEmpty(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
	if cfg.Master != "" {
		t.Errorf("Master = %q, want empty", cfg.Master)
	}
}

func TestLoad_ReadsMaster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write(t, path, `{"master":"http://host:8080"}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Master != "http://host:8080" {
		t.Errorf("Master = %q", cfg.Master)
	}
}

// TestLoad_UnknownKeysAreIgnored is what lets a file written by a newer onley
// still work here.
func TestLoad_UnknownKeysAreIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write(t, path, `{"master":"http://host:8080","future":{"a":1},"debounce":"2s"}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Master != "http://host:8080" {
		t.Errorf("Master = %q, want the known key to survive", cfg.Master)
	}
}

// TestLoad_MalformedIsAnError is the case that must not be silent: a file that
// exists but cannot be read would otherwise be treated as no configuration, and
// in replica mode the consequences are deletions.
func TestLoad_MalformedIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write(t, path, `{"master": `)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a truncated file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error should name the file; got: %v", err)
	}
}

func TestLoad_MasterWrongTypeIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write(t, path, `{"master": 8080}`)

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a numeric master")
	}
}

func TestSave_ThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (Config{Master: "http://host:9999"}).Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Master != "http://host:9999" {
		t.Errorf("Master = %q", cfg.Master)
	}
}

// TestSave_Overwrites checks that setting a value a second time replaces it
// rather than failing on an existing file.
func TestSave_Overwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (Config{Master: "http://first:1"}).Save(path); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := (Config{Master: "http://second:2"}).Save(path); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Master != "http://second:2" {
		t.Errorf("Master = %q, want the second value", cfg.Master)
	}
}

// TestSave_PrivateMode keeps the file unreadable to other users even if the
// directory mode is later relaxed.
func TestSave_PrivateMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not enforced on Windows")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (Config{Master: "http://host:8080"}).Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// TestSave_LeavesNoTemporaryFile: the write goes through a temporary file in the
// same directory, and it must not survive a successful save.
func TestSave_LeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := (Config{Master: "http://host:8080"}).Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("Save left %s behind", e.Name())
		}
	}
}

// TestSave_MissingDirectoryIsAnError keeps the failure visible instead of
// creating a directory tree nobody asked for.
func TestSave_MissingDirectoryIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "config.json")
	if err := (Config{Master: "http://host:8080"}).Save(path); err == nil {
		t.Fatal("Save created a missing parent directory")
	}
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		t.Error("the parent directory was created anyway")
	}
}

// Package config reads and writes onley's per-user settings.
//
// The settings live next to the index so that both follow the user rather than
// the working directory. Only a value that is fixed per machine belongs here:
// something a user types once and forgets.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the contents of the configuration file.
//
// Keys this build does not know are ignored on load, so a file written by a
// newer onley still works here. Anything this build writes comes from this
// struct, so a key that is never read is never written either.
type Config struct {
	// Master is the base URL of the master server, e.g. http://host:8080.
	Master string `json:"master,omitempty"`
}

// Load reads the configuration at path. A missing file is not an error: most
// installations never set anything, and that is a normal state.
//
// A file that exists but cannot be parsed is an error. Silently treating it as
// empty would send the command at a default it was never given, and in replica
// mode the consequences are deletions.
func Load(path string) (Config, error) {
	var c Config

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// Save writes the configuration to path.
//
// The file is written through a temporary file in the same directory and then
// renamed, so an interrupted write cannot leave a half-written file that fails
// to parse on the next run. The mode is 0600: the directory is already 0700, and
// a configuration file does not need to be world-readable inside it.
func (c Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

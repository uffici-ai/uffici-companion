package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// RecordFile is what `uffici setup` remembers about the machine, inside
// `$UFFICI_HOME`, so `uffici upgrade` needs no flags (#2250).
const RecordFile = "setup.json"

// Record is the machine's setup, as the last successful `uffici setup` left it.
//
// 🔴 Never a key, a pairing code or a token: it is read by a command that
// prints what it found, and it outlives the run that wrote it.
type Record struct {
	// Harness is the kind whose plugin was installed; empty when none was.
	Harness string `json:"harness"`
	// HarnessHome is the resolved harness home the plugin went into.
	HarnessHome string `json:"harnessHome,omitempty"`
	// PluginDir is the plugin's directory under HarnessHome, from its manifest.
	PluginDir string `json:"pluginDir,omitempty"`
	// GatewayURL is the agents host the service was installed against.
	GatewayURL string `json:"gatewayUrl,omitempty"`
	// Service reports whether setup installed the companion as a service.
	Service bool `json:"service"`
}

// HarnessName is what a harness may be called: it becomes a repository name.
var HarnessName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// ErrNoRecord is ReadRecord's answer for a machine set up before the record
// existed, or never set up.
var ErrNoRecord = errors.New("no setup record")

// WriteRecord stores the record, private to the user.
func WriteRecord(companionHome string, record Record) error {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(companionHome, 0o700); err != nil {
		return err
	}
	path := filepath.Join(companionHome, RecordFile)
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	// WriteFile keeps an existing file's mode; the record is private either way.
	return os.Chmod(path, 0o600)
}

// ReadRecord reads the record, or ErrNoRecord when there is none.
func ReadRecord(companionHome string) (Record, error) {
	raw, err := os.ReadFile(filepath.Join(companionHome, RecordFile))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNoRecord
	}
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(raw, &record); err != nil {
		return Record{}, fmt.Errorf("%s is not a setup record: %w", RecordFile, err)
	}
	// Setup's own rules, again: the harness names a repository, and the home is
	// where the plugin directory is replaced.
	if record.Harness != "" && !HarnessName.MatchString(record.Harness) {
		return Record{}, fmt.Errorf("%s names the harness %q, which is not a harness name", RecordFile, record.Harness)
	}
	if record.HarnessHome != "" && !filepath.IsAbs(record.HarnessHome) {
		return Record{}, fmt.Errorf("%s names a harness home that is not an absolute path", RecordFile)
	}
	return record, nil
}

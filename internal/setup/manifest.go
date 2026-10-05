// Package setup is `uffici setup`: the part of the install that a shell script
// should not do — installing the companion as a service, pairing, fetching the
// plugin for the person's harness from its own release, verifying it,
// installing it, and running the doctor.
//
// The shell installer bootstraps the binary and hands over here, because Go is
// testable and cross-platform and a POSIX script is neither. Everything
// harness-specific rides with the PLUGIN's release as a manifest
// (`uffici-plugin.json`), so a new harness needs no change in this package.
package setup

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/uffici-ai/uffici-companion/internal/schemas"
)

// ManifestFile is the manifest's name at the top of a plugin release tarball.
const ManifestFile = "uffici-plugin.json"

//go:embed manifest.schema.json
var manifestSchema []byte

// ManifestSchema is the embedded schema, for guards that pin a plugin's copy.
func ManifestSchema() []byte { return manifestSchema }

// ConfigSet is one configuration key the harness must carry.
type ConfigSet struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Note is what setup tells the person after setting the key: what it changes
	// beyond this plugin (a key that is the whole harness's, not one platform's).
	Note string `json:"note,omitempty"`
}

// Manifest is what a plugin release says about installing itself. Every field
// is documented in manifest.schema.json, which is what validates it.
type Manifest struct {
	ManifestVersion int               `json:"manifestVersion"`
	Harness         string            `json:"harness"`
	Name            string            `json:"name"`
	Version         string            `json:"version,omitempty"`
	PluginDir       string            `json:"pluginDir"`
	HomeEnv         string            `json:"homeEnv"`
	DefaultHome     string            `json:"defaultHome"`
	Enable          []string          `json:"enable"`
	ConfigSet       []string          `json:"configSet,omitempty"`
	Config          []ConfigSet       `json:"config,omitempty"`
	EnvPath         []string          `json:"envPath,omitempty"`
	EnvFile         string            `json:"envFile,omitempty"`
	Env             map[string]string `json:"env"`
	Restart         string            `json:"restart"`
	RestartCommand  []string          `json:"restartCommand,omitempty"`
	RestartService  *RestartService   `json:"restartService,omitempty"`
}

// RestartService names the harness's own service definition, per manager.
type RestartService struct {
	Systemd string `json:"systemd,omitempty"`
	Launchd string `json:"launchd,omitempty"`
}

var (
	schemaOnce sync.Once
	schemaDoc  *jsonschema.Schema
	schemaErr  error
)

func compiled() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		const id = "https://uffici.ai/companion/plugin/v1/manifest.schema.json"
		if err := schemas.AddResource(compiler, id, manifestSchema); err != nil {
			schemaErr = fmt.Errorf("the embedded manifest schema did not load: %w", err)
			return
		}
		schemaDoc, schemaErr = compiler.Compile(id)
		if schemaErr != nil {
			schemaErr = fmt.Errorf("the embedded manifest schema did not compile: %w", schemaErr)
		}
	})
	return schemaDoc, schemaErr
}

// ParseManifest validates raw against the schema and returns it.
//
// 🔴 A field the schema does not know is a refusal (`additionalProperties:
// false`), never an ignored key: an unknown field is an install step this
// companion would silently skip, and the person would learn that from a harness
// that never loaded the plugin.
func ParseManifest(raw []byte) (Manifest, error) {
	schema, err := compiled()
	if err != nil {
		return Manifest{}, err
	}
	instance, err := schemas.Parse(raw)
	if err != nil {
		return Manifest{}, fmt.Errorf("parsing %s: %w", ManifestFile, err)
	}
	if err := schema.Validate(instance); err != nil {
		if locations := schemas.SafeLocations(err); len(locations) > 0 {
			return Manifest{}, fmt.Errorf("%s does not match the manifest schema at %s — a plugin release this companion cannot install; update the companion or the plugin",
				ManifestFile, strings.Join(locations, ", "))
		}
		return Manifest{}, fmt.Errorf("%s does not match the manifest schema", ManifestFile)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("reading %s: %w", ManifestFile, err)
	}
	// 🔴 The plugin directory is REPLACED on install, so it must be the plugin's
	// own and nothing broader: at least two segments, the last one the plugin's
	// name. A manifest naming `plugins` alone would have the harness's whole
	// plugin folder removed.
	segments := strings.Split(manifest.PluginDir, "/")
	for _, segment := range segments {
		if segment == ".." || segment == "." || segment == "" {
			return Manifest{}, fmt.Errorf("%s names a pluginDir that leaves the harness home: %q", ManifestFile, manifest.PluginDir)
		}
	}
	if len(segments) < 2 || segments[len(segments)-1] != manifest.Name {
		return Manifest{}, fmt.Errorf("%s names pluginDir %q; it must be a directory named after the plugin (%q) inside the harness home, never the harness's plugin folder itself", ManifestFile, manifest.PluginDir, manifest.Name)
	}
	if manifest.EnvFile == "" {
		manifest.EnvFile = ".env"
	}
	return manifest, nil
}

var placeholder = regexp.MustCompile(`\$\{([a-zA-Z]+)\}`)

// RenderEnv substitutes the manifest's placeholders from vars. An unknown
// placeholder is a refusal: a variable the plugin would read with `${x}` in it
// is a plugin that cannot connect, discovered later and further away.
func (m Manifest) RenderEnv(vars map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(m.Env))
	for name, template := range m.Env {
		var missing []string
		value := placeholder.ReplaceAllStringFunc(template, func(match string) string {
			key := placeholder.FindStringSubmatch(match)[1]
			if v, ok := vars[key]; ok {
				return v
			}
			missing = append(missing, key)
			return match
		})
		if len(missing) > 0 {
			return nil, fmt.Errorf("%s uses a placeholder this companion does not provide in %s: ${%s}",
				ManifestFile, name, strings.Join(missing, "}, ${"))
		}
		out[name] = value
	}
	return out, nil
}

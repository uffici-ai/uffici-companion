package wire_test

import (
	"io/fs"
	"testing"

	contract "github.com/uffici-ai/uffici-companion/wire"
)

// The contract package ships files. These assert the files are actually IN the
// binary — which is the whole claim it makes, and the one thing a reader cannot
// check by looking at the directory.

func TestTheContractIsEmbedded(t *testing.T) {
	names, err := contract.SchemaNames()
	if err != nil {
		t.Fatalf("listing schemas: %v", err)
	}
	if len(names) < 11 {
		t.Fatalf("found %d embedded schema(s) — ten frames and the entry are expected", len(names))
	}
	for _, name := range names {
		raw, readErr := fs.ReadFile(contract.FS(), "schema/"+name+".schema.json")
		if readErr != nil {
			t.Fatalf("schema %s is listed but not readable: %v", name, readErr)
		}
		if len(raw) == 0 {
			t.Fatalf("schema %s embedded as zero bytes", name)
		}
	}
}

func TestTheGoldensAreEmbedded(t *testing.T) {
	for _, kind := range []string{"valid", "invalid"} {
		entries, err := fs.ReadDir(contract.FS(), "golden/"+kind)
		if err != nil {
			t.Fatalf("reading golden/%s: %v", kind, err)
		}
		if len(entries) == 0 {
			t.Fatalf("golden/%s embedded empty — the decoder's tests would assert nothing", kind)
		}
	}
}

func TestManifestLoadsAndAgreesWithItsOwnTotals(t *testing.T) {
	manifest, err := contract.LoadManifest()
	if err != nil {
		t.Fatalf("loading the manifest: %v", err)
	}
	if manifest.Version != contract.Version {
		t.Fatalf("manifest version %d, package version %d", manifest.Version, contract.Version)
	}

	// The totals are a second statement of the same counts, so they can drift
	// from the per-frame maps they summarise. Cheap to pin, and the manifest is
	// what the decoder's tests compare the files against.
	valid := 0
	for _, count := range manifest.ValidGoldensByFrame {
		valid += count
	}
	if valid != manifest.Totals.Valid {
		t.Errorf("totals.valid is %d, the per-frame counts sum to %d", manifest.Totals.Valid, valid)
	}
	invalid := 0
	for _, count := range manifest.InvalidGoldensByCloseReason {
		invalid += count
	}
	// The call tunnel's answered and discarded goldens are invalid too (#2028).
	for _, count := range manifest.InvalidGoldensByCallDisposition {
		invalid += count
	}
	if invalid != manifest.Totals.Invalid {
		t.Errorf("totals.invalid is %d, the per-reason and per-disposition counts sum to %d", manifest.Totals.Invalid, invalid)
	}
	if len(manifest.Schemas) != manifest.Totals.Schemas {
		t.Errorf("totals.schemas is %d, the list has %d", manifest.Totals.Schemas, len(manifest.Schemas))
	}
}

func TestManifestListsTheSchemasOnDisk(t *testing.T) {
	manifest, err := contract.LoadManifest()
	if err != nil {
		t.Fatalf("loading the manifest: %v", err)
	}
	names, err := contract.SchemaNames()
	if err != nil {
		t.Fatalf("listing schemas: %v", err)
	}
	listed := map[string]bool{}
	for _, name := range manifest.Schemas {
		listed[name] = true
	}
	for _, name := range names {
		if !listed[name] {
			t.Errorf("schema %q is embedded but the manifest does not list it", name)
		}
	}
	if len(manifest.Schemas) != len(names) {
		t.Errorf("the manifest lists %d schema(s), %d are embedded", len(manifest.Schemas), len(names))
	}
}

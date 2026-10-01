package wire

import (
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"

	contract "github.com/uffici-ai/uffici-companion/wire"
)

// The call ops' payload contracts (#2264): every op the manifest names has its
// schemas on disk, every golden decides as its directory says, and the counts
// match the manifest exactly — the op-side twin of the frame goldens.
func TestEveryOpGoldenDecidesAsItsDirectorySays(t *testing.T) {
	manifest, err := contract.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Ops) == 0 {
		t.Fatal("the manifest names no call op — logs.push at least")
	}
	counts := map[string]map[string]int{}
	for _, kind := range []string{"valid", "invalid"} {
		entries, err := fs.ReadDir(contract.FS(), "golden/ops/"+kind)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := strings.TrimSuffix(entry.Name(), ".json")
			// `<op>.<direction>-<case>`: the op is everything before the direction.
			dash := strings.Index(name, "-")
			if dash < 0 {
				t.Fatalf("golden %s does not name its direction", entry.Name())
			}
			head := name[:dash]
			dot := strings.LastIndex(head, ".")
			op, direction := head[:dot], head[dot+1:]
			if counts[op] == nil {
				counts[op] = map[string]int{}
			}
			counts[op][kind]++
			raw, err := fs.ReadFile(contract.FS(), path.Join("golden/ops", kind, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			verr := ValidateOpPayload(op, direction, raw)
			if kind == "valid" && verr != nil {
				t.Errorf("%s should validate: %v", entry.Name(), verr)
			}
			if kind == "invalid" && verr == nil {
				t.Errorf("%s should be refused", entry.Name())
			}
		}
	}
	for op, want := range manifest.Ops {
		if counts[op]["valid"] != want.ValidGoldens || counts[op]["invalid"] != want.InvalidGoldens {
			t.Errorf("op %s: %d valid / %d invalid goldens on disk, the manifest says %d / %d",
				op, counts[op]["valid"], counts[op]["invalid"], want.ValidGoldens, want.InvalidGoldens)
		}
	}
	for op := range counts {
		if _, ok := manifest.Ops[op]; !ok {
			t.Errorf("goldens for op %q, which the manifest does not name", op)
		}
	}
}

func TestEveryOpSchemaIsAManifestOp(t *testing.T) {
	manifest, err := contract.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	names, err := OpSchemaNames()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for op, entry := range manifest.Ops {
		for _, direction := range entry.Schemas {
			want = append(want, op+"."+direction)
		}
	}
	sort.Strings(want)
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("ops/ holds %v, the manifest names %v", names, want)
	}
	if _, ok := manifest.Ops[OpLogsPush]; !ok {
		t.Fatal("logs.push is served and must be in the manifest")
	}
}

func TestValidateOpPayloadRefusals(t *testing.T) {
	if err := ValidateOpPayload("nope", OpRequest, []byte(`{}`)); err == nil {
		t.Fatal("an unknown op validated")
	}
	if err := ValidateOpPayload(OpLogsPush, OpRequest, []byte(`not json`)); err == nil {
		t.Fatal("non-JSON validated")
	}
	secret := `{"entries":[{"ts":"x","source":"kernel","level":"info","message":"the secret line"}]}`
	err := ValidateOpPayload(OpLogsPush, OpRequest, []byte(secret))
	if err == nil || strings.Contains(err.Error(), "the secret line") {
		t.Fatalf("the refusal must name where, never what: %v", err)
	}
}

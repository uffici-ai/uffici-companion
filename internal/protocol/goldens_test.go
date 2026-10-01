package protocol

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"path"
	"reflect"
	"strings"
	"testing"

	contract "github.com/uffici-ai/uffici-companion/protocol"
)

// newFor returns a fresh value of the struct a schema decodes into.
func newFor(t *testing.T, schema string) any {
	t.Helper()
	switch schema {
	case "ack":
		return &Ack{}
	case "cancel":
		return &Cancel{}
	case "close-request":
		return &CloseRequest{}
	case "close-response":
		return &CloseResponse{}
	case "connect-ticket-request":
		return &ConnectTicketRequest{}
	case "conversation-ticket-request":
		return &ConversationTicketRequest{}
	case "conversation-ticket-response":
		return &ConversationTicketResponse{}
	case "disconnect":
		return &Disconnect{}
	case "entry":
		return &Entry{}
	case "error":
		return &ErrorEnvelope{}
	case "hello":
		return &Hello{}
	case "log-batch":
		return &LogBatch{}
	case "log-batch-response":
		return &LogBatchResponse{}
	case "logs-ticket-request":
		return &LogsTicketRequest{}
	case "logs-ticket-response":
		return &LogsTicketResponse{}
	case "identity":
		return &Identity{}
	case "pairing-poll-response":
		return &PairingPollResponse{}
	case "pairing-request":
		return &PairingRequest{}
	case "pairing-response":
		return &PairingResponse{}
	case "post-request":
		return &PostRequest{}
	case "post-response":
		return &PostResponse{}
	case "read-response":
		return &ReadResponse{}
	case "ring":
		return &Ring{}
	case "ticket-response":
		return &TicketResponse{}
	case "tool-call-request":
		return &ToolCallRequest{}
	}
	t.Fatalf("no struct for schema %q", schema)
	return nil
}

// clientBodies are the schemas the COMPANION encodes. Each is strict on the
// platform, so its golden must survive a decode→encode round trip unchanged:
// the encoder emits exactly what the contract admits, no more and no less.
var clientBodies = map[string]bool{
	"ack": true, "close-request": true, "connect-ticket-request": true,
	"conversation-ticket-request": true, "pairing-request": true, "post-request": true, "tool-call-request": true,
	"log-batch": true, "logs-ticket-request": true,
}

type golden struct{ schema, name string }

func goldensUnder(t *testing.T, dir string) []golden {
	t.Helper()
	var out []golden
	err := fs.WalkDir(contract.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		out = append(out, golden{schema: path.Base(path.Dir(p)), name: p})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

func TestEveryValidGoldenDecodesAndValidates(t *testing.T) {
	manifest, err := contract.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	goldens := goldensUnder(t, "goldens/valid")
	counts := map[string]int{}
	for _, g := range goldens {
		counts[g.schema]++
	}
	// Non-vacuity and completeness: the files on disk are the manifest's count.
	if !reflect.DeepEqual(counts, manifest.ValidGoldens) {
		t.Fatalf("valid goldens on disk %v, manifest %v", counts, manifest.ValidGoldens)
	}
	for _, g := range goldens {
		t.Run(g.name, func(t *testing.T) {
			raw, err := fs.ReadFile(contract.FS(), g.name)
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(g.schema, raw); err != nil {
				t.Fatalf("the schema refused its own valid golden: %v", err)
			}
			value := newFor(t, g.schema)
			decoder := json.NewDecoder(bytes.NewReader(raw))
			// Strict HERE only: a golden carrying a field the struct lacks is the
			// parity test's failure seen from the data side.
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(value); err != nil {
				t.Fatalf("decoding into %T: %v", value, err)
			}
			if !clientBodies[g.schema] {
				return
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			_ = json.Unmarshal(raw, &want)
			_ = json.Unmarshal(encoded, &got)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("round trip changed a client body.\n  golden:  %s\n  encoded: %s", strings.TrimSpace(string(raw)), encoded)
			}
		})
	}
}

func TestEveryInvalidGoldenIsRefused(t *testing.T) {
	manifest, err := contract.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	goldens := goldensUnder(t, "goldens/invalid")
	if len(goldens) == 0 {
		t.Fatal("no invalid goldens — the negative half of the contract is missing")
	}
	counts := map[string]int{}
	for _, g := range goldens {
		counts[g.schema]++
	}
	if !reflect.DeepEqual(counts, manifest.InvalidGoldens) {
		t.Fatalf("invalid goldens on disk %v, manifest %v", counts, manifest.InvalidGoldens)
	}
	for _, g := range goldens {
		t.Run(g.name, func(t *testing.T) {
			raw, err := fs.ReadFile(contract.FS(), g.name)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Reason string          `json:"reason"`
				Value  json.RawMessage `json:"value"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil || doc.Reason == "" {
				t.Fatalf("an invalid golden carries {reason, value}: %v", err)
			}
			if err := Validate(g.schema, doc.Value); err == nil {
				t.Fatalf("the schema accepted a payload it must refuse: %s", doc.Reason)
			}
		})
	}
}

func TestValidateRefusesWithoutQuotingTheDocument(t *testing.T) {
	secret := `{"postId":"not-a-uuid","requestId":"x","text":"the member's private words","audience":"external"}`
	err := Validate("post-request", []byte(secret))
	if err == nil {
		t.Fatal("an invalid post was accepted")
	}
	if strings.Contains(err.Error(), "private words") {
		t.Fatalf("the refusal quoted the document: %v", err)
	}
	if Validate("no-such-schema", []byte(`{}`)) == nil {
		t.Fatal("an unknown schema name validated")
	}
	if Validate("ack", []byte(`not json`)) == nil {
		t.Fatal("a non-JSON document validated")
	}
}

func TestNewAckIsTheOnlyClientFrame(t *testing.T) {
	raw, err := json.Marshal(NewAck("0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate("ack", raw); err != nil {
		t.Fatalf("NewAck built a frame the doorbell refuses: %v", err)
	}
}

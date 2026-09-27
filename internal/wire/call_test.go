package wire

import (
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	contract "github.com/uffici-ai/uffici-companion/wire"
)

// The call tunnel's decoder rules (#2028). The goldens pin the shapes; these
// pin what the goldens cannot say: that a call over its bound is ANSWERED with
// its own callId rather than closing the connection, and the one case where it
// cannot be.

// callFrame builds a call whose encoded frame is exactly `size` bytes.
func callFrame(t *testing.T, callID string, size int) []byte {
	t.Helper()
	shell := map[string]any{"v": 1, "type": "call", "op": "memory.remember", "deadlineMs": 5000,
		"payload": map[string]any{"text": ""}}
	if callID != "" {
		shell["callId"] = callID
	}
	empty, err := json.Marshal(shell)
	if err != nil {
		t.Fatal(err)
	}
	shell["payload"] = map[string]any{"text": strings.Repeat("x", size-len(empty))}
	raw, err := json.Marshal(shell)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != size {
		t.Fatalf("built a %d-byte frame, wanted %d", len(raw), size)
	}
	return raw
}

func TestTheCallBoundSitsInsideTheFrameBound(t *testing.T) {
	// A call bound at or above the frame bound would never be reached: the frame
	// bound would close the connection first, which is the opposite of what the
	// tunnel promises.
	if MaxCallPayloadBytes != 262_144 {
		t.Fatalf("MaxCallPayloadBytes is %d; #2028 fixes it at 256 KiB", MaxCallPayloadBytes)
	}
	if MaxCallPayloadBytes >= FrameMaxBytes {
		t.Fatalf("the call bound (%d) must sit inside the frame bound (%d)", MaxCallPayloadBytes, FrameMaxBytes)
	}
}

func TestACallAtItsBoundIsAcceptedAndOneByteOverIsAnswered(t *testing.T) {
	if err := Validate(callFrame(t, "call-1", MaxCallPayloadBytes)); err != nil {
		t.Fatalf("a call of exactly the bound was refused: %v", err)
	}
	err := Validate(callFrame(t, "call-2", MaxCallPayloadBytes+1))
	var callErr *CallError
	if !errors.As(err, &callErr) {
		t.Fatalf("a call one byte over the bound must be answered (a *CallError), got %T: %v", err, err)
	}
	if callErr.Class != ClassMalformedPayload || callErr.CallID != "call-2" {
		t.Fatalf("answered %s for %q; want malformed_payload for call-2", callErr.Class, callErr.CallID)
	}
}

func TestAnOversizedCallWithNoCallIDCloses(t *testing.T) {
	// Nothing to echo means nothing to answer: this one is a close, not a result.
	err := Validate(callFrame(t, "", MaxCallPayloadBytes+1))
	var closeErr *CloseError
	if !errors.As(err, &closeErr) || closeErr.Reason != ReasonMalformedFrame {
		t.Fatalf("an oversized call with no callId must close with malformed_frame, got %T: %v", err, err)
	}
}

func TestAResultOverItsBoundIsMalformedPayload(t *testing.T) {
	// The decoder is direction-agnostic: the plugin applies the same bound to the
	// companion's results.
	result := map[string]any{"v": 1, "type": "result", "callId": "call-3",
		"payload": map[string]any{"text": strings.Repeat("y", MaxCallPayloadBytes)}}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var callErr *CallError
	if !errors.As(Validate(raw), &callErr) || callErr.CallID != "call-3" {
		t.Fatalf("an oversized result must be malformed_payload for its own callId")
	}
}

func TestDecodeReturnsTheCallAndTheResult(t *testing.T) {
	decoded, err := Decode(read(t, "golden/valid/call-with-payload.json"), FromPlugin)
	if err != nil {
		t.Fatalf("decoding a valid call: %v", err)
	}
	call, ok := decoded.(*Call)
	if !ok || call.CallID == "" || call.Op != "memory.recall" || call.DeadlineMs != 8000 || len(call.Payload) == 0 {
		t.Fatalf("the call decoded wrong: %#v", decoded)
	}
	decoded, err = Decode(read(t, "golden/valid/result-timed-out.json"), FromCompanion)
	if err != nil {
		t.Fatalf("decoding a valid result: %v", err)
	}
	result, ok := decoded.(*Result)
	if !ok || result.Error == nil || result.Error.Class != string(ClassTimedOut) || result.Payload != nil {
		t.Fatalf("the result decoded wrong: %#v", decoded)
	}
}

func TestCallErrorCarriesNoPayload(t *testing.T) {
	marker := "the Lisbon invoice for 48,000 EUR"
	raw := callFrame(t, "call-4", MaxCallPayloadBytes+1)
	raw = []byte(strings.Replace(string(raw), "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", marker, 1))
	err := Validate(raw)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("a call error must name the class and the callId, never the payload: %v", err)
	}
	if !strings.Contains(err.Error(), "malformed_payload") || !strings.Contains(err.Error(), "call-4") {
		t.Fatalf("a call error must name its class and callId: %s", err.Error())
	}
}

func TestErrorClassesAreTheSchemasClosedFive(t *testing.T) {
	raw, err := fs.ReadFile(contract.FS(), "schema/result.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Properties struct {
			Error struct {
				Properties struct {
					Class struct {
						Enum []string `json:"enum"`
					} `json:"class"`
				} `json:"properties"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(CallErrorClasses()))
	for _, class := range CallErrorClasses() {
		got = append(got, string(class))
	}
	if len(got) != 5 || !reflect.DeepEqual(got, doc.Properties.Error.Properties.Class.Enum) {
		t.Fatalf("the Go classes %v and the schema's enum %v must be the same five, in order", got, doc.Properties.Error.Properties.Class.Enum)
	}
}

func TestCallDispositionsNeverCollideWithCloseReasons(t *testing.T) {
	// The golden file name's prefix is read as either; a shared word would make
	// one golden mean two things.
	for _, disposition := range CallDispositions() {
		for _, reason := range CloseReasons() {
			if string(disposition) == string(reason) {
				t.Fatalf("%q is both a call disposition and a close reason", disposition)
			}
		}
	}
}

func TestAnOversizedCallsIDIsCountedInCharactersLikeTheSchema(t *testing.T) {
	// 64 two-byte characters: 128 bytes, and within the schema's 64-character
	// maxLength — so it is answered, not closed.
	id := strings.Repeat("ç", 64)
	var callErr *CallError
	if !errors.As(Validate(callFrame(t, id, MaxCallPayloadBytes+1)), &callErr) || callErr.CallID != id {
		t.Fatalf("a 64-character callId must be answered, whatever its byte length")
	}
}

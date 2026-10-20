package hostsuite_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
	"github.com/uffici-ai/uffici-companion/internal/transfer"
)

// The file transfers against the gateway double (#2673): the real gateway host —
// go-tus for an upload, a ranged GET for a download — and the real engine, with
// the double cutting connections mid-file. What these prove is the RESUME: an
// upload continues from the offset the server holds, a download from the bytes
// already written, a changed file restarts from zero, and a file whose bytes do
// not match the platform's checksum is deleted, never kept.

const (
	transferConversation = "a1b2c3d4-e5f6-4789-8abc-def012345678"
	transferRequest      = "6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21"
)

type transferRig struct {
	double *gatewaytest.Double
	host   *gateway.Host
	engine *transfer.Engine
	ws     *transfer.Workspace
}

func newTransferRig(t *testing.T) transferRig {
	t.Helper()
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Seed(transferConversation, gatewaytest.Conversation{Offset: "1041", Entries: []protocol.Entry{}})
	host, err := gateway.New(gateway.Options{
		BaseURL: double.URL(), Key: gatewaytest.Key, HTTPClient: double.Client(), LogsHTTPClient: double.Client(),
		FilesHTTPClient: double.Client(), InstanceID: "d4c3b2a1-f6e5-4d7c-8b9a-0f1e2d3c4b5a",
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	claim, err := host.OpenDoorbell(context.Background(), contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = claim.Close() })
	if !eventually(double.Connected) {
		t.Fatal("the doorbell never connected")
	}
	if err := double.Ring(protocol.Ring{
		RingID: "0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23", RequestID: transferRequest, ConversationID: transferConversation,
		Reason: "mention", Deadline: time.Now().Add(time.Minute).UTC(),
		Cause: protocol.RingCause{EntryID: "1042", Author: protocol.RingAuthor{Type: "user", ID: "u-1"}},
		Since: "1041", Through: "1042",
	}); err != nil {
		t.Fatal(err)
	}
	if !eventually(func() bool { return host.KnowsRequest(transferRequest) }) {
		t.Fatal("the host never read the ring")
	}
	ws, err := transfer.OpenWorkspace(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := transfer.New(transfer.Options{
		Host: host, Workspace: ws, ChunkBytes: 1024,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return transferRig{double: double, host: host, engine: engine, ws: ws}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func partialsLeft(t *testing.T, ws *transfer.Workspace) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(ws.Root(), ".uffici", "partial"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestAnUploadInterruptedMidChunkResumesFromTheServersOffset(t *testing.T) {
	rig := newTransferRig(t)
	data := append([]byte("%PDF-1.7\n"), randomBytes(t, 5000)...)
	if err := os.MkdirAll(filepath.Join(rig.ws.Root(), "out"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig.ws.Root(), "out", "report.pdf"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The first chunk is cut after 300 of its bytes reached the server, and its
	// answer never comes back.
	rig.double.CutNextPatchAfter(300)

	got, err := rig.engine.Upload(context.Background(), transferRequest, transfer.UploadRequest{LocalPath: "out/report.pdf"})
	if err != nil {
		t.Fatalf("the upload failed: %v", err)
	}
	uploads := rig.double.Uploads()
	if len(uploads) != 1 || !uploads[0].Completed || !bytes.Equal(uploads[0].Data, data) {
		t.Fatalf("the server holds %d uploads; completed=%v, bytes equal=%v", len(uploads),
			len(uploads) == 1 && uploads[0].Completed, len(uploads) == 1 && bytes.Equal(uploads[0].Data, data))
	}
	if got.ID == "" || got.Ref == "" || got.Size != int64(len(data)) || got.Path != "" {
		t.Fatalf("answered %+v", got)
	}
	var heads, patches int
	for _, request := range rig.double.BytesRequests() {
		if !request.Auth {
			t.Fatalf("a %s reached the files routes without the grant", request.Method)
		}
		switch request.Method {
		case "HEAD":
			heads++
		case "PATCH":
			patches++
		}
	}
	// 🔴 The resume ASKED the server where it stood: a HEAD after the cut.
	if heads == 0 || patches < 6 {
		t.Fatalf("%d HEADs and %d PATCHes — the upload did not resume from the server's offset", heads, patches)
	}
	ask := rig.double.TransferAsks()[0].Body
	if ask.Op != "upload" || ask.Filename != "report.pdf" || ask.ContentType != "application/pdf" ||
		*ask.Size != int64(len(data)) || ask.CRC32C != gatewaytest.CRC32C(data) {
		t.Fatalf("the grant was asked for %+v", ask)
	}
}

func TestAnUploadIntoFilesAnswersItsPath(t *testing.T) {
	rig := newTransferRig(t)
	if err := os.WriteFile(filepath.Join(rig.ws.Root(), "chart.png"), []byte("\x89PNG\r\n\x1a\nrest"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.double.FailNextPatch(503)
	got, err := rig.engine.Upload(context.Background(), transferRequest,
		transfer.UploadRequest{LocalPath: filepath.Join(rig.ws.Root(), "chart.png"), To: "reports/chart.png", Overwrite: true})
	if err != nil {
		t.Fatalf("the upload failed: %v", err)
	}
	if got.Path != "/reports/chart.png" || got.ID != "" {
		t.Fatalf("answered %+v", got)
	}
	if ask := rig.double.TransferAsks()[0].Body; ask.To != "reports/chart.png" || !ask.Overwrite || ask.ContentType != "image/png" {
		t.Fatalf("asked %+v", ask)
	}
}

func TestADownloadInterruptedMidFileResumesWithARange(t *testing.T) {
	rig := newTransferRig(t)
	data := randomBytes(t, 200_000)
	rig.double.SeedAttachment(transferConversation, "1042.0", gatewaytest.File{Name: "scan.bin", ContentType: "application/octet-stream", Data: data})
	rig.double.CutNextDownloadAfter(70_000)

	got, err := rig.engine.Download(context.Background(), transferRequest, transfer.DownloadTarget{AttachmentID: "1042.0"})
	if err != nil {
		t.Fatalf("the download failed: %v", err)
	}
	want := "conversations/" + transferConversation + "/1042.0-scan.bin"
	if got.LocalPath != want || got.Size != int64(len(data)) {
		t.Fatalf("answered %+v, want %s", got, want)
	}
	written, err := os.ReadFile(filepath.Join(rig.ws.Root(), filepath.FromSlash(want)))
	if err != nil || !bytes.Equal(written, data) {
		t.Fatalf("the file is not the platform's: %v", err)
	}
	requests := rig.double.BytesRequests()
	if len(requests) != 2 || requests[0].Range != "" || requests[1].Range != "bytes=70000-" || requests[1].IfRange == "" {
		t.Fatalf("the download did not resume from the partial's size: %+v", requests)
	}
	if partialsLeft(t, rig.ws) != 0 {
		t.Fatal("a partial file outlived its download")
	}
}

func TestADownloadWhoseValidatorChangedRestartsFromZero(t *testing.T) {
	rig := newTransferRig(t)
	data := randomBytes(t, 50_000)
	rig.double.SeedPath("reports/q3.bin", gatewaytest.File{Name: "q3.bin", Data: data})
	rig.double.CutNextDownloadAfter(20_000)
	// The resume's If-Range no longer matches: the server answers 200, the whole file.
	rig.double.RestartNextResume()

	got, err := rig.engine.Download(context.Background(), transferRequest, transfer.DownloadTarget{Path: "/reports/q3.bin"})
	if err != nil {
		t.Fatalf("the download failed: %v", err)
	}
	if got.LocalPath != "files/reports/q3.bin" {
		t.Fatalf("answered %+v", got)
	}
	written, err := os.ReadFile(filepath.Join(rig.ws.Root(), "files", "reports", "q3.bin"))
	if err != nil || !bytes.Equal(written, data) {
		t.Fatal("a restarted download kept the old prefix")
	}
	if requests := rig.double.BytesRequests(); len(requests) != 2 || requests[1].IfRange == "" {
		t.Fatalf("requests %+v", requests)
	}
}

func TestADownloadWhoseChecksumDoesNotMatchFailsAndIsDeleted(t *testing.T) {
	rig := newTransferRig(t)
	rig.double.SeedAttachment(transferConversation, "1042.0", gatewaytest.File{Name: "a.bin", Data: randomBytes(t, 10_000)})
	rig.double.CorruptDownloads()

	_, err := rig.engine.Download(context.Background(), transferRequest, transfer.DownloadTarget{AttachmentID: "1042.0"})
	if transfer.ClassOf(err) != transfer.ClassChecksumMismatch {
		t.Fatalf("a corrupted download answered %v", err)
	}
	if partialsLeft(t, rig.ws) != 0 {
		t.Fatal("the corrupted partial was kept")
	}
	if _, statErr := os.Stat(filepath.Join(rig.ws.Root(), "conversations", transferConversation)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("a corrupted file reached the conversation's directory")
	}
}

func TestAnAbortedDownloadLeavesNoPartial(t *testing.T) {
	rig := newTransferRig(t)
	rig.double.SeedAttachment(transferConversation, "1042.0", gatewaytest.File{Name: "big.bin", Data: randomBytes(t, 100_000)})
	rig.double.StallNextDownloadAfter(10_000)

	done := make(chan error, 1)
	go func() {
		_, err := rig.engine.Download(context.Background(), transferRequest, transfer.DownloadTarget{AttachmentID: "1042.0"})
		done <- err
	}()
	if !eventually(func() bool { return partialsLeft(t, rig.ws) == 1 && rig.engine.Active() == 1 }) {
		t.Fatal("the download never started writing")
	}
	rig.engine.Abort(transferRequest)
	select {
	case err := <-done:
		if transfer.ClassOf(err) != transfer.ClassCancelled {
			t.Fatalf("an aborted download answered %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abort did not stop the download")
	}
	if partialsLeft(t, rig.ws) != 0 || rig.engine.Active() != 0 {
		t.Fatal("an aborted download left its partial behind")
	}
	if strings.Contains(rig.ws.Root(), ".uffici") {
		t.Fatal("the workspace root is the companion's own corner")
	}
}

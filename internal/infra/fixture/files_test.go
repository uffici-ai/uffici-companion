package fixture

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
)

// The scripted host's file side (#2679): a scripted attachment's bytes are
// served to a download, an upload is received through the gateway's own tus
// client — resumed after a dropped connection — and recorded only when intact.

var (
	scannedBytes = append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("p"), 4000)...)
	reportBytes  = append([]byte("%PDF-1.4 [report-canary]\n"), bytes.Repeat([]byte("r"), 50_000)...)
)

func filesHost(t *testing.T) *Host {
	t.Helper()
	script := testScript()
	entries := script.Conversations["invoices"].Entries
	entries[3].Attachments = []ScriptAttachment{
		{ID: "e4.0", Name: "scan.pdf", Ref: "conversations/conv_1/scan.pdf", ContentType: "application/pdf", Data: scannedBytes},
		{ID: "e4.1", Name: "ref-only.png", Ref: "conversations/conv_1/ref-only.png"},
	}
	host, err := New(script)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	host.UseBytesClient(func(base string) BytesClient { return &gateway.FilesClient{BaseURL: base} })
	if _, err := host.OpenDoorbell(context.Background(), contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
	}); err != nil {
		t.Fatal(err)
	}
	return host
}

func ring(t *testing.T, host *Host, requestID string) {
	t.Helper()
	if err := host.DeliverRing(contracts.Ring{RingID: "r-" + requestID, RequestID: requestID, ConversationID: "conv_1", ThroughCursor: "c0004"}); err != nil {
		t.Fatal(err)
	}
}

func TestTheHostServesAScriptedAttachmentWithRanges(t *testing.T) {
	host := filesHost(t)
	ctx := context.Background()
	if _, known := host.ConversationOf("req_1"); known {
		t.Fatal("a request no ring delivered has a conversation")
	}
	if _, err := host.GrantTransfer(ctx, "req_1", contracts.TransferAsk{Op: contracts.TransferDownload, AttachmentID: "e4.0"}); !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("a grant before any ring = %v", err)
	}
	ring(t, host, "req_1")
	for _, id := range []string{"e4.1", "absent", ""} {
		_, err := host.GrantTransfer(ctx, "req_1", contracts.TransferAsk{Op: contracts.TransferDownload, AttachmentID: id})
		var classified contracts.ClassifiedError
		if !errors.As(err, &classified) || classified.ErrorClass() != "not_found" || classified.ErrorInstruction() == "" {
			t.Fatalf("attachment %q = %v", id, err)
		}
	}
	grant, err := host.GrantTransfer(ctx, "req_1", contracts.TransferAsk{Op: contracts.TransferDownload, AttachmentID: "e4.0"})
	if err != nil || grant.File.Size != int64(len(scannedBytes)) || grant.File.CRC32C != crc32cOf(scannedBytes) || grant.File.ContentType != "application/pdf" {
		t.Fatalf("grant = %+v, %v", grant, err)
	}
	meta, body, err := host.OpenDownload(ctx, grant, 1000, "")
	if err != nil || meta.Status != http.StatusPartialContent || meta.Start != 1000 || meta.CRC32C != grant.File.CRC32C {
		t.Fatalf("ranged = %+v, %v", meta, err)
	}
	tail, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(tail, scannedBytes[1000:]) {
		t.Fatal("the range is not the file's tail")
	}
	// A validator that no longer matches restarts from the first byte.
	meta, body, err = host.OpenDownload(ctx, grant, 1000, `"stale"`)
	if err != nil || meta.Status != http.StatusOK {
		t.Fatalf("stale validator = %+v, %v", meta, err)
	}
	whole, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(whole, scannedBytes) || host.Downloads() != 1 {
		t.Fatalf("the whole file did not come back, downloads=%d", host.Downloads())
	}
	if _, _, err := host.OpenDownload(ctx, grant, int64(len(scannedBytes)), ""); err == nil {
		t.Fatal("a range past the file was served")
	}
	if _, _, err := host.OpenDownload(ctx, contracts.TransferGrant{Grant: "forged"}, 0, ""); err == nil {
		t.Fatal("a forged grant was served")
	}
}

func upload(t *testing.T, host *Host, requestID, to string, data []byte) (contracts.TransferGrant, error) {
	t.Helper()
	ctx := context.Background()
	grant, err := host.GrantTransfer(ctx, requestID, contracts.TransferAsk{
		Op: contracts.TransferUpload, Filename: "report.pdf", Size: int64(len(data)), CRC32C: crc32cOf(data),
		ContentType: "application/pdf", To: to,
	})
	if err != nil {
		t.Fatal(err)
	}
	return grant, host.UploadFile(ctx, grant, bytes.NewReader(data), contracts.UploadSpec{
		Size: int64(len(data)), ChunkBytes: 16 << 10, Attempts: 4, Filename: "report.pdf",
		ContentType: "application/pdf", CRC32C: grant.File.CRC32C,
	})
}

// 🔴 The interrupted upload: the connection drops mid-PATCH once, the bytes it
// carried so far are kept, and the gateway's own client resumes from the offset
// the server reports — the recorded file is whole and marked resumed.
func TestAnInterruptedUploadResumesAndIsRecordedIntact(t *testing.T) {
	host := filesHost(t)
	ring(t, host, "req_1")
	host.InterruptNextUpload(5000)
	grant, err := upload(t, host, "req_1", "", reportBytes)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	done, err := host.CompleteTransfer(context.Background(), "req_1", grant.JTI)
	if err != nil || done.ID == "" || done.Name != "report.pdf" || done.Ref != "conversations/conv_1/report.pdf" {
		t.Fatalf("Complete = %+v, %v", done, err)
	}
	uploads := host.Uploads()
	if len(uploads) != 1 || !uploads[0].Resumed || !uploads[0].Intact || uploads[0].ID != done.ID ||
		!bytes.Equal(uploads[0].Data, reportBytes) {
		t.Fatalf("uploads = %+v", uploads)
	}
}

func TestAnUploadIntoFilesAnswersItsPath(t *testing.T) {
	host := filesHost(t)
	ring(t, host, "req_1")
	grant, err := upload(t, host, "req_1", "reports/q3.pdf", reportBytes)
	if err != nil {
		t.Fatal(err)
	}
	done, err := host.CompleteTransfer(context.Background(), "req_1", grant.JTI)
	if err != nil || done.Path != "/reports/q3.pdf" || done.ID != "" {
		t.Fatalf("Complete = %+v, %v", done, err)
	}
	if uploads := host.Uploads(); len(uploads) != 1 || uploads[0].To != "reports/q3.pdf" || uploads[0].Resumed || uploads[0].ID != "" {
		t.Fatalf("uploads = %+v", uploads)
	}
}

// A completion is refused for an upload that did not arrive whole and intact,
// for another request, and for a download's grant — and nothing is recorded.
func TestACompletionIsRefusedUnlessTheBytesArrivedIntact(t *testing.T) {
	host := filesHost(t)
	ctx := context.Background()
	ring(t, host, "req_1")
	ring(t, host, "req_2")
	grant, err := host.GrantTransfer(ctx, "req_1", contracts.TransferAsk{
		Op: contracts.TransferUpload, Filename: "x.bin", Size: 10, CRC32C: crc32cOf([]byte("0123456789")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.CompleteTransfer(ctx, "req_1", grant.JTI); !errors.Is(err, errIncomplete) {
		t.Fatalf("an empty upload completed: %v", err)
	}
	// Ten bytes that are not the declared ten: the server takes them, the
	// completion refuses them.
	if err := host.UploadFile(ctx, grant, strings.NewReader("abcdefghij"), contracts.UploadSpec{Size: 10, ChunkBytes: 4, Attempts: 2}); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if _, err := host.CompleteTransfer(ctx, "req_1", grant.JTI); !errors.Is(err, errCorrupted) {
		t.Fatalf("a corrupted upload completed: %v", err)
	}
	if _, err := host.CompleteTransfer(ctx, "req_2", grant.JTI); !errors.Is(err, errNoSuchFile) {
		t.Fatalf("another request completed it: %v", err)
	}
	download, err := host.GrantTransfer(ctx, "req_1", contracts.TransferAsk{Op: contracts.TransferDownload, AttachmentID: "e4.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.CompleteTransfer(ctx, "req_1", download.JTI); !errors.Is(err, errNoSuchFile) {
		t.Fatalf("a download completed as an upload: %v", err)
	}
	if _, err := host.GrantTransfer(ctx, "req_1", contracts.TransferAsk{Op: "sideways"}); !errors.Is(err, errNoSuchFile) {
		t.Fatalf("an unknown op was granted: %v", err)
	}
	if len(host.Uploads()) != 0 {
		t.Fatal("a refused upload was recorded")
	}
}

// An upload's length must be the grant's, and its bytes are refused past it.
func TestTheFilesRoutesHoldAnUploadToItsGrant(t *testing.T) {
	host := filesHost(t)
	ctx := context.Background()
	ring(t, host, "req_1")
	grant, err := host.GrantTransfer(ctx, "req_1", contracts.TransferAsk{Op: contracts.TransferUpload, Filename: "x", Size: 4, CRC32C: crc32cOf([]byte("abcd"))})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.UploadFile(ctx, grant, strings.NewReader("abcdefgh"), contracts.UploadSpec{Size: 8, ChunkBytes: 8, Attempts: 1}); err == nil {
		t.Fatal("an upload longer than its grant was created")
	}
	base := host.filesURL()
	for _, probe := range []struct{ method, path string }{
		{http.MethodHead, "/uploads/unknown"},
		{http.MethodPut, "/uploads/" + grant.JTI},
		{http.MethodGet, "/uploads"},
	} {
		req, _ := http.NewRequestWithContext(ctx, probe.method, base+probe.path, nil)
		req.Header.Set("Authorization", "Bearer "+grant.Grant)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("%s %s answered %d", probe.method, probe.path, resp.StatusCode)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/uploads", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an upload with no grant answered %d", resp.StatusCode)
	}
}

func TestAnAppendedEntrysAttachmentCanBeDownloaded(t *testing.T) {
	host := filesHost(t)
	if err := host.AppendEntries("invoices", []ScriptEntry{{
		ID: "e5", Cursor: "c0005", At: "2026-09-17T14:05:00Z", Kind: "message", Text: "one more",
		Author:      ScriptAuthor{Type: "member", ID: "m1", Label: "Ana"},
		Attachments: []ScriptAttachment{{ID: "e5.0", Name: "late.txt", Ref: "conversations/conv_1/late.txt", Data: []byte("late")}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := host.AppendEntries("absent", nil); err == nil {
		t.Fatal("an unknown conversation took entries")
	}
	ring(t, host, "req_1")
	if _, err := host.GrantTransfer(context.Background(), "req_1", contracts.TransferAsk{Op: contracts.TransferDownload, AttachmentID: "e5.0"}); err != nil {
		t.Fatalf("an appended attachment: %v", err)
	}
}

// A host nobody handed a bytes client grants nothing — it cannot carry the bytes.
func TestAHostWithNoBytesClientGrantsNothing(t *testing.T) {
	host := newTestHost(t)
	if _, err := host.OpenDoorbell(context.Background(), contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
	}); err != nil {
		t.Fatal(err)
	}
	ring(t, host, "req_1")
	_, err := host.GrantTransfer(context.Background(), "req_1", contracts.TransferAsk{Op: contracts.TransferUpload, Size: 1})
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "platform_unavailable" {
		t.Fatalf("GrantTransfer = %v", err)
	}
}

func TestAPostsAttachmentsAreRecorded(t *testing.T) {
	host := filesHost(t)
	if _, err := host.Post(context.Background(), "req_1", "conv_1", "here", &contracts.PostExtras{
		Attachments: []contracts.Attachment{{ID: "upload-1", Name: "r.pdf", Ref: "conversations/conv_1/r.pdf"}},
	}); err != nil {
		t.Fatal(err)
	}
	if posts := host.Posts(); len(posts) != 1 || len(posts[0].Attachments) != 1 || posts[0].Attachments[0].ID != "upload-1" {
		t.Fatalf("posts = %+v", posts)
	}
}

func TestAScriptRefusesAnInterruptionOfNoBytes(t *testing.T) {
	script := testScript()
	script.Steps = append(script.Steps, Step{Step: "interruptNextUpload"})
	if err := script.Validate(); err == nil || !strings.Contains(err.Error(), "interrupts an upload") {
		t.Fatalf("Validate = %v", err)
	}
	script.Steps[len(script.Steps)-1].AfterBytes = 1
	if err := script.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The change gate's own script loads — schema and validator — and its files
// conversation carries both attachments' bytes and every file step (#2679).
func TestTheChangeGatesScriptLoadsWithItsFiles(t *testing.T) {
	file, err := os.Open(filepath.Join("..", "..", "..", "fixture", "ci", "wave1.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	script, err := Load(file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	files := script.Conversations["files"]
	if files == nil || len(files.Entries) != 1 || len(files.Entries[0].Attachments) != 2 {
		t.Fatal("the files conversation is missing its attachments")
	}
	for _, attachment := range files.Entries[0].Attachments {
		if len(attachment.Data) == 0 || attachment.ContentType == "" {
			t.Fatalf("attachment %s carries no bytes", attachment.ID)
		}
	}
	seen := map[string]bool{}
	for _, step := range script.Steps {
		seen[step.Step] = true
	}
	for _, kind := range []string{"interruptNextUpload", "expectUpload"} {
		if !seen[kind] {
			t.Fatalf("the change gate never plays %s", kind)
		}
	}
	if script.Assert.Uploads == nil || script.Assert.Downloads == nil || script.Assert.UploadsResumed == nil {
		t.Fatal("the change gate does not tally its files")
	}
}

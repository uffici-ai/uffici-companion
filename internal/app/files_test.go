package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
	"github.com/uffici-ai/uffici-companion/internal/setup"
	"github.com/uffici-ai/uffici-companion/internal/transfer"
)

// Files through the whole daemon, with the companion's test plugin and no
// harness (#2673): every case a second plugin will lean on is proven here once.

const (
	filesConversation = "a1b2c3d4-e5f6-4789-8abc-def012345678"
	filesRequest      = "6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21"
	filesRing         = "0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23"
	// Names a log line must never carry.
	invoiceName = "salaries-q3-confidential.pdf"
	scanName    = "meter-scan-private.tiff"
)

var (
	invoiceBytes = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("i"), 11)...) // 20 bytes
	scanBytes    = bytes.Repeat([]byte("s"), 50)
)

type filesRig struct {
	double *gatewaytest.Double
	config Config
	logs   *syncBuffer
}

func newFilesRig(t *testing.T) filesRig {
	t.Helper()
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Seed(filesConversation, gatewaytest.Conversation{Offset: "1041", Entries: []protocol.Entry{{
		ID: "1042", Cursor: "1042", At: time.Now().UTC(), Kind: "message", Text: "the invoice and the scan",
		Author: protocol.Author{Type: "user", ID: "u-1", Label: "Mona"},
		Attachments: []protocol.Attachment{
			{ID: "1042.0", Name: invoiceName, Ref: "conversations/" + filesConversation + "/" + invoiceName},
			{ID: "1042.1", Name: scanName, Ref: "conversations/" + filesConversation + "/" + scanName},
		},
	}}})
	double.SeedAttachment(filesConversation, "1042.0", gatewaytest.File{Name: invoiceName, ContentType: "application/pdf", Data: invoiceBytes})
	double.SeedAttachment(filesConversation, "1042.1", gatewaytest.File{Name: scanName, ContentType: "image/tiff", Data: scanBytes})
	config := doctorConfig(t, double)
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _ := Run(ctx, config, logs)
		done <- code
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	waitUntil(t, "the doorbell", double.Connected)
	return filesRig{double: double, config: config, logs: logs}
}

func (r filesRig) ring(t *testing.T) {
	t.Helper()
	if err := r.double.Ring(protocol.Ring{
		RingID: filesRing, RequestID: filesRequest, ConversationID: filesConversation, Reason: "mention",
		Cause:    protocol.RingCause{EntryID: "1042", Author: protocol.RingAuthor{Type: "user", ID: "u-1"}},
		Deadline: time.Now().Add(5 * time.Minute).UTC(), Since: "1041", Through: "1042",
	}); err != nil {
		t.Fatal(err)
	}
}

func (r filesRig) workspace(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(filepath.Join(r.config.Home, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func (r filesRig) partials(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.workspace(t), ".uffici", "partial"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// call sends one call and reads its result.
func (p *testPlugin) call(ctx context.Context, t *testing.T, callID, op string, payload map[string]any) map[string]any {
	t.Helper()
	p.send(t, map[string]any{"v": 1, "type": "call", "callId": callID, "op": op, "deadlineMs": 20000, "payload": payload})
	result := p.readFrame(ctx, t, "result")
	if result["callId"] != callID {
		t.Fatalf("result for %v, want %s", result["callId"], callID)
	}
	return result
}

func attachmentsOf(t *testing.T, brief map[string]any) []map[string]any {
	t.Helper()
	delta, _ := brief["delta"].([]any)
	if len(delta) != 1 {
		t.Fatalf("the brief carries %d entries", len(delta))
	}
	raw, _ := delta[0].(map[string]any)["attachments"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

func TestTheDaemonMovesFilesForARequest(t *testing.T) {
	rig := newFilesRig(t)
	// The plugin asks for prefetch with a budget the invoice fits and the scan
	// does not: 30 bytes, for a 20-byte invoice and a 50-byte scan.
	plugin, err := dialPluginHello(filepath.Join(rig.config.Home, SocketFile), waitForToken(t, rig.config.Home),
		map[string]any{"capabilities": map[string]any{"prefetchAttachments": map[string]any{"maxBytes": 30}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plugin.conn.CloseNow() })
	features, _ := plugin.hello["features"].([]any)
	if !slices.Contains(features, any("files")) || plugin.hello["workspace"] != rig.workspace(t) {
		t.Fatalf("the hello said %v", plugin.hello)
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()

	rig.ring(t)
	attachments := attachmentsOf(t, plugin.readFrame(ctx, t, "brief"))
	invoicePath := "conversations/" + filesConversation + "/1042.0-" + invoiceName
	if attachments[0]["localPath"] != invoicePath || attachments[0]["prefetchError"] != nil {
		t.Fatalf("the invoice was not prefetched: %v", attachments[0])
	}
	if failure, _ := attachments[1]["prefetchError"].(map[string]any); attachments[1]["localPath"] != nil || failure["class"] != "over_budget" {
		t.Fatalf("the scan was not marked over the budget: %v", attachments[1])
	}
	if written, err := os.ReadFile(filepath.Join(rig.workspace(t), filepath.FromSlash(invoicePath))); err != nil || !bytes.Equal(written, invoiceBytes) {
		t.Fatalf("the prefetched invoice is not the platform's: %v", err)
	}
	plugin.send(t, map[string]any{"v": 1, "type": "ack", "requestId": filesRequest, "outcome": "accepted", "session": "resumed"})

	// The scan, explicitly — no budget on a download the agent asked for.
	got := plugin.call(ctx, t, "c-scan", "files.download", map[string]any{"requestId": filesRequest, "attachmentId": "1042.1"})
	if payload, _ := got["payload"].(map[string]any); payload["localPath"] != "conversations/"+filesConversation+"/1042.1-"+scanName ||
		payload["contentType"] != "image/tiff" || payload["size"] != float64(len(scanBytes)) {
		t.Fatalf("files.download answered %v", got)
	}
	rig.double.SeedPath("reports/q3.txt", gatewaytest.File{Name: "q3.txt", ContentType: "text/plain", Data: []byte("q3 numbers")})
	got = plugin.call(ctx, t, "c-path", "files.download", map[string]any{"requestId": filesRequest, "path": "reports/q3.txt"})
	if payload, _ := got["payload"].(map[string]any); payload["localPath"] != "files/reports/q3.txt" {
		t.Fatalf("files.download by path answered %v", got)
	}

	// The fence, through the wire: refused as a call result, nothing sent.
	asks := len(rig.double.TransferAsks())
	for i, path := range []string{"../../etc/passwd", ".env", "~/.ssh/id_ed25519", "/etc/hosts"} {
		got = plugin.call(ctx, t, "c-fence-"+string(rune('a'+i)), "files.upload", map[string]any{"requestId": filesRequest, "localPath": path})
		failure, _ := got["error"].(map[string]any)
		if failure["class"] != "not_permitted" || failure["instruction"] == "" || !strings.Contains(mustMarshal(t, failure["upstream"]), "path_refused") {
			t.Fatalf("%s answered %v", path, got)
		}
	}
	if len(rig.double.TransferAsks()) != asks {
		t.Fatal("a refused path reached the platform")
	}

	// An upload attached to the reply, which the post then carries.
	answer := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("a"), 3000)...)
	if err := os.MkdirAll(filepath.Join(rig.workspace(t), "out"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig.workspace(t), "out", "answer.pdf"), answer, 0o600); err != nil {
		t.Fatal(err)
	}
	got = plugin.call(ctx, t, "c-up", "files.upload", map[string]any{"requestId": filesRequest, "localPath": "out/answer.pdf"})
	uploaded, _ := got["payload"].(map[string]any)
	if uploaded["id"] == nil || uploaded["ref"] == nil || uploaded["size"] != float64(len(answer)) {
		t.Fatalf("files.upload answered %v", got)
	}
	plugin.send(t, map[string]any{"v": 1, "type": "post", "requestId": filesRequest, "text": "Here it is.",
		"attachments": []any{map[string]any{"id": uploaded["id"], "name": uploaded["name"], "ref": uploaded["ref"]}}})
	plugin.send(t, map[string]any{"v": 1, "type": "done", "requestId": filesRequest, "outcome": "answered"})
	waitUntil(t, "the post", func() bool { return len(rig.double.Posts()) == 1 })
	if posts := rig.double.Posts(); len(posts[0].Attachments) != 1 || posts[0].Attachments[0].ID != uploaded["id"] {
		t.Fatalf("the post carried %+v", posts[0].Attachments)
	}
	if uploads := rig.double.Uploads(); len(uploads) != 1 || !bytes.Equal(uploads[0].Data, answer) || !uploads[0].Completed {
		t.Fatal("the platform does not hold the uploaded bytes")
	}
	if refusals := rig.double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused off-contract bodies: %v", refusals)
	}
	if rig.partials(t) != 0 {
		t.Fatal("a partial outlived its download")
	}

	out := &syncBuffer{}
	if code := Doctor(context.Background(), rig.config, out); code != ExitOK || !strings.Contains(out.String(), "workspace") ||
		!strings.Contains(out.String(), "writable") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	// 🔴 Never a path, a file name or a grant in the daemon's log.
	for _, secret := range []string{invoiceName, scanName, "answer.pdf", "reports/q3", "grant-", rig.workspace(t)} {
		if strings.Contains(rig.logs.String(), secret) {
			t.Fatalf("the daemon's log carries %q", secret)
		}
	}
	if !strings.Contains(rig.logs.String(), `"op":"upload"`) {
		t.Fatalf("the transfers left no line:\n%s", rig.logs.String())
	}
	_ = plugin.conn.Close(ws.StatusNormalClosure, "")
}

func TestLoadConfigReadsTheWorkspaceAndTheTransferSettings(t *testing.T) {
	home := shortHome(t)
	config, err := LoadConfig(env(map[string]string{"UFFICI_HOME": home}), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if config.Workspace != filepath.Join(home, "workspace") || config.WorkspaceFrom != "the default" ||
		config.WorkspaceAllowDotfiles || config.UploadChunkBytes != 16<<20 || config.PrefetchMaxBytes != 50<<20 {
		t.Fatalf("defaults: %+v", config)
	}
	// The plugin's manifest, as setup recorded it, comes before the default.
	recorded := filepath.Join(t.TempDir(), "harness", "uffici-workspace")
	if err := setup.WriteRecord(home, setup.Record{Harness: "hermes", Workspace: recorded}); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(env(map[string]string{"UFFICI_HOME": home}), Config{})
	if err != nil || config.Workspace != recorded || !strings.Contains(config.WorkspaceFrom, "manifest") {
		t.Fatalf("recorded: %+v, %v", config, err)
	}
	// And the environment before both.
	config, err = LoadConfig(env(map[string]string{
		"UFFICI_HOME": home, "UFFICI_WORKSPACE": "/srv/shared/workspace/", "UFFICI_WORKSPACE_ALLOW_DOTFILES": "1",
		"UFFICI_UPLOAD_CHUNK_BYTES": "1048576", "UFFICI_PREFETCH_MAX_BYTES": "1000",
	}), Config{})
	if err != nil || config.Workspace != "/srv/shared/workspace" || config.WorkspaceFrom != "UFFICI_WORKSPACE" ||
		!config.WorkspaceAllowDotfiles || config.UploadChunkBytes != 1<<20 || config.PrefetchMaxBytes != 1000 {
		t.Fatalf("environment: %+v, %v", config, err)
	}
	for name, value := range map[string][2]string{
		"a relative workspace":           {"UFFICI_WORKSPACE", "relative/dir"},
		"a dotfile switch that is not 1": {"UFFICI_WORKSPACE_ALLOW_DOTFILES", "yes"},
		"a chunk over the edge's cap":    {"UFFICI_UPLOAD_CHUNK_BYTES", "33554433"},
		"a chunk that is not a number":   {"UFFICI_UPLOAD_CHUNK_BYTES", "16MiB"},
		"no prefetch budget at all":      {"UFFICI_PREFETCH_MAX_BYTES", "0"},
	} {
		if _, err := LoadConfig(env(map[string]string{"UFFICI_HOME": home, value[0]: value[1]}), Config{}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestTheDoctorDescribesTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	if got := describeWorkspace(Config{Workspace: dir, WorkspaceFrom: "UFFICI_WORKSPACE"}); !strings.HasSuffix(got, "— writable") ||
		!strings.Contains(got, "(from UFFICI_WORKSPACE)") {
		t.Fatal(got)
	}
	if got := describeWorkspace(Config{Home: dir}); !strings.Contains(got, "not created yet") || !strings.Contains(got, "the default") {
		t.Fatal(got)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := describeWorkspace(Config{Workspace: file}); !strings.Contains(got, "NOT writable") {
		t.Fatal(got)
	}
}

// A workspace the disk refuses serves no file op and announces no `files`
// feature — the companion still answers in text.
func TestAWorkspaceThatCannotBeOpenedServesNoFiles(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	config := doctorConfig(t, double)
	blocked := filepath.Join(config.Home, "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	config.Workspace = filepath.Join(blocked, "workspace")
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _ := Run(ctx, config, logs)
		done <- code
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	plugin := dialPlugin(t, filepath.Join(config.Home, SocketFile), waitForToken(t, config.Home))
	t.Cleanup(func() { _ = plugin.conn.CloseNow() })
	if features, _ := plugin.hello["features"].([]any); slices.Contains(features, any("files")) || plugin.hello["workspace"] != nil {
		t.Fatalf("the hello said %v", plugin.hello)
	}
	if !strings.Contains(logs.String(), "files are not served") || strings.Contains(logs.String(), blocked) {
		t.Fatalf("the log:\n%s", logs.String())
	}
	for err, want := range map[error]string{os.ErrPermission: "permission", os.ErrNotExist: "not_found", errors.New("x"): "unavailable"} {
		if got := errorKind(err); got != want {
			t.Errorf("errorKind(%v) = %q", err, got)
		}
	}
}

// 🔴 Prefetch is the PLUGIN's choice: one that does not ask gets the brief as
// the platform sent it, and nothing is fetched.
func TestNothingIsPrefetchedForAPluginThatDidNotAsk(t *testing.T) {
	rig := newFilesRig(t)
	plugin := dialPlugin(t, filepath.Join(rig.config.Home, SocketFile), waitForToken(t, rig.config.Home))
	t.Cleanup(func() { _ = plugin.conn.CloseNow() })
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	rig.ring(t)
	for _, attachment := range attachmentsOf(t, plugin.readFrame(ctx, t, "brief")) {
		if attachment["localPath"] != nil || attachment["prefetchError"] != nil {
			t.Fatalf("an unasked brief carries %v", attachment)
		}
	}
	if asks := rig.double.TransferAsks(); len(asks) != 0 {
		t.Fatalf("%d grants were asked for an unasked prefetch", len(asks))
	}
}

// 🔴 A request that ends takes its transfers with it, and leaves no partial
// file in the workspace (#2673 requirement 8).
func TestACancelledRequestRemovesItsPartialFiles(t *testing.T) {
	rig := newFilesRig(t)
	big := bytes.Repeat([]byte("b"), 200_000)
	rig.double.SeedAttachment(filesConversation, "1042.1", gatewaytest.File{Name: scanName, Data: big})
	rig.double.StallNextDownloadAfter(50_000)
	plugin := dialPlugin(t, filepath.Join(rig.config.Home, SocketFile), waitForToken(t, rig.config.Home))
	t.Cleanup(func() { _ = plugin.conn.CloseNow() })
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	rig.ring(t)
	plugin.readFrame(ctx, t, "brief")
	plugin.send(t, map[string]any{"v": 1, "type": "ack", "requestId": filesRequest, "outcome": "accepted", "session": "resumed"})

	plugin.send(t, map[string]any{"v": 1, "type": "call", "callId": "c-big", "op": "files.download", "deadlineMs": 20000,
		"payload": map[string]any{"requestId": filesRequest, "attachmentId": "1042.1"}})
	waitUntil(t, "the partial download", func() bool { return rig.partials(t) == 1 })
	if err := rig.double.Cancel(filesRequest, "stop"); err != nil {
		t.Fatal(err)
	}
	result := plugin.readFrame(ctx, t, "result")
	if failure, _ := result["error"].(map[string]any); failure == nil || !strings.Contains(mustMarshal(t, failure["upstream"]), "cancelled") {
		t.Fatalf("a cancelled download answered %v", result)
	}
	waitUntil(t, "the partial removed", func() bool { return rig.partials(t) == 0 })
	if _, err := os.Stat(filepath.Join(rig.workspace(t), "conversations", filesConversation)); !os.IsNotExist(err) {
		t.Fatal("a cancelled download reached the conversation's directory")
	}
}

// 🔴 #2673 self-review H4, through the REAL doorbell queue: a ring whose
// prefetch stalls on a download, then the platform's cancel (a person's stop,
// Art. 14). The cancel is handled while the download is still stalled — the
// plugin is told, the platform hears the close — the prefetch is aborted with
// its partial, and no brief follows.
func TestAStalledPrefetchNeverHoldsThePlatformsCancel(t *testing.T) {
	rig := newFilesRig(t)
	big := bytes.Repeat([]byte("b"), 200_000)
	rig.double.SeedAttachment(filesConversation, "1042.0", gatewaytest.File{Name: invoiceName, Data: big})
	rig.double.StallNextDownloadAfter(50_000)
	plugin, err := dialPluginHello(filepath.Join(rig.config.Home, SocketFile), waitForToken(t, rig.config.Home),
		map[string]any{"capabilities": map[string]any{"prefetchAttachments": map[string]any{"maxBytes": 10_000_000}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plugin.conn.CloseNow() })

	rig.ring(t)
	waitUntil(t, "the stalled prefetch", func() bool { return rig.partials(t) == 1 })
	if err := rig.double.Cancel(filesRequest, "stop"); err != nil {
		t.Fatal(err)
	}
	// Well inside the prefetch's own window (15 s): only a cancel handled while
	// the download is still stalled arrives in time.
	readCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	var told map[string]any
	for told == nil {
		_, raw, err := plugin.conn.Read(readCtx)
		if err != nil {
			t.Fatalf("no cancel reached the plugin while the prefetch was stalled: %v", err)
		}
		var frame map[string]any
		if json.Unmarshal(raw, &frame) == nil && (frame["type"] == "cancel" || frame["type"] == "brief") {
			told = frame
		}
	}
	if told["type"] != "cancel" {
		t.Fatalf("the plugin was told %v", told)
	}
	waitUntil(t, "the close upstream", func() bool {
		for _, closed := range rig.double.Closes() {
			if closed.RequestID == filesRequest {
				return true
			}
		}
		return false
	})
	waitUntil(t, "the prefetch's partial removed", func() bool { return rig.partials(t) == 0 })
}

// 🔴 The change gate's scripted host, built exactly as `--fixture` builds it,
// serves the transfer engine both ways (#2679): the files conversation's two
// attachments download whole, and an upload interrupted mid-PATCH resumes and is
// saved at its place. A grant the engine refuses (a jti not in the platform's
// shape) failed every prefetch of the first Hermes run with nothing else red.
func TestTheScriptedHostMovesFilesForTheEngine(t *testing.T) {
	_, host, err := loadFixture("../../fixture/ci/wave1.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	if _, err := host.OpenDoorbell(context.Background(), contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.DeliverRing(contracts.Ring{RingID: "r", RequestID: "req_1", ConversationID: "conv_ci_files", ThroughCursor: "c0001"}); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	engine, root := buildTransfers(Config{Workspace: workspace, UploadChunkBytes: 1 << 20, PrefetchMaxBytes: 1 << 20},
		host, slog.New(slog.DiscardHandler), func() (int64, bool) { return 1 << 20, true })
	if engine == nil {
		t.Fatal("the scripted host serves no files")
	}
	ctx := context.Background()
	for _, id := range []string{"f1.0", "f1.1"} {
		got, err := engine.Download(ctx, "req_1", transfer.DownloadTarget{AttachmentID: id})
		if err != nil || got.Size == 0 || got.LocalPath == "" {
			t.Fatalf("download %s = %+v, %v (%s)", id, got, err, transfer.ClassOf(err))
		}
	}
	report := append([]byte("%PDF-1.4 [report-canary]\n"), bytes.Repeat([]byte("r"), 100_000)...)
	if err := os.WriteFile(filepath.Join(root, "q3.pdf"), report, 0o600); err != nil {
		t.Fatal(err)
	}
	host.InterruptNextUpload(30_000)
	saved, err := engine.Upload(ctx, "req_1", transfer.UploadRequest{LocalPath: "q3.pdf", To: "reports/q3.pdf"})
	if err != nil || saved.Path != "/reports/q3.pdf" {
		t.Fatalf("upload = %+v, %v (%s)", saved, err, transfer.ClassOf(err))
	}
	if uploads := host.Uploads(); len(uploads) != 1 || !uploads[0].Resumed || !uploads[0].Intact || host.Downloads() != 2 {
		t.Fatalf("uploads %+v, downloads %d", uploads, host.Downloads())
	}
}

// 🔴 The second use case's other half, through the whole daemon with no harness
// (#2679): a file the harness made is saved into Files at the path the agent
// chose (`to`), and an upload whose connection drops half-way resumes from the
// platform's offset and still arrives whole — the call answers once, intact.
func TestTheDaemonSavesIntoFilesAndResumesADroppedUpload(t *testing.T) {
	rig := newFilesRig(t)
	plugin := dialPlugin(t, filepath.Join(rig.config.Home, SocketFile), waitForToken(t, rig.config.Home))
	t.Cleanup(func() { _ = plugin.conn.CloseNow() })
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	rig.ring(t)
	plugin.readFrame(ctx, t, "brief")
	plugin.send(t, map[string]any{"v": 1, "type": "ack", "requestId": filesRequest, "outcome": "accepted", "session": "resumed"})

	report := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("q"), 40_000)...)
	if err := os.MkdirAll(filepath.Join(rig.workspace(t), "out"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig.workspace(t), "out", "q3.pdf"), report, 0o600); err != nil {
		t.Fatal(err)
	}
	rig.double.CutNextPatchAfter(10_000)
	got := plugin.call(ctx, t, "c-save", "files.upload", map[string]any{
		"requestId": filesRequest, "localPath": "out/q3.pdf", "to": "reports/q3.pdf",
	})
	saved, _ := got["payload"].(map[string]any)
	if saved["path"] != "/reports/q3.pdf" || saved["size"] != float64(len(report)) || saved["id"] != nil {
		t.Fatalf("files.upload with to answered %v", got)
	}
	uploads := rig.double.Uploads()
	if len(uploads) != 1 || uploads[0].To != "reports/q3.pdf" || !uploads[0].Completed || !bytes.Equal(uploads[0].Data, report) {
		t.Fatal("the platform does not hold the saved file whole")
	}
	resumed := false
	for _, request := range rig.double.BytesRequests() {
		if request.Method == "HEAD" {
			resumed = true
		}
	}
	if !resumed {
		t.Fatal("the dropped upload was not resumed from the platform's offset")
	}
	if asks := rig.double.TransferAsks(); len(asks) != 1 || asks[0].Body.To != "reports/q3.pdf" {
		t.Fatalf("the grant was asked %d time(s): %+v", len(asks), asks)
	}
	if refusals := rig.double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused off-contract bodies: %v", refusals)
	}
}

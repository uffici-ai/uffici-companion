package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

const (
	testRequest      = "6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21"
	testConversation = "a1b2c3d4-e5f6-4789-8abc-def012345678"
	testJTI          = "0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23"
	testGrant        = "grant-token-secret-never-logged"
)

// hostError is a classified host failure, like the gateway's.
type hostError struct {
	class       string
	instruction string
	retry       bool
	after       time.Duration
}

func (e *hostError) Error() string                  { return "host: " + e.class }
func (e *hostError) ErrorClass() string             { return e.class }
func (e *hostError) ErrorInstruction() string       { return e.instruction }
func (e *hostError) CanRetry() bool                 { return e.retry }
func (e *hostError) RetryAfterDelay() time.Duration { return e.after }

// fakeHost is a TransferHost serving one file from memory.
type fakeHost struct {
	mu        sync.Mutex
	file      []byte
	name      string
	ctype     string
	grantCRC  string
	grantErr  error
	asks      []contracts.TransferAsk
	opens     []int64
	open      func(call int, from int64, ifRange string) (contracts.DownloadMeta, io.ReadCloser, error)
	uploadErr error
	uploaded  []byte
	spec      contracts.UploadSpec
	complete  contracts.TransferCompletion
	completeE error
	jti       string
}

func newFakeHost(data []byte) *fakeHost {
	crc, _, _ := CRC32C(bytes.NewReader(data))
	return &fakeHost{file: data, name: "invoice.pdf", ctype: "application/pdf", grantCRC: crc, jti: testJTI,
		complete: contracts.TransferCompletion{ID: "u-1", Name: "out.pdf", Ref: "conversations/c/out.pdf"}}
}

func (h *fakeHost) ConversationOf(requestID string) (string, bool) {
	return testConversation, requestID == testRequest
}

func (h *fakeHost) GrantTransfer(_ context.Context, requestID string, ask contracts.TransferAsk) (contracts.TransferGrant, error) {
	if requestID != testRequest {
		return contracts.TransferGrant{}, contracts.ErrUnknownRequest
	}
	h.mu.Lock()
	h.asks = append(h.asks, ask)
	h.mu.Unlock()
	if h.grantErr != nil {
		return contracts.TransferGrant{}, h.grantErr
	}
	size := int64(len(h.file))
	if ask.Op == contracts.TransferUpload {
		size = ask.Size
	}
	return contracts.TransferGrant{
		Op: ask.Op, JTI: h.jti, Grant: testGrant, CallURN: "urn:uffici:tool_call:9",
		File: contracts.TransferFile{Name: h.name, Size: size, ContentType: h.ctype, CRC32C: h.grantCRC},
	}, nil
}

func (h *fakeHost) CompleteTransfer(context.Context, string, string) (contracts.TransferCompletion, error) {
	return h.complete, h.completeE
}

func (h *fakeHost) UploadFile(_ context.Context, _ contracts.TransferGrant, src io.ReadSeeker, spec contracts.UploadSpec) error {
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.uploaded, h.spec = data, spec
	h.mu.Unlock()
	return h.uploadErr
}

func (h *fakeHost) OpenDownload(_ context.Context, _ contracts.TransferGrant, from int64, ifRange string) (contracts.DownloadMeta, io.ReadCloser, error) {
	h.mu.Lock()
	h.opens = append(h.opens, from)
	call := len(h.opens)
	h.mu.Unlock()
	if h.open != nil {
		return h.open(call, from, ifRange)
	}
	if from > 0 {
		return contracts.DownloadMeta{Status: 206, Start: from, Total: int64(len(h.file)), ETag: `"e"`},
			io.NopCloser(bytes.NewReader(h.file[from:])), nil
	}
	return contracts.DownloadMeta{Status: 200, Total: int64(len(h.file)), ETag: `"e"`, ContentType: "application/pdf"},
		io.NopCloser(bytes.NewReader(h.file)), nil
}

func newEngine(t *testing.T, host *fakeHost, options ...func(*Options)) (*Engine, *Workspace) {
	t.Helper()
	ws, err := OpenWorkspace(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Host: host, Workspace: ws, Sleep: func(context.Context, time.Duration) error { return nil }}
	for _, option := range options {
		option(&opts)
	}
	engine, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return engine, ws
}

func partials(t *testing.T, ws *Workspace) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(ws.Root(), filepath.FromSlash(partialDir)))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestNewRefusesAMissingHostAndClearsLeftoverPartials(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("an engine with no host was built")
	}
	ws, err := OpenWorkspace(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(ws.Root(), filepath.FromSlash(partialDir), testJTI))
	engine, err := New(Options{Host: newFakeHost(nil), Workspace: ws, ChunkBytes: MaxChunkBytes * 2})
	if err != nil {
		t.Fatal(err)
	}
	if partials(t, ws) != 0 {
		t.Fatal("a previous process's partial survived the start")
	}
	if engine.chunk != MaxChunkBytes || engine.prefetchMax != DefaultPrefetchMaxBytes || engine.attempts != DefaultAttempts {
		t.Fatalf("defaults: chunk %d, prefetch %d, attempts %d", engine.chunk, engine.prefetchMax, engine.attempts)
	}
	if got := engine.backoff(2); got != time.Second {
		t.Fatalf("the default backoff of attempt 2 is %v", got)
	}
	if maxBytes, asked := engine.prefetchAsked(); asked || maxBytes != 0 {
		t.Fatal("prefetch was asked by default")
	}
	if err := engine.sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(ws.Root(), ".uffici"))
	if _, err := New(Options{Host: newFakeHost(nil), Workspace: ws}); err == nil {
		t.Fatal("a workspace without its partial directory was accepted")
	}
}

func TestADownloadedAttachmentIsWrittenAndThenReusedWithoutAGrant(t *testing.T) {
	data := []byte("%PDF-1.7\nhello")
	host := newFakeHost(data)
	host.name = "../../etc/inv\x01oice.pdf"
	engine, ws := newEngine(t, host)

	got, err := engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1042.0"})
	if err != nil {
		t.Fatal(err)
	}
	want := "conversations/" + testConversation + "/1042.0-_.._etc_inv_oice.pdf"
	if got.LocalPath != want || got.Size != int64(len(data)) || got.ContentType != "application/pdf" {
		t.Fatalf("answered %+v, want %s", got, want)
	}
	if got.String() != "Downloaded{size=14}" {
		t.Fatal(got.String())
	}
	again, err := engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1042.0"})
	if err != nil || again.LocalPath != want || again.ContentType != "application/pdf" {
		t.Fatalf("the second download answered %+v, %v", again, err)
	}
	if len(host.asks) != 1 {
		t.Fatalf("%d grants for one immutable attachment", len(host.asks))
	}
	if partials(t, ws) != 0 {
		t.Fatal("a partial outlived its download")
	}
}

func TestADownloadRefusesBeforeAnythingIsSent(t *testing.T) {
	host := newFakeHost([]byte("x"))
	engine, _ := newEngine(t, host)
	cases := map[string]DownloadTarget{
		"an unsafe attachment id": {AttachmentID: "../x"},
		"a path leaving Files":    {Path: "a/../../x"},
		"a hidden path":           {Path: "/.ssh/id_ed25519"},
	}
	for name, target := range cases {
		if _, err := engine.Download(context.Background(), testRequest, target); ClassOf(err) != ClassPathRefused {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := engine.Download(context.Background(), "other", DownloadTarget{Path: "x"}); !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("an unknown request: %v", err)
	}
	if len(host.asks) != 0 {
		t.Fatalf("%d grants asked for refused downloads", len(host.asks))
	}
}

func TestADownloadOverItsBudgetIsNotFetched(t *testing.T) {
	host := newFakeHost([]byte("0123456789"))
	engine, _ := newEngine(t, host)
	if _, err := engine.download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1.0"}, 5); ClassOf(err) != ClassOverBudget {
		t.Fatalf("answered %v", err)
	}
	if len(host.opens) != 0 {
		t.Fatal("an over-budget file was fetched")
	}
	if _, err := engine.download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1.0"}, noLimit); err != nil {
		t.Fatal(err)
	}
	// Already on disk, and still over the budget.
	if _, err := engine.download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1.0"}, 5); ClassOf(err) != ClassOverBudget {
		t.Fatalf("a reused file over the budget answered %v", err)
	}
}

func TestADownloadRestartsWhenTheServerCannotResumeIt(t *testing.T) {
	data := []byte("abcdefghijklmnopqrstuvwxyz")
	host := newFakeHost(data)
	host.grantCRC = ""
	host.ctype = ""
	answerCRC, _, _ := CRC32C(bytes.NewReader(data))
	host.open = func(call int, _ int64, _ string) (contracts.DownloadMeta, io.ReadCloser, error) {
		switch call {
		case 1: // a short answer: the connection dropped after 10 bytes
			return contracts.DownloadMeta{Status: 200, ETag: `"v1"`, CRC32C: answerCRC}, io.NopCloser(bytes.NewReader(data[:10])), nil
		case 2: // the range is past the file: the partial is not its prefix
			return contracts.DownloadMeta{}, nil, &hostError{class: "range_not_satisfiable", retry: true}
		case 3: // a range this side did not ask for
			return contracts.DownloadMeta{Status: 206, Start: 3}, io.NopCloser(bytes.NewReader(data[3:])), nil
		}
		crc, _, _ := CRC32C(bytes.NewReader(data))
		return contracts.DownloadMeta{Status: 200, ContentType: "text/plain; charset=utf-8", CRC32C: crc},
			io.NopCloser(bytes.NewReader(data)), nil
	}
	engine, ws := newEngine(t, host, func(o *Options) { o.Attempts = 5 })
	got, err := engine.Download(context.Background(), testRequest, DownloadTarget{Path: "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalPath != "files/notes.txt" || got.ContentType != "text/plain" {
		t.Fatalf("answered %+v", got)
	}
	written, _ := os.ReadFile(filepath.Join(ws.Root(), "files", "notes.txt"))
	if !bytes.Equal(written, data) {
		t.Fatalf("wrote %q", written)
	}
	if want := []int64{0, 10, 0, 0}; len(host.opens) != 4 || host.opens[1] != want[1] || host.opens[3] != 0 {
		t.Fatalf("opened from %v", host.opens)
	}
}

// 🔴 #2673 self-review M5: a resume splices bytes onto a partial, so it happens
// only when the server can say "the same file" (an ETag for `If-Range`) AND the
// end can prove it (a CRC32C) — otherwise an interrupted download restarts from
// zero. A `206` whose total is not the grant's size is another file's range.
func TestADownloadResumesOnlyWithAValidatorADigestAndTheRightTotal(t *testing.T) {
	data := []byte("abcdefghijklmnopqrstuvwxyz")
	crc, _, _ := CRC32C(bytes.NewReader(data))
	cases := map[string]struct {
		grantCRC string
		first    contracts.DownloadMeta
		second   func(from int64) contracts.DownloadMeta
		opens    []int64
	}{
		"no validator": {
			grantCRC: crc, first: contracts.DownloadMeta{Status: 200},
			opens: []int64{0, 0},
		},
		"no digest anywhere": {
			first: contracts.DownloadMeta{Status: 200, ETag: `"v1"`},
			opens: []int64{0, 0},
		},
		"a 206 of another size": {
			grantCRC: crc, first: contracts.DownloadMeta{Status: 200, ETag: `"v1"`},
			second: func(from int64) contracts.DownloadMeta {
				return contracts.DownloadMeta{Status: 206, Start: from, Total: int64(len(data)) + 5}
			},
			opens: []int64{0, 10, 0},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			host := newFakeHost(data)
			host.grantCRC = tc.grantCRC
			host.open = func(call int, from int64, _ string) (contracts.DownloadMeta, io.ReadCloser, error) {
				switch {
				case call == 1:
					return tc.first, io.NopCloser(bytes.NewReader(data[:10])), nil
				case call == 2 && tc.second != nil:
					return tc.second(from), io.NopCloser(bytes.NewReader([]byte("ZZZZZZZZZZZZZZZZ"))), nil
				case from > 0:
					// A resume would be served another version's tail: spliced, it
					// would be the wrong file — and with no digest, nothing would say so.
					return contracts.DownloadMeta{Status: 206, Start: from, Total: int64(len(data))},
						io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("Z"), len(data)-int(from)))), nil
				}
				return contracts.DownloadMeta{Status: 200}, io.NopCloser(bytes.NewReader(data)), nil
			}
			engine, ws := newEngine(t, host, func(o *Options) { o.Attempts = 5 })
			if _, err := engine.Download(context.Background(), testRequest, DownloadTarget{Path: "notes.txt"}); err != nil {
				t.Fatal(err)
			}
			written, _ := os.ReadFile(filepath.Join(ws.Root(), "files", "notes.txt"))
			if !bytes.Equal(written, data) {
				t.Fatalf("wrote %q", written)
			}
			if !slices.Equal(host.opens, tc.opens) {
				t.Fatalf("opened from %v, want %v", host.opens, tc.opens)
			}
		})
	}
}

func TestADownloadThatGivesUpIsClassifiedAndLeavesNothing(t *testing.T) {
	cases := map[string]struct {
		open func(call int, from int64, ifRange string) (contracts.DownloadMeta, io.ReadCloser, error)
		want string
	}{
		"a refusal": {func(int, int64, string) (contracts.DownloadMeta, io.ReadCloser, error) {
			return contracts.DownloadMeta{}, nil, &hostError{class: "grant_refused", instruction: "x"}
		}, "grant_refused"},
		"retries exhausted": {func(int, int64, string) (contracts.DownloadMeta, io.ReadCloser, error) {
			return contracts.DownloadMeta{}, nil, &hostError{class: "unreachable", retry: true}
		}, "unreachable"},
		"an unclassified failure": {func(int, int64, string) (contracts.DownloadMeta, io.ReadCloser, error) {
			return contracts.DownloadMeta{}, nil, errors.New("boom")
		}, ClassTransferFailed},
		"a short body every time": {func(int, int64, string) (contracts.DownloadMeta, io.ReadCloser, error) {
			return contracts.DownloadMeta{Status: 200}, io.NopCloser(strings.NewReader("ab")), nil
		}, ClassTransferFailed},
		"a body that breaks": {func(int, int64, string) (contracts.DownloadMeta, io.ReadCloser, error) {
			return contracts.DownloadMeta{Status: 200}, io.NopCloser(io.MultiReader(strings.NewReader("a"), failingReader{})), nil
		}, ClassTransferFailed},
		"a longer file": {func(int, int64, string) (contracts.DownloadMeta, io.ReadCloser, error) {
			return contracts.DownloadMeta{Status: 200}, io.NopCloser(strings.NewReader("0123456789-and-more")), nil
		}, ClassSizeMismatch},
		"a wrong resume every time": {func(call int, _ int64, _ string) (contracts.DownloadMeta, io.ReadCloser, error) {
			if call == 1 {
				return contracts.DownloadMeta{Status: 200}, io.NopCloser(strings.NewReader("01")), nil
			}
			return contracts.DownloadMeta{Status: 206, Start: 7}, io.NopCloser(strings.NewReader("789")), nil
		}, ClassTransferFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			host := newFakeHost([]byte("0123456789"))
			host.open = tc.open
			var slept []time.Duration
			engine, ws := newEngine(t, host, func(o *Options) {
				o.Attempts = 3
				o.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
			})
			_, err := engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1.0"})
			if ClassOf(err) != tc.want {
				t.Fatalf("answered %v (%s), want %s", err, ClassOf(err), tc.want)
			}
			if partials(t, ws) != 0 {
				t.Fatal("a failed download kept its partial")
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestARetryHonoursTheDelayThePlatformNamed(t *testing.T) {
	host := newFakeHost([]byte("0123456789"))
	calls := 0
	host.open = func(int, int64, string) (contracts.DownloadMeta, io.ReadCloser, error) {
		calls++
		if calls == 1 {
			return contracts.DownloadMeta{}, nil, &hostError{class: "rate_limited", retry: true, after: 7 * time.Second}
		}
		return contracts.DownloadMeta{Status: 200}, io.NopCloser(bytes.NewReader(host.file)), nil
	}
	var slept []time.Duration
	engine, _ := newEngine(t, host, func(o *Options) {
		o.Backoff = func(int) time.Duration { return time.Second }
		o.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	})
	if _, err := engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1.0"}); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Fatalf("slept %v", slept)
	}
}

func TestAChecksumMismatchDeletesTheBytes(t *testing.T) {
	host := newFakeHost([]byte("0123456789"))
	host.grantCRC = "AAAAAA=="
	engine, ws := newEngine(t, host)
	if _, err := engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1.0"}); ClassOf(err) != ClassChecksumMismatch {
		t.Fatalf("answered %v", err)
	}
	if partials(t, ws) != 0 {
		t.Fatal("mismatched bytes were kept")
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), ConversationsDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("mismatched bytes reached the conversation's directory")
	}
}

func TestADownloadThatCannotBeWrittenSaysSo(t *testing.T) {
	host := newFakeHost([]byte("0123456789"))
	engine, ws := newEngine(t, host)
	// The conversations directory is a FILE: the destination's parent cannot exist.
	mustWrite(t, filepath.Join(ws.Root(), ConversationsDir))
	if _, err := engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1.0"}); ClassOf(err) != ClassLocalWriteFailed {
		t.Fatalf("answered %v", err)
	}
	if partials(t, ws) != 0 {
		t.Fatal("a partial was kept")
	}
	// The partial directory itself cannot be written.
	if err := os.RemoveAll(filepath.Join(ws.Root(), ".uffici")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Download(context.Background(), testRequest, DownloadTarget{Path: "a.txt"}); ClassOf(err) != ClassLocalWriteFailed {
		t.Fatalf("answered %v", err)
	}
}

func TestAGrantWithAnUnusableIDIsRefused(t *testing.T) {
	host := newFakeHost([]byte("0123456789"))
	host.jti = "../../x"
	engine, _ := newEngine(t, host)
	if _, err := engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1.0"}); ClassOf(err) != ClassTransferFailed {
		t.Fatalf("answered %v", err)
	}
	host.jti, host.grantErr = testJTI, &hostError{class: "transfer_denied", instruction: "denied"}
	if _, err := engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "1.0"}); ClassOf(err) != "transfer_denied" {
		t.Fatalf("answered %v", err)
	}
}

func TestAnUploadSendsTheFilesTypeChecksumAndSize(t *testing.T) {
	host := newFakeHost(nil)
	engine, ws := newEngine(t, host, func(o *Options) { o.ChunkBytes = 4096 })
	data := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("p"), 9000)...)
	mustMkdir(t, filepath.Join(ws.Root(), "out"))
	if err := os.WriteFile(filepath.Join(ws.Root(), "out", "chart.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Upload(context.Background(), testRequest, UploadRequest{LocalPath: "out/chart.png", To: "r/c.png", Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	crc, _, _ := CRC32C(bytes.NewReader(data))
	ask := host.asks[0]
	if ask.Op != contracts.TransferUpload || ask.Filename != "chart.png" || ask.Size != int64(len(data)) ||
		ask.CRC32C != crc || ask.ContentType != "image/png" || ask.To != "r/c.png" || !ask.Overwrite {
		t.Fatalf("asked %+v", ask)
	}
	if !bytes.Equal(host.uploaded, data) || host.spec.ChunkBytes != 4096 || host.spec.Size != int64(len(data)) {
		t.Fatalf("uploaded %d bytes with %+v", len(host.uploaded), host.spec)
	}
	if got.ID != "u-1" || got.Size != int64(len(data)) {
		t.Fatalf("answered %+v", got)
	}
}

func TestAnUploadRefusesWhatItMayNotRead(t *testing.T) {
	host := newFakeHost(nil)
	engine, ws := newEngine(t, host)
	outside := filepath.Join(t.TempDir(), "secret")
	mustWrite(t, outside)
	mustLink(t, outside, filepath.Join(ws.Root(), "link"))
	mustMkdir(t, filepath.Join(ws.Root(), "dir"))
	cases := map[string]string{
		"../etc/passwd":        ClassPathRefused,
		"link":                 ClassPathRefused,
		".env":                 ClassPathRefused,
		"missing.pdf":          ClassLocalFileMissing,
		"dir":                  ClassLocalFileMissing,
		"~/.ssh/id_ed25519":    ClassPathRefused,
		"/etc/hosts":           ClassPathRefused,
		"dir/../../etc/passwd": ClassPathRefused,
	}
	for path, want := range cases {
		if _, err := engine.Upload(context.Background(), testRequest, UploadRequest{LocalPath: path}); ClassOf(err) != want {
			t.Errorf("%s: %v (%s), want %s", path, err, ClassOf(err), want)
		}
	}
	if _, err := engine.Upload(context.Background(), "other", UploadRequest{LocalPath: "x"}); !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("an unknown request: %v", err)
	}
	if len(host.asks) != 0 {
		t.Fatalf("%d grants asked for refused uploads", len(host.asks))
	}
}

func TestAnUploadCarriesThePlatformsFailure(t *testing.T) {
	refused := &hostError{class: "file_exists", instruction: "a file is already there"}
	for name, mutate := range map[string]func(*fakeHost){
		"the grant":      func(h *fakeHost) { h.grantErr = refused },
		"the bytes":      func(h *fakeHost) { h.uploadErr = refused },
		"the completion": func(h *fakeHost) { h.completeE = refused },
	} {
		t.Run(name, func(t *testing.T) {
			host := newFakeHost(nil)
			mutate(host)
			engine, ws := newEngine(t, host)
			mustWrite(t, filepath.Join(ws.Root(), "a.txt"))
			if _, err := engine.Upload(context.Background(), testRequest, UploadRequest{LocalPath: "a.txt"}); ClassOf(err) != "file_exists" {
				t.Fatalf("answered %v", err)
			}
		})
	}
}

func TestTheOpsAnswerOnTheWire(t *testing.T) {
	data := []byte("%PDF-1.7 body")
	host := newFakeHost(data)
	engine, ws := newEngine(t, host)
	handlers := engine.Handlers()
	if len(handlers) != 2 || handlers[wire.OpFilesDownload] == nil || handlers[wire.OpFilesUpload] == nil {
		t.Fatalf("handlers %v", handlers)
	}
	var none *Engine
	if none.Handlers() != nil {
		t.Fatal("a nil engine served ops")
	}
	call := func(op string, payload string) (map[string]any, *surface.CallFailure) {
		t.Helper()
		raw, err := handlers[op](context.Background(), json.RawMessage(payload))
		if err != nil {
			var failure *surface.CallFailure
			if !errors.As(err, &failure) {
				t.Fatalf("%s answered an unclassified %v", op, err)
			}
			return nil, failure
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out, nil
	}

	got, failure := call(wire.OpFilesDownload, `{"requestId":"`+testRequest+`","attachmentId":"1042.0"}`)
	if failure != nil || got["localPath"] != "conversations/"+testConversation+"/1042.0-invoice.pdf" || got["size"] != float64(len(data)) {
		t.Fatalf("download answered %v %+v", got, failure)
	}
	mustWrite(t, filepath.Join(ws.Root(), "a.txt"))
	got, failure = call(wire.OpFilesUpload, `{"requestId":"`+testRequest+`","localPath":"a.txt"}`)
	if failure != nil || got["ref"] == nil || got["id"] != "u-1" || got["size"] != float64(1) {
		t.Fatalf("upload answered %v %+v", got, failure)
	}
	host.complete = contracts.TransferCompletion{Path: "/r/a.txt"}
	got, failure = call(wire.OpFilesUpload, `{"requestId":"`+testRequest+`","localPath":"a.txt","to":"r/a.txt"}`)
	if failure != nil || got["path"] != "/r/a.txt" || got["ref"] != nil {
		t.Fatalf("upload into Files answered %v %+v", got, failure)
	}

	cases := []struct {
		op, payload string
		class       wire.CallErrorClass
		upstream    string
	}{
		{wire.OpFilesDownload, `{"requestId":"x","attachmentId":"1","path":"p"}`, wire.ClassMalformedPayload, ""},
		{wire.OpFilesDownload, `not json`, wire.ClassMalformedPayload, ""},
		{wire.OpFilesUpload, `{"requestId":"x"}`, wire.ClassMalformedPayload, ""},
		{wire.OpFilesDownload, `{"requestId":"other","path":"p"}`, wire.ClassNotPermitted, ""},
		{wire.OpFilesUpload, `{"requestId":"` + testRequest + `","localPath":"../x"}`, wire.ClassNotPermitted, ClassPathRefused},
		{wire.OpFilesUpload, `{"requestId":"` + testRequest + `","localPath":"nothing.txt"}`, wire.ClassMalformedPayload, ClassLocalFileMissing},
	}
	for _, tc := range cases {
		_, failure := call(tc.op, tc.payload)
		if failure == nil || failure.Class != tc.class {
			t.Errorf("%s %s answered %+v, want %s", tc.op, tc.payload, failure, tc.class)
			continue
		}
		if tc.upstream != "" && !strings.Contains(string(failure.Upstream), tc.upstream) {
			t.Errorf("%s upstream %s, want %s", tc.payload, failure.Upstream, tc.upstream)
		}
		if failure.Class != wire.ClassMalformedPayload || tc.upstream != "" {
			if failure.Instruction == "" {
				t.Errorf("%s carries no instruction", tc.payload)
			}
		}
	}

	// The platform's refusal carries its own class and sentence.
	host.grantErr = &hostError{class: "transfer_denied", instruction: "Files are off for this agent."}
	_, failure = call(wire.OpFilesDownload, `{"requestId":"`+testRequest+`","path":"p.txt"}`)
	if failure == nil || failure.Class != wire.ClassUpstreamFailed || failure.Instruction != "Files are off for this agent." ||
		!strings.Contains(string(failure.Upstream), "transfer_denied") {
		t.Fatalf("a platform refusal answered %+v", failure)
	}
	// A failure with no sentence of its own gets the companion's.
	host.grantErr = errors.New("boom")
	if _, failure = call(wire.OpFilesDownload, `{"requestId":"`+testRequest+`","path":"p.txt"}`); failure == nil || failure.Instruction == "" {
		t.Fatalf("an unclassified failure answered %+v", failure)
	}
	// A completion off the result's shape is never put on the wire.
	host.grantErr = nil
	host.complete = contracts.TransferCompletion{ID: "u-1", Name: "a", Ref: ""}
	if _, failure = call(wire.OpFilesUpload, `{"requestId":"`+testRequest+`","localPath":"a.txt"}`); failure == nil ||
		failure.Class != wire.ClassUpstreamFailed {
		t.Fatalf("an off-shape completion answered %+v", failure)
	}
}

func TestPrefetchIsThePluginsChoice(t *testing.T) {
	entries := []contracts.Entry{
		{ID: "1", Text: "no files"},
		{ID: "2", Attachments: []contracts.Attachment{{ID: "2.0", Name: "a.pdf"}, {ID: "", Name: "unnamed"}, {ID: "2.1", Name: "b.pdf"}}},
	}

	t.Run("not asked: nothing is fetched", func(t *testing.T) {
		host := newFakeHost([]byte("0123456789"))
		engine, _ := newEngine(t, host)
		out := engine.Prefetch(context.Background(), testRequest, entries)
		if len(host.asks) != 0 || out[1].Attachments[0].LocalPath != "" || out[1].Attachments[0].PrefetchClass != "" {
			t.Fatalf("an unasked prefetch fetched %d files", len(host.asks))
		}
	})

	t.Run("within the budget", func(t *testing.T) {
		host := newFakeHost([]byte("0123456789"))
		engine, _ := newEngine(t, host, func(o *Options) {
			o.PrefetchAsked = func() (int64, bool) { return 100, true }
		})
		out := engine.Prefetch(context.Background(), testRequest, entries)
		if out[1].Attachments[0].LocalPath == "" || out[1].Attachments[2].LocalPath == "" || out[1].Attachments[1].LocalPath != "" {
			t.Fatalf("prefetched %+v", out[1].Attachments)
		}
		// 🔴 Copied, never edited in place: the host's read is the platform's.
		if entries[1].Attachments[0].LocalPath != "" {
			t.Fatal("the caller's entries were edited")
		}
	})

	t.Run("over the budget", func(t *testing.T) {
		host := newFakeHost([]byte("0123456789"))
		engine, _ := newEngine(t, host, func(o *Options) {
			o.PrefetchAsked = func() (int64, bool) { return 15, true }
		})
		out := engine.Prefetch(context.Background(), testRequest, entries)
		first, second := out[1].Attachments[0], out[1].Attachments[2]
		if first.LocalPath == "" || second.LocalPath != "" || second.PrefetchClass != ClassOverBudget {
			t.Fatalf("prefetched %+v and %+v", first, second)
		}
	})

	t.Run("the companion's own cap wins", func(t *testing.T) {
		host := newFakeHost([]byte("0123456789"))
		engine, _ := newEngine(t, host, func(o *Options) {
			o.PrefetchMaxBytes = 5
			o.PrefetchAsked = func() (int64, bool) { return 1 << 30, true }
		})
		out := engine.Prefetch(context.Background(), testRequest, entries)
		if out[1].Attachments[0].PrefetchClass != ClassOverBudget {
			t.Fatalf("prefetched %+v", out[1].Attachments[0])
		}
	})

	t.Run("an ended request fetches nothing more", func(t *testing.T) {
		host := newFakeHost([]byte("0123456789"))
		engine, _ := newEngine(t, host, func(o *Options) {
			o.PrefetchAsked = func() (int64, bool) { return 100, true }
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		out := engine.Prefetch(ctx, testRequest, entries)
		if out[1].Attachments[0].PrefetchClass != ClassCancelled || len(host.asks) != 0 {
			t.Fatalf("prefetched %+v", out[1].Attachments[0])
		}
	})

	t.Run("no attachments: the same entries", func(t *testing.T) {
		host := newFakeHost(nil)
		engine, _ := newEngine(t, host, func(o *Options) {
			o.PrefetchAsked = func() (int64, bool) { return 100, true }
		})
		plain := []contracts.Entry{{ID: "1"}}
		if out := engine.Prefetch(context.Background(), testRequest, plain); &out[0] != &plain[0] {
			t.Fatal("entries with no attachments were copied")
		}
	})
}

func TestAnEndedRequestTakesItsTransfersWithIt(t *testing.T) {
	host := newFakeHost([]byte("0123456789"))
	started := make(chan struct{})
	host.open = func(int, int64, string) (contracts.DownloadMeta, io.ReadCloser, error) {
		return contracts.DownloadMeta{Status: 200}, io.NopCloser(&blockingReader{started: started}), nil
	}
	engine, ws := newEngine(t, host)
	for _, end := range []func(){
		func() { engine.Observer().Cancelled(testRequest, contracts.CancelStop) },
		func() { engine.Observer().Closed(testRequest, contracts.OutcomeAnswered, "") },
	} {
		ctx := context.Background()
		done := make(chan error, 1)
		started = make(chan struct{})
		go func() {
			_, err := engine.Download(ctx, testRequest, DownloadTarget{AttachmentID: "1.0"})
			done <- err
		}()
		<-started
		if engine.Active() != 1 || partials(t, ws) != 1 {
			t.Fatalf("%d active, %d partials", engine.Active(), partials(t, ws))
		}
		engine.Abort("another-request")
		end()
		select {
		case err := <-done:
			if ClassOf(err) != ClassCancelled {
				t.Fatalf("answered %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the transfer outlived its request")
		}
		if partials(t, ws) != 0 || engine.Active() != 0 {
			t.Fatal("a partial outlived its request")
		}
	}
}

// blockingReader writes a little, then waits for the transfer's context to end —
// which, for an io.Copy, means the body is closed under it.
type blockingReader struct {
	started chan struct{}
	sent    bool
	once    sync.Once
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "01"), nil
	}
	r.once.Do(func() { close(r.started) })
	time.Sleep(10 * time.Millisecond)
	return 0, io.ErrUnexpectedEOF
}

func TestClassOf(t *testing.T) {
	cases := map[string]error{
		"":                  nil,
		ClassPathRefused:    &FenceError{Reason: FenceDotfile},
		ClassOverBudget:     fail(ClassOverBudget, nil),
		ClassCancelled:      context.Canceled,
		"unknown_request":   contracts.ErrUnknownRequest,
		"grant_refused":     &hostError{class: "grant_refused"},
		ClassTransferFailed: errors.New("x"),
	}
	for want, err := range cases {
		if got := ClassOf(err); got != want {
			t.Errorf("ClassOf(%v) = %q, want %q", err, got, want)
		}
	}
	failure := fail(ClassChecksumMismatch, io.EOF)
	if !errors.Is(failure, io.EOF) || failure.ErrorClass() != ClassChecksumMismatch || failure.ErrorInstruction() == "" ||
		failure.Error() != "transfer: checksum_mismatch" {
		t.Fatalf("%+v", failure)
	}
	if (&FenceError{Reason: FenceParent}).Error() != "workspace: path refused (parent)" {
		t.Fatal("the fence error's text")
	}
}

func TestLocalName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":             "report.pdf",
		"../../etc/passwd":       "_.._etc_passwd",
		".hidden":                "hidden",
		"   ":                    "file",
		"a\\b\x00c\x7fd":         "a_b_c_d",
		strings.Repeat("é", 200): strings.Repeat("é", 87),
	}
	for in, want := range cases {
		if got := localName(in); got != want {
			t.Errorf("localName(%q) = %q, want %q", in, got, want)
		}
	}
	if _, size := lastRune("a"); size != 1 {
		t.Fatal("lastRune")
	}
	if _, size := lastRune("\x80\x80"); size != 1 {
		t.Fatal("lastRune on continuation bytes alone")
	}
}

func TestCRC32CIsGCSsChecksum(t *testing.T) {
	// The CRC32C check value: "123456789" is 0xE3069283, big-endian, in base64.
	sum, n, err := CRC32C(strings.NewReader("123456789"))
	if err != nil || sum != "4waSgw==" || n != 9 {
		t.Fatalf("CRC32C = %q, %d, %v", sum, n, err)
	}
	if _, _, err := CRC32C(failingReader{}); err == nil {
		t.Fatal("a failing reader was summed")
	}
}

func TestSniffContentTypeReadsTheBytesNotTheName(t *testing.T) {
	cases := map[string]string{
		"%PDF-1.7\n":            "application/pdf",
		"\x89PNG\r\n\x1a\nxxxx": "image/png",
		"hello, plain text":     "text/plain",
		"\x00\x01\x02\x03":      "application/octet-stream",
	}
	for head, want := range cases {
		if got, err := SniffContentType(strings.NewReader(head)); err != nil || got != want {
			t.Errorf("sniff(%q) = %q, %v; want %q", head, got, err, want)
		}
	}
	if _, err := SniffContentType(failingReader{}); err == nil {
		t.Fatal("a failing reader was sniffed")
	}
	if got := cleanContentType("not a type"); got != octetStream {
		t.Fatal(got)
	}
	if got := cleanContentType("TEXT/HTML; charset=utf-8"); got != "text/html" {
		t.Fatal(got)
	}
}

// 🔴 Logs are a published surface: a transfer leaves ids, sizes and classes —
// never a path, a file name or the grant (#2673 requirement 6).
func TestATransferNeverLogsAPathAFilenameOrTheGrant(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	host := newFakeHost([]byte("%PDF secret-body"))
	host.name = "quarterly-salaries.pdf"
	engine, ws := newEngine(t, host, func(o *Options) {
		o.Logger = logger
		o.PrefetchAsked = func() (int64, bool) { return 1 << 20, true }
	})
	if _, err := engine.Download(context.Background(), testRequest, DownloadTarget{Path: "hr/quarterly-salaries.pdf"}); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(ws.Root(), "private-folder"))
	if err := os.WriteFile(filepath.Join(ws.Root(), "private-folder", "board-minutes.txt"), []byte("minutes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Upload(context.Background(), testRequest, UploadRequest{LocalPath: "private-folder/board-minutes.txt", To: "hr/minutes.txt"}); err != nil {
		t.Fatal(err)
	}
	engine.Prefetch(context.Background(), testRequest, []contracts.Entry{{Attachments: []contracts.Attachment{{ID: "9.0", Name: "quarterly-salaries.pdf"}}}})
	host.grantErr = &hostError{class: "transfer_denied", instruction: "denied"}
	_, _ = engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "8.0"})
	host.grantErr, host.uploadErr = nil, &hostError{class: "file_too_large"}
	_, _ = engine.Upload(context.Background(), testRequest, UploadRequest{LocalPath: "private-folder/board-minutes.txt"})
	host.open = func(int, int64, string) (contracts.DownloadMeta, io.ReadCloser, error) {
		return contracts.DownloadMeta{}, nil, &hostError{class: "grant_refused"}
	}
	_, _ = engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "7.0"})

	out := logs.String()
	if !strings.Contains(out, testJTI) || !strings.Contains(out, `"class":"transfer_denied"`) {
		t.Fatalf("the transfer lines carry no ids or classes:\n%s", out)
	}
	for _, secret := range []string{
		"quarterly-salaries", "board-minutes", "private-folder", "hr/", testGrant, ws.Root(), "secret-body", "minutes.txt",
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("the log carries %q:\n%s", secret, out)
		}
	}
}

var _ contracts.TransferHost = (*fakeHost)(nil)

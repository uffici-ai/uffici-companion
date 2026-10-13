package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	tus "github.com/eventials/go-tus"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/waits"
)

// The bytes of a transfer (#2673), on the agents host's files routes (#2672):
// tus for an upload, an HTTP range for a download — both authorized by the
// grant ALONE, never a ticket or the key.
//
// 🔴 go-tus (`github.com/eventials/go-tus`) is the upload client, and three of
// its edges are worked around here rather than patched in a fork:
//
//   - it takes no context: every request it makes goes through ctxTransport,
//     which binds the upload's context, so an abort stops a chunk in flight;
//   - its Config.Header rides every request, which is how `Authorization:
//     Bearer <grant>` reaches the creation, each HEAD and each PATCH; the
//     gateway answers a RELATIVE Location, which go-tus resolves against the
//     creation URL itself;
//   - it reads one chunk into a buffer of ChunkBytes per PATCH — bounded by the
//     chunk (≤ 32 MiB, the edge's body cap), never by the file;
//   - each Uploader parks one goroutine on its progress channel for the life of
//     the process, and CreateOrResumeUpload makes a new Uploader per ATTEMPT —
//     the first, and one per resume (go-tus exposes no way to move an existing
//     Uploader to the server's offset). So an upload leaves 1 + its retries
//     goroutines behind, a few kilobytes each, holding the client and with it
//     the request header: 🔴 that header's grant is deleted when UploadFile
//     returns, so what stays parked holds no secret. Accepted and recorded.
//
// 🔴 The grant is SECRET-CLASS: it is in a header and nowhere else. go-tus's
// errors carry a status and the gateway's envelope, never the grant; only a
// class and a status leave this file.

// downloadRefusalBytes bounds what is read of a refused download's body: the
// gateway's envelope, never a file.
const downloadRefusalBytes = 64 << 10

// terminateBound bounds the best-effort termination of an abandoned upload.
const terminateBound = 5 * time.Second

// The waits a transfer's connection may spend silent (#2673 self-review H4).
const (
	// DefaultFilesHeaderTimeout bounds the wait for an answer's headers.
	DefaultFilesHeaderTimeout = 30 * time.Second
	// DefaultFilesIdleTimeout bounds the wait for each read of a download's body.
	DefaultFilesIdleTimeout = 60 * time.Second
)

// withHeaderTimeout is the files client with its transport's
// ResponseHeaderTimeout bounded: a clone, so the caller's transport is not
// changed under anyone else. A transport that is not an *http.Transport is
// kept as it is — the idle bound on a download's reads still holds.
func withHeaderTimeout(client *http.Client, timeout time.Duration) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	base, ok := client.Transport.(*http.Transport)
	if client.Transport == nil {
		base, ok = http.DefaultTransport.(*http.Transport)
	}
	if !ok {
		return client
	}
	transport := base.Clone()
	if transport.ResponseHeaderTimeout <= 0 || transport.ResponseHeaderTimeout > timeout {
		transport.ResponseHeaderTimeout = timeout
	}
	bounded := *client
	bounded.Transport = transport
	return &bounded
}

// idleBody ends a download's request when one read waits longer than idle: a
// server that stops sending mid-file fails the attempt — which is retried, and
// resumed — instead of holding the transfer until the request's deadline.
type idleBody struct {
	body  io.ReadCloser
	idle  time.Duration
	timer *time.Timer
	stop  context.CancelFunc
}

func newIdleBody(body io.ReadCloser, idle time.Duration, stop context.CancelFunc) *idleBody {
	timer := time.AfterFunc(idle, stop)
	timer.Stop()
	return &idleBody{body: body, idle: idle, timer: timer, stop: stop}
}

func (b *idleBody) Read(p []byte) (int, error) {
	// Only the wait on the network counts: the time the caller spends writing
	// what it read to disk is not the server's silence.
	b.timer.Reset(b.idle)
	n, err := b.body.Read(p)
	b.timer.Stop()
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.body.Close()
	b.stop()
	return err
}

// The classes the bytes routes add to the companion's own.
const (
	// ClassRangeNotSatisfiable — a resume asked for a range past the file: the
	// partial is not this file's prefix, and the download restarts.
	ClassRangeNotSatisfiable = "range_not_satisfiable"
	// ClassFileTooLarge — the platform refused the upload's length.
	ClassFileTooLarge = "file_too_large"
	// ClassUploadGone — the upload the grant names is no longer on the server.
	ClassUploadGone = "upload_gone"
)

var filesInstructions = map[string]string{
	ClassFileTooLarge: "The file is larger than the platform accepts; tell the person it could not be uploaded.",
	ClassUploadGone: "The upload expired on the platform before it finished; tell the person, and try the upload " +
		"again.",
	ClassRangeNotSatisfiable: "The file changed while it was being downloaded; try the download again.",
}

func filesError(class string, status int, retryable bool) *Error {
	return &Error{Class: class, Instruction: filesInstructions[class], Status: status, Retryable: retryable}
}

// ctxTransport binds one upload's context to every request go-tus makes.
type ctxTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t ctxTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.WithContext(t.ctx))
}

// oneUpload is go-tus's resume store for exactly one upload: the URL the
// creation answered, under the grant's jti.
type oneUpload struct{ url string }

func (s *oneUpload) Get(string) (string, bool) { return s.url, s.url != "" }
func (s *oneUpload) Set(_, url string)         { s.url = url }
func (s *oneUpload) Delete(string)             { s.url = "" }
func (s *oneUpload) Close()                    {}

// UploadFile sends an upload's bytes with tus (#2673): created, sent in chunks,
// and after any failure resumed from the server's own offset (a `HEAD`), with
// the control plane's backoff, until spec.Attempts tries in a row made no
// progress. An abort terminates the upload on the server, best effort.
func (h *Host) UploadFile(ctx context.Context, grant contracts.TransferGrant, src io.ReadSeeker, spec contracts.UploadSpec) error {
	return h.files().UploadFile(ctx, grant, src, spec)
}

// OpenDownload is one ranged GET of a download (#2673).
func (h *Host) OpenDownload(ctx context.Context, grant contracts.TransferGrant, from int64, ifRange string) (contracts.DownloadMeta, io.ReadCloser, error) {
	return h.files().OpenDownload(ctx, grant, from, ifRange)
}

// files is the host's bytes client: the agents host's files routes, the
// companion's user agent, the files timeouts and the control plane's clock.
func (h *Host) files() *FilesClient {
	return &FilesClient{
		BaseURL: h.filesURL(), HTTP: h.filesHTTP, UserAgent: h.api.userAgent, Sleep: h.sleep,
		Logger: h.log, IdleTimeout: h.filesIdle,
	}
}

// FilesClient carries a transfer's bytes on an agents host's files routes
// (#2673): tus up, an HTTP range down, under the grant alone. The gateway host
// uses it against `agents.<domain>`; the scripted host (#2679) against its own
// loopback files routes — so a fixture run moves its bytes through the SAME
// client, resume, timeouts and grant hygiene included, and not a stand-in for it.
type FilesClient struct {
	// BaseURL is the files routes' base, `…/v1/files`.
	BaseURL string
	// HTTP carries the bytes; its Transport is the one an upload binds its
	// context to. Nil is a fresh default transport with the header timeout.
	HTTP *http.Client
	// UserAgent rides every request.
	UserAgent string
	// Sleep waits a retry's backoff; nil waits on the context.
	Sleep func(ctx context.Context, d time.Duration) error
	// Logger receives classified failures only; nil discards.
	Logger *slog.Logger
	// IdleTimeout bounds each read of a download's body; zero is the default.
	IdleTimeout time.Duration
}

func (c *FilesClient) httpClient() *http.Client {
	if c.HTTP == nil {
		return withHeaderTimeout(nil, DefaultFilesHeaderTimeout)
	}
	return c.HTTP
}

func (c *FilesClient) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep == nil {
		return waits.Sleep(ctx, d)
	}
	return c.Sleep(ctx, d)
}

func (c *FilesClient) idle() time.Duration {
	if c.IdleTimeout <= 0 {
		return DefaultFilesIdleTimeout
	}
	return c.IdleTimeout
}

// UploadFile sends an upload's bytes with tus: see Host.UploadFile.
func (c *FilesClient) UploadFile(ctx context.Context, grant contracts.TransferGrant, src io.ReadSeeker, spec contracts.UploadSpec) error {
	transport := c.httpClient().Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	store := &oneUpload{}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+grant.Grant)
	header.Set("User-Agent", c.UserAgent)
	// 🔴 Wiped when the upload returns: every go-tus Uploader this upload made
	// keeps its client — and this header — reachable from a goroutine that never
	// ends, so the grant must not outlive the upload inside it.
	defer header.Del("Authorization")
	client, err := newTusClient(c.BaseURL+"/uploads", &tus.Config{
		ChunkSize: spec.ChunkBytes, Resume: true, Store: store, Header: header,
		HttpClient: &http.Client{Transport: ctxTransport{ctx: ctx, base: transport}},
	})
	if err != nil {
		return localError(ClassPlatformError, 0)
	}
	metadata := tus.Metadata{"filename": spec.Filename, "contentType": spec.ContentType, "crc32c": spec.CRC32C}
	upload := tus.NewUpload(src, spec.Size, metadata, grant.JTI)
	attempts := max(spec.Attempts, 1)

	failures := 0
	for {
		// The first pass creates the upload; every later one RESUMES it — 🔴 from
		// the SERVER's offset (a `HEAD`), never this side's: a chunk whose answer
		// was lost may have been written, and a PATCH at a stale offset is a 409.
		uploader, err := client.CreateOrResumeUpload(upload)
		if err == nil {
			before := uploader.Offset()
			err = uploader.Upload()
			if err == nil {
				return nil
			}
			if uploader.Offset() > before {
				failures = 0
			}
		}
		failures++
		if !c.retryTransfer(ctx, err, failures, attempts) {
			c.terminate(store.url, header, transport)
			return c.transferFailure(ctx, err)
		}
	}
}

// newTusClient is go-tus's constructor; a variable so a test can hold the
// config the upload built.
var newTusClient = tus.NewClient

// retryTransfer decides, after a failure, whether to try again — and waits the
// backoff when it will. A context that ended never retries. Its twin for a
// download's ranged GETs is the transfer engine's `Engine.retry`
// (internal/transfer/engine.go): the same rule over the host's classified
// errors. Change one, change both.
func (c *FilesClient) retryTransfer(ctx context.Context, err error, failures, attempts int) bool {
	if ctx.Err() != nil || failures >= attempts {
		return false
	}
	classified := tusError(err)
	if !classified.Retryable {
		return false
	}
	delay := retryDelay(failures - 1)
	if classified.RetryAfter > delay {
		delay = classified.RetryAfter
	}
	return c.sleep(ctx, delay) == nil
}

// transferFailure is the error an upload ends with: the context's when it ended,
// else the classified failure — logged by class and status alone.
func (c *FilesClient) transferFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	classified := tusError(err)
	logPlatformFailure(c.Logger, "transfer_bytes", classified)
	return classified
}

// terminate deletes an unfinished upload on the server (tus termination), best
// effort and bounded: the bytes already sent are nobody's now.
func (c *FilesClient) terminate(uploadURL string, header http.Header, transport http.RoundTripper) {
	if uploadURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminateBound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, uploadURL, nil)
	if err != nil {
		return
	}
	req.Header = header.Clone()
	req.Header.Set("Tus-Resumable", tus.ProtocolVersion)
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, downloadRefusalBytes))
	_ = resp.Body.Close()
}

// tusError classifies whatever go-tus or the transport returned.
func tusError(err error) *Error {
	var classified *Error
	if errors.As(err, &classified) {
		return classified
	}
	var client tus.ClientError
	if errors.As(err, &client) {
		contentType := ""
		if bytes.HasPrefix(bytes.TrimSpace(client.Body), []byte("{")) {
			contentType = "application/json"
		}
		out := classifyResponse(client.Code, contentType, client.Body)
		if client.Code == http.StatusTooManyRequests || client.Code == http.StatusLocked || client.Code >= 500 {
			out.Retryable = true
		}
		return out
	}
	switch {
	case errors.Is(err, tus.ErrOffsetMismatch):
		// The server holds more (or less) than this side thought: a HEAD says how much.
		return filesError(ClassPlatformError, http.StatusConflict, true)
	case errors.Is(err, tus.ErrLargeUpload):
		return filesError(ClassFileTooLarge, http.StatusRequestEntityTooLarge, false)
	case errors.Is(err, tus.ErrUploadNotFound):
		return filesError(ClassUploadGone, http.StatusNotFound, false)
	case errors.Is(err, tus.ErrVersionMismatch):
		return localError(ClassPlatformError, http.StatusPreconditionFailed)
	}
	// A transport failure: the connection dropped, or never opened. 🔴 Never the
	// error's own text — it quotes the URL.
	return localError(ClassUnreachable, 0)
}

// OpenDownload is one ranged GET of a download: see Host.OpenDownload.
func (c *FilesClient) OpenDownload(ctx context.Context, grant contracts.TransferGrant, from int64, ifRange string) (contracts.DownloadMeta, io.ReadCloser, error) {
	// The request's own context, which the idle bound ends: never the caller's.
	reqCtx, stop := context.WithCancel(ctx)
	meta, body, err := c.openDownload(ctx, reqCtx, grant, from, ifRange)
	if err != nil {
		stop()
		return meta, nil, err
	}
	return meta, newIdleBody(body, c.idle(), stop), nil
}

func (c *FilesClient) openDownload(ctx, reqCtx context.Context, grant contracts.TransferGrant, from int64, ifRange string) (contracts.DownloadMeta, io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.BaseURL+"/download", nil)
	if err != nil {
		return contracts.DownloadMeta{}, nil, localError(ClassPlatformError, 0)
	}
	req.Header.Set("Authorization", "Bearer "+grant.Grant)
	req.Header.Set("User-Agent", c.UserAgent)
	if from > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
		if ifRange != "" {
			req.Header.Set("If-Range", ifRange)
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return contracts.DownloadMeta{}, nil, ctx.Err()
		}
		return contracts.DownloadMeta{}, nil, localError(ClassUnreachable, 0)
	}
	meta := contracts.DownloadMeta{
		Status: resp.StatusCode, Total: -1, ETag: resp.Header.Get("ETag"),
		ContentType: resp.Header.Get("Content-Type"), CRC32C: reprDigestCRC32C(resp.Header.Get("Repr-Digest")),
	}
	switch resp.StatusCode {
	case http.StatusOK:
		meta.Total = resp.ContentLength
		return meta, resp.Body, nil
	case http.StatusPartialContent:
		start, total, ok := contentRange(resp.Header.Get("Content-Range"))
		if !ok {
			_ = resp.Body.Close()
			return contracts.DownloadMeta{}, nil, localError(ClassPlatformError, resp.StatusCode)
		}
		meta.Start, meta.Total = start, total
		return meta, resp.Body, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return contracts.DownloadMeta{}, nil, filesError(ClassRangeNotSatisfiable, resp.StatusCode, true)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, downloadRefusalBytes))
	out := classifyResponse(resp.StatusCode, resp.Header.Get("Content-Type"), raw)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusLocked || resp.StatusCode >= 500 {
		out.Retryable = true
	}
	if seconds, convErr := strconv.Atoi(resp.Header.Get("Retry-After")); convErr == nil && seconds > 0 && out.RetryAfter == 0 {
		out.RetryAfter = time.Duration(seconds) * time.Second
	}
	return contracts.DownloadMeta{}, nil, out
}

var contentRangePattern = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+|\*)$`)

// contentRange reads `bytes a-b/total`; total is -1 when the server says `*`.
func contentRange(value string) (start, total int64, ok bool) {
	match := contentRangePattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	total = -1
	if match[3] != "*" {
		if total, err = strconv.ParseInt(match[3], 10, 64); err != nil {
			return 0, 0, false
		}
	}
	return start, total, true
}

var crc32cDigest = regexp.MustCompile(`(?:^|,)\s*crc32c=:([A-Za-z0-9+/]{6}==):`)

// reprDigestCRC32C reads the whole file's CRC32C from `Repr-Digest`
// (RFC 9530): `crc32c=:<base64>:`. Empty when the header names none.
func reprDigestCRC32C(value string) string {
	if match := crc32cDigest.FindStringSubmatch(value); match != nil {
		return match[1]
	}
	return ""
}

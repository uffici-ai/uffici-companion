package gatewaytest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// File is a file the double holds: an attachment or a Files path (#2673).
type File struct {
	Name        string
	ContentType string
	Data        []byte
	// ETag is the validator a resume names; empty takes one derived from the bytes.
	ETag string
}

// TransferAsk is one transfer grant the double minted: what was asked.
type TransferAsk struct {
	RequestID string
	Body      protocol.TransferRequest
	JTI       string
}

// Upload is one finished upload: what the bytes were, and where they went.
type Upload struct {
	JTI       string
	RequestID string
	Filename  string
	To        string
	Data      []byte
	Completed bool
}

// BytesRequest is one request on the files routes, for the resume assertions.
type BytesRequest struct {
	Method  string
	Range   string
	IfRange string
	Offset  string
	// Auth is whether the request carried the grant it was minted with.
	Auth bool
}

type grant struct {
	token, jti, op, requestID, conversationID string
	file                                      File
	size                                      int64
	crc                                       string
	to                                        string
	uploadID                                  string
	created                                   bool
	received                                  []byte
	completed                                 bool
	expiresAt                                 time.Time
}

// transferState is the double's file side.
type transferState struct {
	attachments map[string]map[string]File
	paths       map[string]File
	grants      map[string]*grant
	byJTI       map[string]*grant
	asks        []TransferAsk
	uploads     []*grant
	bytes       []BytesRequest
	// Faults, each consumed once.
	cutDownloadAfter   int64
	stallDownloadAfter int64
	restartOnResume    bool
	corruptDownload    bool
	cutPatchAfter      int64
	failPatchStatus    int
}

func (d *Double) transfers() *transferState {
	if d.files == nil {
		d.files = &transferState{
			attachments: map[string]map[string]File{}, paths: map[string]File{},
			grants: map[string]*grant{}, byJTI: map[string]*grant{},
			cutDownloadAfter: -1, stallDownloadAfter: -1, cutPatchAfter: -1,
		}
	}
	return d.files
}

// SeedAttachment gives a conversation an attachment the platform can grant,
// under its wire id (`<messageId>.<index>`).
func (d *Double) SeedAttachment(conversationID, attachmentID string, file File) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.transfers()
	if state.attachments[conversationID] == nil {
		state.attachments[conversationID] = map[string]File{}
	}
	state.attachments[conversationID][attachmentID] = file
}

// SeedPath puts a file in the organization's Files.
func (d *Double) SeedPath(path string, file File) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.transfers().paths[strings.TrimLeft(path, "/")] = file
}

// CutNextDownloadAfter makes the next download answer send n bytes of the body
// and drop the connection — a transfer interrupted mid-file.
func (d *Double) CutNextDownloadAfter(n int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.transfers().cutDownloadAfter = n
}

// StallNextDownloadAfter makes the next download answer send n bytes and then
// nothing more until the client goes away — a transfer still in flight.
func (d *Double) StallNextDownloadAfter(n int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.transfers().stallDownloadAfter = n
}

// RestartNextResume makes the next ranged download answer `200` with the whole
// file — the validator no longer matched (`If-Range`).
func (d *Double) RestartNextResume() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.transfers().restartOnResume = true
}

// CorruptDownloads serves every download with one byte flipped: the bytes no
// longer match the CRC32C the grant names.
func (d *Double) CorruptDownloads() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.transfers().corruptDownload = true
}

// CutNextPatchAfter makes the next tus PATCH keep n bytes of its body and drop
// the connection before answering — a chunk interrupted half-way.
func (d *Double) CutNextPatchAfter(n int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.transfers().cutPatchAfter = n
}

// FailNextPatch answers the next tus PATCH with status, keeping nothing.
func (d *Double) FailNextPatch(status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.transfers().failPatchStatus = status
}

// TransferAsks returns every grant the double minted, in order.
func (d *Double) TransferAsks() []TransferAsk {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]TransferAsk(nil), d.transfers().asks...)
}

// Uploads returns every upload the double created, finished or not.
func (d *Double) Uploads() []Upload {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []Upload{}
	for _, g := range d.transfers().uploads {
		out = append(out, Upload{
			JTI: g.jti, RequestID: g.requestID, To: g.to, Completed: g.completed,
			Data: append([]byte(nil), g.received...), Filename: g.file.Name,
		})
	}
	return out
}

// BytesRequests returns every request the files routes saw.
func (d *Double) BytesRequests() []BytesRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]BytesRequest(nil), d.transfers().bytes...)
}

// CRC32C is a file's checksum as the platform names it: the four bytes of its
// CRC32C, big-endian, in base64.
func CRC32C(data []byte) string {
	sum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	return base64.StdEncoding.EncodeToString([]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)})
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func etagOf(file File) string {
	if file.ETag != "" {
		return file.ETag
	}
	return `"` + CRC32C(file.Data) + `"`
}

// transferRoute serves `POST /v1/requests/:id/transfers` and `…/:jti/complete`
// with the gateway's rules: the conversation ticket, a request this double rang
// in the ticket's conversation and still open. Every body is checked against
// the protocol, and every answer too.
func (d *Double) transferRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/requests/"), "/")
	if answer, ok := d.next("transfers"); ok {
		writeInjected(w, answer)
		return
	}
	conversationID, ok := d.convTicket(w, r, "")
	if !ok {
		return
	}
	requestID := parts[0]
	d.mu.Lock()
	request, rung := d.requests[requestID]
	open := rung && request.conversationID == conversationID && !d.closedReqs[requestID]
	d.mu.Unlock()
	if !open {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if len(parts) == 4 && parts[3] == "complete" {
		d.complete(w, requestID, parts[2])
		return
	}
	raw := readBody(r)
	if err := protocol.Validate("transfer-request", raw); err != nil {
		d.refuse("transfers: " + err.Error())
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var body protocol.TransferRequest
	_ = json.Unmarshal(raw, &body)

	d.mu.Lock()
	state := d.transfers()
	g := &grant{
		token: "grant-" + newUUID(), jti: newUUID(), op: body.Op, requestID: requestID,
		conversationID: conversationID, expiresAt: time.Now().Add(10 * time.Minute).UTC(),
	}
	switch {
	case body.Op == "download" && body.AttachmentID != "":
		file, found := state.attachments[conversationID][body.AttachmentID]
		if !found {
			d.mu.Unlock()
			fail(w, http.StatusNotFound, "not_found")
			return
		}
		g.file, g.size, g.crc = file, int64(len(file.Data)), CRC32C(file.Data)
	case body.Op == "download":
		file, found := state.paths[strings.TrimLeft(body.Path, "/")]
		if !found {
			d.mu.Unlock()
			fail(w, http.StatusNotFound, "not_found")
			return
		}
		g.file, g.size, g.crc = file, int64(len(file.Data)), CRC32C(file.Data)
	default:
		g.file = File{Name: body.Filename, ContentType: body.ContentType}
		g.size, g.crc, g.to = *body.Size, body.CRC32C, body.To
		g.uploadID = "org1~" + strings.ReplaceAll(g.jti, "-", "")
		state.uploads = append(state.uploads, g)
	}
	state.grants[g.token] = g
	state.byJTI[g.jti] = g
	state.asks = append(state.asks, TransferAsk{RequestID: requestID, Body: body, JTI: g.jti})
	// The platform records the call on the request's ledger; the double numbers it.
	callURN := protocol.ToolCallURN(strconv.Itoa(900 + len(state.asks)))
	d.mu.Unlock()

	answer := protocol.TransferResponse{
		Op: g.op, JTI: g.jti, Grant: g.token, ExpiresAt: g.expiresAt, CallURN: callURN,
		File: protocol.TransferFile{Name: g.file.Name, Size: g.size, ContentType: g.file.ContentType, CRC32C: g.crc},
	}
	encoded := mustJSON(answer)
	if err := protocol.Validate("transfer-response", encoded); err != nil {
		panic("gatewaytest: a transfer answer is off-contract: " + err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

func (d *Double) complete(w http.ResponseWriter, requestID, jti string) {
	d.mu.Lock()
	g, known := d.transfers().byJTI[jti]
	if !known || g.op != "upload" || g.requestID != requestID {
		d.mu.Unlock()
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if int64(len(g.received)) != g.size {
		d.mu.Unlock()
		fail(w, http.StatusConflict, "transfer_incomplete")
		return
	}
	g.completed = true
	var answer protocol.TransferCompleteResponse
	if g.to != "" {
		answer.Path = "/" + strings.TrimLeft(g.to, "/")
	} else {
		answer.ID, answer.Name = "u-"+jti[:8], g.file.Name
		answer.Ref = "conversations/" + g.conversationID + "/" + g.file.Name
	}
	d.mu.Unlock()
	encoded := mustJSON(answer)
	if err := protocol.Validate("transfer-complete-response", encoded); err != nil {
		panic("gatewaytest: a completion is off-contract: " + err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

// grantOf reads the grant the bytes route was called with, recording the call.
func (d *Double) grantOf(r *http.Request, op string) (*grant, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.transfers()
	g, ok := state.grants[bearer(r)]
	state.bytes = append(state.bytes, BytesRequest{
		Method: r.Method, Range: r.Header.Get("Range"), IfRange: r.Header.Get("If-Range"),
		Offset: r.Header.Get("Upload-Offset"), Auth: ok,
	})
	if !ok || g.op != op || time.Now().After(g.expiresAt) {
		return nil, false
	}
	return g, true
}

// download serves `GET /v1/files/download` with HTTP ranges, `If-Range` on the
// ETag, and `Repr-Digest`.
func (d *Double) download(w http.ResponseWriter, r *http.Request) {
	g, ok := d.grantOf(r, "download")
	if !ok {
		fail(w, http.StatusUnauthorized, "grant_refused")
		return
	}
	d.mu.Lock()
	state := d.transfers()
	data := append([]byte(nil), g.file.Data...)
	if state.corruptDownload && len(data) > 0 {
		data[len(data)/2] ^= 0xff
	}
	cut := state.cutDownloadAfter
	state.cutDownloadAfter = -1
	stall := state.stallDownloadAfter
	state.stallDownloadAfter = -1
	restart := state.restartOnResume && r.Header.Get("Range") != ""
	if restart {
		state.restartOnResume = false
	}
	d.mu.Unlock()

	etag := etagOf(g.file)
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Repr-Digest", "crc32c=:"+g.crc+":")
	if g.file.ContentType != "" {
		w.Header().Set("Content-Type", g.file.ContentType)
	}
	start := int64(0)
	status := http.StatusOK
	if spec := r.Header.Get("Range"); spec != "" && !restart && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == etag) {
		from, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(spec, "bytes="), "-"), 10, 64)
		if err != nil || from >= int64(len(data)) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			fail(w, http.StatusRequestedRangeNotSatisfiable, "invalid_request")
			return
		}
		start, status = from, http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
	}
	body := data[start:]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if stall >= 0 && stall < int64(len(body)) {
		_, _ = w.Write(body[:stall])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// The bytes stop coming until the client goes away.
		<-r.Context().Done()
		return
	}
	if cut >= 0 && cut < int64(len(body)) {
		_, _ = w.Write(body[:cut])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// The connection drops mid-body: the client reads a short answer.
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(body)
}

// uploads serves the tus routes: creation, `HEAD`, `PATCH`, termination.
func (d *Double) uploads(w http.ResponseWriter, r *http.Request) {
	g, ok := d.grantOf(r, "upload")
	if !ok {
		fail(w, http.StatusUnauthorized, "grant_refused")
		return
	}
	if r.Header.Get("Tus-Resumable") != "1.0.0" {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	w.Header().Set("Tus-Resumable", "1.0.0")
	id := strings.TrimPrefix(r.URL.Path, "/v1/files/uploads")
	id = strings.TrimPrefix(id, "/")
	if r.Method == http.MethodPost && id == "" {
		length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
		if err != nil || length != g.size {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
		d.mu.Lock()
		g.created = true
		d.mu.Unlock()
		// 🔴 RELATIVE, as the gateway rewrites it.
		w.Header().Set("Location", "/v1/files/uploads/"+g.uploadID)
		w.Header().Set("Upload-Offset", "0")
		w.WriteHeader(http.StatusCreated)
		return
	}
	d.mu.Lock()
	created := g.created && id == g.uploadID
	offset := int64(len(g.received))
	d.mu.Unlock()
	if !created {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	switch r.Method {
	case http.MethodHead:
		w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
		w.Header().Set("Upload-Length", strconv.FormatInt(g.size, 10))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	case http.MethodPatch:
		d.patch(w, r, g, offset)
	case http.MethodDelete:
		d.mu.Lock()
		g.created = false
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, http.StatusMethodNotAllowed, "invalid_request")
	}
}

func (d *Double) patch(w http.ResponseWriter, r *http.Request, g *grant, offset int64) {
	if r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		fail(w, http.StatusUnsupportedMediaType, "invalid_request")
		return
	}
	claimed, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || claimed != offset {
		fail(w, http.StatusConflict, "offset_mismatch")
		return
	}
	d.mu.Lock()
	state := d.transfers()
	cut, failStatus := state.cutPatchAfter, state.failPatchStatus
	state.cutPatchAfter, state.failPatchStatus = -1, 0
	d.mu.Unlock()
	if failStatus != 0 {
		_, _ = io.Copy(io.Discard, r.Body)
		fail(w, failStatus, "platform_unavailable")
		return
	}
	chunk, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if cut >= 0 && cut < int64(len(chunk)) {
		d.mu.Lock()
		g.received = append(g.received, chunk[:cut]...)
		d.mu.Unlock()
		// Part of the chunk was written, and the answer never comes back.
		panic(http.ErrAbortHandler)
	}
	d.mu.Lock()
	g.received = append(g.received, chunk...)
	if int64(len(g.received)) > g.size {
		g.received = g.received[:offset]
		d.mu.Unlock()
		fail(w, http.StatusRequestEntityTooLarge, "file_too_large")
		return
	}
	done := int64(len(g.received)) == g.size
	whole := append([]byte(nil), g.received...)
	newOffset := int64(len(g.received))
	d.mu.Unlock()
	if done && CRC32C(whole) != g.crc {
		d.mu.Lock()
		g.received = g.received[:offset]
		d.mu.Unlock()
		fail(w, 460, "checksum_mismatch")
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	w.WriteHeader(http.StatusNoContent)
}

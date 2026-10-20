package fixture

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// The scripted host moves files (#2679): a script's attachment carries its bytes
// (the platform's copy), a download is granted for it, and an upload is
// received, checked and recorded — so a fixture run proves both file use cases
// end to end with a real harness and no platform.
//
// 🔴 The BYTES cross a real HTTP hop on loopback, through the gateway host's own
// bytes client (`gateway.FilesClient`): tus up with resume from the server's
// offset, an HTTP range down. A stand-in that handed the bytes over in memory
// would prove the transfer engine and not the client a companion actually runs —
// and the interrupted-upload step would have nothing to interrupt.
//
// The grant is a random token held here and nowhere else; it never reaches a
// log line, like the platform's.

// filesPath is the files routes' base, as the agents host serves it.
const filesPath = "/v1/files"

// fixtureFile is one file the script holds: an attachment's bytes.
type fixtureFile struct {
	name, contentType string
	data              []byte
}

// transferGrant is one grant this host minted, and — for an upload — what arrived.
type transferGrant struct {
	token, jti, requestID, conversationID string
	op                                    contracts.TransferOp
	file                                  fixtureFile
	size                                  int64
	crc                                   string
	to                                    string
	created, completed, resumed, served   bool
	received                              []byte
}

// UploadRecord is one finished upload, for the runner's expectations and tally.
// The bytes are kept for `expectUpload`'s `contains` — a test double's privilege;
// they are never logged.
type UploadRecord struct {
	RequestID string
	To        string
	// ID is the attachment id a post names it by; empty for an upload into Files.
	ID string
	// Intact is whether the bytes that arrived carry the CRC32C the grant was minted
	// for — what the platform checks before it records the file.
	Intact  bool
	Resumed bool
	Data    []byte
}

// filesState is the host's file side, created with the host.
type filesState struct {
	attachments map[string]map[string]fixtureFile // conversation id → attachment id → file
	grants      map[string]*transferGrant         // by token
	byJTI       map[string]*transferGrant
	uploads     []*transferGrant
	cutNext     int64
	client      func(baseURL string) BytesClient
	server      *http.Server
	base        string
}

func newFilesState() *filesState {
	return &filesState{
		attachments: map[string]map[string]fixtureFile{}, grants: map[string]*transferGrant{},
		byJTI: map[string]*transferGrant{}, cutNext: -1,
	}
}

// holdAttachments records the bytes a scripted entry's attachments carry.
func (f *filesState) holdAttachments(conversationID string, entries []ScriptEntry) {
	for _, entry := range entries {
		for _, attachment := range entry.Attachments {
			if attachment.Data == nil {
				continue
			}
			if f.attachments[conversationID] == nil {
				f.attachments[conversationID] = map[string]fixtureFile{}
			}
			f.attachments[conversationID][attachment.ID] = fixtureFile{
				name: attachment.Name, contentType: attachment.ContentType, data: attachment.Data,
			}
		}
	}
}

// transferRefused is the platform's refusal of a grant or a completion: a class
// and the sentence the agent relays (decision 32).
type transferRefused struct{ class, instruction string }

func (e transferRefused) Error() string            { return "fixture: transfer refused: " + e.class }
func (e transferRefused) ErrorClass() string       { return e.class }
func (e transferRefused) ErrorInstruction() string { return e.instruction }

var (
	errNoSuchFile = transferRefused{"not_found", "That file is not in this conversation; tell the person it could not be found."}
	errIncomplete = transferRefused{"transfer_incomplete", "The upload did not finish; try it again."}
	errCorrupted  = transferRefused{"checksum_mismatch", "The file changed while it was sent; try the upload again."}
)

// ConversationOf returns the conversation of a request a ring delivered.
func (h *Host) ConversationOf(requestID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conversationID, ok := h.rung[requestID]
	return conversationID, ok
}

// GrantTransfer mints a grant for a request a ring delivered: a download of one
// of the conversation's scripted attachments, or an upload of the size and
// CRC32C asked. A request never rung is refused before anything is recorded.
func (h *Host) GrantTransfer(_ context.Context, requestID string, ask contracts.TransferAsk) (contracts.TransferGrant, error) {
	conversationID, known := h.ConversationOf(requestID)
	if !known {
		return contracts.TransferGrant{}, contracts.ErrUnknownRequest
	}
	h.mu.Lock()
	carried := h.files.client != nil
	h.mu.Unlock()
	if !carried {
		return contracts.TransferGrant{}, errNoBytesClient
	}
	if err := h.serveFiles(); err != nil {
		return contracts.TransferGrant{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	g := &transferGrant{
		token: randomHex(32), jti: randomUUID(), requestID: requestID, conversationID: conversationID, op: ask.Op,
	}
	switch ask.Op {
	case contracts.TransferDownload:
		file, found := h.files.attachments[conversationID][ask.AttachmentID]
		if ask.AttachmentID == "" || !found {
			return contracts.TransferGrant{}, errNoSuchFile
		}
		g.file, g.size, g.crc = file, int64(len(file.data)), crc32cOf(file.data)
	case contracts.TransferUpload:
		g.file = fixtureFile{name: ask.Filename, contentType: ask.ContentType}
		g.size, g.crc, g.to = ask.Size, ask.CRC32C, ask.To
		h.files.uploads = append(h.files.uploads, g)
	default:
		return contracts.TransferGrant{}, errNoSuchFile
	}
	h.files.grants[g.token] = g
	h.files.byJTI[g.jti] = g
	return contracts.TransferGrant{
		Op: ask.Op, JTI: g.jti, Grant: g.token, ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
		File: contracts.TransferFile{Name: g.file.name, Size: g.size, ContentType: g.file.contentType, CRC32C: g.crc},
	}, nil
}

// CompleteTransfer records an upload whose every byte arrived with the CRC32C its
// grant names: attached to the request's next post, or saved at `to`.
func (h *Host) CompleteTransfer(_ context.Context, requestID, jti string) (contracts.TransferCompletion, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	g, known := h.files.byJTI[jti]
	if !known || g.op != contracts.TransferUpload || g.requestID != requestID {
		return contracts.TransferCompletion{}, errNoSuchFile
	}
	if int64(len(g.received)) != g.size {
		return contracts.TransferCompletion{}, errIncomplete
	}
	if crc32cOf(g.received) != g.crc {
		return contracts.TransferCompletion{}, errCorrupted
	}
	g.completed = true
	if g.to != "" {
		return contracts.TransferCompletion{Path: "/" + strings.TrimLeft(g.to, "/")}, nil
	}
	return contracts.TransferCompletion{
		ID: uploadID(g), Name: g.file.name, Ref: "conversations/" + g.conversationID + "/" + g.file.name,
	}, nil
}

// BytesClient carries a transfer's bytes on an agents host's files routes: the
// gateway host's `gateway.FilesClient`, handed in by the companion (`UseBytesClient`)
// — the fixture cannot import the gateway package, whose own test double imports
// this one.
type BytesClient interface {
	UploadFile(ctx context.Context, grant contracts.TransferGrant, src io.ReadSeeker, spec contracts.UploadSpec) error
	OpenDownload(ctx context.Context, grant contracts.TransferGrant, from int64, ifRange string) (contracts.DownloadMeta, io.ReadCloser, error)
}

// UseBytesClient sets how the host's bytes are carried: a client for the files
// routes at `baseURL` (`…/v1/files`). Without one the host grants nothing.
func (h *Host) UseBytesClient(client func(baseURL string) BytesClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files.client = client
}

// UploadFile carries an upload's bytes with the companion's own tus client.
func (h *Host) UploadFile(ctx context.Context, grant contracts.TransferGrant, src io.ReadSeeker, spec contracts.UploadSpec) error {
	return h.bytesClient().UploadFile(ctx, grant, src, spec)
}

// OpenDownload is one ranged GET, with the companion's own client.
func (h *Host) OpenDownload(ctx context.Context, grant contracts.TransferGrant, from int64, ifRange string) (contracts.DownloadMeta, io.ReadCloser, error) {
	return h.bytesClient().OpenDownload(ctx, grant, from, ifRange)
}

// filesURL is the loopback files routes' base.
func (h *Host) filesURL() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.files.base + filesPath
}

func (h *Host) bytesClient() BytesClient {
	base := h.filesURL()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.files.client(base)
}

// errNoBytesClient refuses a grant on a host that cannot carry the bytes.
var errNoBytesClient = transferRefused{"platform_unavailable", "Files cannot be moved right now; tell the person."}

// InterruptNextUpload drops the connection of the next upload's PATCH once,
// after `afterBytes` of it were written — the bytes kept, the answer lost, as a
// dropped connection leaves them. The companion must resume from the offset a
// HEAD reports.
func (h *Host) InterruptNextUpload(afterBytes int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files.cutNext = afterBytes
}

// Uploads returns every finished upload so far.
func (h *Host) Uploads() []UploadRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []UploadRecord{}
	for _, g := range h.files.uploads {
		if !g.completed {
			continue
		}
		record := UploadRecord{
			RequestID: g.requestID, To: g.to, Intact: crc32cOf(g.received) == g.crc,
			Resumed: g.resumed, Data: append([]byte(nil), g.received...),
		}
		if g.to == "" {
			record.ID = uploadID(g)
		}
		out = append(out, record)
	}
	return out
}

// Downloads reports how many downloads were served to their last byte.
func (h *Host) Downloads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, g := range h.files.grants {
		if g.op == contracts.TransferDownload && g.served {
			count++
		}
	}
	return count
}

// serveFiles starts the loopback files routes once.
func (h *Host) serveFiles() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.files.server != nil {
		return nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("fixture: the files routes could not listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(filesPath+"/download", h.serveDownload)
	mux.HandleFunc(filesPath+"/uploads", h.serveUploads)
	mux.HandleFunc(filesPath+"/uploads/", h.serveUploads)
	h.files.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	h.files.base = "http://" + listener.Addr().String()
	go func(server *http.Server) { _ = server.Serve(listener) }(h.files.server)
	return nil
}

// closeFiles stops the files routes.
func (h *Host) closeFiles() {
	if h.files.server != nil {
		_ = h.files.server.Close()
	}
}

// grantFor reads the bearer grant a bytes request carries.
func (h *Host) grantFor(r *http.Request, op contracts.TransferOp) *transferGrant {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	h.mu.Lock()
	defer h.mu.Unlock()
	g, ok := h.files.grants[token]
	if !ok || g.op != op {
		return nil
	}
	return g
}

// serveDownload is `GET /v1/files/download`: ranges, `If-Range` on the ETag, and
// `Repr-Digest`.
func (h *Host) serveDownload(w http.ResponseWriter, r *http.Request) {
	g := h.grantFor(r, contracts.TransferDownload)
	if g == nil {
		http.Error(w, "grant refused", http.StatusUnauthorized)
		return
	}
	data := g.file.data
	etag := `"` + g.crc + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Repr-Digest", "crc32c=:"+g.crc+":")
	if g.file.contentType != "" {
		w.Header().Set("Content-Type", g.file.contentType)
	}
	start, status := int64(0), http.StatusOK
	if spec := r.Header.Get("Range"); spec != "" && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == etag) {
		from, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(spec, "bytes="), "-"), 10, 64)
		if err != nil || from >= int64(len(data)) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, status = from, http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)-int(start)))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(data[start:]); err == nil {
		h.mu.Lock()
		g.served = true
		h.mu.Unlock()
	}
}

// serveUploads is tus 1.0: creation, `HEAD`, `PATCH`, termination.
func (h *Host) serveUploads(w http.ResponseWriter, r *http.Request) {
	g := h.grantFor(r, contracts.TransferUpload)
	if g == nil {
		http.Error(w, "grant refused", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Tus-Resumable", "1.0.0")
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, filesPath+"/uploads"), "/")
	if r.Method == http.MethodPost && id == "" {
		if length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64); err != nil || length != g.size {
			http.Error(w, "upload length is not the grant's", http.StatusForbidden)
			return
		}
		h.mu.Lock()
		g.created = true
		h.mu.Unlock()
		w.Header().Set("Location", filesPath+"/uploads/"+g.jti)
		w.Header().Set("Upload-Offset", "0")
		w.WriteHeader(http.StatusCreated)
		return
	}
	h.mu.Lock()
	created := g.created && id == g.jti
	offset := int64(len(g.received))
	h.mu.Unlock()
	if !created {
		http.Error(w, "no such upload", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodHead:
		h.mu.Lock()
		if offset > 0 && offset < g.size {
			// A HEAD part-way is a client finding out where to resume.
			g.resumed = true
		}
		h.mu.Unlock()
		w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
		w.Header().Set("Upload-Length", strconv.FormatInt(g.size, 10))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	case http.MethodPatch:
		h.patch(w, r, g, offset)
	case http.MethodDelete:
		h.mu.Lock()
		g.created = false
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Host) patch(w http.ResponseWriter, r *http.Request, g *transferGrant, offset int64) {
	claimed, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || claimed != offset || r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		http.Error(w, "offset mismatch", http.StatusConflict)
		return
	}
	h.mu.Lock()
	cut := h.files.cutNext
	h.files.cutNext = -1
	h.mu.Unlock()
	limit := g.size - offset
	if cut >= 0 && cut < limit {
		// Part of the chunk is written, then the connection drops: the answer never
		// comes back, and only a HEAD can say how much arrived.
		chunk, _ := io.ReadAll(io.LimitReader(r.Body, cut))
		h.mu.Lock()
		g.received = append(g.received, chunk...)
		h.mu.Unlock()
		panic(http.ErrAbortHandler)
	}
	chunk, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return
	}
	if int64(len(chunk)) > limit {
		http.Error(w, "longer than the grant", http.StatusRequestEntityTooLarge)
		return
	}
	h.mu.Lock()
	g.received = append(g.received, chunk...)
	next := int64(len(g.received))
	h.mu.Unlock()
	w.Header().Set("Upload-Offset", strconv.FormatInt(next, 10))
	w.WriteHeader(http.StatusNoContent)
}

func uploadID(g *transferGrant) string { return "upload-" + g.jti[:8] }

// crc32cOf is a file's checksum as the platform names it: the four bytes of its
// CRC32C, big-endian, in base64.
func crc32cOf(data []byte) string {
	sum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	return base64.StdEncoding.EncodeToString([]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)})
}

// randomUUID is a grant's jti in the platform's shape — a UUID, which the
// companion checks before it trusts the grant.
func randomUUID() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("fixture: no randomness"))
	}
	return hex.EncodeToString(b)
}

var _ contracts.TransferHost = (*Host)(nil)

package contracts

import (
	"context"
	"io"
	"time"
)

// TransferHost is an agents host that can move a file for a request (#2673):
// the platform verifies the request, records the call and mints a short-lived
// grant; the bytes then cross the files routes under that grant alone; an
// upload ends with a completion that records it.
//
// Optional, like LogHost: the in-memory host does not move files, and a
// companion over it serves no file op and announces no `files` feature. The
// fixture does (#2679), over loopback, with the gateway host's own bytes client.
//
// 🔴 The grant is SECRET-CLASS — it opens one file, for minutes. It goes into an
// `Authorization` header and nowhere else: never a log line, an error string or
// a URL. Its `jti` is an id and may be logged.
type TransferHost interface {
	// ConversationOf returns the conversation of a request this companion was
	// rung for, and false for any other. Nothing is sent.
	ConversationOf(requestID string) (string, bool)

	// GrantTransfer asks the platform for a grant on a request this companion was
	// rung for. A request never rung is ErrUnknownRequest, before anything is
	// sent. 🔴 Sent at most once: a grant records a call on the request's ledger.
	GrantTransfer(ctx context.Context, requestID string, ask TransferAsk) (TransferGrant, error)

	// CompleteTransfer records an uploaded file: attached to the request's next
	// post, or saved into Files when the grant named a destination.
	CompleteTransfer(ctx context.Context, requestID, jti string) (TransferCompletion, error)

	// UploadFile carries an upload's bytes under its grant (tus): the upload is
	// created, sent in chunks of spec.ChunkBytes, and after a failure resumed from
	// the offset the server reports, at most spec.Attempts times in a row without
	// progress. 🔴 Reads src a chunk at a time — never the whole file — and
	// honours ctx between and inside chunks; an aborted upload is terminated on
	// the server, best effort.
	UploadFile(ctx context.Context, grant TransferGrant, src io.ReadSeeker, spec UploadSpec) error

	// OpenDownload is ONE ranged GET of a download under its grant: from 0, or
	// from `from` with `If-Range: ifRange` (the ETag a previous answer named). The
	// caller reads and closes the body; Meta says whether the server resumed
	// (206) or restarted (200). A non-2xx answer is a ClassifiedError.
	OpenDownload(ctx context.Context, grant TransferGrant, from int64, ifRange string) (DownloadMeta, io.ReadCloser, error)
}

// RetryableError is a host failure that says whether trying again could help:
// a dropped connection or a platform outage can, a refusal cannot.
type RetryableError interface {
	error
	CanRetry() bool
}

// UploadSpec shapes one upload's bytes.
type UploadSpec struct {
	Size        int64
	ChunkBytes  int64
	Attempts    int
	Filename    string
	ContentType string
	CRC32C      string
}

// DownloadMeta is what one download answer said about the bytes coming.
type DownloadMeta struct {
	// Status is 200 (the whole file, from 0) or 206 (from Start).
	Status int
	// Start is the first byte's offset; Total the whole file's size, -1 unknown.
	Start, Total int64
	// ETag is the validator a resume names in `If-Range`.
	ETag string
	// ContentType is the server's; CRC32C the whole file's from `Repr-Digest`,
	// base64, empty when absent.
	ContentType string
	CRC32C      string
}

// TransferOp is which way a transfer's bytes flow.
type TransferOp string

// The two directions, as the platform names them.
const (
	TransferDownload TransferOp = "download"
	TransferUpload   TransferOp = "upload"
)

// TransferAsk is what a grant is asked for: a download names an entry's
// attachment or a Files path; an upload names the file's name, size, CRC32C and
// type, and To to save it into Files instead of attaching it to the reply.
type TransferAsk struct {
	Op           TransferOp
	AttachmentID string
	Path         string
	Filename     string
	Size         int64
	CRC32C       string
	ContentType  string
	To           string
	Overwrite    bool
}

// TransferGrant is the platform's answer: the grant and what it knows of the
// file. Grant is SECRET-CLASS.
type TransferGrant struct {
	Op        TransferOp
	JTI       string
	Grant     string
	ExpiresAt time.Time
	CallURN   string
	File      TransferFile
}

// TransferFile is what the platform knows of a transferred file. CRC32C is the
// base64 of its four big-endian bytes, as GCS reports it; empty when unknown.
type TransferFile struct {
	Name        string
	Size        int64
	ContentType string
	CRC32C      string
}

// TransferCompletion is a finished upload: the attachment a post carries (ID,
// Name, Ref) for an upload into the conversation, or Path for one into Files.
type TransferCompletion struct {
	ID   string
	Name string
	Ref  string
	Path string
}

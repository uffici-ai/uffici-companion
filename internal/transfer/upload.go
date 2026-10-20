package transfer

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// UploadRequest is what the plugin asks to upload: a workspace file, and To to
// save it into Files instead of attaching it to the reply.
type UploadRequest struct {
	LocalPath string
	To        string
	Overwrite bool
}

// Uploaded is a finished upload: the attachment the reply carries (ID, Name,
// Ref), or the Files path it was saved at — and its size.
type Uploaded struct {
	ID, Name, Ref string
	Path          string
	Size          int64
}

// Upload sends a workspace file to the platform for a request this companion is
// answering: stat, the type from the file's first bytes, the CRC32C streamed, a
// grant for exactly that size and checksum, the bytes with tus, the completion.
func (e *Engine) Upload(ctx context.Context, requestID string, ask UploadRequest) (Uploaded, error) {
	if _, known := e.host.ConversationOf(requestID); !known {
		return Uploaded{}, contracts.ErrUnknownRequest
	}
	ctx, end := e.begin(ctx, requestID)
	defer end()

	// The check, for the refusal's reason; then the act, through the root — a
	// directory swapped for a link in between leads nowhere outside.
	resolved, err := e.ws.Resolve(ask.LocalPath)
	if err != nil {
		return Uploaded{}, err
	}
	file, info, err := e.ws.openRegular(e.ws.Rel(resolved))
	if err != nil {
		return Uploaded{}, fail(ClassLocalFileMissing, err)
	}
	defer func() { _ = file.Close() }()
	// 🔴 One name only: a hard link is a file from anywhere on this filesystem —
	// the agent key, a key under ~/.ssh — wearing a name inside the workspace.
	if linkCount(info) > 1 {
		return Uploaded{}, &FenceError{Reason: FenceLinked}
	}
	contentType, err := SniffContentType(file)
	if err != nil {
		return Uploaded{}, fail(ClassLocalFileMissing, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Uploaded{}, fail(ClassLocalFileMissing, err)
	}
	crc, size, err := CRC32C(file)
	if err != nil || size != info.Size() {
		// A file that changed while it was read would be uploaded as something
		// its checksum does not describe.
		return Uploaded{}, fail(ClassLocalFileMissing, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Uploaded{}, fail(ClassLocalFileMissing, err)
	}
	name := filepath.Base(resolved)

	grant, err := e.host.GrantTransfer(ctx, requestID, contracts.TransferAsk{
		Op: contracts.TransferUpload, Filename: name, Size: size, CRC32C: crc, ContentType: contentType,
		To: ask.To, Overwrite: ask.Overwrite,
	})
	if err != nil {
		e.logUpload(requestID, "", size, ClassOf(err))
		return Uploaded{}, err
	}
	if err := e.host.UploadFile(ctx, grant, file, contracts.UploadSpec{
		Size: size, ChunkBytes: e.chunk, Attempts: e.attempts,
		Filename: name, ContentType: contentType, CRC32C: crc,
	}); err != nil {
		e.logUpload(requestID, grant.JTI, size, ClassOf(err))
		return Uploaded{}, err
	}
	done, err := e.host.CompleteTransfer(ctx, requestID, grant.JTI)
	if err != nil {
		e.logUpload(requestID, grant.JTI, size, ClassOf(err))
		return Uploaded{}, err
	}
	e.logUpload(requestID, grant.JTI, size, "")
	return Uploaded{ID: done.ID, Name: done.Name, Ref: done.Ref, Path: done.Path, Size: size}, nil
}

// logUpload is the one line an upload leaves: ids, the size and the class —
// 🔴 never a path, a filename or the grant.
func (e *Engine) logUpload(requestID, jti string, size int64, class string) {
	attrs := []any{
		slog.String("op", "upload"), slog.String("request", requestID), slog.String("jti", jti),
		slog.Int64("bytes", size),
	}
	if class == "" {
		e.log.Info("file transferred", append(attrs, slog.String("class", "ok"))...)
		return
	}
	e.log.Warn("file transfer failed", append(attrs, slog.String("class", class))...)
}

package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// Downloaded is a file in the workspace: its path relative to the workspace,
// its size and its type.
type Downloaded struct {
	LocalPath   string
	Size        int64
	ContentType string
}

// DownloadTarget names what to download: an entry's attachment (by its wire
// id) or a Files path.
type DownloadTarget struct {
	AttachmentID string
	Path         string
}

// noLimit is a download with no size budget.
const noLimit = int64(-1)

// Download writes a file of the organization into the workspace, for a request
// this companion is answering.
func (e *Engine) Download(ctx context.Context, requestID string, target DownloadTarget) (Downloaded, error) {
	ctx, end := e.begin(ctx, requestID)
	defer end()
	return e.download(ctx, requestID, target, noLimit)
}

// download is Download under a budget: a file larger than limit (when limit is
// not noLimit) is not fetched and fails ClassOverBudget.
//
// 🔴 An attachment never changes once posted, so one already in the workspace
// is answered from disk with no grant — a redelivered brief does not record a
// second call. A Files path is always fetched: it may have changed.
func (e *Engine) download(ctx context.Context, requestID string, target DownloadTarget, limit int64) (Downloaded, error) {
	conversationID, known := e.host.ConversationOf(requestID)
	if !known {
		return Downloaded{}, contracts.ErrUnknownRequest
	}
	var dest string
	ask := contracts.TransferAsk{Op: contracts.TransferDownload}
	if target.AttachmentID != "" {
		if !attachmentIDPattern.MatchString(target.AttachmentID) || !safeSegment.MatchString(conversationID) {
			return Downloaded{}, &FenceError{Reason: FenceInvalid}
		}
		if found, ok := e.downloadedAttachment(conversationID, target.AttachmentID); ok {
			if limit != noLimit && found.Size > limit {
				return Downloaded{}, fail(ClassOverBudget, nil)
			}
			return found, nil
		}
		ask.AttachmentID = target.AttachmentID
	} else {
		// 🔴 Joined as a string and fenced as written: path.Join would CLEAN a
		// `..` away before the fence could refuse it.
		resolved, err := e.ws.Resolve(FilesDir + "/" + strings.TrimLeft(target.Path, "/"))
		if err != nil {
			return Downloaded{}, err
		}
		dest, ask.Path = resolved, target.Path
	}

	grant, err := e.host.GrantTransfer(ctx, requestID, ask)
	if err != nil {
		e.logDownload(requestID, contracts.TransferGrant{}, 0, ClassOf(err))
		return Downloaded{}, err
	}
	if !jtiPattern.MatchString(grant.JTI) {
		return Downloaded{}, fail(ClassTransferFailed, nil)
	}
	if limit != noLimit && grant.File.Size > limit {
		return Downloaded{}, fail(ClassOverBudget, nil)
	}
	if dest == "" {
		name := target.AttachmentID + "-" + localName(grant.File.Name)
		resolved, err := e.ws.Resolve(ConversationsDir + "/" + conversationID + "/" + name)
		if err != nil {
			return Downloaded{}, err
		}
		dest = resolved
	}

	partial, err := e.ws.createPartial(grant.JTI)
	if err != nil {
		e.logDownload(requestID, grant, 0, ClassLocalWriteFailed)
		return Downloaded{}, fail(ClassLocalWriteFailed, err)
	}
	defer func() { _ = partial.Close() }()
	meta, err := e.fetch(ctx, grant, partial)
	if err == nil {
		err = e.ensureParent(dest)
	}
	if err == nil {
		if finalErr := e.ws.finalize(partial, grant.JTI, e.ws.Rel(dest)); finalErr != nil {
			err = fail(ClassLocalWriteFailed, finalErr)
		}
	}
	if err != nil {
		e.ws.removePartial(grant.JTI)
		e.logDownload(requestID, grant, 0, ClassOf(err))
		return Downloaded{}, err
	}
	contentType := cleanContentType(grant.File.ContentType)
	if grant.File.ContentType == "" {
		contentType = cleanContentType(meta.ContentType)
	}
	e.logDownload(requestID, grant, grant.File.Size, "")
	return Downloaded{LocalPath: e.ws.Rel(dest), Size: grant.File.Size, ContentType: contentType}, nil
}

// downloadedAttachment finds an attachment already in the workspace —
// `<attachmentId>-<name>` in its conversation's directory. 🔴 Matched EXACTLY:
// an attachment id carries no `-` (attachmentIDPattern), so the first `-` in a
// name ends the id, and attachment `7.1` never answers for `7.1-…` of another.
func (e *Engine) downloadedAttachment(conversationID, attachmentID string) (Downloaded, bool) {
	dir := ConversationsDir + "/" + conversationID
	if _, err := e.ws.Resolve(dir); err != nil {
		return Downloaded{}, false
	}
	entries, err := e.ws.readDir(dir)
	if err != nil {
		return Downloaded{}, false
	}
	for _, entry := range entries {
		id, _, named := strings.Cut(entry.Name(), "-")
		if !entry.Type().IsRegular() || !named || id != attachmentID {
			continue
		}
		rel := dir + "/" + entry.Name()
		file, info, err := e.ws.openRegular(rel)
		if err != nil {
			continue
		}
		contentType, sniffErr := SniffContentType(file)
		_ = file.Close()
		if sniffErr != nil {
			continue
		}
		return Downloaded{LocalPath: rel, Size: info.Size(), ContentType: contentType}, true
	}
	return Downloaded{}, false
}

// fetch writes a download into its partial file: from the start, then — after
// a dropped connection — RESUMED from the partial's size with `Range` and
// `If-Range` (the ETag the first answer named). A `200` to a resume means the
// file changed under the validator, and the partial restarts from zero. Then
// the size and the CRC32C are verified against the platform's.
//
// 🔴 A resume needs BOTH a validator and a digest. With no ETag, `If-Range`
// cannot say "only if it is still the same file", and a changed file would be
// spliced onto the old one's prefix; with no CRC32C (the grant's or the
// answer's `Repr-Digest`), nothing would catch that splice at the end. Missing
// either, an interrupted download restarts from zero. A `206` whose total is
// not the grant's size is another file's range, and restarts it too.
func (e *Engine) fetch(ctx context.Context, grant contracts.TransferGrant, file *os.File) (contracts.DownloadMeta, error) {
	expected := grant.File.Size
	buffer := make([]byte, copyBufferBytes)
	var (
		etag      string
		digest    string
		last      contracts.DownloadMeta
		failures  int
		highWater int64
	)
	for {
		offset, err := file.Seek(0, io.SeekEnd)
		if err != nil {
			return last, fail(ClassLocalWriteFailed, err)
		}
		if offset > 0 && (etag == "" || (grant.File.CRC32C == "" && digest == "")) {
			// Nothing to resume against safely: start again from zero.
			if truncErr := file.Truncate(0); truncErr != nil {
				return last, fail(ClassLocalWriteFailed, truncErr)
			}
			if offset, err = file.Seek(0, io.SeekStart); err != nil {
				return last, fail(ClassLocalWriteFailed, err)
			}
		}
		meta, body, err := e.host.OpenDownload(ctx, grant, offset, etag)
		if err != nil {
			if ClassOf(err) == "range_not_satisfiable" {
				// The partial is not a prefix of this file: start again.
				if truncErr := file.Truncate(0); truncErr != nil {
					return last, fail(ClassLocalWriteFailed, truncErr)
				}
				etag = ""
			}
			failures++
			if !e.retry(ctx, err, failures) {
				return last, e.fetchFailure(ctx, err)
			}
			continue
		}
		if meta.Status == 206 && (meta.Start != offset || meta.Total != expected) {
			// A range this side did not ask for, or of a file of another size: drop
			// it and ask again from zero.
			_ = body.Close()
			if truncErr := file.Truncate(0); truncErr != nil {
				return last, fail(ClassLocalWriteFailed, truncErr)
			}
			etag = ""
			failures++
			if !e.retry(ctx, fail(ClassTransferFailed, nil), failures) {
				return last, e.fetchFailure(ctx, fail(ClassTransferFailed, nil))
			}
			continue
		}
		if meta.Status != 206 && offset > 0 {
			// 🔴 A 200 to a resume is the whole file again — the validator no longer
			// matched (`If-Range`), so the bytes kept are another version's prefix.
			if truncErr := file.Truncate(0); truncErr != nil {
				_ = body.Close()
				return last, fail(ClassLocalWriteFailed, truncErr)
			}
			if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
				_ = body.Close()
				return last, fail(ClassLocalWriteFailed, seekErr)
			}
			offset = 0
		}
		last = meta
		if meta.ETag != "" {
			etag = meta.ETag
		}
		if meta.CRC32C != "" {
			digest = meta.CRC32C
		}
		// One byte past the expected size is read, so a longer file is caught.
		written, copyErr := io.CopyBuffer(file, io.LimitReader(body, expected-offset+1), buffer)
		_ = body.Close()
		size := offset + written
		if size > expected {
			return last, fail(ClassSizeMismatch, nil)
		}
		if copyErr == nil && size == expected {
			break
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
		// Progress is a partial longer than ever before — never merely bytes
		// written, or a server restarting every answer would be retried forever.
		if size > highWater {
			highWater, failures = size, 0
		}
		failures++
		cause := error(fail(ClassTransferFailed, copyErr))
		if copyErr == nil {
			// The answer ended early with no error: the connection was cut cleanly.
			cause = fail(ClassTransferFailed, io.ErrUnexpectedEOF)
		}
		if !e.retry(ctx, cause, failures) {
			return last, e.fetchFailure(ctx, cause)
		}
	}
	return last, e.verify(file, expected, grant.File.CRC32C, digest)
}

// verify checks the partial's size and CRC32C — the grant's, else the
// `Repr-Digest` the answer carried — streaming the file once more.
func (e *Engine) verify(file *os.File, expected int64, grantCRC, answerCRC string) error {
	want := grantCRC
	if want == "" {
		want = answerCRC
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail(ClassLocalWriteFailed, err)
	}
	sum, n, err := CRC32C(file)
	if err != nil {
		return fail(ClassLocalWriteFailed, err)
	}
	if n != expected {
		return fail(ClassSizeMismatch, nil)
	}
	if want != "" && sum != want {
		return fail(ClassChecksumMismatch, nil)
	}
	return nil
}

// fetchFailure is how a download that gave up ends: the context's error when it
// ended, else the classified cause.
func (e *Engine) fetchFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var classified contracts.ClassifiedError
	if errors.As(err, &classified) {
		return err
	}
	return fail(ClassTransferFailed, err)
}

// logDownload is the one line a download leaves: ids, the size and the class —
// 🔴 never a path, a filename or the grant.
func (e *Engine) logDownload(requestID string, grant contracts.TransferGrant, size int64, class string) {
	attrs := []any{
		slog.String("op", "download"), slog.String("request", requestID), slog.String("jti", grant.JTI),
		slog.Int64("bytes", size),
	}
	if class == "" {
		e.log.Info("file transferred", append(attrs, slog.String("class", "ok"))...)
		return
	}
	e.log.Warn("file transfer failed", append(attrs, slog.String("class", class))...)
}

// String keeps a Downloaded out of a careless log line's reach.
func (d Downloaded) String() string { return fmt.Sprintf("Downloaded{size=%d}", d.Size) }

package transfer

import (
	"context"
	"log/slog"
	"slices"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// Prefetch downloads a brief's attachments before the brief is handed over —
// 🔴 ONLY when the attached plugin's hello asked for it (`capabilities.
// prefetchAttachments`): what a harness wants from a file is its plugin's
// choice, never this side's. The budget is the smaller of the plugin's
// `maxBytes` and the companion's own UFFICI_PREFETCH_MAX_BYTES, per brief.
//
// Each attachment it fetched carries its workspace path; one it did not — over
// the budget, refused, failed — carries a class instead, and the plugin can
// still download it explicitly. The entries are copied, never edited in place.
func (e *Engine) Prefetch(ctx context.Context, requestID string, entries []contracts.Entry) []contracts.Entry {
	maxBytes, asked := e.prefetchAsked()
	if !asked || !hasAttachments(entries) {
		return entries
	}
	budget := min(maxBytes, e.prefetchMax)
	ctx, end := e.begin(ctx, requestID)
	defer end()

	out := slices.Clone(entries)
	var fetched, failed int
	var used int64
	for i := range out {
		if len(out[i].Attachments) == 0 {
			continue
		}
		attachments := slices.Clone(out[i].Attachments)
		for j := range attachments {
			attachment := &attachments[j]
			if attachment.ID == "" {
				continue
			}
			if ctx.Err() != nil {
				attachment.PrefetchClass = ClassCancelled
				failed++
				continue
			}
			got, err := e.download(ctx, requestID, DownloadTarget{AttachmentID: attachment.ID}, max(budget-used, 0))
			if err != nil {
				attachment.PrefetchClass = ClassOf(err)
				failed++
				continue
			}
			attachment.LocalPath = got.LocalPath
			used += got.Size
			fetched++
		}
		out[i].Attachments = attachments
	}
	e.log.Info("attachments prefetched", slog.String("request", requestID),
		slog.Int("fetched", fetched), slog.Int("notFetched", failed), slog.Int64("bytes", used))
	return out
}

func hasAttachments(entries []contracts.Entry) bool {
	for _, entry := range entries {
		if len(entry.Attachments) > 0 {
			return true
		}
	}
	return false
}

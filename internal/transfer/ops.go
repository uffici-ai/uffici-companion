package transfer

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// UnknownRequestInstruction answers a file op naming a request the companion was
// never rung for: nothing was sent.
const UnknownRequestInstruction = "This file request names a request you are not answering, so nothing was transferred."

// Handlers maps the two file ops to their CallHandlers — both, or the `files`
// feature is not announced.
func (e *Engine) Handlers() map[string]surface.CallHandler {
	if e == nil {
		// No engine — a host that moves no files: no op, and no feature.
		return nil
	}
	return map[string]surface.CallHandler{
		wire.OpFilesDownload: e.serveDownload,
		wire.OpFilesUpload:   e.serveUpload,
	}
}

type downloadPayload struct {
	RequestID    string `json:"requestId"`
	AttachmentID string `json:"attachmentId"`
	Path         string `json:"path"`
}

type downloadResult struct {
	LocalPath   string `json:"localPath"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
}

type uploadPayload struct {
	RequestID string `json:"requestId"`
	LocalPath string `json:"localPath"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
}

type attachedResult struct {
	Ref  string `json:"ref"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type savedResult struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// serveDownload is `files.download`: `{requestId, attachmentId | path}` →
// `{localPath, size, contentType}`, the path relative to the workspace.
func (e *Engine) serveDownload(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var request downloadPayload
	if err := decode(wire.OpFilesDownload, payload, &request); err != nil {
		return nil, err
	}
	got, err := e.Download(ctx, request.RequestID, DownloadTarget{AttachmentID: request.AttachmentID, Path: request.Path})
	if err != nil {
		return nil, callFailure(err)
	}
	return answer(wire.OpFilesDownload, downloadResult(got))
}

// serveUpload is `files.upload`: `{requestId, localPath, to?, overwrite?}` →
// `{ref, id, name, size}` (attached to the reply) or `{path, size}` (saved into
// Files).
func (e *Engine) serveUpload(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var request uploadPayload
	if err := decode(wire.OpFilesUpload, payload, &request); err != nil {
		return nil, err
	}
	got, err := e.Upload(ctx, request.RequestID, UploadRequest{LocalPath: request.LocalPath, To: request.To, Overwrite: request.Overwrite})
	if err != nil {
		return nil, callFailure(err)
	}
	if got.Path != "" {
		return answer(wire.OpFilesUpload, savedResult{Path: got.Path, Size: got.Size})
	}
	return answer(wire.OpFilesUpload, attachedResult{Ref: got.Ref, ID: got.ID, Name: got.Name, Size: got.Size})
}

func decode(op string, payload json.RawMessage, into any) error {
	if err := wire.ValidateOpPayload(op, wire.OpRequest, payload); err != nil {
		return &surface.CallFailure{Class: wire.ClassMalformedPayload}
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return &surface.CallFailure{Class: wire.ClassMalformedPayload}
	}
	return nil
}

func answer(op string, result any) (json.RawMessage, error) {
	raw, err := json.Marshal(result)
	if err == nil {
		err = wire.ValidateOpPayload(op, wire.OpResult, raw)
	}
	if err != nil {
		// A platform answer this companion cannot put on the wire as its result.
		return nil, &surface.CallFailure{Class: wire.ClassUpstreamFailed, Instruction: instructions[ClassTransferFailed]}
	}
	return raw, nil
}

// callFailure maps a transfer's failure onto the wire's closed vocabulary: a
// request never rung and a refused path are `not_permitted` — nothing left the
// machine; a missing local file is `malformed_payload` — the input named
// nothing; everything else is `upstream_failed`, with the class in `upstream`
// and the sentence the agent relays.
func callFailure(err error) *surface.CallFailure {
	class := ClassOf(err)
	upstream, _ := json.Marshal(map[string]string{"class": class})
	var fence *FenceError
	switch {
	case errors.Is(err, contracts.ErrUnknownRequest):
		return &surface.CallFailure{Class: wire.ClassNotPermitted, Instruction: UnknownRequestInstruction}
	case errors.As(err, &fence):
		return &surface.CallFailure{Class: wire.ClassNotPermitted, Instruction: instructions[ClassPathRefused], Upstream: upstream}
	case class == ClassLocalFileMissing:
		return &surface.CallFailure{Class: wire.ClassMalformedPayload, Instruction: instructions[ClassLocalFileMissing], Upstream: upstream}
	}
	instruction := instructions[class]
	var classified contracts.ClassifiedError
	if errors.As(err, &classified) && classified.ErrorInstruction() != "" {
		instruction = classified.ErrorInstruction()
	}
	if instruction == "" {
		instruction = instructions[ClassTransferFailed]
	}
	return &surface.CallFailure{Class: wire.ClassUpstreamFailed, Instruction: instruction, Upstream: upstream}
}

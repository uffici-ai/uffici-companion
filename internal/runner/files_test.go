package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"hash/crc32"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/fixture"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/requests"
)

// #2679: `expectPost` with `attachments`, `expectUpload`, `interruptNextUpload`
// and the files tally — the steps the change gate's files conversation plays.

func crc(data []byte) string {
	sum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	return base64.StdEncoding.EncodeToString([]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)})
}

// uploadAndPost is a harness's turn that makes a file: it uploads `data` (into
// Files when `to` is set) and posts — with the attachment when there is one.
func uploadAndPost(t *testing.T, r *rig, requestID, to string, data []byte, declared string) {
	t.Helper()
	ctx := context.Background()
	grant, err := r.host.GrantTransfer(ctx, requestID, contracts.TransferAsk{
		Op: contracts.TransferUpload, Filename: "report.pdf", Size: int64(len(data)), CRC32C: declared, To: to,
	})
	if err != nil {
		t.Errorf("GrantTransfer: %v", err)
		return
	}
	if err := r.host.UploadFile(ctx, grant, bytes.NewReader(data), contracts.UploadSpec{
		Size: int64(len(data)), ChunkBytes: 1 << 10, Attempts: 4,
	}); err != nil {
		t.Errorf("UploadFile: %v", err)
		return
	}
	extras := &contracts.PostExtras{}
	if done, err := r.host.CompleteTransfer(ctx, requestID, grant.JTI); err == nil && done.ID != "" {
		extras.Attachments = []contracts.Attachment{{ID: done.ID, Name: done.Name, Ref: done.Ref}}
	}
	if _, err := r.host.Post(ctx, requestID, "conv_1", "here it is", extras); err != nil {
		t.Errorf("Post: %v", err)
	}
}

func TestTheFileStepsAndTheFilesTally(t *testing.T) {
	report := append([]byte("%PDF-1.4 [report-canary]\n"), bytes.Repeat([]byte("r"), 6000)...)
	ring := fixture.Step{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"}
	one, zero := 1, 0
	for name, tc := range map[string]struct {
		to       string
		declared string
		steps    []fixture.Step
		tally    fixture.Assertions
		pass     bool
		resumed  int
	}{
		"an attached upload, interrupted and resumed": {
			declared: crc(report),
			steps: []fixture.Step{
				{Step: "interruptNextUpload", AfterBytes: 500}, ring,
				{Step: "expectPost", Attachments: &one, WithinMs: 5000},
				{Step: "expectUpload", Contains: "[report-canary]", WithinMs: 5000},
			},
			tally: fixture.Assertions{RingsDelivered: 1, Uploads: &one, UploadsResumed: &one, Downloads: &zero},
			pass:  true, resumed: 1,
		},
		"an upload into Files": {
			to: "reports/q3.pdf", declared: crc(report),
			steps: []fixture.Step{ring,
				{Step: "expectUpload", To: "/reports/q3.pdf", Contains: "[report-canary]", WithinMs: 5000},
				{Step: "expectPost", Attachments: &zero, WithinMs: 5000},
			},
			tally: fixture.Assertions{RingsDelivered: 1, Uploads: &one, UploadsResumed: &zero},
			pass:  true,
		},
		"bytes that are not the declared file attach nothing": {
			declared: crc([]byte("something else")),
			steps: []fixture.Step{ring,
				{Step: "expectPost", Attachments: &one, WithinMs: 300},
				{Step: "expectUpload", WithinMs: 300},
			},
			tally: fixture.Assertions{RingsDelivered: 1},
		},
		"an upload saved elsewhere is a miss": {
			to: "reports/other.pdf", declared: crc(report),
			steps: []fixture.Step{ring, {Step: "expectUpload", To: "reports/q3.pdf", WithinMs: 300}},
			tally: fixture.Assertions{RingsDelivered: 1},
		},
		"a canary the bytes do not carry is a miss": {
			declared: crc(report),
			steps:    []fixture.Step{ring, {Step: "expectUpload", Contains: "[absent]", WithinMs: 300}},
			tally:    fixture.Assertions{RingsDelivered: 1},
		},
		"a tally that disagrees fails": {
			declared: crc(report),
			steps:    []fixture.Step{ring, {Step: "expectUpload", WithinMs: 5000}},
			tally:    fixture.Assertions{RingsDelivered: 1, Uploads: &zero, UploadsResumed: &one, Downloads: &one},
		},
	} {
		t.Run(name, func(t *testing.T) {
			plugin := &autoPlugin{session: requests.SessionFresh, answer: "ok"}
			r := newRig(t, script(tc.steps, tc.tally), plugin)
			t.Cleanup(func() { _ = r.host.Close() })
			r.host.UseBytesClient(func(base string) fixture.BytesClient { return &gateway.FilesClient{BaseURL: base} })
			plugin.beforePost = func(_ context.Context, requestID string) {
				uploadAndPost(t, r, requestID, tc.to, report, tc.declared)
			}
			result, err := r.runner.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.OK() != tc.pass {
				t.Fatalf("OK() = %v, want %v: %v", result.OK(), tc.pass, result.Unmet)
			}
			attached := 0
			if tc.to == "" {
				attached = 1
			}
			if tc.pass && (result.Counts.UploadsResumed != tc.resumed || result.Counts.AttachmentsPosted != attached) {
				t.Fatalf("counts = %+v", result.Counts)
			}
		})
	}
}

//go:build !windows

package transfer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// 🔴 #2673 self-review H1–H3, M6, M7, L8–L11: the fence holds at the ACT, not
// only at the check. Whoever can write the workspace — a prompt-injected agent,
// the harness's container in a pod — swaps a directory for a link between the
// fence's check and the act it guards (`beforeAct`, the seam between the two),
// and nothing outside the workspace is read, written, chmodded or deleted.

// swapForLink replaces the directory at path with a link to target, keeping
// the directory's contents under `<path>-old` so nothing is lost by the swap.
func swapForLink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// home is a stand-in for $UFFICI_HOME, holding the agent key.
func home(t *testing.T) (dir, key string) {
	t.Helper()
	dir = t.TempDir()
	key = filepath.Join(dir, "agent-key")
	if err := os.WriteFile(key, []byte("uffici_ak_secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, key
}

func TestAnUploadNeverReadsThroughADirectorySwappedForALink(t *testing.T) {
	homeDir, _ := home(t)
	host := newFakeHost(nil)
	engine, ws := newEngine(t, host)
	mustMkdir(t, filepath.Join(ws.Root(), "docs"))
	if err := os.WriteFile(filepath.Join(ws.Root(), "docs", "agent-key"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws.beforeAct = func(act string) {
		if act == actOpen {
			swapForLink(t, filepath.Join(ws.Root(), "docs"), homeDir)
		}
	}
	_, err := engine.Upload(context.Background(), testRequest, UploadRequest{LocalPath: "docs/agent-key"})
	if ClassOf(err) != ClassLocalFileMissing {
		t.Fatalf("an upload through a swapped directory answered %v", err)
	}
	if len(host.asks) != 0 || host.uploaded != nil {
		t.Fatalf("the agent key was offered for upload: %d grants, %q", len(host.asks), host.uploaded)
	}
}

func TestADownloadIsNeverRenamedIntoADirectorySwappedForALink(t *testing.T) {
	outside := t.TempDir()
	engine, ws := newEngine(t, newFakeHost([]byte("attacker-chosen bytes")))
	mustMkdir(t, filepath.Join(ws.Root(), "files", "ssh"))
	ws.beforeAct = func(act string) {
		if act == actRename {
			swapForLink(t, filepath.Join(ws.Root(), "files", "ssh"), outside)
		}
	}
	_, err := engine.Download(context.Background(), testRequest, DownloadTarget{Path: "ssh/authorized_keys"})
	if ClassOf(err) != ClassLocalWriteFailed {
		t.Fatalf("a rename through a swapped directory answered %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the download landed outside the workspace: %d entries", len(entries))
	}
	if partials(t, ws) != 0 {
		t.Fatal("the refused download left its partial behind")
	}
}

func TestNoChmodFollowsALinkPutAtTheDestination(t *testing.T) {
	_, key := home(t)
	engine, ws := newEngine(t, newFakeHost([]byte("report")))
	dest := filepath.Join(ws.Root(), "files", "report.txt")
	ws.beforeAct = func(act string) {
		if act == actRenamed {
			if err := os.Remove(dest); err != nil {
				t.Fatal(err)
			}
			mustLink(t, key, dest)
		}
	}
	_, err := engine.Download(context.Background(), testRequest, DownloadTarget{Path: "report.txt"})
	if ClassOf(err) != ClassLocalWriteFailed {
		t.Fatalf("a destination swapped for a link answered %v", err)
	}
	info, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the agent key's mode became %v", info.Mode().Perm())
	}
}

func TestAPartialIsNeverWrittenThroughALink(t *testing.T) {
	outside := t.TempDir()
	precious := filepath.Join(outside, testJTI)
	if err := os.WriteFile(precious, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine, ws := newEngine(t, newFakeHost([]byte("attacker-chosen bytes")))
	swapForLink(t, filepath.Join(ws.Root(), filepath.FromSlash(partialDir)), outside)
	if _, err := engine.Download(context.Background(), testRequest, DownloadTarget{Path: "x.txt"}); err == nil {
		t.Fatal("a download finished through a partial directory swapped for a link")
	}
	if got, _ := os.ReadFile(precious); !bytes.Equal(got, []byte("precious")) {
		t.Fatalf("a file outside the workspace was overwritten: %q", got)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("the outside directory holds %d entries", len(entries))
	}
}

func TestClearingPartialsDeletesNothingThroughALink(t *testing.T) {
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	mustWrite(t, keep)

	// Swapped after the workspace opened: the pinned corner reaches nothing.
	ws, err := OpenWorkspace(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	swapForLink(t, filepath.Join(ws.Root(), filepath.FromSlash(partialDir)), outside)
	_ = ws.clearPartials()
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("clearing partials deleted a file outside the workspace: %v", err)
	}

	// A link already there when the workspace opens — out of it, or to another
	// directory inside it — is refused, and deletes nothing either.
	for name, target := range map[string]func(root string) string{
		"outside": func(string) string { return outside },
		// Relative, as the root follows only those: it stays inside the workspace.
		"inside": func(string) string { return filepath.Join("..", "conversations") },
	} {
		root := t.TempDir()
		mustMkdir(t, filepath.Join(root, internalDir))
		mustMkdir(t, filepath.Join(root, "conversations", "c"))
		mustWrite(t, filepath.Join(root, "conversations", "c", "a"))
		mustLink(t, target(root), filepath.Join(root, filepath.FromSlash(partialDir)))
		if _, err := OpenWorkspace(root, false, ""); err == nil {
			t.Errorf("%s: a workspace whose partial directory is a link was opened", name)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("%s: a file outside the workspace was deleted", name)
		}
		if _, err := os.Stat(filepath.Join(root, "conversations", "c", "a")); err != nil {
			t.Fatalf("%s: a conversation's file was deleted", name)
		}
	}
}

func TestAnUploadOfAFIFOIsRefusedAndNeverBlocks(t *testing.T) {
	host := newFakeHost(nil)
	engine, ws := newEngine(t, host)
	if err := syscall.Mkfifo(filepath.Join(ws.Root(), "pipe"), 0o600); err != nil {
		t.Skipf("no FIFO on this filesystem: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := engine.Upload(context.Background(), testRequest, UploadRequest{LocalPath: "pipe"})
		done <- err
	}()
	select {
	case err := <-done:
		if ClassOf(err) != ClassLocalFileMissing {
			t.Fatalf("a FIFO answered %v", err)
		}
	case <-time.After(5 * time.Second):
		// Unblock the stuck open before failing, so the test binary can exit.
		if writer, err := os.OpenFile(filepath.Join(ws.Root(), "pipe"), os.O_WRONLY, 0); err == nil {
			_ = writer.Close()
		}
		t.Fatal("opening a FIFO blocked the upload")
	}
	if len(host.asks) != 0 {
		t.Fatal("a FIFO was offered for upload")
	}
}

func TestAnUploadRefusesAHardLink(t *testing.T) {
	_, key := home(t)
	host := newFakeHost(nil)
	engine, ws := newEngine(t, host)
	if err := os.Link(key, filepath.Join(ws.Root(), "notes.txt")); err != nil {
		t.Skipf("no hard link across these directories: %v", err)
	}
	_, err := engine.Upload(context.Background(), testRequest, UploadRequest{LocalPath: "notes.txt"})
	var fence *FenceError
	if !errors.As(err, &fence) || fence.Reason != FenceLinked {
		t.Fatalf("a hard link answered %v", err)
	}
	if len(host.asks) != 0 {
		t.Fatal("a hard-linked file was offered for upload")
	}
}

func TestTheWorkspaceIsNeverTheHomeNorAboveIt(t *testing.T) {
	homeDir, _ := home(t)
	parent := filepath.Dir(homeDir)
	for name, root := range map[string]string{"the home": homeDir, "above the home": parent} {
		if _, err := OpenWorkspace(root, false, homeDir); !errors.Is(err, ErrWorkspaceHoldsHome) {
			t.Errorf("%s as the workspace answered %v", name, err)
		}
	}
	// Reached through a link, it is still the home.
	alias := filepath.Join(t.TempDir(), "alias")
	mustLink(t, homeDir, alias)
	if _, err := OpenWorkspace(alias, false, homeDir); !errors.Is(err, ErrWorkspaceHoldsHome) {
		t.Errorf("the home through a link answered %v", err)
	}
	// The default, inside the home, is the workspace's ordinary place.
	ws, err := OpenWorkspace(filepath.Join(homeDir, "workspace"), false, homeDir)
	if err != nil {
		t.Fatalf("the default workspace was refused: %v", err)
	}
	if _, err := ws.Resolve("../agent-key"); err == nil {
		t.Fatal("the agent key was reachable from the default workspace")
	}
}

// 🔴 L9: the cached file's name is `<attachmentId>-<name>`, so an id with a `-`
// of its own would make two attachments' names collide; it is refused before
// any grant, and so can never be written.
func TestAnAttachmentIDWithADashIsRefused(t *testing.T) {
	host := newFakeHost([]byte("data"))
	engine, _ := newEngine(t, host)
	_, err := engine.Download(context.Background(), testRequest, DownloadTarget{AttachmentID: "7-1"})
	var fence *FenceError
	if !errors.As(err, &fence) || fence.Reason != FenceInvalid {
		t.Fatalf("an id with a dash answered %v", err)
	}
	if len(host.asks) != 0 {
		t.Fatal("an id with a dash was asked for")
	}
}

func TestAnAbsolutePathIsFencedAsWrittenToo(t *testing.T) {
	given := t.TempDir()
	ws, err := OpenWorkspace(given, false, "")
	if err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(ws.Root(), "public"))
	mustLink(t, filepath.Join(ws.Root(), "public"), filepath.Join(ws.Root(), ".hidden"))
	for _, base := range []string{given, ws.Root()} {
		var fence *FenceError
		if _, err := ws.Resolve(filepath.Join(base, ".hidden", "x")); !errors.As(err, &fence) || fence.Reason != FenceDotfile {
			t.Errorf("an absolute hidden name under %s answered %v", base, err)
		}
	}
}

func TestTheCompanionsCornerIsRefusedInAnyCase(t *testing.T) {
	ws, err := OpenWorkspace(t.TempDir(), true, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".UFFICI/partial/x", ".Uffici/partial", "docs/.uFFici"} {
		var fence *FenceError
		if _, err := ws.Resolve(name); !errors.As(err, &fence) || fence.Reason != FenceDotfile {
			t.Errorf("Resolve(%q) = %v", name, err)
		}
	}
}

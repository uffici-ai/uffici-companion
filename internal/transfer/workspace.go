package transfer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Directory and file modes for what the companion writes into the workspace:
// the companion's own user, and its group — in a pod the harness reads the
// shared volume through the pod's group.
const (
	dirMode  fs.FileMode = 0o750
	fileMode fs.FileMode = 0o640
)

// The companion's own corner of the workspace: partial downloads, keyed by the
// grant's jti. 🔴 A dot directory, so the fence refuses it to every path a
// plugin or the platform names — nothing outside this package reaches it.
const (
	internalDir = ".uffici"
	partialDir  = ".uffici/partial"
)

// The workspace layout every plugin is told about — the same for every harness.
const (
	// ConversationsDir holds a conversation's downloaded attachments:
	// `conversations/<conversationId>/<attachmentId>-<name>`.
	ConversationsDir = "conversations"
	// FilesDir holds files downloaded from the organization's Files by path:
	// `files/<path>`.
	FilesDir = "files"
)

// Workspace is the one directory the companion reads and writes on the
// harness's behalf (#2673, epic #2667 decision 8).
//
// 🔴 Every path it is handed is FENCED twice. Resolve is the CHECK: the path is
// resolved through the filesystem's own symlinks to inside the root, never by a
// string comparison alone, and refused when it names a parent (`..`), leaves
// the root, or names a hidden file — a dot segment, `~/.ssh` above all — unless
// the machine's owner allowed dotfiles. A refusal is a FenceError, never a
// panic, and its reason is what the plugin is told.
//
// 🔴 And every ACT — an open, a create, a directory made, a rename, a removal —
// goes through an `os.Root` opened once on the workspace, never through a path
// string. A check on a string is a moment in time: whoever can write the
// workspace (a prompt-injected agent, the harness's container in a pod) can swap
// a directory for a link between the check and the act, and a path-based open
// would follow it out — to `$UFFICI_HOME/agent-key`, to `~/.ssh`. The root
// refuses any component that leads out of the workspace at the moment of the
// act (#2673 self-review). Its price: a link whose target is absolute is refused
// even when it points inside the workspace.
type Workspace struct {
	root string
	// given is the root as configured, cleaned but not resolved: an absolute path
	// a plugin names is read as written against it as well as the real path.
	given         string
	allowDotfiles bool
	// fs is the workspace itself; partials is the companion's own corner of it,
	// opened once and pinned, so a link put in its place later reaches nothing.
	fs       *os.Root
	partials *os.Root
	// beforeAct runs between a check and the act it guards. Tests only: it is
	// where they swap a directory for a link.
	beforeAct func(act string)
}

// The acts beforeAct names.
const (
	actOpen    = "open"
	actMkdir   = "mkdir"
	actRename  = "rename"
	actRenamed = "renamed"
)

func (w *Workspace) act(name string) {
	if w.beforeAct != nil {
		w.beforeAct(name)
	}
}

// FenceReason says why a path was refused.
type FenceReason string

// The reasons.
const (
	FenceEmpty   FenceReason = "empty"
	FenceParent  FenceReason = "parent"
	FenceOutside FenceReason = "outside"
	FenceDotfile FenceReason = "dotfile"
	FenceInvalid FenceReason = "invalid"
	// FenceLinked — a file with more than one name: a hard link can bring a file
	// from anywhere on the same filesystem into the workspace.
	FenceLinked FenceReason = "linked"
)

// FenceError is a path the workspace refused. 🔴 It carries the reason and never
// the path: a path names a person's files, and this error reaches log lines.
type FenceError struct {
	Reason FenceReason
}

func (e *FenceError) Error() string { return "workspace: path refused (" + string(e.Reason) + ")" }

// ErrWorkspaceHoldsHome refuses a workspace that is, or contains, the
// companion's own directory.
var ErrWorkspaceHoldsHome = errors.New("the workspace must not be, or contain, the companion's home directory")

// OpenWorkspace creates the root when it is missing and resolves it through
// its own symlinks, so every later comparison is between real paths; then opens
// the root every act goes through.
//
// 🔴 home is the companion's own directory ($UFFICI_HOME — the agent key, the
// wire token): a workspace that IS it, or contains it, would put the machine's
// secrets inside the fence, and is refused. A workspace inside it is the
// default (`$UFFICI_HOME/workspace`) and is admitted: the root fences every act
// to the workspace, so the home's other files are never reachable from it. An
// empty home checks nothing.
func OpenWorkspace(root string, allowDotfiles bool, home string) (*Workspace, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("the workspace must be an absolute path")
	}
	if err := os.MkdirAll(root, dirMode); err != nil {
		return nil, fmt.Errorf("creating the workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolving the workspace: %w", err)
	}
	if home != "" && holdsHome(resolved, home) {
		return nil, ErrWorkspaceHoldsHome
	}
	fsRoot, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("opening the workspace: %w", err)
	}
	if err := fsRoot.MkdirAll(filepath.FromSlash(partialDir), 0o700); err != nil {
		_ = fsRoot.Close()
		return nil, fmt.Errorf("creating the workspace's partial directory: %w", err)
	}
	partials, err := openPinned(fsRoot, internalDir, "partial")
	if err != nil {
		_ = fsRoot.Close()
		return nil, fmt.Errorf("opening the workspace's partial directory: %w", err)
	}
	return &Workspace{
		root: resolved, given: filepath.Clean(root), allowDotfiles: allowDotfiles,
		fs: fsRoot, partials: partials,
	}, nil
}

// holdsHome reports whether the workspace's real path is the home or one of its
// ancestors. A home that does not resolve is compared as written.
func holdsHome(workspace, home string) bool {
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		resolvedHome = filepath.Clean(home)
	}
	_, inside := lexicallyInside(workspace, resolvedHome)
	return inside
}

// openPinned opens a directory under root one component at a time, each one
// required to BE a directory (never a link to one) and to be the very directory
// that was looked at — so a link swapped in while it opens is refused, never
// followed.
func openPinned(root *os.Root, components ...string) (*os.Root, error) {
	current := root
	for i, name := range components {
		next, err := openPinnedOne(current, name)
		if i > 0 {
			_ = current.Close()
		}
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func openPinnedOne(parent *os.Root, name string) (*os.Root, error) {
	seen, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !seen.IsDir() {
		return nil, errNotDirectory
	}
	next, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := next.Stat(".")
	if err == nil && !os.SameFile(seen, opened) {
		err = errNotDirectory
	}
	if err != nil {
		_ = next.Close()
		return nil, err
	}
	return next, nil
}

var errNotDirectory = errors.New("not the directory that was looked at")

// Close releases the workspace's open directories.
func (w *Workspace) Close() error {
	return errors.Join(w.partials.Close(), w.fs.Close())
}

// Root is the workspace's real path.
func (w *Workspace) Root() string { return w.root }

// Resolve fences a path a plugin or the platform named — relative to the root,
// or absolute and inside it — and returns its real absolute path. The path need
// not exist: the deepest part that does is resolved through its symlinks, and
// the rest is joined to it.
//
// 🔴 Resolve is the CHECK and yields the refusal's reason; it is never the act.
// What it returns is handed to the act as Rel's relative path, through the root.
func (w *Workspace) Resolve(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", &FenceError{Reason: FenceEmpty}
	}
	if strings.ContainsRune(name, 0) {
		return "", &FenceError{Reason: FenceInvalid}
	}
	// 🔴 Any `..` is refused outright, not cleaned away: `link/..` is lexically
	// nothing, and on disk it is the PARENT OF THE LINK'S TARGET.
	for _, segment := range strings.FieldsFunc(filepath.ToSlash(name), func(r rune) bool { return r == '/' }) {
		if segment == ".." {
			return "", &FenceError{Reason: FenceParent}
		}
	}
	candidate := filepath.Clean(name)
	// A hidden name is refused as WRITTEN too, not only as it resolves: a
	// `.secrets` that is a link to an ordinary directory is still a name the
	// person hid — and so is one inside an absolute path naming the workspace.
	if filepath.IsAbs(candidate) {
		for _, base := range []string{w.given, w.root} {
			if rel, ok := lexicallyInside(base, candidate); ok && rel != "." {
				if err := w.dotSegments(rel); err != nil {
					return "", err
				}
			}
		}
	} else {
		if err := w.dotSegments(filepath.ToSlash(candidate)); err != nil {
			return "", err
		}
		candidate = filepath.Join(w.root, candidate)
	}
	resolved, err := realPath(candidate)
	if err != nil {
		return "", &FenceError{Reason: FenceInvalid}
	}
	rel, ok := w.inside(resolved)
	if !ok {
		return "", &FenceError{Reason: FenceOutside}
	}
	if rel == "." {
		return resolved, nil
	}
	if err := w.dotSegments(rel); err != nil {
		return "", err
	}
	return resolved, nil
}

// dotSegments refuses a hidden segment — and the companion's own corner even
// when dotfiles are allowed.
func (w *Workspace) dotSegments(rel string) error {
	for _, segment := range strings.Split(rel, "/") {
		// 🔴 Case-insensitively: on a case-insensitive filesystem (macOS by
		// default) `.UFFICI` IS the companion's corner.
		if strings.EqualFold(segment, internalDir) || (strings.HasPrefix(segment, ".") && segment != "." && !w.allowDotfiles) {
			return &FenceError{Reason: FenceDotfile}
		}
	}
	return nil
}

// Rel is a resolved path relative to the root, with forward slashes — what the
// wire carries, and what an act hands the root.
func (w *Workspace) Rel(resolved string) string {
	rel, _ := w.inside(resolved)
	return rel
}

// inside reports whether a real path is the root or under it, and its relative
// form with forward slashes.
func (w *Workspace) inside(resolved string) (string, bool) {
	return lexicallyInside(w.root, resolved)
}

// lexicallyInside reports whether target is base or under it, compared as
// written, and its relative form with forward slashes.
func lexicallyInside(base, target string) (string, bool) {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// realPath resolves a path through the filesystem's symlinks: the deepest
// existing ancestor with EvalSymlinks, the missing rest joined to it. 🔴 This,
// and not filepath.Clean, is the check: a symlink inside the root pointing out
// of it resolves OUT here.
func realPath(path string) (string, error) {
	missing := []string{}
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		// A path through a regular file does not exist either; whatever writes
		// there next fails on its own.
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// openRegular opens a workspace file for reading THROUGH THE ROOT — rel is
// relative to the workspace, as Rel returns it — and only when it is a regular
// file: never a directory, a device or a FIFO. 🔴 Opened non-blocking, so a FIFO
// planted where a file was named cannot hold the open forever; a regular file
// is then returned to ordinary reads.
func (w *Workspace) openRegular(rel string) (*os.File, fs.FileInfo, error) {
	w.act(actOpen)
	file, err := w.fs.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errNotRegular
	}
	if err == nil {
		err = clearNonblock(file)
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

var errNotRegular = errors.New("not a regular file")

// readDir lists a workspace directory through the root.
func (w *Workspace) readDir(rel string) ([]fs.DirEntry, error) {
	w.act(actOpen)
	dir, err := w.fs.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return dir.ReadDir(-1)
}

// mkdirParent creates the parent of a destination, rel to the workspace,
// through the root.
func (w *Workspace) mkdirParent(rel string) error {
	w.act(actMkdir)
	return w.fs.MkdirAll(filepath.FromSlash(path.Dir(rel)), dirMode)
}

// createPartial creates the file a download under the grant `jti` is written
// to until it is verified — in the companion's own pinned corner, never a path
// anyone named, and never one that already exists (O_EXCL: not even a link).
func (w *Workspace) createPartial(jti string) (*os.File, error) {
	return w.partials.OpenFile(jti, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
}

// removePartial deletes a partial download.
func (w *Workspace) removePartial(jti string) {
	_ = w.partials.Remove(jti)
}

// finalize moves a verified partial to its destination, rel to the workspace.
// 🔴 Its mode is set on the open descriptor first — never a chmod by path,
// which follows a link put at the destination — and the rename goes through the
// root. The file at the destination afterwards must BE the partial: a partial
// directory swapped for a link would have moved someone else's file.
func (w *Workspace) finalize(partial *os.File, jti, rel string) error {
	if err := partial.Chmod(fileMode); err != nil {
		return err
	}
	written, err := partial.Stat()
	if err != nil {
		return err
	}
	w.act(actRename)
	if err := w.fs.Rename(filepath.FromSlash(path.Join(partialDir, jti)), filepath.FromSlash(rel)); err != nil {
		return err
	}
	w.act(actRenamed)
	landed, err := w.fs.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return err
	}
	if !os.SameFile(written, landed) {
		return errNotThePartial
	}
	return nil
}

var errNotThePartial = errors.New("the destination is not the verified download")

// clearPartials removes every partial download: at start, none is in flight,
// and a partial is resumable only within the call that holds its grant.
// 🔴 Through the pinned corner: a link put in its place reaches nothing.
func (w *Workspace) clearPartials() error {
	// The pinned corner must still be where finalize renames from: one that was
	// removed or replaced would take every download with it.
	pinned, err := w.partials.Stat(".")
	if err != nil {
		return err
	}
	if placed, err := w.fs.Lstat(filepath.FromSlash(partialDir)); err != nil || !os.SameFile(pinned, placed) {
		return errors.Join(errNotDirectory, err)
	}
	dir, err := w.partials.Open(".")
	if err != nil {
		return err
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := w.partials.RemoveAll(name); err != nil {
			return err
		}
	}
	return nil
}

// Writable reports whether the companion can create a file in the workspace —
// `uffici doctor`'s check. Creates nothing that outlives the call.
func Writable(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	probe, err := os.CreateTemp(root, ".uffici-doctor-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

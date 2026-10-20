//go:build !windows

package transfer

import (
	"io/fs"
	"os"
	"syscall"
)

// openNonblock opens without waiting for a writer: a FIFO planted in the
// workspace would otherwise block the open — and the request answering it —
// forever. A regular file ignores the flag; clearNonblock drops it once the
// descriptor is known to be one.
const openNonblock = syscall.O_NONBLOCK

// clearNonblock returns an opened regular file to ordinary blocking reads.
func clearNonblock(file *os.File) error {
	raw, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) { setErr = syscall.SetNonblock(int(fd), false) }); err != nil {
		return err
	}
	return setErr
}

// linkCount is how many directory entries name the file: more than one is a
// hard link, which can make a file from anywhere on the same filesystem — the
// agent key among them — appear inside the workspace.
func linkCount(info fs.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink) //nolint:unconvert // uint16 on darwin, uint64 on linux
	}
	return 1
}

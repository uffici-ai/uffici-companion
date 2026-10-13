//go:build windows

package transfer

import (
	"io/fs"
	"os"
)

// openNonblock has no Windows flag. Windows compiles and is not a release target.
const openNonblock = 0

// clearNonblock has nothing to clear on Windows.
func clearNonblock(*os.File) error { return nil }

// linkCount is not read on Windows: the stat carries no link count there.
func linkCount(fs.FileInfo) uint64 { return 1 }

//go:build !windows

package rootfs

import (
	"io/fs"
	"syscall"
)

func inodeKey(info fs.FileInfo) (uint64, bool) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino), true
	}
	return 0, false
}

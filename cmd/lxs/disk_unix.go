//go:build unix

package main

import "syscall"

// diskFree reports free and total bytes of the filesystem holding path, for the
// health monitor. Bavail (space available to a non-root process) is the honest
// usable-free figure.
func diskFree(path string) (free, total uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0 // unreadable => unknown, never a false alarm
	}
	bs := uint64(st.Bsize)
	return st.Bavail * bs, st.Blocks * bs
}

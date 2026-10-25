//go:build unix

package main

import (
	"os"
	"syscall"
)

func copyOwner(from os.FileInfo, to string) {
	if st, ok := from.Sys().(*syscall.Stat_t); ok {
		os.Chown(to, int(st.Uid), int(st.Gid))
	}
}

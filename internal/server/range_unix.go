//go:build unix

package server

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentityToken(_ *os.File, info os.FileInfo) string {
	if info == nil {
		return "0"
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		return fmt.Sprintf("%x-%x", st.Dev, st.Ino)
	}
	return "0"
}

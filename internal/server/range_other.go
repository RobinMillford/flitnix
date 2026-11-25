//go:build !unix && !windows

package server

import "os"

func fileIdentityToken(_ *os.File, _ os.FileInfo) string {
	return "0"
}

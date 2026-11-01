//go:build !linux && !darwin && !windows

package server

import (
	"io"
	"os"
)

func checkOSImmutabilityPlatform(f *os.File, path string) (bool, io.Closer) {
	return false, nil
}

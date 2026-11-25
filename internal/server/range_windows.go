//go:build windows

package server

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentityToken(f *os.File, _ os.FileInfo) string {
	if f == nil {
		return "0"
	}
	var bhfi syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &bhfi); err == nil {
		return fmt.Sprintf("%x-%x-%x", bhfi.VolumeSerialNumber, bhfi.FileIndexHigh, bhfi.FileIndexLow)
	}
	return "0"
}

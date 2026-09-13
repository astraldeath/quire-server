//go:build windows

package server

import (
	"fmt"
	"os"
	"syscall"
)

func rootIdentity(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var info syscall.ByHandleFileInformation
	if err = syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

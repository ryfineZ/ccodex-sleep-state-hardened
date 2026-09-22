//go:build !windows

package requestrecorder

import (
	"errors"
	"os"
)

func makeRecordingDirectory(path string) error { return os.MkdirAll(path, 0700) }

func checkPrivateHandle(f *os.File, directory bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("recording storage must be private (directory 0700, files 0600)")
	}
	return nil
}

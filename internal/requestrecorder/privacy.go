package requestrecorder

import (
	"errors"
	"os"
	"path/filepath"
)

// openRecordingRoot validates the directory actually opened, not only its path.
// Existing permissions are never widened or silently rewritten.
func openRecordingRoot(path string) (*os.Root, error) {
	path = filepath.Clean(path)
	if err := makeRecordingDirectory(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("recording directory must be a real private directory, not a link")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	dir, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("recording directory changed during open")
	}
	if err := checkPrivateHandle(dir, true); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func recordingEntries(root *os.Root) ([]os.DirEntry, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err := checkPrivateHandle(dir, true); err != nil {
		return nil, err
	}
	return dir.ReadDir(-1)
}

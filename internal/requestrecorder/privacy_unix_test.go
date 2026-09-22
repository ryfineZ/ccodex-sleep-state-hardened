//go:build !windows

package requestrecorder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makePublicTestDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
}

func TestUnixRecordingPermissionChecksRemainStrict(t *testing.T) {
	c := Default()
	c.Directory = filepath.Join(t.TempDir(), "private")
	s, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := strings.Repeat("d", 32)
	s.save(Record{Schema: 1, ID: id, Mode: "metadata", Started: time.Now()})
	path := filepath.Join(c.Directory, id+".json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("file is not 0600", err)
	}
	if _, err := s.Read(id); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(id); err == nil {
		t.Fatal("non-private existing file was served")
	}
	if err := os.Chmod(c.Directory, 0755); err != nil {
		t.Fatal(err)
	}
	if other, err := NewStore(c); err == nil {
		other.Close()
		t.Fatal("non-private directory was accepted")
	}
	info, _ = os.Stat(c.Directory)
	if info.Mode().Perm() != 0755 {
		t.Fatal("existing directory was silently chmodded")
	}
}

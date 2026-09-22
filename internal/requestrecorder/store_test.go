package requestrecorder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueueFullNonblockingAndQuotaDrops(t *testing.T) {
	s := &Store{queue: make(chan pending, 1)}
	if !s.Submit(pending{}) || s.Submit(pending{}) || s.queueDrops.Load() != 1 {
		t.Fatal("queue did not fail open")
	}
	c := Default()
	c.Directory = filepath.Join(t.TempDir(), "captures")
	real, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	real.mu.Lock()
	real.bytes = int64(c.DiskMiB) << 20
	real.mu.Unlock()
	real.Submit(pending{record: Record{ID: strings.Repeat("a", 32), Schema: 1, Mode: "redacted", Started: time.Now()}})
	end := time.Now().Add(time.Second)
	for real.quotaDrops.Load() == 0 && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	if real.quotaDrops.Load() != 1 || real.saved.Load() != 0 {
		t.Fatal(real.Status())
	}
}
func TestPrivateDirectoryAndSymlinkProtection(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "public")
	makePublicTestDirectory(t, dir)
	c := Default()
	c.Directory = dir
	if s, err := NewStore(c); err == nil {
		s.Close()
		t.Fatal("public directory accepted")
	}
	t.Run("directory_symlink", func(t *testing.T) {
		linked := c
		linked.Directory = filepath.Join(root, "link")
		if err := os.Symlink(dir, linked.Directory); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		if s, err := NewStore(linked); err == nil {
			s.Close()
			t.Fatal("symlink accepted")
		}
	})
	c.Directory = filepath.Join(root, "private")
	s, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	outside := filepath.Join(root, "secret.json")
	data, _ := json.Marshal(Record{Schema: 1, ID: strings.Repeat("b", 32)})
	os.WriteFile(outside, data, 0600)
	t.Run("file_symlink", func(t *testing.T) {
		if err := os.Symlink(outside, filepath.Join(c.Directory, strings.Repeat("b", 32)+".json")); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		if _, err := s.Read(strings.Repeat("b", 32)); err == nil {
			t.Fatal("symlink read exposed outside data")
		}
	})
	if _, err := s.Read("../../secret"); err == nil {
		t.Fatal("path traversal")
	}
}
func TestPersistenceReopenAndNoOverwrite(t *testing.T) {
	c := Default()
	c.Directory = filepath.Join(t.TempDir(), "captures")
	s, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	r := Record{ID: strings.Repeat("c", 32), Schema: 1, Started: time.Now(), Mode: "metadata"}
	s.Submit(pending{record: r})
	s.Close()
	again, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if len(again.List(10)) != 1 {
		t.Fatal("lost index after restart")
	}
	again.Submit(pending{record: r})
	end := time.Now().Add(time.Second)
	for again.writeErrors.Load() == 0 && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	if again.writeErrors.Load() != 1 {
		t.Fatal("existing record overwritten")
	}
}

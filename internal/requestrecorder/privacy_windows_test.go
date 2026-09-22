//go:build windows

package requestrecorder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func testWindowsDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}
func makePublicTestDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	testWindowsDACL(t, path, "D:P(A;OICI;FA;;;WD)")
}
func TestWindowsACLClassifierRejectsBroadNullAndComplexPermissions(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	prefix := "O:" + sid
	own := "(A;OICI;FA;;;" + sid + ")"
	for _, tc := range []struct {
		name, sddl string
		valid      bool
	}{
		{"private_user_and_system", prefix + "D:P" + own + "(A;OICI;FA;;;SY)", true},
		{"private_user_only", prefix + "D:P" + own, true},
		{"world_read", prefix + "D:P" + own + "(A;OICI;FR;;;WD)", false},
		{"users_read", prefix + "D:P" + own + "(A;OICI;FR;;;BU)", false},
		{"null_dacl", prefix + "D:NO_ACCESS_CONTROL", false},
		{"absent_dacl", prefix, false},
		{"empty_dacl", prefix + "D:P", false},
		{"parent_can_add_grants", prefix + "D:" + own, false},
		{"no_child_inheritance", prefix + "D:P(A;;FA;;;" + sid + ")", false},
		{"inherit_only", prefix + "D:P(A;OICIIO;FA;;;" + sid + ")", false},
		{"user_read_only", prefix + "D:P(A;OICI;FR;;;" + sid + ")", false},
		{"complex_deny", prefix + "D:P(D;;FW;;;WD)" + own, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal("invalid test security descriptor", err)
			}
			err = validateRecordingACL(sd, user.User.Sid, true)
			if (err == nil) != tc.valid {
				t.Fatalf("accepted=%v want=%v: %v", err == nil, tc.valid, err)
			}
		})
	}
}
func TestWindowsNewStoreHasPrivateInheritableACLAndReopens(t *testing.T) {
	c := Default()
	c.Directory = filepath.Join(t.TempDir(), "nested", "记录")
	s, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := s.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	err = checkPrivateHandle(dir, true)
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("d", 32)
	s.save(Record{Schema: 1, ID: id, Mode: "metadata", Started: time.Now()})
	if s.saved.Load() != 1 {
		t.Fatal("record not saved", s.Status())
	}
	file, err := s.root.Open(id + ".json")
	if err != nil {
		t.Fatal(err)
	}
	err = checkPrivateHandle(file, false)
	file.Close()
	if err != nil {
		t.Fatal("file did not inherit a private ACL", err)
	}
	s.Close()
	reopened, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Read(id); err != nil {
		t.Fatal(err)
	}
}
func TestWindowsExistingBroadDirectoryNotSilentlyModified(t *testing.T) {
	c := Default()
	c.Directory = filepath.Join(t.TempDir(), "shared")
	makePublicTestDirectory(t, c.Directory)
	const flags = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION
	before, err := windows.GetNamedSecurityInfo(c.Directory, windows.SE_FILE_OBJECT, flags)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := NewStore(c); err == nil {
		s.Close()
		t.Fatal("shared directory accepted")
	}
	after, err := windows.GetNamedSecurityInfo(c.Directory, windows.SE_FILE_OBJECT, flags)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatal("existing directory ACL changed")
	}
}
func TestWindowsFileACLRecheckedBeforeServing(t *testing.T) {
	c := Default()
	c.Directory = filepath.Join(t.TempDir(), "records")
	s, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := strings.Repeat("e", 32)
	s.save(Record{Schema: 1, ID: id, Mode: "metadata", Started: time.Now()})
	if _, err := s.Read(id); err != nil {
		t.Fatal(err)
	}
	testWindowsDACL(t, filepath.Join(c.Directory, id+".json"), "D:P(A;;FA;;;WD)")
	if _, err := s.Read(id); err == nil {
		t.Fatal("broad file ACL accepted")
	}
}
func TestWindowsChangedDirectoryACLPreventsSensitiveWrite(t *testing.T) {
	c := Default()
	c.Directory = filepath.Join(t.TempDir(), "records")
	s, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	testWindowsDACL(t, c.Directory, "D:P(A;OICI;FA;;;WD)")
	id := strings.Repeat("f", 32)
	s.save(Record{Schema: 1, ID: id, Mode: "metadata", Started: time.Now()})
	if s.writeErrors.Load() != 1 || s.saved.Load() != 0 {
		t.Fatal("unsafe file written", s.Status())
	}
	for _, suffix := range []string{".part", ".json"} {
		if _, err := os.Stat(filepath.Join(c.Directory, id+suffix)); !os.IsNotExist(err) {
			t.Fatal("unsafe artifact remains", err)
		}
	}
}
func TestWindowsRecordWithExtraHardLinkRejected(t *testing.T) {
	c := Default()
	c.Directory = filepath.Join(t.TempDir(), "records")
	s, err := NewStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := strings.Repeat("a", 32)
	s.save(Record{Schema: 1, ID: id, Mode: "metadata", Started: time.Now()})
	if err := os.Link(filepath.Join(c.Directory, id+".json"), filepath.Join(t.TempDir(), "extra.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(id); err == nil {
		t.Fatal("multiply-linked record accepted")
	}
}

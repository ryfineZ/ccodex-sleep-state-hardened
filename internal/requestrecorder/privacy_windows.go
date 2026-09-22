//go:build windows

package requestrecorder

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows chmod does not implement POSIX confidentiality bits. Create the leaf
// directory with its ACL already private, before any recording can be written.
// Only the process user and LocalSystem are granted access; ordinary users and
// broad groups are not. Administrative privilege is outside this threat model.
func makeRecordingDirectory(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil // validate, rather than modify, an existing directory below
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return errors.New("cannot identify Windows recording owner")
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	err = windows.CreateDirectory(name, &sa)
	runtime.KeepAlive(sd)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil
	}
	return err
}

func checkPrivateHandle(f *os.File, directory bool) error {
	h := windows.Handle(f.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	// This includes junctions and other reparse points, not just symbolic links.
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return errors.New("recording storage cannot be a Windows reparse point or wrong object type")
	}
	if !directory && info.NumberOfLinks != 1 {
		return errors.New("recording file has unexpected hard links")
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return errors.New("cannot verify recording ACL; an ACL-capable filesystem is required")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return validateRecordingACL(sd, user.User.Sid, directory)
}

// Deliberately conservative: allow only ordinary allow ACEs for this user or
// SYSTEM, with full user access. Reject absent/null/complex/broad DACLs. A
// directory must block inheritance from its parent and pass access to children.
func validateRecordingACL(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID, directory bool) error {
	invalid := errors.New("recording ACL must grant only the current user and SYSTEM; use a new private recording directory")
	if sd == nil || !sd.IsValid() || user == nil || !user.IsValid() {
		return invalid
	}
	defer runtime.KeepAlive(sd)
	owner, _, err := sd.Owner()
	// Elevated Windows processes may create files owned by Administrators.
	// These privileged owners are not equivalent to granting ordinary users access.
	if err != nil || owner == nil || (!owner.Equals(user) && !owner.IsWellKnown(windows.WinLocalSystemSid) && !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid)) {
		return invalid
	}
	control, _, err := sd.Control()
	if err != nil || (directory && control&windows.SE_DACL_PROTECTED == 0) {
		return invalid
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 || acl.AceCount > 16 {
		return invalid
	}
	const inherit = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	const allowedFlags = inherit | windows.INHERITED_ACE
	const full = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff
	userFull, userInherits := false, false
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags & ^uint8(allowedFlags) != 0 || ace.Header.AceSize < 16 {
			return invalid
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || sid.Len()+8 > int(ace.Header.AceSize) {
			return invalid
		}
		isUser := sid.Equals(user)
		if !isUser && !sid.IsWellKnown(windows.WinLocalSystemSid) {
			return invalid
		}
		if isUser && (uint32(ace.Mask)&full == full || uint32(ace.Mask)&windows.GENERIC_ALL != 0) {
			userFull = true
			if ace.Header.AceFlags&inherit == inherit {
				userInherits = true
			}
		}
	}
	if !userFull || (directory && !userInherits) {
		return invalid
	}
	return nil
}

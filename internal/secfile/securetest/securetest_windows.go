//go:build windows

// Package securetest holds the Windows access-control assertions shared by the
// recovery and transfer permission regression tests.
//
// It exists so both stores are held to one policy through one implementation.
// Two hand-copied DACL checkers drift, and a checker that drifts is exactly how
// a broad allow ACE reaches production unnoticed.
package securetest

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/itsyamin/flitnix/internal/secfile"
)

// Minimum rights each principal must hold, written as independent literals
// rather than by reusing secfile's masks. If a production mask silently loses
// a right, these literals still demand it and the test fails.
//
//	0x00000001 FILE_READ_DATA        / FILE_LIST_DIRECTORY
//	0x00000002 FILE_WRITE_DATA       / FILE_ADD_FILE
//	0x00000004 FILE_APPEND_DATA      / FILE_ADD_SUBDIRECTORY
//	0x00000008 FILE_READ_EA          GENERIC_READ expansion
//	0x00000010 FILE_WRITE_EA         GENERIC_WRITE and O_APPEND expansion
//	0x00000020 FILE_EXECUTE          / FILE_TRAVERSE
//	0x00000100 FILE_WRITE_ATTRIBUTES GENERIC_WRITE and O_APPEND expansion
//	0x00010000 DELETE
//	0x00020000 READ_CONTROL
const (
	// Directories need traversal and child creation plus the generic-read
	// rights Go requests; files need write and append but never execution.
	//
	// FILE_WRITE_EA is required on files because every write this application
	// performs goes through GENERIC_WRITE or the explicit O_APPEND set, and an
	// access check demands every specific right a generic right expands to. It
	// is not required on a directory, which Flitnix never opens for writing.
	needUserDir    = 0x00000002 | 0x00000004 | 0x00000008 | 0x00000020 | 0x00010000 | 0x00020000
	needUserFile   = 0x00000002 | 0x00000004 | 0x00000008 | 0x00000010 | 0x00000100 | 0x00010000 | 0x00020000
	needSystemDir  = 0x00000001 | 0x00000002 | 0x00000004 | 0x00000008 | 0x00000020 | 0x00010000
	needSystemFile = 0x00000001 | 0x00000002 | 0x00000004 | 0x00000008 | 0x00000010 | 0x00000100 | 0x00010000
	needAdminDir   = 0x00000001 | 0x00000008 | 0x00000020 | 0x00010000
	needAdminFile  = 0x00000001 | 0x00000008 | 0x00010000
)

// Rights this policy must never grant to anyone.
//
//	0x00040000 WRITE_DAC
//	0x00080000 WRITE_OWNER
//	0x01000000 ACCESS_SYSTEM_SECURITY
//
// FILE_WRITE_EA (0x00000010) is deliberately not here. It reads like an
// unnecessary right - Flitnix never touches extended attributes - but Go opens
// files with GENERIC_WRITE and with O_APPEND, whose kernel expansions include
// FILE_WRITE_EA, and the access check requires all of it. A DACL without it
// makes every write path fail with ACCESS_DENIED.
const forbiddenRights = 0x00040000 | 0x00080000 | 0x01000000

// FileExecute is the bit that means FILE_TRAVERSE on a directory and
// FILE_EXECUTE on a file. Received files must never carry it.
const FileExecute = 0x00000020

// rightDeleteChild is FILE_DELETE_CHILD. It is meaningful only on a
// directory, and reserved (ignored) on a file.
const rightDeleteChild = 0x00000040

// Violations returns every way path's DACL departs from the approved policy,
// sorted for a stable failure message. An empty result means the descriptor
// grants the intended rights to the approved principals and nothing to anyone
// else.
//
// The expected rights follow the object type: a file is held to FileMask, a
// directory to DirMask. Only ACEs that are effective on the object count - an
// INHERIT_ONLY_ACE grants nothing to the object it is attached to, it only
// exists so descendants can inherit it - so the masks are not simply ORed
// together. A protected root additionally carries the object (file) ACE as an
// effective ACE, which is why a directory's effective rights are checked as
// "everything DirMask requires, and nothing beyond DirMask|FileMask" rather
// than as an exact equality that would fail on the root.
func Violations(t testing.TB, path string) []string {
	t.Helper()

	principals, err := secfile.ApprovedPrincipals()
	if err != nil {
		t.Fatalf("resolve approved principals: %v", err)
	}
	if len(principals) < 3 {
		t.Fatalf("approved principal list has %d entries, want the user, SYSTEM and Administrators", len(principals))
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	isDir := info.IsDir()
	// The current user is resolved from the process token, not from a position
	// in the list, so a reordering of the policy cannot make the user's
	// assertion silently check SYSTEM instead.
	userSID, err := secfile.CurrentUserSID()
	if err != nil {
		t.Fatalf("resolve current user SID: %v", err)
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("create Local SYSTEM SID: %v", err)
	}
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf(`create BUILTIN\Administrators SID: %v`, err)
	}

	dacl := readDACL(t, path)
	if dacl == nil {
		// A null DACL grants every account full control; it is strictly worse
		// than an empty one, which at least denies everyone.
		return []string{"DACL is NULL: unrestricted access for every account"}
	}
	if dacl.AceCount == 0 {
		return []string{"DACL is empty: denies access to every account, including the application"}
	}

	var violations []string
	// granted accumulates the rights that are effective on this object per
	// SID, so a principal's rights are only ever measured from its own ACEs.
	// Inherit-only ACEs are excluded: they do not apply to this object.
	granted := make(map[string]windows.ACCESS_MASK)
	inheritOnly := make(map[string]windows.ACCESS_MASK)

	for i := uint16(0); i < dacl.AceCount; i++ {
		ace, sid, ok := readACE(t, dacl, path, i)
		if !ok {
			violations = append(violations, fmt.Sprintf(
				"ACE %d has a type this policy never creates", i))
			continue
		}
		mask := windows.ACCESS_MASK(ace.Mask)

		if bad := mask & forbiddenRights; bad != 0 {
			violations = append(violations, fmt.Sprintf(
				"ACE %d for %s grants forbidden rights %#x (WRITE_DAC, WRITE_OWNER or ACCESS_SYSTEM_SECURITY)",
				i, sid.String(), uint32(bad)))
		}
		if !isDir && mask&FileExecute != 0 {
			violations = append(violations, fmt.Sprintf(
				"ACE %d for %s grants FILE_EXECUTE (%#x) on a file", i, sid.String(), uint32(FileExecute)))
		}
		if !isDir && mask&rightDeleteChild != 0 {
			violations = append(violations, fmt.Sprintf(
				"ACE %d for %s grants FILE_DELETE_CHILD (%#x) on a file, where it is meaningless",
				i, sid.String(), uint32(rightDeleteChild)))
		}
		if !approvedSID(principals, sid) {
			origin := "explicit"
			if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
				origin = "inherited"
			}
			violations = append(violations, fmt.Sprintf(
				"ACE %d grants mask %#x to unapproved principal %s (%s ACE)",
				i, uint32(mask), sid.String(), origin))
			continue
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			inheritOnly[sid.String()] |= mask
			continue
		}
		granted[sid.String()] |= mask
	}

	// Exact comparison for files: catches an over-granted principal, a
	// principal the policy intended to grant but omitted, and any directory
	// rights that leaked onto a file.
	//
	// A directory is compared as a range instead, because the protected root's
	// effective rights legitimately include the file ACE while a subdirectory's
	// do not:
	//
	//   - every right DirMask requires must be present (a lost FILE_READ_EA
	//     breaks os.ReadDir just as surely as a lost FILE_TRAVERSE), and
	//   - nothing beyond DirMask|FileMask may be effective (an inherited
	//     WRITE_DAC or FILE_EXECUTE from a wider parent is reported here).
	for _, p := range principals {
		have, ok := granted[p.SID.String()]
		if !ok {
			violations = append(violations, fmt.Sprintf("%s holds no effective ACE on this path", p.Name))
			continue
		}
		if !isDir {
			// A file is held to the file mask exactly: it must never pick up
			// the directory mask's FILE_TRAVERSE-as-FILE_EXECUTE or
			// FILE_DELETE_CHILD.
			expect := p.FileMask
			if have != expect {
				violations = append(violations, fmt.Sprintf(
					"%s has mask %#x, want exactly %#x (missing %#x, excess %#x)",
					p.Name, uint32(have), uint32(expect), uint32(expect&^have), uint32(have&^expect)))
			}
			continue
		}
		if missing := p.DirMask &^ have; missing != 0 {
			violations = append(violations, fmt.Sprintf(
				"%s is missing directory rights %#x", p.Name, uint32(missing)))
		}
		allowed := p.DirMask | p.FileMask
		if excess := have &^ allowed; excess != 0 {
			violations = append(violations, fmt.Sprintf(
				"%s has effective mask %#x, which exceeds the directory and file masks by %#x",
				p.Name, uint32(have), uint32(excess)))
		}
		if have&FileExecute != 0 && p.DirMask&FileExecute == 0 {
			violations = append(violations, fmt.Sprintf(
				"%s has FILE_TRAVERSE on a directory it should only read", p.Name))
		}
		// The file policy is the only thing that may be inherit-only on a
		// directory. Anything else carried for descendants and not in effect
		// here would be an unreviewed grant one level down.
		if bad := inheritOnly[p.SID.String()] &^ p.FileMask; bad != 0 {
			violations = append(violations, fmt.Sprintf(
				"%s has an inherit-only mask %#x that exceeds the file mask", p.Name, uint32(bad)))
		}
	}
	// Independent minimum: each principal is checked on its own SID, so an
	// Administrators or SYSTEM ACE can never satisfy the current-user test.
	checks := []struct {
		name string
		sid  *windows.SID
		need windows.ACCESS_MASK
	}{
		{"current process user " + userSID.String(), userSID, needUserDir},
		{"Local SYSTEM", systemSID, needSystemDir},
		{`BUILTIN\Administrators`, adminsSID, needAdminDir},
	}
	if !isDir {
		checks = []struct {
			name string
			sid  *windows.SID
			need windows.ACCESS_MASK
		}{
			{"current process user " + userSID.String(), userSID, needUserFile},
			{"Local SYSTEM", systemSID, needSystemFile},
			{`BUILTIN\Administrators`, adminsSID, needAdminFile},
		}
	}
	for _, c := range checks {
		if missing := c.need &^ granted[c.sid.String()]; missing != 0 {
			violations = append(violations, fmt.Sprintf(
				"%s is missing required rights %#x", c.name, uint32(missing)))
		}
	}

	sort.Strings(violations)
	return violations
}

// RequirePrivate fails the test unless every allow ACE on path names an
// approved principal with exactly the rights the policy grants that object
// type, and each principal is measured from its own ACE.
//
// Inherited ACEs are inspected exactly like explicit ones: an ACE inherited
// from a wider parent grants the same access as an explicit one, so skipping
// them is what lets a "private" directory stay readable by BUILTIN\Users.
func RequirePrivate(t testing.TB, path string) {
	t.Helper()
	if violations := Violations(t, path); len(violations) > 0 {
		t.Fatalf("%s is not private:\n  %s", path, strings.Join(violations, "\n  "))
	}
}

// RequireProtectedRoot additionally proves that dir's DACL is protected from
// its parent, that each principal has one ACE granting the directory mask to
// sub-containers, and one granting the file mask to objects.
//
// Asserting the two ACE kinds separately is what proves the inheritance design:
// child directories cannot lose FILE_TRAVERSE, child files cannot gain
// FILE_EXECUTE, and no file can pick up FILE_DELETE_CHILD.
//
// Both ACEs must also be effective on the root itself, so neither may carry
// INHERIT_ONLY_ACE. The mask of an inheritable ACE applies to the object it is
// set on; for the file ACE that is intended, because the root is where the
// policy starts and the FILE_WRITE_EA it adds is inert on a directory.
func RequireProtectedRoot(t testing.TB, dir string) {
	t.Helper()
	RequirePrivate(t, dir)

	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read security descriptor of %s: %v", dir, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("read security descriptor control of %s: %v", dir, err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("%s has an unprotected DACL: ACEs inherited from its parent are still in effect", dir)
	}

	dacl := readDACL(t, dir)
	if dacl == nil {
		t.Fatalf("%s has a NULL DACL", dir)
	}

	principals, err := secfile.ApprovedPrincipals()
	if err != nil {
		t.Fatalf("resolve approved principals: %v", err)
	}

	for _, p := range principals {
		var container, object bool
		for i := uint16(0); i < dacl.AceCount; i++ {
			ace, sid, ok := readACE(t, dacl, dir, i)
			if !ok || !p.SID.Equals(sid) {
				continue
			}
			if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
				continue
			}
			flags := ace.Header.AceFlags
			mask := windows.ACCESS_MASK(ace.Mask)
			if flags&windows.INHERIT_ONLY_ACE != 0 {
				t.Fatalf("%s: explicit ACE for %s has mask %#x and is inherit-only, so the root itself is not protected by it",
					dir, p.Name, uint32(mask))
			}
			switch {
			case flags&windows.OBJECT_INHERIT_ACE != 0 && flags&windows.CONTAINER_INHERIT_ACE == 0:
				// This ACE reaches files only, so it must be the file policy.
				if mask != p.FileMask {
					t.Fatalf("%s: ACE for %s inherited by objects has mask %#x, want file mask %#x",
						dir, p.Name, uint32(mask), uint32(p.FileMask))
				}
				if mask&FileExecute != 0 || mask&rightDeleteChild != 0 {
					t.Fatalf("%s: ACE for %s inherited by files carries FILE_EXECUTE or FILE_DELETE_CHILD (%#x)",
						dir, p.Name, uint32(mask))
				}
				object = true
			case flags&windows.CONTAINER_INHERIT_ACE != 0 && flags&windows.OBJECT_INHERIT_ACE == 0:
				// This ACE reaches subdirectories only, so it must be the
				// directory policy.
				if mask != p.DirMask {
					t.Fatalf("%s: ACE for %s inherited by containers has mask %#x, want directory mask %#x",
						dir, p.Name, uint32(mask), uint32(p.DirMask))
				}
				container = true
			default:
				t.Fatalf("%s: ACE for %s has inheritance flags %#x; want exactly one of container-only or object-only",
					dir, p.Name, flags)
			}
		}
		if !container || !object {
			missing := "container"
			if !object {
				missing = "object"
			}
			if !container && !object {
				missing = "container and object"
			}
			t.Fatalf("%s: %s has no %s ACE", dir, p.Name, missing)
		}
	}
}

// RequireInheritedContainer proves a directory created inside a protected root
// carries the policy in the exact shape inheritance is supposed to produce:
//
//   - the directory rights are effective on it, inherited (not re-applied by a
//     per-directory ACL call, which is what the earlier fail-open design did);
//   - the file rights are present but inherit-only, so they reach the files
//     below without granting this directory anything a directory cannot use;
//   - nothing else is effective, in particular no FILE_EXECUTE and no
//     FILE_WRITE_EA from the object ACE.
//
// This is the distinction a plain union of every ACE mask cannot make: the
// inherit-only file ACE is deliberately part of this directory's DACL while
// contributing nothing to its effective rights.
func RequireInheritedContainer(t testing.TB, dir string) {
	t.Helper()
	RequirePrivate(t, dir)

	dacl := readDACL(t, dir)
	if dacl == nil {
		t.Fatalf("%s has a NULL DACL", dir)
	}
	principals, err := secfile.ApprovedPrincipals()
	if err != nil {
		t.Fatalf("resolve approved principals: %v", err)
	}

	for _, p := range principals {
		var effective, inheritOnly windows.ACCESS_MASK
		var sawObject bool
		for i := uint16(0); i < dacl.AceCount; i++ {
			ace, sid, ok := readACE(t, dacl, dir, i)
			if !ok || !p.SID.Equals(sid) {
				continue
			}
			if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
				t.Fatalf("%s: ACE for %s is explicit; a child directory must inherit its policy from the protected root",
					dir, p.Name)
			}
			flags := ace.Header.AceFlags
			mask := windows.ACCESS_MASK(ace.Mask)
			if flags&windows.INHERIT_ONLY_ACE != 0 {
				inheritOnly |= mask
				if flags&windows.OBJECT_INHERIT_ACE != 0 {
					sawObject = true
					if mask != p.FileMask {
						t.Fatalf("%s: inherit-only ACE for %s has mask %#x, want file mask %#x",
							dir, p.Name, uint32(mask), uint32(p.FileMask))
					}
				}
				continue
			}
			effective |= mask
		}

		if effective != p.DirMask {
			t.Fatalf("%s: %s has effective mask %#x, want exactly the directory mask %#x (missing %#x, excess %#x)",
				dir, p.Name, uint32(effective), uint32(p.DirMask),
				uint32(p.DirMask&^effective), uint32(effective&^p.DirMask))
		}
		if !sawObject {
			t.Fatalf("%s: %s has no inherit-only file ACE, so files created below it would inherit no file policy",
				dir, p.Name)
		}
		if excess := inheritOnly &^ p.FileMask; excess != 0 {
			t.Fatalf("%s: %s has an inherit-only mask %#x beyond the file mask", dir, p.Name, uint32(excess))
		}
	}
}

// EffectiveMask reports the union of the rights path effectively grants sid,
// including inherited ACEs but excluding inherit-only ones, which grant
// nothing to the object carrying them. It is used to assert a specific bit
// rather than a whole mask.
func EffectiveMask(t testing.TB, path string, sid *windows.SID) windows.ACCESS_MASK {
	t.Helper()
	dacl := readDACL(t, path)
	if dacl == nil {
		t.Fatalf("%s has a NULL DACL", path)
	}
	var mask windows.ACCESS_MASK
	for i := uint16(0); i < dacl.AceCount; i++ {
		ace, aceSID, ok := readACE(t, dacl, path, i)
		if !ok || !sid.Equals(aceSID) {
			continue
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		mask |= windows.ACCESS_MASK(ace.Mask)
	}
	return mask
}

// GrantEveryone writes a broad Everyone allow ACE onto path. Regression tests
// use it to reproduce a directory created before hardening existed, or one
// sitting under a wide parent such as a shared drive.
func GrantEveryone(t testing.TB, path string) {
	t.Helper()
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("create Everyone SID: %v", err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.ACCESS_MASK(0x1F01FF),
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(world),
		},
	}}, nil)
	if err != nil {
		t.Fatalf("build Everyone DACL for %s: %v", path, err)
	}
	// Deliberately unprotected: this reproduces a directory whose DACL still
	// carries inherited broad access, which is the state being defended
	// against.
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatalf("grant Everyone access to %s: %v", path, err)
	}
}

// readDACL returns the DACL of path, or nil when the descriptor has none.
func readDACL(t testing.TB, path string) *windows.ACL {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read security descriptor of %s: %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("read DACL of %s: %v", path, err)
	}
	return dacl
}

// readACE decodes one allow ACE. ok is false for any other ACE type, which this
// policy never produces and which must therefore be reviewed rather than
// decoded by guesswork.
func readACE(t testing.TB, dacl *windows.ACL, path string, i uint16) (*windows.ACCESS_ALLOWED_ACE, *windows.SID, bool) {
	t.Helper()
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
		t.Fatalf("%s: read ACE %d: %v", path, i, err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return ace, nil, false
	}
	// ACCESS_ALLOWED_ACE lays the SID out immediately after Mask + SidStart,
	// which is the documented layout the Windows API itself relies on.
	return ace, (*windows.SID)(unsafe.Pointer(&ace.SidStart)), true
}

func approvedSID(principals []secfile.Principal, sid *windows.SID) bool {
	for _, p := range principals {
		if p.SID.Equals(sid) {
			return true
		}
	}
	return false
}

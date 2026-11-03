//go:build windows

package secfile

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// Access rights this package grants, written as explicit masks rather than
// GENERIC_READ or GENERIC_ALL so each ACE states the rights it actually means
// instead of depending on how the generic mapping is resolved at the moment
// the DACL is set.
//
// The same bit positions are type-specific aliases. That shared meaning is why
// the directory and file policies below are separate masks: on a directory
// 0x20 is FILE_TRAVERSE, but on a file the very same bit is FILE_EXECUTE, so a
// single mask reused for both would grant execution on every received file.
//
//	0x00000001  FILE_READ_DATA        / FILE_LIST_DIRECTORY
//	0x00000002  FILE_WRITE_DATA       / FILE_ADD_FILE
//	0x00000004  FILE_APPEND_DATA      / FILE_ADD_SUBDIRECTORY
//	0x00000008  FILE_READ_EA
//	0x00000010  FILE_WRITE_EA
//	0x00000020  FILE_EXECUTE          / FILE_TRAVERSE
//	0x00000040  FILE_DELETE_CHILD      directories only; reserved on files
//	0x00000080  FILE_READ_ATTRIBUTES
//	0x00000100  FILE_WRITE_ATTRIBUTES
//	0x00010000  DELETE
//	0x00020000  READ_CONTROL
//	0x00100000  SYNCHRONIZE
//
// FILE_READ_EA and FILE_WRITE_EA are not decoration. Go does not open files
// with these specific rights; it opens them with the *generic* rights, and the
// kernel expands a generic right to a fixed set before the access check runs.
// From syscall.Open in the Go version this repository builds with:
//
//	O_RDONLY  -> GENERIC_READ
//	O_WRONLY  -> GENERIC_WRITE
//	O_RDWR    -> GENERIC_READ|GENERIC_WRITE
//	O_CREATE  -> additionally GENERIC_WRITE
//	O_APPEND  -> drops GENERIC_WRITE (unless O_TRUNC) and adds
//	             FILE_APPEND_DATA|FILE_WRITE_ATTRIBUTES|FILE_WRITE_EA|
//	             STANDARD_RIGHTS_WRITE|SYNCHRONIZE
//
// and the generic file mappings are
//
//	FILE_GENERIC_READ  = STANDARD_RIGHTS_READ|FILE_READ_DATA|
//	                     FILE_READ_ATTRIBUTES|FILE_READ_EA|SYNCHRONIZE
//	FILE_GENERIC_WRITE = STANDARD_RIGHTS_WRITE|FILE_WRITE_DATA|
//	                     FILE_WRITE_ATTRIBUTES|FILE_WRITE_EA|
//	                     FILE_APPEND_DATA|SYNCHRONIZE
//
// An access check requires *every* specific right a generic right expands to.
// So a DACL that omits FILE_READ_EA fails os.Open with ACCESS_DENIED even
// though nothing in Flitnix reads extended attributes, and one that omits
// FILE_WRITE_EA fails os.OpenFile with O_WRONLY, O_RDWR or O_APPEND - which is
// every write path this application has, including the recovery journal
// (created O_RDWR|O_APPEND), the installation key and every part file.
// STANDARD_RIGHTS_READ and STANDARD_RIGHTS_WRITE are both READ_CONTROL, which
// is already granted.
//
// Go's second open path, the openat used by os.Root and directory-relative
// opens, is stricter still: it appends STANDARD_RIGHTS_READ|FILE_READ_ATTRIBUTES|
// FILE_READ_EA to every single open, whatever the flags. FILE_READ_EA is
// therefore required on every object type this policy protects.
//
// Not granted anywhere:
//
//	0x00040000  WRITE_DAC              ACL management. The object's owner holds
//	                                   WRITE_DAC implicitly, so re-protecting a
//	                                   directory this process created keeps
//	                                   working without granting it.
//	0x00080000  WRITE_OWNER            Ownership transfer; would also let the
//	                                   holder rewrite the DACL via ownership.
//	0x01000000  ACCESS_SYSTEM_SECURITY SACL access; never needed.
//
// Note that 0x1F01FF, conventionally called FILE_ALL_ACCESS, is
// STANDARD_RIGHTS_REQUIRED|SYNCHRONIZE|0x1FF and therefore contains WRITE_DAC
// and WRITE_OWNER. That is why no mask below is written as FILE_ALL_ACCESS.
const (
	rightRead            = 0x00000001
	rightWrite           = 0x00000002
	rightAppend          = 0x00000004
	rightReadEA          = 0x00000008 // required by GENERIC_READ
	rightWriteEA         = 0x00000010 // required by GENERIC_WRITE and O_APPEND
	rightTraverse        = 0x00000020 // FILE_EXECUTE on a file
	rightDeleteChild     = 0x00000040 // directories only
	rightReadAttributes  = 0x00000080
	rightWriteAttributes = 0x00000100 // required by GENERIC_WRITE and O_APPEND
	rightDelete          = 0x00010000
	rightReadControl     = 0x00020000
	rightSynchronize     = 0x00100000

	// DirOperate is what running the application requires of a directory: list
	// it, add files and subdirectories, traverse into it, delete entries
	// beneath it, clear the read-only attribute before a delete, remove it,
	// read its ACL and wait on handles. FILE_TRAVERSE (0x20) is required here
	// and means traversal, not execution.
	//
	// FILE_READ_EA is required because os.ReadDir and os.Open on a directory
	// request GENERIC_READ.
	//
	// FILE_WRITE_EA is deliberately absent. A generic write is the only thing
	// that demands it, and nothing in this repository opens a directory for
	// writing: there is no os.Truncate, os.Chtimes or explicit os.OpenFile on a
	// directory anywhere, creation goes through CreateDirectory, deletion
	// through delete-on-dispose, and every byte is written to a file inside
	// the directory. Granting 0x10 here would be an unnecessary right on every
	// subdirectory the policy inherits into.
	DirOperate windows.ACCESS_MASK = rightRead | rightWrite | rightAppend |
		rightReadEA | rightTraverse | rightDeleteChild | rightReadAttributes |
		rightWriteAttributes | rightDelete | rightReadControl | rightSynchronize // 0x001301EF

	// FileOperate is what running the application requires of a received file:
	// read it, write and append to it, read and adjust attributes, delete and
	// rename it, read its ACL and wait on handles.
	//
	// FILE_READ_EA and FILE_WRITE_EA are both required by the generic mappings
	// above, not by any Flitnix call that mentions extended attributes.
	// FILE_WRITE_ATTRIBUTES is required by GENERIC_WRITE and by the explicit
	// O_APPEND set, and is also what lets a delete clear FILE_ATTRIBUTE_READONLY
	// first.
	//
	// FILE_EXECUTE is deliberately absent. Flitnix receives documents, media
	// and archives; nothing in the application launches a received file, and
	// "receiving a file must never make it runnable" is the safer default. The
	// 0x20 bit is therefore not granted on files, and FILE_DELETE_CHILD (0x40)
	// is meaningless on a file and is not granted either.
	FileOperate windows.ACCESS_MASK = rightRead | rightWrite | rightAppend |
		rightReadEA | rightWriteEA | rightReadAttributes | rightWriteAttributes |
		rightDelete | rightReadControl | rightSynchronize // 0x0013019F

	// DirAdminister is what backup, support and repair require of a directory:
	// list and traverse it, read attributes, remove a corrupt entry, read the
	// ACL and wait. No write, append or execute right is included.
	//
	// FILE_READ_EA is present so an administrator can still enumerate and read
	// the tree through the same generic-read path the application uses, which
	// includes inspecting what the application stored. FILE_WRITE_EA is not:
	// administration never writes application state.
	DirAdminister windows.ACCESS_MASK = rightRead | rightReadEA | rightTraverse |
		rightReadAttributes | rightDelete | rightReadControl | rightSynchronize // 0x001300A9

	// FileAdminister is the same intent for a file. FILE_EXECUTE (0x20) is
	// absent: administration never launches received data.
	FileAdminister windows.ACCESS_MASK = rightRead | rightReadEA |
		rightReadAttributes | rightDelete | rightReadControl | rightSynchronize // 0x00130089
)

// Principal is one account the application intentionally permits to hold
// access to protected application state.
type Principal struct {
	// Name is the human-readable principal, used for diagnostics.
	Name string
	// SID is the security identifier the DACL is built from.
	SID *windows.SID
	// DirMask is granted on a directory and inherited by subdirectories.
	DirMask windows.ACCESS_MASK
	// FileMask is granted on a file and inherited by child files.
	FileMask windows.ACCESS_MASK
}

// ApprovedPrincipals returns the only principals permitted to access
// protected application state, in DACL order.
//
//   - The account the process runs as. Resolved from the live process token
//     rather than a hardcoded account or profile name, so renamed accounts,
//     domain accounts and non-default profile locations all match.
//   - Local SYSTEM: backup reads exactly what the user reads, and restore must
//     reproduce exactly what the user can write and delete, so its masks are
//     deliberately equal to the user's.
//   - BUILTIN\Administrators: inspection, backup and repair only. Writing,
//     renaming or publishing application state is not an administrative
//     operation.
//
// No other principal is justified. Authenticated Users, Everyone,
// BUILTIN\Users and INTERACTIVE are deliberately absent: every interactive
// logon is a member of the first three, which would make the policy
// meaningless on a multi-user machine.
func ApprovedPrincipals() ([]Principal, error) {
	user, err := CurrentUserSID()
	if err != nil {
		return nil, fmt.Errorf("resolve current user SID: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("resolve Local SYSTEM SID: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf(`resolve BUILTIN\Administrators SID: %w`, err)
	}
	return []Principal{
		{
			Name:     "current user " + user.String(),
			SID:      user,
			DirMask:  DirOperate,
			FileMask: FileOperate,
		},
		{
			Name:     "Local SYSTEM",
			SID:      system,
			DirMask:  DirOperate,
			FileMask: FileOperate,
		},
		{
			Name:     `BUILTIN\Administrators`,
			SID:      admins,
			DirMask:  DirAdminister,
			FileMask: FileAdminister,
		},
	}, nil
}

// hookMu guards protectHook. Production never sets it; tests do, to prove a
// failure to apply the policy is propagated instead of silently ignored.
var (
	hookMu      sync.RWMutex
	protectHook func(string) error
)

// SetProtectHook installs a fault-injection hook consulted before the policy
// is applied to a path, mirroring the fsHooks pattern used by the transfer
// package. Passing nil removes it. Tests only.
func SetProtectHook(fn func(string) error) {
	hookMu.Lock()
	protectHook = fn
	hookMu.Unlock()
}

func hook() func(string) error {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return protectHook
}

// ProtectDir replaces dir's DACL with the protected policy.
//
// Two ACEs are written per principal, because one mask cannot serve both object
// types safely:
//
//   - DirMask with CONTAINER_INHERIT_ACE only, so subdirectories inherit the
//     directory rights and child files do not.
//   - FileMask with OBJECT_INHERIT_ACE only, so child files inherit the file
//     rights and no file ever receives FILE_TRAVERSE-as-FILE_EXECUTE or the
//     directory-only FILE_DELETE_CHILD.
//
// The object ACE does not need INHERIT_ONLY_ACE. Per the documented ACE
// inheritance rules, an OBJECT_INHERIT_ACE-only ACE reaches container children
// as an inherit-only ACE and non-container children as an effective one. So the
// system already produces exactly the intended shape one level down: the file
// ACE is carried by child directories without being effective there, and lands
// effective on child files. Adding INHERIT_ONLY_ACE here would grant nothing
// extra; it would only make the correct behaviour of the root ACE depend on
// whether the flag is cleared on propagation.
//
// The mask of an inheritable ACE also applies to the object it is set on, so
// the protected directory itself ends up with DirMask|FileMask. That is
// 0x001301FF: the directory rights plus FILE_WRITE_EA from the file ACE. The
// extra bit is harmless on a directory - it cannot be used to open one for
// writing, which Windows refuses for directories - and must not be granted by
// DirOperate, which every subdirectory inherits as an effective ACE.
//
// Effective rights by level, for one principal:
//
//	protected root      DirMask|FileMask  (both ACEs are effective on it)
//	child directory     DirMask          (file ACE inherited inherit-only)
//	grandchild          DirMask          (same, inherited again)
//	child file          FileMask         (dir ACE has no OBJECT_INHERIT)
//	explicitly secured  FileMask         (ProtectFile, no inheritance)
//
// Only call this for a directory Flitnix owns outright: its own application
// state, or a directory Flitnix itself created. Applying it to a directory the
// user already had discards explicit and inherited access other users and
// programs were deliberately granted.
func ProtectDir(dir string) error {
	principals, err := ApprovedPrincipals()
	if err != nil {
		return fmt.Errorf("secfile: %w", err)
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, 2*len(principals))
	for _, p := range principals {
		// Container ACE: reaches subdirectories, never files.
		entries = append(entries,
			grantEntry(p, p.DirMask, windows.CONTAINER_INHERIT_ACE),
		)
		// Object ACE: reaches files, and containers only as inherit-only,
		// which is what carries the file policy further down the tree.
		entries = append(entries,
			grantEntry(p, p.FileMask, windows.OBJECT_INHERIT_ACE),
		)
	}
	return applyACL(dir, entries)
}

// ProtectFile applies the file policy to a single file.
//
// The DACL is protected, so the file does not inherit from the directory it
// lands in. That is what keeps received bytes private inside a directory whose
// own ACL Flitnix leaves alone, and it survives being renamed into that
// directory: the security descriptor travels with the file, so publication
// stays atomic and no ACL work happens per chunk.
func ProtectFile(path string) error {
	principals, err := ApprovedPrincipals()
	if err != nil {
		return fmt.Errorf("secfile: %w", err)
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(principals))
	for _, p := range principals {
		entries = append(entries, grantEntry(p, p.FileMask, windows.NO_INHERITANCE))
	}
	return applyACL(path, entries)
}

func grantEntry(p Principal, mask windows.ACCESS_MASK, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: mask,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			// Identified by SID rather than by name, so a localized account
			// name can never resolve to the wrong principal.
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(p.SID),
		},
	}
}

func applyACL(path string, entries []windows.EXPLICIT_ACCESS) error {
	if fn := hook(); fn != nil {
		if err := fn(path); err != nil {
			return err
		}
	}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("secfile: build DACL for %s: %w", path, err)
	}

	// PROTECTED_DACL_SECURITY_INFORMATION discards every ACE inherited from
	// the parent. Without it the broad parent ACEs survive beside ours and
	// grant exactly the access this policy exists to deny.
	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	)
	if err != nil {
		return fmt.Errorf("secfile: apply DACL to %s: %w", path, err)
	}
	return nil
}

// CurrentUserSID returns the SID of the account the process runs as.
func CurrentUserSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()

	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user.User.Sid == nil {
		return nil, errors.New("process token carries no user SID")
	}
	// Copy onto the Go heap: the token buffer is released with the token.
	return user.User.Sid.Copy()
}

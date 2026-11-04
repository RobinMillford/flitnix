//go:build !windows

package secfile

// ProtectDir is a no-op off Windows.
//
// The 0700 mode the callers pass to os.MkdirAll is enforced by the kernel here
// and already restricts the directory and everything created inside it to the
// owning user, so there is no inherited ACL to correct and no reason to touch
// a directory the user already had.
func ProtectDir(string) error { return nil }

// ProtectFile is a no-op off Windows; the 0600 mode on the file is enforced by
// the kernel.
func ProtectFile(string) error { return nil }

// SetProtectHook is a no-op off Windows: no policy is applied, so nothing
// consults a hook.
func SetProtectHook(func(string) error) {}

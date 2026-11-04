// Package secfile applies the platform confidentiality policy to the
// application state Flitnix creates: the recovery directory, the resumable
// staging directory, and the individual files that arrive through a transfer.
//
// Callers already pass 0600 and 0700 to os.MkdirAll and os.OpenFile. On the
// Unix family those modes are enforced by the kernel and express the policy
// completely. Windows ignores them: a new file or directory simply inherits
// the DACL of its parent. On a shared, public or redirected path that
// inherited DACL can grant Authenticated Users or Everyone read access to the
// installation key, the journal, or partially received bytes.
//
// Two operations exist because two very different kinds of path are involved.
//
// ProtectDir replaces a directory's DACL with the explicit policy. It is only
// correct for a directory Flitnix owns outright: its own application state, or
// a directory Flitnix created inside the user's chosen receive root. It must
// never be applied to a directory the user already had - a Downloads folder, a
// documents folder, a shared drive, or a per-transfer destination - because
// replacing that DACL silently revokes access other users and other programs
// were deliberately granted.
//
// ProtectFile applies the same policy to a single file, with the DACL marked
// protected so that moving the file into a directory Flitnix does not own
// cannot cause it to inherit wider access from that directory. This is how
// received bytes stay private inside a directory whose ACL is left alone.
//
// Off Windows both are no-ops and the mode bits remain the whole mechanism, so
// call sites read identically on every platform.
package secfile

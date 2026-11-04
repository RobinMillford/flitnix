//go:build !windows

// Package securetest holds the Windows access-control assertions shared by the
// recovery and transfer permission regression tests.
//
// Off Windows the assertions are unreachable: the Unix tests check 0600 and
// 0700 modes directly, because those are what the kernel enforces there. The
// stubs keep `go build ./...` and `go vet ./...` clean on every platform.
package securetest

import "testing"

// RequirePrivate is a no-op off Windows.
func RequirePrivate(testing.TB, string) {}

// RequireProtectedRoot is a no-op off Windows.
func RequireProtectedRoot(testing.TB, string) {}

// RequireInheritedContainer is a no-op off Windows.
func RequireInheritedContainer(testing.TB, string) {}

// Violations reports nothing off Windows.
func Violations(testing.TB, string) []string { return nil }

// GrantEveryone is a no-op off Windows.
func GrantEveryone(testing.TB, string) {}

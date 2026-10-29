package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	// ErrUnsupportedScheme indicates a URI scheme other than file:// was provided.
	ErrUnsupportedScheme = errors.New("unsupported URI scheme (only file:// is supported)")

	// ErrRemoteAuthority indicates a file:// URI contains a remote host rather than localhost or empty.
	ErrRemoteAuthority = errors.New("remote URI authorities are not supported")

	// ErrEmptyPath indicates that an empty path was provided.
	ErrEmptyPath = errors.New("path is empty")

	// ErrSpecialFile indicates the path exists but is not a regular file or directory.
	ErrSpecialFile = errors.New("path is not a regular file or directory")
)

// NormalizePathArgument takes a raw command-line or URI argument and resolves it to a
// canonical, absolute filesystem path.
//
// Relative paths are resolved against workingDir (or the current working directory if workingDir is empty).
// file:// URIs are parsed, percent-decoded, and validated to ensure no remote authority is accessed.
// Top-level symlinks are resolved to their target using filepath.EvalSymlinks.
// The resolved path must exist on disk and point to a regular file or directory.
func NormalizePathArgument(raw string, workingDir string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrEmptyPath
	}

	cleanPath := raw

	// Handle URI schemes (e.g. file:///home/user/doc.pdf or file://localhost/...)
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("malformed URI %q: %w", raw, err)
		}
		if !strings.EqualFold(u.Scheme, "file") {
			return "", fmt.Errorf("%w: %q", ErrUnsupportedScheme, u.Scheme)
		}

		path := u.Path
		// On Windows, url.Parse can set Host to drive letter (e.g. file://C:/path)
		if runtime.GOOS == "windows" && len(u.Host) == 2 && u.Host[1] == ':' {
			path = u.Host + u.Path
		} else if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			return "", fmt.Errorf("%w: %q", ErrRemoteAuthority, u.Host)
		}

		// On Windows, url.Path for file:///C:/foo is /C:/foo; strip leading slash.
		if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		cleanPath = filepath.FromSlash(path)
	}

	// Resolve relative path against workingDir
	if !filepath.IsAbs(cleanPath) {
		if workingDir != "" {
			cleanPath = filepath.Join(workingDir, cleanPath)
		} else if cwd, err := os.Getwd(); err == nil {
			cleanPath = filepath.Join(cwd, cleanPath)
		}
	}

	absPath, err := filepath.Abs(cleanPath)
	if err != nil {
		return "", fmt.Errorf("cannot resolve absolute path for %q: %w", raw, err)
	}

	// Resolve top-level symlinks according to policy
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("cannot access path %q: %w", raw, err)
	}

	// Verify target exists and is readable
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return "", fmt.Errorf("cannot stat path %q: %w", resolvedPath, err)
	}

	// Only accept regular files and directories (no named pipes, sockets, devices)
	if !info.Mode().IsDir() && !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %q", ErrSpecialFile, resolvedPath)
	}

	return resolvedPath, nil
}

// ExtractSharePaths parses a list of command-line arguments against a working directory,
// filtering out flags and subcommands, normalizing paths, and deduplicating results.
// Diagnostic errors for invalid paths are passed to onDiagnostic if non-nil.
func ExtractSharePaths(args []string, workingDir string, onDiagnostic func(raw string, err error)) []string {
	var results []string
	seen := make(map[string]bool)

	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}

		// Filter out known subcommands
		switch arg {
		case "send", "serve", "doctor", "version", "help":
			continue
		}

		normPath, err := NormalizePathArgument(arg, workingDir)
		if err != nil {
			if onDiagnostic != nil {
				onDiagnostic(arg, err)
			}
			continue
		}

		if !seen[normPath] {
			seen[normPath] = true
			results = append(results, normPath)
		}
	}

	return results
}

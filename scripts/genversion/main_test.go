package main

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "plain", input: "0.2.7.4", want: "0.2.7.4", ok: true},
		{name: "tagged", input: "v0.2.7.4", want: "0.2.7.4", ok: true},
		{name: "whitespace", input: " v1.2.3\n", want: "1.2.3", ok: true},
		{name: "prerelease", input: "v1.2.3-rc.1", want: "1.2.3-rc.1", ok: true},
		{name: "branch ref", input: "refs/heads/main", ok: false},
		{name: "branch name", input: "main", ok: false},
		{name: "empty", input: "", ok: false},
		{name: "partial", input: "1", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalize(tc.input)
			if ok != tc.ok {
				t.Fatalf("normalize(%q) ok = %v, want %v", tc.input, ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("normalize(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestResolveVersionExplicitWins(t *testing.T) {
	t.Setenv("FLITNIX_VERSION", "v9.9.9")
	t.Setenv("GITHUB_REF_NAME", "v1.1.1")

	got, err := resolveVersion(t.TempDir())
	if err != nil {
		t.Fatalf("resolveVersion: %v", err)
	}
	if got != "9.9.9" {
		t.Fatalf("resolveVersion = %q, want %q", got, "9.9.9")
	}
}

func TestResolveVersionRejectsInvalidExplicit(t *testing.T) {
	t.Setenv("FLITNIX_VERSION", "not-a-version")
	t.Setenv("GITHUB_REF_NAME", "")

	if _, err := resolveVersion(t.TempDir()); err == nil {
		t.Fatal("expected an error for an invalid FLITNIX_VERSION")
	}
}

func TestResolveVersionFromGitHubRef(t *testing.T) {
	t.Setenv("FLITNIX_VERSION", "")
	t.Setenv("GITHUB_REF_NAME", "v2.0.0")

	got, err := resolveVersion(t.TempDir())
	if err != nil {
		t.Fatalf("resolveVersion: %v", err)
	}
	if got != "2.0.0" {
		t.Fatalf("resolveVersion = %q, want %q", got, "2.0.0")
	}
}

func TestResolveVersionIgnoresBranchRef(t *testing.T) {
	t.Setenv("FLITNIX_VERSION", "")
	t.Setenv("GITHUB_REF_NAME", "refs/heads/main")

	got, err := resolveVersion(t.TempDir())
	if err != nil {
		t.Fatalf("resolveVersion: %v", err)
	}
	if got != "" {
		t.Fatalf("resolveVersion = %q, want empty outside a tag", got)
	}
}

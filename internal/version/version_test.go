package version

import (
	"testing"
)

func TestParseSemVer(t *testing.T) {
	tests := []struct {
		input   string
		want    SemVer
		wantErr bool
	}{
		{"0.2.7.1", SemVer{0, 2, 7, 1, ""}, false},
		{"v0.2.7.1", SemVer{0, 2, 7, 1, ""}, false},
		{"v0.2.7.1-rc1", SemVer{0, 2, 7, 1, "rc1"}, false},
		{"2.7.0", SemVer{2, 7, 0, 0, ""}, false},
		{"v2.7.0", SemVer{2, 7, 0, 0, ""}, false},
		{"v2.7", SemVer{2, 7, 0, 0, ""}, false},
		{"2", SemVer{2, 0, 0, 0, ""}, false},
		{"v2.7.0-rc1", SemVer{2, 7, 0, 0, "rc1"}, false},
		{"2.7.0-beta.2", SemVer{2, 7, 0, 0, "beta.2"}, false},
		{"v2.7.0+build123", SemVer{2, 7, 0, 0, ""}, false},
		{"invalid", SemVer{}, true},
		{"", SemVer{}, true},
	}

	for _, tt := range tests {
		got, err := ParseSemVer(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSemVer(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseSemVer(%q) = %+v, want %+v", tt.input, got, tt.want)
		}
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		v1   string
		v2   string
		want int
	}{
		{"0.2.7.1", "0.2.7.1", 0},
		{"0.2.7.1", "0.2.7.0", 1},
		{"0.2.7.0", "0.2.7.1", -1},
		{"0.2.7.1", "0.2.7", 1},
		{"0.2.7", "0.2.7.1", -1},
		{"2.7.0", "2.7.0", 0},
		{"v2.7.0", "2.7.0", 0},
		{"2.8.0", "2.7.0", 1},
		{"2.7.0", "2.8.0", -1},
		{"2.6.1", "2.6.0", 1},
		{"3.0.0", "2.9.9", 1},
		{"2.6.0", "2.6.0-rc1", 1},
		{"2.6.0-rc1", "2.6.0", -1},
		{"2.6.0-rc2", "2.6.0-rc1", 1},
	}

	for _, tt := range tests {
		got, err := Compare(tt.v1, tt.v2)
		if err != nil {
			t.Fatalf("Compare(%q, %q) returned unexpected error: %v", tt.v1, tt.v2, err)
		}
		if got != tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.v1, tt.v2, got, tt.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	if !IsNewer("2.6.0", "2.6.1") {
		t.Error("expected 2.6.1 to be newer than 2.6.0")
	}
	if !IsNewer("2.6.0", "v2.7.0") {
		t.Error("expected v2.7.0 to be newer than 2.6.0")
	}
	if IsNewer("2.6.0", "2.6.0") {
		t.Error("expected 2.6.0 not to be newer than 2.6.0")
	}
	if IsNewer("2.6.0", "2.5.9") {
		t.Error("expected 2.5.9 not to be newer than 2.6.0")
	}
	if IsNewer("2.6.0", "invalid") {
		t.Error("expected invalid candidate not to be newer")
	}
}

func TestGetInfo(t *testing.T) {
	info := Get()
	if info.Version != Version {
		t.Errorf("Get().Version = %q, want %q", info.Version, Version)
	}
	if info.Platform == "" || info.Architecture == "" {
		t.Error("expected Platform and Architecture to be populated")
	}
	if info.FullString == "" {
		t.Error("expected FullString to be populated")
	}
}

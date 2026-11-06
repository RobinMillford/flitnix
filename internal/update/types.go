package update

import (
	"time"
)

// InstallType represents how an update should be applied on the current host.
type InstallType string

const (
	// InstallTypeDirect atomically replaces the current standalone/portable binary.
	InstallTypeDirect InstallType = "direct"
	// InstallTypeInstaller launches a dedicated installer (e.g. NSIS setup executable on Windows).
	InstallTypeInstaller InstallType = "installer"
	// InstallTypePackageManager guides the user to use their system package manager (e.g. deb/rpm).
	InstallTypePackageManager InstallType = "package_manager"
	// InstallTypeManual downloads the verified artifact and opens the file or release page (e.g. macOS DMG).
	InstallTypeManual InstallType = "manual"
)

// ReleaseAsset holds metadata for a specific release asset.
type ReleaseAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"downloadUrl"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
}

// ReleaseInfo holds the discovered release metadata.
type ReleaseInfo struct {
	Tag          string       `json:"tag"`
	Version      string       `json:"version"`
	Name         string       `json:"name"`
	ReleaseNotes string       `json:"releaseNotes"`
	PublishedAt  time.Time    `json:"publishedAt"`
	HTMLURL      string       `json:"htmlUrl"`
	Prerelease   bool         `json:"prerelease"`
	Asset        ReleaseAsset `json:"asset"`
	ChecksumURL  string       `json:"checksumUrl"`
	SignatureURL string       `json:"signatureUrl"`
}

// CheckResult represents the outcome of an update check.
type CheckResult struct {
	CurrentVersion  string      `json:"currentVersion"`
	LatestVersion   string      `json:"latestVersion"`
	UpdateAvailable bool        `json:"updateAvailable"`
	Release         ReleaseInfo `json:"release,omitempty"`
	InstallType     InstallType `json:"installType"`
	CheckedAt       time.Time   `json:"checkedAt"`
	Error           string      `json:"error,omitempty"`
}

// Progress represents live download and installation progress.
type Progress struct {
	Status            string  `json:"status"` // "idle", "checking", "downloading", "verifying", "ready_to_install", "installed", "error"
	DownloadedBytes   int64   `json:"downloadedBytes"`
	TotalBytes        int64   `json:"totalBytes"`
	Percentage        float64 `json:"percentage"`
	SpeedBytesPerSec  float64 `json:"speedBytesPerSec"`
	StagedFilePath    string  `json:"stagedFilePath,omitempty"`
	TargetInstallPath string  `json:"targetInstallPath,omitempty"`
	Error             string  `json:"error,omitempty"`
}

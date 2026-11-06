package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/version"
)

// Config defines the configuration for the update service.
type Config struct {
	RepoOwner             string
	RepoName              string
	BaseAPIURL            string
	HTTPClient            *http.Client
	PublicKeyHex          string
	CurrentExecutablePath string
	CurrentOS             string
	CurrentArch           string
	DownloadDir           string
	CacheDuration         time.Duration

	// AllowEmptyExecutablePath prevents New from defaulting CurrentExecutablePath to os.Executable().
	AllowEmptyExecutablePath bool

	// PackageManagerChecker allows platform-specific ownership detection or test overrides.
	PackageManagerChecker func(path string) (bool, error)

	// IsAppImage allows environment detection or test overrides.
	IsAppImage func() bool
}

// Service manages discovery, verification, and staging of application updates.
type Service struct {
	cfg Config

	mu           sync.Mutex
	lastCheck    time.Time
	cachedResult *CheckResult
	eTag         string
	activeProg   Progress
}

// New creates an update service with sensible defaults.
func New(cfg Config) *Service {
	if cfg.RepoOwner == "" {
		cfg.RepoOwner = "RobinMillford"
	}
	if cfg.RepoName == "" {
		cfg.RepoName = "flitnix"
	}
	if cfg.BaseAPIURL == "" {
		cfg.BaseAPIURL = "https://api.github.com"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.CurrentOS == "" {
		cfg.CurrentOS = runtime.GOOS
	}
	if cfg.CurrentArch == "" {
		cfg.CurrentArch = runtime.GOARCH
	}
	if cfg.CacheDuration == 0 {
		cfg.CacheDuration = 1 * time.Hour
	}
	if cfg.CurrentExecutablePath == "" && !cfg.AllowEmptyExecutablePath {
		if exe, err := os.Executable(); err == nil {
			cfg.CurrentExecutablePath = exe
		}
	}
	if cfg.DownloadDir == "" {
		cfg.DownloadDir = os.TempDir()
	}

	return &Service{cfg: cfg}
}

type gitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	Prerelease  bool          `json:"prerelease"`
	PublishedAt time.Time     `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Assets      []gitHubAsset `json:"assets"`
}

type gitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
}

// Check queries GitHub Releases for updates.
func (s *Service) Check(ctx context.Context, force bool) (*CheckResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	currentVer := version.Version
	if !force && s.cachedResult != nil && time.Since(s.lastCheck) < s.cfg.CacheDuration {
		return s.cachedResult, nil
	}

	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", s.cfg.BaseAPIURL, s.cfg.RepoOwner, s.cfg.RepoName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create update check request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", fmt.Sprintf("Flitnix-Updater/%s (%s; %s)", currentVer, s.cfg.CurrentOS, s.cfg.CurrentArch))

	if !force && s.eTag != "" {
		req.Header.Set("If-None-Match", s.eTag)
	}

	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		res := &CheckResult{
			CurrentVersion:  currentVer,
			UpdateAvailable: false,
			CheckedAt:       time.Now(),
			Error:           fmt.Sprintf("offline or unable to reach update server: %v", err),
			InstallType:     s.determineInstallType(),
		}
		s.cachedResult = res
		s.lastCheck = time.Now()
		return res, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && s.cachedResult != nil {
		s.lastCheck = time.Now()
		return s.cachedResult, nil
	}

	if resp.StatusCode != http.StatusOK {
		res := &CheckResult{
			CurrentVersion:  currentVer,
			UpdateAvailable: false,
			CheckedAt:       time.Now(),
			Error:           fmt.Sprintf("update server returned status HTTP %d", resp.StatusCode),
			InstallType:     s.determineInstallType(),
		}
		s.cachedResult = res
		s.lastCheck = time.Now()
		return res, nil
	}

	if et := resp.Header.Get("ETag"); et != "" {
		s.eTag = et
	}

	var ghRel gitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&ghRel); err != nil {
		return nil, fmt.Errorf("decode release metadata: %w", err)
	}

	candidateVer := strings.TrimPrefix(ghRel.TagName, "v")
	isAvailable := version.IsNewer(currentVer, candidateVer)

	// Development builds shouldn't falsely advertise newer if same or dev
	if version.Get().IsDev && candidateVer == currentVer {
		isAvailable = false
	}

	var matchedAsset ReleaseAsset
	var checksumURL, signatureURL string

	for _, a := range ghRel.Assets {
		if a.Name == "SHA256SUMS.txt" {
			checksumURL = a.BrowserDownloadURL
		} else if a.Name == "SHA256SUMS.txt.sig" || a.Name == "SHA256SUMS.sig" {
			signatureURL = a.BrowserDownloadURL
		} else if s.isAssetMatch(a.Name) {
			matchedAsset = ReleaseAsset{
				Name:        a.Name,
				DownloadURL: a.BrowserDownloadURL,
				Size:        a.Size,
				ContentType: a.ContentType,
			}
		}
	}

	res := &CheckResult{
		CurrentVersion:  currentVer,
		LatestVersion:   candidateVer,
		UpdateAvailable: isAvailable && matchedAsset.DownloadURL != "",
		InstallType:     s.determineInstallType(),
		CheckedAt:       time.Now(),
		Release: ReleaseInfo{
			Tag:          ghRel.TagName,
			Version:      candidateVer,
			Name:         ghRel.Name,
			ReleaseNotes: ghRel.Body,
			PublishedAt:  ghRel.PublishedAt,
			HTMLURL:      ghRel.HTMLURL,
			Prerelease:   ghRel.Prerelease,
			Asset:        matchedAsset,
			ChecksumURL:  checksumURL,
			SignatureURL: signatureURL,
		},
	}

	s.cachedResult = res
	s.lastCheck = time.Now()
	return res, nil
}

// Download downloads the matched release asset, computes its SHA-256 digest,
// downloads and parses SHA256SUMS.txt, optionally verifies SHA256SUMS.txt.sig,
// and stages the verified file.
func (s *Service) Download(ctx context.Context, progressCb func(Progress)) (*Progress, error) {
	s.mu.Lock()
	if s.cachedResult == nil || !s.cachedResult.UpdateAvailable || s.cachedResult.Release.Asset.DownloadURL == "" {
		s.mu.Unlock()
		return nil, errors.New("no update asset available to download")
	}
	rel := s.cachedResult.Release
	s.mu.Unlock()

	updateProgress := func(p Progress) {
		s.mu.Lock()
		s.activeProg = p
		s.mu.Unlock()
		if progressCb != nil {
			progressCb(p)
		}
	}

	updateProgress(Progress{
		Status:     "downloading",
		TotalBytes: rel.Asset.Size,
		Percentage: 0,
	})

	// Download the asset to a temporary staged file
	stagedFile := filepath.Join(s.cfg.DownloadDir, fmt.Sprintf(".flitnix-update-%s-%s.tmp", rel.Version, rel.Asset.Name))
	f, err := os.OpenFile(stagedFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		errProg := Progress{Status: "error", Error: fmt.Sprintf("create staged file: %v", err)}
		updateProgress(errProg)
		return &errProg, err
	}
	defer f.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.Asset.DownloadURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		errProg := Progress{Status: "error", Error: fmt.Sprintf("download failed: %v", err)}
		updateProgress(errProg)
		return &errProg, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errProg := Progress{Status: "error", Error: fmt.Sprintf("asset download returned HTTP %d", resp.StatusCode)}
		updateProgress(errProg)
		return &errProg, errors.New(errProg.Error)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(f, hasher)

	buf := make([]byte, 64*1024)
	var downloaded int64
	total := rel.Asset.Size
	if resp.ContentLength > 0 {
		total = resp.ContentLength
	}

	startTime := time.Now()
	lastReport := time.Now()

	for {
		n, rErr := resp.Body.Read(buf)
		if n > 0 {
			wn, wErr := writer.Write(buf[:n])
			if wErr != nil {
				return nil, fmt.Errorf("write staged file: %w", wErr)
			}
			downloaded += int64(wn)

			if time.Since(lastReport) >= 150*time.Millisecond || rErr != nil {
				lastReport = time.Now()
				elapsed := time.Since(startTime).Seconds()
				var speed float64
				if elapsed > 0 {
					speed = float64(downloaded) / elapsed
				}
				pct := float64(0)
				if total > 0 {
					pct = float64(downloaded) / float64(total) * 100
					if pct > 100 {
						pct = 100
					}
				}
				updateProgress(Progress{
					Status:           "downloading",
					DownloadedBytes:  downloaded,
					TotalBytes:       total,
					Percentage:       pct,
					SpeedBytesPerSec: speed,
					StagedFilePath:   stagedFile,
				})
			}
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			errProg := Progress{Status: "error", Error: fmt.Sprintf("stream interrupted: %v", rErr)}
			updateProgress(errProg)
			_ = os.Remove(stagedFile)
			return &errProg, rErr
		}
	}

	_ = f.Close()
	computedSHA := hex.EncodeToString(hasher.Sum(nil))

	// Verification Phase
	updateProgress(Progress{
		Status:          "verifying",
		DownloadedBytes: downloaded,
		TotalBytes:      total,
		Percentage:      100,
		StagedFilePath:  stagedFile,
	})

	// Check against SHA256SUMS.txt if present
	if rel.ChecksumURL != "" {
		checksumData, err := s.fetchBytes(ctx, rel.ChecksumURL)
		if err != nil {
			errProg := Progress{Status: "error", Error: fmt.Sprintf("fetch checksum manifest: %v", err)}
			updateProgress(errProg)
			_ = os.Remove(stagedFile)
			return &errProg, err
		}

		// Verify signature if signature URL is present
		if rel.SignatureURL != "" {
			sigData, err := s.fetchBytes(ctx, rel.SignatureURL)
			if err != nil {
				errProg := Progress{Status: "error", Error: fmt.Sprintf("fetch release signature: %v", err)}
				updateProgress(errProg)
				_ = os.Remove(stagedFile)
				return &errProg, err
			}
			sigHex := strings.TrimSpace(string(sigData))
			if err := VerifySignature(checksumData, sigHex, s.cfg.PublicKeyHex); err != nil {
				errProg := Progress{Status: "error", Error: fmt.Sprintf("signature verification rejected: %v", err)}
				updateProgress(errProg)
				_ = os.Remove(stagedFile)
				return &errProg, err
			}
		}

		// Verify asset digest in manifest
		expectedSHA, err := parseChecksumForFile(checksumData, rel.Asset.Name)
		if err != nil {
			errProg := Progress{Status: "error", Error: fmt.Sprintf("asset not found in checksum manifest: %v", err)}
			updateProgress(errProg)
			_ = os.Remove(stagedFile)
			return &errProg, err
		}

		if !strings.EqualFold(computedSHA, expectedSHA) {
			errProg := Progress{Status: "error", Error: fmt.Sprintf("SHA-256 mismatch: expected %s, got %s", expectedSHA, computedSHA)}
			updateProgress(errProg)
			_ = os.Remove(stagedFile)
			return &errProg, errors.New(errProg.Error)
		}
	}

	finalProg := Progress{
		Status:            "ready_to_install",
		DownloadedBytes:   downloaded,
		TotalBytes:        total,
		Percentage:        100,
		StagedFilePath:    stagedFile,
		TargetInstallPath: s.cfg.CurrentExecutablePath,
	}
	updateProgress(finalProg)
	return &finalProg, nil
}

// Apply installs the downloaded and verified update.
func (s *Service) Apply(ctx context.Context) error {
	s.mu.Lock()
	prog := s.activeProg
	installType := s.determineInstallType()
	s.mu.Unlock()

	if prog.Status != "ready_to_install" || prog.StagedFilePath == "" {
		return errors.New("no verified update is staged and ready to install")
	}

	if _, err := os.Stat(prog.StagedFilePath); err != nil {
		return fmt.Errorf("staged update file missing: %w", err)
	}

	switch installType {
	case InstallTypeInstaller:
		// Windows: Launch the NSIS installer with elevated prompt
		cmd := exec.Command(prog.StagedFilePath)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("launch installer: %w", err)
		}
		// Application can now initiate graceful shutdown to let the installer run
		return nil

	case InstallTypeDirect:
		// Linux/macOS standalone portable binary
		if s.cfg.CurrentExecutablePath == "" {
			return errors.New("current executable path is unknown")
		}

		// Ensure staged file is executable
		_ = os.Chmod(prog.StagedFilePath, 0o755)

		// Atomic swap: rename staged file over current executable
		backupPath := s.cfg.CurrentExecutablePath + ".old"
		_ = os.Remove(backupPath)
		_ = os.Rename(s.cfg.CurrentExecutablePath, backupPath)

		if err := os.Rename(prog.StagedFilePath, s.cfg.CurrentExecutablePath); err != nil {
			// Rollback on failure
			_ = os.Rename(backupPath, s.cfg.CurrentExecutablePath)
			return fmt.Errorf("atomic binary replacement failed: %w", err)
		}
		_ = os.Remove(backupPath)
		return nil

	case InstallTypePackageManager, InstallTypeManual:
		// Open the directory containing the verified downloaded artifact
		dir := filepath.Dir(prog.StagedFilePath)
		return openFileManager(dir)

	default:
		return fmt.Errorf("unsupported install type %q", installType)
	}
}

func (s *Service) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (s *Service) isAssetMatch(assetName string) bool {
	assetLower := strings.ToLower(assetName)
	switch s.cfg.CurrentOS {
	case "windows":
		return strings.HasSuffix(assetLower, ".exe") && strings.Contains(assetLower, "installer") || strings.Contains(assetLower, "setup")
	case "darwin":
		return strings.HasSuffix(assetLower, ".dmg") || (strings.HasSuffix(assetLower, ".zip") && strings.Contains(assetLower, "macos"))
	case "linux":
		installType := s.determineInstallType()
		if installType == InstallTypePackageManager {
			if s.isDebian() && strings.HasSuffix(assetLower, ".deb") && strings.Contains(assetLower, s.cfg.CurrentArch) {
				return true
			}
			if s.isRpm() && strings.HasSuffix(assetLower, ".rpm") && strings.Contains(assetLower, s.cfg.CurrentArch) {
				return true
			}
		}
		return strings.HasSuffix(assetLower, ".tar.gz") && strings.Contains(assetLower, "linux") && strings.Contains(assetLower, s.cfg.CurrentArch)
	default:
		return false
	}
}

func normalizePath(p string) string {
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	return path.Clean(p)
}

func (s *Service) determineInstallType() InstallType {
	switch s.cfg.CurrentOS {
	case "windows":
		return InstallTypeInstaller
	case "darwin":
		return InstallTypeManual
	case "linux":
		exe := s.cfg.CurrentExecutablePath
		if exe == "" {
			// Missing executable path: cannot safely replace in-place, fall back to manual
			return InstallTypeManual
		}

		clean := normalizePath(exe)

		// 1. Portable AppImage installation: read-only mount, direct overwrite unsupported
		if s.isAppImage(clean) {
			return InstallTypeManual
		}

		// 2. Unsupported sandbox/container formats (Snap, Flatpak)
		if isUnsupportedFormat(clean) {
			return InstallTypeManual
		}

		// 3. Package-managed installation
		managed, err := s.isPackageManagerInstalled(clean)
		if err != nil {
			// Detection error: fall back safely to manual rather than attempting risky direct overwrite
			return InstallTypeManual
		}
		if managed {
			return InstallTypePackageManager
		}

		// 4. Default for standalone portable Linux binary
		return InstallTypeDirect

	default:
		return InstallTypeManual
	}
}

func (s *Service) isAppImage(cleanPath string) bool {
	if s.cfg.IsAppImage != nil {
		return s.cfg.IsAppImage()
	}
	if os.Getenv("APPIMAGE") != "" || os.Getenv("APPDIR") != "" {
		return true
	}
	lower := strings.ToLower(cleanPath)
	return strings.HasSuffix(lower, ".appimage")
}

func isUnsupportedFormat(cleanPath string) bool {
	lower := strings.ToLower(cleanPath)
	if strings.HasPrefix(lower, "/snap/") || os.Getenv("SNAP") != "" {
		return true
	}
	if strings.Contains(lower, "/flatpak/") || os.Getenv("FLATPAK_ID") != "" {
		return true
	}
	return false
}

func (s *Service) isPackageManagerInstalled(cleanPath string) (bool, error) {
	if s.cfg.PackageManagerChecker != nil {
		return s.cfg.PackageManagerChecker(cleanPath)
	}

	// Application contract: /usr/bin/flitnix is the documented target of .deb and .rpm packages
	// (see nfpm.yaml and docs/architecture.md).
	if cleanPath == "/usr/bin/flitnix" || strings.HasPrefix(cleanPath, "/usr/bin/") {
		return true, nil
	}

	return checkSystemPackageManager(cleanPath)
}

func checkSystemPackageManager(cleanPath string) (bool, error) {
	if runtime.GOOS != "linux" {
		return false, nil
	}

	// Check dpkg
	if dpkgPath, err := exec.LookPath("dpkg"); err == nil && dpkgPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, dpkgPath, "-S", cleanPath)
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			return true, nil
		}
	}

	// Check rpm
	if rpmPath, err := exec.LookPath("rpm"); err == nil && rpmPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, rpmPath, "-qf", cleanPath)
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			return true, nil
		}
	}

	// Check pacman
	if pacmanPath, err := exec.LookPath("pacman"); err == nil && pacmanPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, pacmanPath, "-Qo", cleanPath)
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			return true, nil
		}
	}

	return false, nil
}

func (s *Service) isDebian() bool {
	if _, err := os.Stat("/etc/debian_version"); err == nil {
		return true
	}
	return false
}

func (s *Service) isRpm() bool {
	if _, err := os.Stat("/etc/redhat-release"); err == nil {
		return true
	}
	if _, err := os.Stat("/etc/fedora-release"); err == nil {
		return true
	}
	return false
}

func parseChecksumForFile(manifestData []byte, filename string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(manifestData))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			entryFile := strings.TrimPrefix(fields[1], "*")
			entryFile = filepath.Base(entryFile)
			if entryFile == filename {
				return fields[0], nil
			}
		}
	}
	return "", fmt.Errorf("file %q not found in checksum manifest", filename)
}

func openFileManager(dir string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", dir).Start()
	case "darwin":
		return exec.Command("open", dir).Start()
	case "linux":
		return exec.Command("xdg-open", dir).Start()
	default:
		return fmt.Errorf("unsupported platform for file manager opening: %s", runtime.GOOS)
	}
}

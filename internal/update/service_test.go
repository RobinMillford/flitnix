package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestUpdateCheck_NoUpdateWhenEqualOrOlder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := gitHubRelease{
			TagName:     "v2.7.0",
			Name:        "Flitnix v2.7.0",
			PublishedAt: time.Now(),
			Assets: []gitHubAsset{
				{Name: "flitnix_2.6.0_linux_amd64.tar.gz", BrowserDownloadURL: "http://example.com/asset.tar.gz", Size: 1024},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	}))
	defer server.Close()

	svc := New(Config{
		BaseAPIURL:  server.URL,
		HTTPClient:  server.Client(),
		CurrentOS:   "linux",
		CurrentArch: "amd64",
	})

	res, err := svc.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}

	if res.UpdateAvailable {
		t.Errorf("expected UpdateAvailable=false when version is same (%s vs %s)", res.CurrentVersion, res.LatestVersion)
	}
}

func TestUpdateCheck_UpdateAvailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := gitHubRelease{
			TagName:     "v2.8.0",
			Name:        "Flitnix v2.8.0",
			Body:        "## Release Notes\n- Polished UI\n- High speed streaming",
			PublishedAt: time.Now(),
			Assets: []gitHubAsset{
				{Name: "flitnix_2.8.0_linux_amd64.tar.gz", BrowserDownloadURL: "http://example.com/asset.tar.gz", Size: 1024},
				{Name: "SHA256SUMS.txt", BrowserDownloadURL: "http://example.com/SHA256SUMS.txt", Size: 120},
				{Name: "SHA256SUMS.txt.sig", BrowserDownloadURL: "http://example.com/SHA256SUMS.txt.sig", Size: 128},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	}))
	defer server.Close()

	svc := New(Config{
		BaseAPIURL:  server.URL,
		HTTPClient:  server.Client(),
		CurrentOS:   "linux",
		CurrentArch: "amd64",
	})

	res, err := svc.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}

	if !res.UpdateAvailable {
		t.Errorf("expected UpdateAvailable=true for v2.8.0 > %s", res.CurrentVersion)
	}
	if res.Release.Asset.Name != "flitnix_2.8.0_linux_amd64.tar.gz" {
		t.Errorf("unexpected matched asset: %s", res.Release.Asset.Name)
	}
	if res.Release.ChecksumURL != "http://example.com/SHA256SUMS.txt" {
		t.Errorf("unexpected checksum URL: %s", res.Release.ChecksumURL)
	}
}

func TestUpdateCheck_OfflineGraceful(t *testing.T) {
	// Points to an unreachable port
	svc := New(Config{
		BaseAPIURL:  "http://127.0.0.1:59999",
		HTTPClient:  &http.Client{Timeout: 100 * time.Millisecond},
		CurrentOS:   "linux",
		CurrentArch: "amd64",
	})

	res, err := svc.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("expected Check to return result with error message, not hard error: %v", err)
	}
	if res.UpdateAvailable {
		t.Error("expected UpdateAvailable=false when offline")
	}
	if res.Error == "" {
		t.Error("expected non-empty Error string when offline")
	}
}

func TestDownloadAndVerify_ValidSignatureAndChecksum(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)

	assetContent := []byte("binary payload for v2.7.0 update")
	assetHash := sha256.Sum256(assetContent)
	assetHashHex := hex.EncodeToString(assetHash[:])
	assetName := "flitnix_2.8.0_linux_amd64.tar.gz"

	checksumManifest := []byte(fmt.Sprintf("%s  %s\n", assetHashHex, assetName))
	signature := ed25519.Sign(priv, checksumManifest)
	sigHex := hex.EncodeToString(signature)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/RobinMillford/flitnix/releases/latest":
			rel := gitHubRelease{
				TagName:     "v2.8.0",
				Name:        "Flitnix v2.8.0",
				PublishedAt: time.Now(),
				Assets: []gitHubAsset{
					{Name: assetName, BrowserDownloadURL: "/asset", Size: int64(len(assetContent))},
					{Name: "SHA256SUMS.txt", BrowserDownloadURL: "/checksums", Size: int64(len(checksumManifest))},
					{Name: "SHA256SUMS.txt.sig", BrowserDownloadURL: "/sig", Size: int64(len(sigHex))},
				},
			}
			_ = json.NewEncoder(w).Encode(rel)
		case "/asset":
			_, _ = w.Write(assetContent)
		case "/checksums":
			_, _ = w.Write(checksumManifest)
		case "/sig":
			_, _ = w.Write([]byte(sigHex))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	svc := New(Config{
		BaseAPIURL:   server.URL,
		HTTPClient:   server.Client(),
		PublicKeyHex: pubHex,
		CurrentOS:    "linux",
		CurrentArch:  "amd64",
		DownloadDir:  tempDir,
	})

	// 1. Check
	checkRes, err := svc.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	// rewrite URLs to test server
	checkRes.Release.Asset.DownloadURL = server.URL + "/asset"
	checkRes.Release.ChecksumURL = server.URL + "/checksums"
	checkRes.Release.SignatureURL = server.URL + "/sig"

	// 2. Download and Verify
	var reportedPct float64
	prog, err := svc.Download(context.Background(), func(p Progress) {
		if p.Percentage > reportedPct {
			reportedPct = p.Percentage
		}
	})
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}

	if prog.Status != "ready_to_install" {
		t.Fatalf("expected status 'ready_to_install', got %q", prog.Status)
	}
	if prog.DownloadedBytes != int64(len(assetContent)) {
		t.Errorf("downloaded bytes = %d, want %d", prog.DownloadedBytes, len(assetContent))
	}
	if _, statErr := os.Stat(prog.StagedFilePath); statErr != nil {
		t.Fatalf("staged file not found: %v", statErr)
	}
}

func TestDownloadAndVerify_TamperedSignatureRejected(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)

	assetContent := []byte("binary payload")
	assetHash := sha256.Sum256(assetContent)
	assetHashHex := hex.EncodeToString(assetHash[:])
	assetName := "flitnix_2.8.0_linux_amd64.tar.gz"

	checksumManifest := []byte(fmt.Sprintf("%s  %s\n", assetHashHex, assetName))
	bogusSig := hex.EncodeToString(make([]byte, ed25519.SignatureSize)) // all zeros

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/RobinMillford/flitnix/releases/latest":
			rel := gitHubRelease{
				TagName: "v2.8.0",
				Assets: []gitHubAsset{
					{Name: assetName, BrowserDownloadURL: "/asset", Size: int64(len(assetContent))},
					{Name: "SHA256SUMS.txt", BrowserDownloadURL: "/checksums", Size: int64(len(checksumManifest))},
					{Name: "SHA256SUMS.txt.sig", BrowserDownloadURL: "/sig", Size: int64(len(bogusSig))},
				},
			}
			_ = json.NewEncoder(w).Encode(rel)
		case "/asset":
			_, _ = w.Write(assetContent)
		case "/checksums":
			_, _ = w.Write(checksumManifest)
		case "/sig":
			_, _ = w.Write([]byte(bogusSig))
		}
	}))
	defer server.Close()

	svc := New(Config{
		BaseAPIURL:   server.URL,
		HTTPClient:   server.Client(),
		PublicKeyHex: pubHex,
		CurrentOS:    "linux",
		CurrentArch:  "amd64",
		DownloadDir:  t.TempDir(),
	})

	checkRes, _ := svc.Check(context.Background(), true)
	checkRes.Release.Asset.DownloadURL = server.URL + "/asset"
	checkRes.Release.ChecksumURL = server.URL + "/checksums"
	checkRes.Release.SignatureURL = server.URL + "/sig"

	_, err = svc.Download(context.Background(), nil)
	if err == nil {
		t.Fatal("expected download to fail due to forged signature, but it succeeded")
	}
}

func TestDownloadAndVerify_ChecksumMismatchRejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubHex := hex.EncodeToString(pub)

	assetContent := []byte("actual binary content")
	assetName := "flitnix_2.8.0_linux_amd64.tar.gz"

	wrongHashHex := "0000000000000000000000000000000000000000000000000000000000000000"
	checksumManifest := []byte(fmt.Sprintf("%s  %s\n", wrongHashHex, assetName))
	sig := hex.EncodeToString(ed25519.Sign(priv, checksumManifest))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/RobinMillford/flitnix/releases/latest":
			rel := gitHubRelease{
				TagName: "v2.8.0",
				Assets: []gitHubAsset{
					{Name: assetName, BrowserDownloadURL: "/asset", Size: int64(len(assetContent))},
					{Name: "SHA256SUMS.txt", BrowserDownloadURL: "/checksums", Size: int64(len(checksumManifest))},
					{Name: "SHA256SUMS.txt.sig", BrowserDownloadURL: "/sig", Size: int64(len(sig))},
				},
			}
			_ = json.NewEncoder(w).Encode(rel)
		case "/asset":
			_, _ = w.Write(assetContent)
		case "/checksums":
			_, _ = w.Write(checksumManifest)
		case "/sig":
			_, _ = w.Write([]byte(sig))
		}
	}))
	defer server.Close()

	svc := New(Config{
		BaseAPIURL:   server.URL,
		HTTPClient:   server.Client(),
		PublicKeyHex: pubHex,
		CurrentOS:    "linux",
		CurrentArch:  "amd64",
		DownloadDir:  t.TempDir(),
	})

	checkRes, _ := svc.Check(context.Background(), true)
	checkRes.Release.Asset.DownloadURL = server.URL + "/asset"
	checkRes.Release.ChecksumURL = server.URL + "/checksums"
	checkRes.Release.SignatureURL = server.URL + "/sig"

	_, err := svc.Download(context.Background(), nil)
	if err == nil {
		t.Fatal("expected download to fail due to SHA-256 mismatch, but it succeeded")
	}
}

func TestAssetMatching(t *testing.T) {
	svcLinux := New(Config{CurrentOS: "linux", CurrentArch: "amd64"})
	if !svcLinux.isAssetMatch("flitnix_2.7.0_linux_amd64.tar.gz") {
		t.Error("expected linux tar.gz match")
	}
	if svcLinux.isAssetMatch("flitnix_2.7.0_windows_amd64_setup.exe") {
		t.Error("unexpected match of windows on linux")
	}

	svcWin := New(Config{CurrentOS: "windows", CurrentArch: "amd64"})
	if !svcWin.isAssetMatch("flitnix_2.7.0_windows_amd64_setup.exe") {
		t.Error("expected windows setup.exe match")
	}

	svcMac := New(Config{CurrentOS: "darwin", CurrentArch: "arm64"})
	if !svcMac.isAssetMatch("flitnix_2.7.0_macOS_universal.dmg") {
		t.Error("expected macos dmg match")
	}
}

func TestInstallTypeDetermination(t *testing.T) {
	svcWin := New(Config{CurrentOS: "windows"})
	if svcWin.determineInstallType() != InstallTypeInstaller {
		t.Errorf("want installer on windows, got %s", svcWin.determineInstallType())
	}

	svcMac := New(Config{CurrentOS: "darwin"})
	if svcMac.determineInstallType() != InstallTypeManual {
		t.Errorf("want manual on macos, got %s", svcMac.determineInstallType())
	}

	svcLinuxPortable := New(Config{CurrentOS: "linux", CurrentExecutablePath: "/home/user/bin/flitnix"})
	if svcLinuxPortable.determineInstallType() != InstallTypeDirect {
		t.Errorf("want direct on linux portable, got %s", svcLinuxPortable.determineInstallType())
	}

	svcLinuxSystem := New(Config{CurrentOS: "linux", CurrentExecutablePath: "/usr/bin/flitnix"})
	if svcLinuxSystem.determineInstallType() != InstallTypePackageManager {
		t.Errorf("want package_manager on /usr/bin/flitnix, got %s", svcLinuxSystem.determineInstallType())
	}
}

func TestParseChecksumForFile(t *testing.T) {
	data := []byte(`
# Checksums for Flitnix release
a1b2c3d4e5f6  flitnix_2.7.0_linux_amd64.tar.gz
112233445566 *flitnix_2.7.0_windows_amd64_setup.exe
`)
	sha, err := parseChecksumForFile(data, "flitnix_2.7.0_linux_amd64.tar.gz")
	if err != nil || sha != "a1b2c3d4e5f6" {
		t.Fatalf("parse failed: got %q, err %v", sha, err)
	}

	shaWin, err := parseChecksumForFile(data, "flitnix_2.7.0_windows_amd64_setup.exe")
	if err != nil || shaWin != "112233445566" {
		t.Fatalf("parse failed: got %q, err %v", shaWin, err)
	}

	_, errMissing := parseChecksumForFile(data, "nonexistent.bin")
	if errMissing == nil {
		t.Fatal("expected error for missing file")
	}
}

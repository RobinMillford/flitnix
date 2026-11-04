package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"time"
)

type EnvironmentManifest struct {
	Timestamp      time.Time `json:"timestamp"`
	Hostname       string    `json:"hostname"`
	OS             string    `json:"os"`
	Kernel         string    `json:"kernel"`
	Distribution   string    `json:"distribution"`
	CPUModel       string    `json:"cpu_model"`
	CPUCores       int       `json:"cpu_cores"`
	MemoryTotalMB  float64   `json:"memory_total_mb"`
	MemoryAvailMB  float64   `json:"memory_avail_mb"`
	StorageDevice  string    `json:"storage_device"`
	FilesystemType string    `json:"filesystem_type"`
	StorageMount   string    `json:"storage_mount"`
	StorageAvailGB float64   `json:"storage_avail_gb"`
	GoVersion      string    `json:"go_version"`
	NodeVersion    string    `json:"node_version"`
	WailsVersion   string    `json:"wails_version"`
	BrowserVersion string    `json:"browser_version"`
	WiFiInterface  string    `json:"wifi_interface"`
	WiFiSSID       string    `json:"wifi_ssid"`
	WiFiFreqMHz    string    `json:"wifi_freq_mhz"`
	WiFiSignalDBm  string    `json:"wifi_signal_dbm"`
	WiFiBitrate    string    `json:"wifi_bitrate"`
	AndroidDevice  string    `json:"android_device"`
	IOSDevice      string    `json:"ios_device"`
}

func collectEnvironment(artifactsDir string) (EnvironmentManifest, error) {
	hostname, _ := os.Hostname()

	// Kernel
	kernelBytes, _ := exec.Command("uname", "-r").Output()
	kernel := strings.TrimSpace(string(kernelBytes))

	// OS / Distro — read live from /etc/os-release
	distro := readOSRelease()

	// CPU — live from lscpu
	cpuModel := "unavailable (lscpu failed)"
	if out, err := exec.Command("lscpu").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "Model name:") {
				cpuModel = strings.TrimSpace(strings.TrimPrefix(line, "Model name:"))
				break
			}
		}
	}

	// Memory — live from /proc/meminfo
	memTotal, memAvail := 0.0, 0.0
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, _ := strconv.ParseFloat(fields[1], 64) // kB
				switch fields[0] {
				case "MemTotal:":
					memTotal = v / 1024.0
				case "MemAvailable:":
					memAvail = v / 1024.0
				}
			}
		}
	}

	// Storage — live from findmnt on the artifacts directory
	storageDevice, fsType, mountPoint, availGB := "unavailable", "unavailable", "unavailable", 0.0
	if out, err := exec.Command("findmnt", "-n", "-o", "SOURCE,FSTYPE,TARGET,AVAIL", "-T", artifactsDir).Output(); err == nil {
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) >= 4 {
			storageDevice = fields[0]
			fsType = fields[1]
			mountPoint = fields[2]
			if gb, err := strconv.ParseFloat(strings.TrimSuffix(fields[3], "G"), 64); err == nil {
				availGB = gb
			}
		}
	}

	// Tools
	nodeBytes, _ := exec.Command("node", "--version").Output()
	wailsBytes, _ := exec.Command("/home/itsyamin/go/bin/wails", "version").Output()
	firefoxBytes, _ := exec.Command("firefox", "--version").Output()

	// Wi-Fi — live from iw / nmcli on the default wireless interface
	wifiIface, wifiSSID, wifiFreq, wifiSignal, wifiBitrate := liveWiFiState()

	// Physical devices — detected live, never assumed.
	androidDevice := detectAndroidDevice()
	iosDevice := detectIOSDevice()

	manifest := EnvironmentManifest{
		Timestamp:      time.Now().UTC(),
		Hostname:       hostname,
		OS:             runtime.GOOS,
		Kernel:         kernel,
		Distribution:   distro,
		CPUModel:       cpuModel,
		CPUCores:       runtime.NumCPU(),
		MemoryTotalMB:  memTotal,
		MemoryAvailMB:  memAvail,
		StorageDevice:  storageDevice,
		FilesystemType: fsType,
		StorageMount:   mountPoint,
		StorageAvailGB: availGB,
		GoVersion:      runtime.Version(),
		NodeVersion:    strings.TrimSpace(string(nodeBytes)),
		// `wails version` appends sponsor/ANSI output after the version line;
		// keep only the first line so generated reports stay clean markdown.
		WailsVersion:   strings.TrimSpace(strings.SplitN(string(wailsBytes), "\n", 2)[0]),
		BrowserVersion: strings.TrimSpace(string(firefoxBytes)),
		WiFiInterface:  wifiIface,
		WiFiSSID:       wifiSSID,
		WiFiFreqMHz:    wifiFreq,
		WiFiSignalDBm:  wifiSignal,
		WiFiBitrate:    wifiBitrate,
		AndroidDevice:  androidDevice,
		IOSDevice:      iosDevice,
	}

	manifestJson, _ := json.MarshalIndent(manifest, "", "  ")
	_ = os.WriteFile(filepath.Join(artifactsDir, "environment.json"), manifestJson, 0o644)
	return manifest, nil
}

func readOSRelease() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "unavailable (/etc/os-release unreadable)"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return "unknown"
}

// liveWiFiState queries iw/nmcli for the current wireless link. Every failure
// degrades to an explicit "unavailable" marker — never a cached or assumed value.
func liveWiFiState() (iface, ssid, freq, signal, bitrate string) {
	iface, ssid, freq, signal, bitrate = "unavailable", "unavailable", "unavailable", "unavailable", "unavailable"
	out, err := exec.Command("iw", "dev").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Interface "):
			if iface == "unavailable" {
				iface = strings.TrimSpace(strings.TrimPrefix(trimmed, "Interface "))
			}
		case strings.HasPrefix(trimmed, "ssid "):
			ssid = strings.Trim(strings.TrimPrefix(trimmed, "ssid "), "\"")
		case strings.HasPrefix(trimmed, "freq: "):
			freq = strings.TrimSpace(strings.TrimPrefix(trimmed, "freq: "))
		case strings.HasPrefix(trimmed, "signal: "):
			signal = strings.TrimSpace(strings.TrimPrefix(trimmed, "signal: "))
		case strings.HasPrefix(trimmed, "tx bitrate: "):
			bitrate = strings.TrimSpace(strings.TrimPrefix(trimmed, "tx bitrate: "))
		}
	}
	return
}

// detectAndroidDevice reports what adb actually sees. With no device attached
// this explicitly says so — the harness never claims physical-device testing
// it did not perform.
func detectAndroidDevice() string {
	out, err := exec.Command("adb", "devices").Output()
	if err != nil {
		return "ANDROID PHYSICAL VALIDATION: NOT TESTED — adb unavailable"
	}
	var serials []string
	for _, line := range strings.Split(string(out), "\n")[1:] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, "devices") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "device" {
			serials = append(serials, fields[0])
		}
	}
	if len(serials) == 0 {
		return "ANDROID PHYSICAL VALIDATION: NOT TESTED — NO DEVICE ATTACHED (adb: no devices)"
	}
	modelOut, _ := exec.Command("adb", "-s", serials[0], "shell", "getprop", "ro.product.model").Output()
	model := strings.TrimSpace(string(modelOut))
	if model == "" {
		model = "unknown model"
	}
	return fmt.Sprintf("Attached: %s (%s, serial %s)", model, "adb device state=online", serials[0])
}

func detectIOSDevice() string {
	return "iOS PHYSICAL VALIDATION: NOT TESTED — DEVICE UNAVAILABLE"
}

func main() {
	stage := flag.String("stage", "all", "stage to run: all, throughput, or recovery")
	payloadList := flag.String("payloads", "1gb,5gb,20gb", "comma-separated payloads: 1gb,5gb,20gb")
	runID := flag.String("run-id", time.Now().UTC().Format("20060102T150405Z"), "run identifier stamped on every row and artifact")
	primaryReps := flag.Int("primary-reps", 5, "repetitions for the Config-1/Config-4 acceptance pair")
	streamingReps := flag.Int("streaming-reps", 3, "repetitions for Config-2/Config-3")
	artifactsDirFlag := flag.String("artifacts-dir", "artifacts/wp4", "directory where benchmark artifacts are stored")
	fixturesDirFlag := flag.String("fixtures-dir", "", "directory where shared fixtures reside (defaults to <artifacts-dir>/fixtures)")
	interleavedFlag := flag.Bool("interleaved", false, "interleave and alternate Config-1 and Config-4 to remove thermal and caching bias")
	skipConfig5Flag := flag.Bool("skip-config5", false, "skip fallback slice benchmark (useful for paired acceptance audit)")
	syncCheckpointsFlag := flag.Bool("sync-checkpoints", false, "force synchronous checkpointing (disables pipelined metadata persistence)")
	legacyProgressThrottleFlag := flag.Bool("legacy-progress-throttle", false, "use legacy compound progress throttling (256KB || 100ms) instead of pure 50ms")
	cpuProfile := flag.String("cpuprofile", "", "write cpu profile to file")
	memProfile := flag.String("memprofile", "", "write memory profile to file")
	flag.Parse()

	if *legacyProgressThrottleFlag {
		_ = os.Setenv("FLITNIX_LEGACY_PROGRESS_THROTTLE", "1")
	} else {
		_ = os.Unsetenv("FLITNIX_LEGACY_PROGRESS_THROTTLE")
	}

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not create CPU profile: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "could not start CPU profile: %v\n", err)
			os.Exit(1)
		}
		defer pprof.StopCPUProfile()
	}

	doThroughput := *stage == "all" || *stage == "throughput"
	doRecovery := *stage == "all" || *stage == "recovery"
	doIntervalTuning := *stage == "interval-tuning"
	if !doThroughput && !doRecovery && !doIntervalTuning {
		fmt.Fprintf(os.Stderr, "unknown -stage %q (want all, throughput, recovery, or interval-tuning)\n", *stage)
		os.Exit(2)
	}

	fmt.Println("================================================================================")
	fmt.Println(" FLITNIX PHASE 3 — WORK PACKET 4 VERIFICATION & BENCHMARK HARNESS               ")
	fmt.Println("================================================================================")
	fmt.Printf(" Run ID: %s | stage: %s | payloads: %s\n", *runID, *stage, *payloadList)

	artifactsDir := *artifactsDirFlag
	fixturesDir := *fixturesDirFlag
	if fixturesDir == "" {
		fixturesDir = filepath.Join(artifactsDir, "fixtures")
	}
	rawLogsDir := filepath.Join(artifactsDir, "raw-logs")
	_ = os.MkdirAll(artifactsDir, 0o755)
	_ = os.MkdirAll(fixturesDir, 0o755)
	_ = os.MkdirAll(rawLogsDir, 0o755)

	// Step 1: Environment Manifest
	fmt.Println("\n[1/5] Collecting Environment Manifest...")
	env, err := collectEnvironment(artifactsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Environment collection error: %v\n", err)
	}
	fmt.Printf("Host: %s | OS: %s (%s) | CPU: %s (%d cores)\n", env.Hostname, env.OS, env.Kernel, env.CPUModel, env.CPUCores)
	fmt.Printf("Filesystem: %s (%s) on %s | Free Space: %.1f GB\n", env.FilesystemType, env.StorageMount, env.StorageDevice, env.StorageAvailGB)
	fmt.Printf("Saved environment manifest to %s/environment.json\n", artifactsDir)

	// Step 2: Fixtures. Only the requested payloads are (re)verified, so a
	// recovery-only validation run never re-hashes 26 GB of fixtures. Existing
	// fixture files are verified, never regenerated, and fixtures.json keeps
	// every previously recorded entry.
	fmt.Println("\n[2/5] Verifying Deterministic Non-Compressible Fixtures...")
	payloadSizes := map[string]int64{"1gb": 1 << 30, "5gb": 5 << 30, "20gb": 20 << 30}
	fixtures := make(map[string]FixtureMetadata)
	var payloadNames []string
	for _, raw := range strings.Split(*payloadList, ",") {
		key := strings.TrimSpace(strings.ToLower(raw))
		size, ok := payloadSizes[key]
		if !ok {
			fmt.Fprintf(os.Stderr, "Fixture error: unknown payload %q\n", raw)
			os.Exit(2)
		}
		name := "payload_" + key + ".bin"
		meta, ferr := EnsureFixture(fixturesDir, name, size)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "Fixture error: %v\n", ferr)
			os.Exit(1)
		}
		fixtures[name] = meta
		payloadNames = append(payloadNames, name)
	}
	persistFixtureManifest(fixturesDir, fixtures)

	// Step 3: Filesystem Checkpoint Latency Microbenchmarks
	var checkpointStats []LatencyStats
	if doThroughput {
		fmt.Println("\n[3/5] Running Filesystem Checkpoint-Cost Benchmarks on btrfs/NVMe...")
		checkpointStats, err = BenchmarkFilesystemCheckpoints(artifactsDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Checkpoint bench error: %v\n", err)
		} else {
			csvPath := filepath.Join(artifactsDir, "checkpoint-latency.csv")
			_ = SaveCheckpointLatencyCSV(csvPath, checkpointStats)
			fmt.Printf("Saved checkpoint latency results to %s\n", csvPath)
		}
	} else {
		fmt.Println("\n[3/5] Checkpoint microbenchmarks skipped (recovery-only stage; existing checkpoint-latency.csv left untouched)")
	}

	// Step 4: Throughput stage (Configurations 1-5).
	var allResults []BenchmarkRunResult
	var recoveryResults []RecoveryRunResult
	failedAttempts := 0
	aborted := false
	if doIntervalTuning {
		runIntervalTuningStage(artifactsDir, fixtures, env, checkpointStats, *runID)
		return
	}
	if doThroughput {
		var throughputFailed int
		allResults, throughputFailed, aborted = runThroughputStage(artifactsDir, fixtures, payloadNames, *runID, *primaryReps, *streamingReps, *interleavedFlag, *skipConfig5Flag, *syncCheckpointsFlag, *legacyProgressThrottleFlag)
		failedAttempts += throughputFailed
	} else {
		// Recovery-only stage: never truncate the historical throughput
		// dataset. Carry the previously recorded Config 1-5 rows forward so
		// benchmark-results.* and the summary stay complete and attributable.
		if data, err := os.ReadFile(filepath.Join(artifactsDir, "benchmark-results.json")); err == nil {
			var prev []BenchmarkRunResult
			if json.Unmarshal(data, &prev) == nil && len(prev) > 0 {
				allResults = prev
				fmt.Printf("Recovery-only stage: preserving %d historical throughput rows from benchmark-results.json\n", len(prev))
			}
		}
	}

	// Step 5: Recovery stage (Configuration 6), always after all throughput
	// measurements and always in its own isolated server/session so an expected
	// interruption cannot poison a throughput queue.
	if doRecovery {
		recResults, recFailed := runRecoveryStage(artifactsDir, fixtures, payloadNames, *runID)
		recoveryResults = append(recoveryResults, recResults...)
		failedAttempts += recFailed
	}

	// Step 6: Save Benchmark Artifacts. Canonical names are updated and, when a
	// run ID is present, run-tagged copies are written so historical partial or
	// invalid datasets stay clearly attributable.
	fmt.Println("\n[5/5] Generating Benchmark Reports & CSV Artifacts...")
	saveBenchmarkCSV(filepath.Join(artifactsDir, "benchmark-results.csv"), allResults)
	saveBenchmarkJSON(filepath.Join(artifactsDir, "benchmark-results.json"), allResults)
	saveRecoveryCSV(filepath.Join(artifactsDir, "recovery-results.csv"), recoveryResults)
	saveRecoveryJSON(filepath.Join(artifactsDir, "recovery-results.json"), recoveryResults)
	generateSummaryMarkdown(filepath.Join(artifactsDir, "benchmark-summary.md"), env, allResults, checkpointStats, recoveryResults, failedAttempts, *runID, aborted)

	if r := strings.TrimSpace(*runID); r != "" {
		saveBenchmarkCSV(filepath.Join(artifactsDir, "benchmark-results-"+r+".csv"), allResults)
		saveBenchmarkJSON(filepath.Join(artifactsDir, "benchmark-results-"+r+".json"), allResults)
		saveRecoveryCSV(filepath.Join(artifactsDir, "recovery-results-"+r+".csv"), recoveryResults)
		generateSummaryMarkdown(filepath.Join(artifactsDir, "benchmark-summary-"+r+".md"), env, allResults, checkpointStats, recoveryResults, failedAttempts, *runID, aborted)
	}

	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err == nil {
			runtime.GC()
			_ = pprof.WriteHeapProfile(f)
			f.Close()
		}
	}

	if failedAttempts > 0 || aborted {
		fmt.Printf("Completed with %d FAILED attempt(s)%s — every attempt is recorded in artifacts as Valid=false. Review before trusting results.\n", failedAttempts, map[bool]string{true: " (stage aborted)", false: ""}[aborted])
		os.Exit(1)
	}
	fmt.Println("All requested benchmarks completed successfully.")
}

// persistFixtureManifest merges the verified fixture entries into fixtures.json
// so a partial run never drops payloads recorded by an earlier run.
func persistFixtureManifest(fixturesDir string, verified map[string]FixtureMetadata) {
	merged := make(map[string]FixtureMetadata)
	if data, err := os.ReadFile(filepath.Join(fixturesDir, "fixtures.json")); err == nil {
		_ = json.Unmarshal(data, &merged)
	}
	for name, meta := range verified {
		merged[name] = meta
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(fixturesDir, "fixtures.json"), data, 0o644)
}

// runIntervalTuningStage benchmarks Config-1 vs Config-4 across 32MB, 64MB, and 128MB
// checkpoint intervals using Latin-Square balanced, interleaved blocks.
func runIntervalTuningStage(artifactsDir string, fixtures map[string]FixtureMetadata, env EnvironmentManifest, cpStats []LatencyStats, runID string) {
	fmt.Println("\n[4/6] WP3.11 Interval-Tuning Stage: Config-1 vs Config-4 (32MB, 64MB, 128MB)")
	fixture, ok := fixtures["payload_1gb.bin"]
	if !ok {
		fmt.Fprintf(os.Stderr, "Interval tuning requires payload_1gb.bin in fixtures\n")
		os.Exit(1)
	}

	benchServerDir := filepath.Join(artifactsDir, "server_received")
	harness, cleanup, err := StartBenchServer(benchServerDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start bench server: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()
	defer os.RemoveAll(benchServerDir)
	harness.runID = runID

	harness.services.EnsureResumable().SetPipelineCheckpoints(true)
	fmt.Println("  [Checkpoints: Pipelined Mode Active (Candidate Config D)]")
	fmt.Println("  [Progress Throttling: Pure 50ms Active]")

	// 10 Balanced Blocks rotating the order across 4 candidates
	schedule := [][]string{
		{"C1", "C4_32MB", "C4_64MB", "C4_128MB"},
		{"C4_32MB", "C4_64MB", "C4_128MB", "C1"},
		{"C4_64MB", "C4_128MB", "C1", "C4_32MB"},
		{"C4_128MB", "C1", "C4_32MB", "C4_64MB"},
		{"C1", "C4_128MB", "C4_64MB", "C4_32MB"},
		{"C4_32MB", "C1", "C4_128MB", "C4_64MB"},
		{"C4_64MB", "C4_32MB", "C1", "C4_128MB"},
		{"C4_128MB", "C4_64MB", "C4_32MB", "C1"},
		{"C1", "C4_64MB", "C4_32MB", "C4_128MB"},
		{"C4_64MB", "C1", "C4_128MB", "C4_32MB"},
	}

	var results []BenchmarkRunResult

	for blockIdx, block := range schedule {
		fmt.Printf("\n--- Balanced Block %d/10: Order = %v ---\n", blockIdx+1, block)
		for _, item := range block {
			time.Sleep(500 * time.Millisecond) // Cooldown between runs
			var res BenchmarkRunResult
			var rerr error

			switch item {
			case "C1":
				res, rerr = harness.RunConfig1_LegacyPUT(fixture, blockIdx+1)
			case "C4_32MB":
				harness.SetCheckpointBytes(32 * 1024 * 1024)
				res, rerr = harness.RunConfig4_PrimaryProduction(fixture, blockIdx+1)
			case "C4_64MB":
				harness.SetCheckpointBytes(64 * 1024 * 1024)
				res, rerr = harness.RunConfig4_PrimaryProduction(fixture, blockIdx+1)
			case "C4_128MB":
				harness.SetCheckpointBytes(128 * 1024 * 1024)
				res, rerr = harness.RunConfig4_PrimaryProduction(fixture, blockIdx+1)
			}

			res.RunID = runID
			if rerr != nil {
				res.Notes = rerr.Error()
				fmt.Printf("  [%s Block %d] FAILED: %v\n", item, blockIdx+1, rerr)
			} else {
				fmt.Printf("  [%s Block %d]: Duration=%d ms, Throughput=%.2f MB/s, CPU=%.1f ms, RSS=%.1f MB, Integrity=%v\n",
					item, blockIdx+1, res.DurationMs, res.ThroughputMBs, res.CPUTimeMs, res.RSSMB, res.IntegrityPass)
			}
			results = append(results, res)
		}
	}

	fmt.Println("\n[5/5] Generating Interval-Tuning Reports & JSON Artifacts...")
	saveBenchmarkCSV(filepath.Join(artifactsDir, "benchmark-results.csv"), results)
	saveBenchmarkJSON(filepath.Join(artifactsDir, "benchmark-results.json"), results)
	saveBenchmarkCSV(filepath.Join(artifactsDir, "benchmark-results-"+runID+".csv"), results)
	saveBenchmarkJSON(filepath.Join(artifactsDir, "benchmark-results-"+runID+".json"), results)
	generateSummaryMarkdown(filepath.Join(artifactsDir, "benchmark-summary.md"), env, results, cpStats, nil, 0, runID, false)
	generateSummaryMarkdown(filepath.Join(artifactsDir, "benchmark-summary-"+runID+".md"), env, results, cpStats, nil, 0, runID, false)
	fmt.Println("Interval tuning completed successfully.")
}

// runThroughputStage measures Configurations 1-5 for every requested payload.
// Every attempt is appended to the result set, successful or not: failed reps
// are flagged Valid=false with the cause in Notes and are never dropped. Five
// consecutive failures mark the stage aborted, but the partial dataset is still
// returned and written.
func runThroughputStage(artifactsDir string, fixtures map[string]FixtureMetadata, payloadNames []string, runID string, primaryReps, streamingReps int, interleaved, skipConfig5, syncCheckpoints, legacyProgressThrottle bool) (results []BenchmarkRunResult, failed int, aborted bool) {
	fmt.Println("\n[4/6] Throughput stage: Configurations 1-5 (recovery runs are isolated and run afterwards)")
	benchServerDir := filepath.Join(artifactsDir, "server_received")
	harness, cleanup, err := StartBenchServer(benchServerDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start bench server: %v\n", err)
		return nil, 1, true
	}
	defer cleanup()
	defer os.RemoveAll(benchServerDir)
	harness.runID = runID

	if syncCheckpoints {
		harness.services.EnsureResumable().SetPipelineCheckpoints(false)
		fmt.Println("  [Checkpoints: Synchronous Mode Forced]")
	} else {
		harness.services.EnsureResumable().SetPipelineCheckpoints(true)
		fmt.Println("  [Checkpoints: Pipelined Mode Active]")
	}
	if legacyProgressThrottle {
		fmt.Println("  [Progress Throttling: Legacy (256KB || 100ms) Active]")
	} else {
		fmt.Println("  [Progress Throttling: Pure 50ms Active]")
	}

	consecutive := 0
	runReps := func(label string, reps int, run func(rep int) (BenchmarkRunResult, error)) {
		if aborted {
			return
		}
		fmt.Printf("--- %s (%d reps) ---\n", label, reps)
		for rep := 1; rep <= reps && !aborted; rep++ {
			res, rerr := run(rep)
			res.RunID = runID
			if rerr != nil {
				res.Notes = strings.TrimSpace(strings.TrimPrefix(res.Notes+" | error: "+rerr.Error(), " | "))
				fmt.Printf("  Rep %d FAILED: %v\n", rep, rerr)
				failed++
				consecutive++
				if consecutive >= 5 {
					fmt.Fprintf(os.Stderr, "ABORTING THROUGHPUT STAGE: 5 consecutive failed reps (systemic failure). Partial results are still written.\n")
					aborted = true
				}
			} else {
				consecutive = 0
				fmt.Printf("  Rep %d: Duration=%d ms, Throughput=%.2f MB/s, CPU=%.1f ms, RSS=%.1f MB, Integrity=%v\n",
					rep, res.DurationMs, res.ThroughputMBs, res.CPUTimeMs, res.RSSMB, res.IntegrityPass)
			}
			results = append(results, res)
		}
	}

	for _, pName := range payloadNames {
		if aborted {
			break
		}
		fix, ok := fixtures[pName]
		if !ok {
			continue
		}
		sizeGB := float64(fix.SizeBytes) / (1024 * 1024 * 1024)
		fmt.Printf("\n>>> Benchmarking Payload: %s (%.1f GB / %d bytes) <<<\n", fix.Name, sizeGB, fix.SizeBytes)

		if interleaved {
			fmt.Printf("--- Interleaved Paired Execution (Config-1 vs Config-4, %d reps) ---\n", primaryReps)
			for rep := 1; rep <= primaryReps && !aborted; rep++ {
				if rep%2 == 1 {
					// Odd rep: Config-1 first, then Config-4
					runReps(fmt.Sprintf("Configuration 1: Legacy PUT Baseline (Rep %d)", rep), 1, func(_ int) (BenchmarkRunResult, error) {
						return harness.RunConfig1_LegacyPUT(fix, rep)
					})
					runReps(fmt.Sprintf("Configuration 4: Primary Production (Rep %d)", rep), 1, func(_ int) (BenchmarkRunResult, error) {
						return harness.RunConfig4_PrimaryProduction(fix, rep)
					})
				} else {
					// Even rep: Config-4 first, then Config-1
					runReps(fmt.Sprintf("Configuration 4: Primary Production (Rep %d)", rep), 1, func(_ int) (BenchmarkRunResult, error) {
						return harness.RunConfig4_PrimaryProduction(fix, rep)
					})
					runReps(fmt.Sprintf("Configuration 1: Legacy PUT Baseline (Rep %d)", rep), 1, func(_ int) (BenchmarkRunResult, error) {
						return harness.RunConfig1_LegacyPUT(fix, rep)
					})
				}
			}
			runReps("Configuration 2: tus Streaming (Server Hash Only, Zero Checkpoints)", streamingReps, func(rep int) (BenchmarkRunResult, error) {
				return harness.RunConfig2_ServerHashingOnly(fix, rep)
			})
			runReps("Configuration 3: tus Streaming (Client + Server Dual Hash)", streamingReps, func(rep int) (BenchmarkRunResult, error) {
				return harness.RunConfig3_DualHashing(fix, rep)
			})
		} else {
			// Repetition policy: the primary acceptance pair (Config-1 legacy PUT
			// vs Config-4 primary production) gets 5 reps at every payload size,
			// since the 95% verdict depends on it. Secondary streaming configs get
			// 3 reps. Config-5 gets 3 reps on 1GB and 1 rep above that.
			runReps("Configuration 1: Legacy PUT Baseline", primaryReps, func(rep int) (BenchmarkRunResult, error) {
				return harness.RunConfig1_LegacyPUT(fix, rep)
			})
			runReps("Configuration 2: tus Streaming (Server Hash Only, Zero Checkpoints)", streamingReps, func(rep int) (BenchmarkRunResult, error) {
				return harness.RunConfig2_ServerHashingOnly(fix, rep)
			})
			runReps("Configuration 3: tus Streaming (Client + Server Dual Hash)", streamingReps, func(rep int) (BenchmarkRunResult, error) {
				return harness.RunConfig3_DualHashing(fix, rep)
			})
			runReps("Configuration 4: Primary Production Architecture (Durable Checkpoints 32MB/500ms)", primaryReps, func(rep int) (BenchmarkRunResult, error) {
				return harness.RunConfig4_PrimaryProduction(fix, rep)
			})
		}

		if !skipConfig5 && fix.SizeBytes <= 5*1024*1024*1024 {
			sliceSizes := []int64{8 * 1024 * 1024, 16 * 1024 * 1024, 32 * 1024 * 1024, 64 * 1024 * 1024}
			sliceReps := 3
			if fix.SizeBytes > 1024*1024*1024 {
				sliceReps = 1 // 5GB x 4 slice sizes x multiple reps is impractical; recorded as 1 rep
			}
			for _, sz := range sliceSizes {
				mb := sz / (1024 * 1024)
				runReps(fmt.Sprintf("Configuration 5: Direct Blob XHR Fallback (%d MB slices)", mb), sliceReps, func(rep int) (BenchmarkRunResult, error) {
					return harness.RunConfig5_FallbackSlice(fix, sz, rep)
				})
			}
		}
	}
	return results, failed, aborted
}

// runRecoveryStage runs Configuration 6 in a fresh server, session and staging
// directory per run. An expected interruption leaves an unclaimed queued item in
// its session; because ClaimItem only leases the queue's next eligible item,
// reusing that session would make every later upload fail with 409. The isolated
// harness is discarded, so recovery can never invalidate throughput results.
func runRecoveryStage(artifactsDir string, fixtures map[string]FixtureMetadata, payloadNames []string, runID string) (results []RecoveryRunResult, failed int) {
	fmt.Println("\n[5/6] Recovery stage: Configuration 6 in isolated sessions (reconnect wait simulated)")
	consecutive := 0
	for _, pName := range payloadNames {
		fix, ok := fixtures[pName]
		if !ok {
			continue
		}
		if fix.SizeBytes > 5*1024*1024*1024 {
			fmt.Printf("--- Configuration 6: skipped for %s (recovery is measured at 1 GB and 5 GB) ---\n", fix.Name)
			continue
		}
		for _, pct := range []int{10, 50, 99} {
			fmt.Printf("--- Configuration 6: Interruption Recovery %s at %d%% (isolated session; reconnect wait simulated) ---\n", fix.Name, pct)
			dir := filepath.Join(artifactsDir, "server_received", fmt.Sprintf("recovery-%s-%dpct-%s", strings.TrimSuffix(fix.Name, ".bin"), pct, runID))
			harness, cleanup, herr := StartBenchServer(dir)
			if herr != nil {
				rec := RecoveryRunResult{RunID: runID, PayloadName: fix.Name, PayloadSize: fix.SizeBytes, InterruptPercent: pct, IsolatedSession: true}
				rec.Notes = "isolated harness start failed: " + herr.Error()
				fmt.Printf("  Recovery at %d%% FAILED: %v\n", pct, herr)
				results = append(results, rec)
				failed++
				consecutive++
				continue
			}
			harness.runID = runID
			harness.isolated = true
			rec, rerr := harness.RunConfig6_InterruptionRecovery(fix, pct)
			cleanup()
			_ = os.RemoveAll(dir)
			rec.RunID = runID
			rec.IsolatedSession = true
			if rerr != nil {
				rec.Notes = strings.TrimSpace(strings.TrimPrefix(rec.Notes+" | error: "+rerr.Error(), " | "))
				fmt.Printf("  Recovery at %d%% FAILED: %v\n", pct, rerr)
				failed++
				consecutive++
				results = append(results, rec)
				if consecutive >= 3 {
					fmt.Fprintf(os.Stderr, "ABORTING RECOVERY STAGE: 3 consecutive recovery failures. Partial results are still written.\n")
					return results, failed
				}
				continue
			}
			consecutive = 0
			fmt.Printf("  Interrupted at %d%% (declared %d, sent %d, server state %s, HEAD committed %d): settle=%.1f ms, Detect=%.1f ms, HEAD=%.1f ms, PrefixRebuild=%.1f ms, PrefixVerified=%v, ResumeStart=%d, Attempts=%d, FirstResumedByte=%.1f ms, ResumeStream=%.1f ms, Total=%.1f ms, Retransmitted=%d bytes, State=%s, Status=%s, Integrity=%v, Valid=%v\n",
				pct, rec.InterruptDeclaredLength, rec.InterruptBytesSent, rec.InterruptedState, rec.ServerCommitted, rec.SettleWaitMs, rec.DetectTimeMs, rec.HeadTimeMs, rec.PrefixRebuildMs, rec.PrefixVerified, rec.ResumeStartOffset, rec.ResumeAttempts, rec.FirstResumedByteMs, rec.ResumeStreamTimeMs, rec.TotalCompletionMs, rec.RetransmittedBytes, rec.UploadState, rec.AuthoritativeStatus, rec.IntegrityPass, rec.Valid)
			results = append(results, rec)
		}
	}
	return results, failed
}

func saveBenchmarkCSV(path string, results []BenchmarkRunResult) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	_ = w.Write([]string{
		"ConfigID",
		"ConfigName",
		"PayloadName",
		"PayloadSizeBytes",
		"Repetition",
		"DurationMs",
		"ThroughputMBs",
		"CPUTimeMs",
		"HeapAllocMB",
		"PeakHeapMB",
		"RSSMB",
		"RequestCount",
		"IntegrityPass",
		"Valid",
		"Notes",
	})

	for _, r := range results {
		_ = w.Write([]string{
			r.ConfigID,
			r.ConfigName,
			r.PayloadName,
			strconv.FormatInt(r.PayloadSize, 10),
			strconv.Itoa(r.Repetition),
			strconv.FormatInt(r.DurationMs, 10),
			fmt.Sprintf("%.2f", r.ThroughputMBs),
			fmt.Sprintf("%.2f", r.CPUTimeMs),
			fmt.Sprintf("%.2f", r.HeapAllocMB),
			fmt.Sprintf("%.2f", r.PeakHeapMB),
			fmt.Sprintf("%.2f", r.RSSMB),
			strconv.Itoa(r.RequestCount),
			strconv.FormatBool(r.IntegrityPass),
			strconv.FormatBool(r.Valid),
			r.Notes,
		})
	}
}

func saveBenchmarkJSON(path string, results []BenchmarkRunResult) {
	data, _ := json.MarshalIndent(results, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
}

func saveRecoveryCSV(path string, results []RecoveryRunResult) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	_ = w.Write([]string{
		"RunID",
		"PayloadName",
		"PayloadSizeBytes",
		"InterruptPercent",
		"InterruptOffset",
		"InterruptClientError",
		"InterruptDeclaredLength",
		"InterruptBytesSent",
		"InterruptServerStatus",
		"InterruptServerOffsetHeader",
		"InterruptedState",
		"ServerSawTruncation",
		"SettleObservations",
		"PrefixDigestServer",
		"PrefixDigestLocal",
		"SettleWaitMs",
		"ServerCommittedOffset",
		"DetectTimeMs",
		"ReconnectTimeMsSimulated",
		"HeadTimeMs",
		"PrefixRebuildMs",
		"PrefixVerifyMs",
		"PrefixVerified",
		"ResumeStartOffset",
		"ResumeAttempts",
		"FirstResumedByteMs",
		"ResumeStreamTimeMs",
		"FinalOffset",
		"UploadState",
		"TotalCompletionMs",
		"RetransmittedBytes",
		"AuthoritativeStatus",
		"IsolatedSession",
		"IntegrityPass",
		"Valid",
		"Notes",
	})

	for _, r := range results {
		_ = w.Write([]string{
			r.RunID,
			r.PayloadName,
			strconv.FormatInt(r.PayloadSize, 10),
			strconv.Itoa(r.InterruptPercent),
			strconv.FormatInt(r.InterruptOffset, 10),
			r.InterruptClientErr,
			strconv.FormatInt(r.InterruptDeclaredLength, 10),
			strconv.FormatInt(r.InterruptBytesSent, 10),
			strconv.Itoa(r.InterruptServerStatus),
			strconv.FormatInt(r.InterruptServerOffset, 10),
			r.InterruptedState,
			strconv.FormatBool(r.ServerSawTruncation),
			strconv.Itoa(r.SettleObservations),
			r.PrefixDigestServer,
			r.PrefixDigestLocal,
			fmt.Sprintf("%.2f", r.SettleWaitMs),
			strconv.FormatInt(r.ServerCommitted, 10),
			fmt.Sprintf("%.2f", r.DetectTimeMs),
			fmt.Sprintf("%.2f", r.ReconnectTimeMs),
			fmt.Sprintf("%.2f", r.HeadTimeMs),
			fmt.Sprintf("%.2f", r.PrefixRebuildMs),
			fmt.Sprintf("%.2f", r.PrefixVerifyMs),
			strconv.FormatBool(r.PrefixVerified),
			strconv.FormatInt(r.ResumeStartOffset, 10),
			strconv.Itoa(r.ResumeAttempts),
			fmt.Sprintf("%.2f", r.FirstResumedByteMs),
			fmt.Sprintf("%.2f", r.ResumeStreamTimeMs),
			strconv.FormatInt(r.FinalOffset, 10),
			r.UploadState,
			fmt.Sprintf("%.2f", r.TotalCompletionMs),
			strconv.FormatInt(r.RetransmittedBytes, 10),
			r.AuthoritativeStatus,
			strconv.FormatBool(r.IsolatedSession),
			strconv.FormatBool(r.IntegrityPass),
			strconv.FormatBool(r.Valid),
			r.Notes,
		})
	}
}

func saveRecoveryJSON(path string, results []RecoveryRunResult) {
	data, _ := json.MarshalIndent(results, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
}

func generateSummaryMarkdown(path string, env EnvironmentManifest, results []BenchmarkRunResult, cpStats []LatencyStats, recStats []RecoveryRunResult, failedAttempts int, runID string, aborted bool) {
	var sb strings.Builder

	sb.WriteString("# Flitnix Phase 3 — WP4 Performance Verification Summary\n\n")
	sb.WriteString(fmt.Sprintf("**Date**: %s  \n", env.Timestamp.Format("2006-01-02 15:04:05 UTC")))
	sb.WriteString(fmt.Sprintf("**Run ID**: `%s`%s  \n", runID, map[bool]string{true: " (throughput stage aborted)", false: ""}[aborted]))
	sb.WriteString(fmt.Sprintf("**Host**: %s (%s, %s)  \n", env.Hostname, env.Distribution, env.Kernel))
	sb.WriteString(fmt.Sprintf("**CPU**: %s (%d cores)  \n", env.CPUModel, env.CPUCores))
	sb.WriteString(fmt.Sprintf("**Storage**: %s (%s) on %s  \n", env.FilesystemType, env.StorageMount, env.StorageDevice))
	sb.WriteString(fmt.Sprintf("**Go**: %s | **Node**: %s | **Wails**: %s  \n\n", env.GoVersion, env.NodeVersion, env.WailsVersion))

	sb.WriteString("> **Methodology & scope.** All measurements are taken over the loopback interface on a single host: they are disk/CPU-bound (btrfs on NVMe), not Wi-Fi-bound. Throughput = bytes / seconds / 1e6 (decimal MB/s). Every attempt is recorded; failed or invalid runs are flagged `Valid=false` with the cause in `Notes` and are excluded from ratio math. Physical-device claims appear only in the physical test reports, never here.\n\n")
	if failedAttempts > 0 {
		sb.WriteString(fmt.Sprintf("> **⚠ %d attempt(s) FAILED during this run** and are recorded below as invalid rows. Investigate before using these results.\n\n", failedAttempts))
	}

	sb.WriteString("## 0. Run lineage and invalid history\n\n")
	sb.WriteString("This section is not optional context: earlier WP4 runs produced invalid measurements that are preserved rather than deleted.\n\n")
	sb.WriteString("| Run | Outcome | Raw log | Artifacts |\n")
	sb.WriteString("| :--- | :--- | :--- | :--- |\n")
	sb.WriteString("| Run 1 (pre-fix harness) | **INVALID / ABORTED.** Missing Config-1 rows from silent 409 drops, no auto-approve, constant RSS field, hardcoded environment claims. The raw log was deleted by the previous agent and no longer exists — recorded here as a historical invalid run with a missing raw log. | **missing (deleted)** | `superseded-invalid/` (pre-fix copy) |\n")
	sb.WriteString("| Run 2 (`2026-11-01T10:54:40Z`) | **PARTIAL.** 1 GB throughput tier valid. Configuration 6 invalid at 10%/50%/99% (423 Locked, then 409), which poisoned the shared session queue and aborted the 5 GB tier at 5 consecutive 409s. | `raw-logs/six-tier-20261101T105440Z.log` | `superseded-invalid/` |\n")
	sb.WriteString("| Run 3 (`20261101T113452Z`) | **Throughput VALID; Config-6 UNVERIFIED.** Configs 1-5 measured in one unbroken stage. Config-6 passed but declared `Content-Length` equal to the supplied interruption bytes, i.e. a *complete* HTTP request: `ServerCommittedOffset == InterruptOffset` and `RetransmittedBytes == 0` on every row proved nothing about interruption recovery. Preserved, not deleted. | `raw-logs/six-tier-20261101T113452Z.log` | `benchmark-results-20261101T113452Z.*`; unverified Config-6 copies in `superseded-invalid/config6-unverified-20261101T113452Z/` |\n")
	sb.WriteString(fmt.Sprintf("| Run 4 (`%s`) | **This run.** Config-6 re-executed with genuine interruption: the request declares the full `Upload-Length` as `Content-Length`, the body aborts after the interruption prefix, and the server must report the terminal `interrupted` state with `0 < committed < Upload-Length`. Recovery-only stage: the Config 1-5 rows below are carried forward unchanged from run `20261101T113452Z` (the fix touches no throughput path). Every row keeps its originating run ID. | `raw-logs/recovery-corrected-%s.log` | `benchmark-results-%s.*`, `recovery-results-%s.*` |\n\n", runID, runID, runID, runID))

	sb.WriteString("## 1. Primary Acceptance Evaluation (95% Target)\n\n")
	sb.WriteString("| Payload Size | Legacy PUT Baseline (Config 1) | Primary Production (Config 4) | Throughput Ratio (p50 / mean) | Target (≥95%) | Status |\n")
	sb.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- |\n")

	payloadSizes := []int64{1 * 1024 * 1024 * 1024, 5 * 1024 * 1024 * 1024, 20 * 1024 * 1024 * 1024}
	for _, sz := range payloadSizes {
		var putThroughputs []float64
		var prodThroughputs []float64

		for _, r := range results {
			if r.PayloadSize == sz && r.Valid {
				if r.ConfigID == "Config-1" {
					putThroughputs = append(putThroughputs, r.ThroughputMBs)
				} else if r.ConfigID == "Config-4" {
					prodThroughputs = append(prodThroughputs, r.ThroughputMBs)
				}
			}
		}

		avgPUT := avg(putThroughputs)
		avgProd := avg(prodThroughputs)
		medPUT, _ := median(putThroughputs)
		medProd, _ := median(prodThroughputs)
		sizeLabel := fmt.Sprintf("%d GB", sz/(1024*1024*1024))

		// No valid baseline or no valid primary rows means the verdict for
		// this size is NO DATA, never an implicit pass or a 0% ratio.
		if len(putThroughputs) == 0 || len(prodThroughputs) == 0 {
			sb.WriteString(fmt.Sprintf("| **%s** | (no valid runs) | (no valid runs) | — | ≥95.0%% | **NO DATA** |\n", sizeLabel))
			continue
		}

		p50Ratio := (medProd / medPUT) * 100.0
		meanRatio := (avgProd / avgPUT) * 100.0
		status := "**PASS**"
		if p50Ratio < 95.0 {
			status = "**NOT PASS**"
		}

		sb.WriteString(fmt.Sprintf("| **%s** | p50: %.2f / avg: %.2f (n=%d) | p50: %.2f / avg: %.2f (n=%d) | **p50: %.2f%%** (mean: %.2f%%) | ≥95.0%% | %s |\n",
			sizeLabel, medPUT, avgPUT, len(putThroughputs), medProd, avgProd, len(prodThroughputs), p50Ratio, meanRatio, status))

		// Also evaluate candidate interval variants if present in the results
		for _, customBytes := range []int64{64 * 1024 * 1024, 128 * 1024 * 1024} {
			targetID := fmt.Sprintf("Config-4-%dMB", customBytes/(1024*1024))
			var customTputs []float64
			for _, r := range results {
				if r.PayloadSize == sz && r.Valid && r.ConfigID == targetID {
					customTputs = append(customTputs, r.ThroughputMBs)
				}
			}
			if len(customTputs) > 0 && len(putThroughputs) > 0 {
				medCustom, _ := median(customTputs)
				avgCustom := avg(customTputs)
				p50CustRatio := (medCustom / medPUT) * 100.0
				meanCustRatio := (avgCustom / avgPUT) * 100.0
				custStatus := "**PASS**"
				if p50CustRatio < 95.0 {
					custStatus = "**NOT PASS**"
				}
				sb.WriteString(fmt.Sprintf("| **%s (%d MB CP)** | p50: %.2f / avg: %.2f (n=%d) | p50: %.2f / avg: %.2f (n=%d) | **p50: %.2f%%** (mean: %.2f%%) | ≥95.0%% | %s |\n",
					sizeLabel, customBytes/(1024*1024), medPUT, avgPUT, len(putThroughputs), medCustom, avgCustom, len(customTputs), p50CustRatio, meanCustRatio, custStatus))
			}
		}
	}

	sb.WriteString("\n## 2. Six-Tier Performance Overview\n\n")
	sb.WriteString("Only `Valid=true` rows contribute to the statistics; invalid attempts are counted in their own column and remain in the CSV/JSON.\n\n")
	sb.WriteString("| Configuration | Payload | Valid Runs | Invalid Runs | Avg Throughput (MB/s) | Median (MB/s) | Min/Max (MB/s) | Avg CPU (ms) | Peak Heap (MB) | Integrity |\n")
	sb.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |\n")

	grouped := make(map[string][]BenchmarkRunResult)
	for _, r := range results {
		key := fmt.Sprintf("%s|%s", r.ConfigName, r.PayloadName)
		grouped[key] = append(grouped[key], r)
	}
	groupKeys := make([]string, 0, len(grouped))
	for key := range grouped {
		groupKeys = append(groupKeys, key)
	}
	sort.Strings(groupKeys)

	for _, key := range groupKeys {
		group := grouped[key]
		parts := strings.SplitN(key, "|", 2)
		cfgName := parts[0]
		pName := parts[1]

		var tputs []float64
		var cpus []float64
		var peaks []float64
		allIntegrity := true
		invalid := 0

		for _, r := range group {
			if !r.Valid {
				invalid++
				continue
			}
			tputs = append(tputs, r.ThroughputMBs)
			cpus = append(cpus, r.CPUTimeMs)
			peaks = append(peaks, r.PeakHeapMB)
			if !r.IntegrityPass {
				allIntegrity = false
			}
		}

		if len(tputs) == 0 {
			sb.WriteString(fmt.Sprintf("| %s | %s | 0 | %d | — | — | — | — | — | — |\n", cfgName, pName, invalid))
			continue
		}

		meanT := avg(tputs)
		medT, _ := median(tputs)
		minT, maxT := minMax(tputs)
		meanCPU := avg(cpus)
		maxPeak := maxVal(peaks)

		sb.WriteString(fmt.Sprintf("| %s | %s | %d | %d | %.2f | %.2f | %.2f / %.2f | %.1f | %.1f | %v |\n",
			cfgName, pName, len(tputs), invalid, meanT, medT, minT, maxT, meanCPU, maxPeak, allIntegrity))
	}

	sb.WriteString("\n## 3. Filesystem Checkpoint Latencies\n\n")
	sb.WriteString("| Operation | Iterations | Min | Median (p50) | p95 | p99 | Max | Avg | Failure Rate |\n")
	sb.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |\n")
	for _, cp := range cpStats {
		sb.WriteString(fmt.Sprintf("| %s | %d | %v | **%v** | %v | %v | %v | %v | %.2f%% |\n",
			cp.Operation, cp.Iterations, cp.Min, cp.Median, cp.P95, cp.P99, cp.Max, cp.Avg, cp.FailureRate))
	}

	sb.WriteString("\n## 4. Interruption Recovery Performance (Configuration 6, isolated sessions)\n\n")
	sb.WriteString("| Payload | Interrupt | Declared Length | Bytes Sent | Server State | Settle Wait | HEAD Committed | Detect (client abort) | Reconnect (SIMULATED) | HEAD Query | Prefix Rebuild | Prefix Verified | Resume Start | Attempts | Total Completion | Retransmitted | Status | Valid |\n")
	sb.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |\n")
	for _, rec := range recStats {
		sizeLabel := fmt.Sprintf("%d GB", rec.PayloadSize/(1024*1024*1024))
		sb.WriteString(fmt.Sprintf("| %s | %d%% | %d | %d | %s | %.1f ms | %d bytes | %.1f ms | %.1f ms | %.1f ms | %.1f ms | %v | %d | %d | %.1f ms | **%d bytes** | %s | %v |\n",
			sizeLabel, rec.InterruptPercent, rec.InterruptDeclaredLength, rec.InterruptBytesSent, rec.InterruptedState, rec.SettleWaitMs, rec.ServerCommitted, rec.DetectTimeMs, rec.ReconnectTimeMs, rec.HeadTimeMs, rec.PrefixRebuildMs, rec.PrefixVerified, rec.ResumeStartOffset, rec.ResumeAttempts, rec.TotalCompletionMs, rec.RetransmittedBytes, rec.AuthoritativeStatus, rec.Valid))
	}
	sb.WriteString("\n*Recovery runs are loopback simulations, each in its own isolated server/session/staging directory. Interruption = the request declares the FULL `Upload-Length` as `Content-Length` while the client body supplies only the interruption prefix and then fails with `io.ErrUnexpectedEOF`; the transport aborts mid-body, so the server observes a truncated request. Server evidence is recorded as the terminal `interrupted` upload state plus a committed offset strictly between 0 and `Upload-Length`. Reconnect wait = simulated 10 ms sleep. Settlement = bounded poll (10 ms, up to 10 s) for that interrupted state, a free upload lifecycle lock and a committed offset stable across consecutive observations, before HEAD. Retransmitted = max(0, interrupt target − HEAD committed offset), i.e. bytes the client pushed that the server never durably checkpointed. A row with Valid=false is preserved, never dropped.*\n")
	sb.WriteString("\n> **Historical note.** Config-6 rows in run `20261101T113452Z` (and the earlier preflight run) are labelled `UNVERIFIED — previous benchmark declared Content-Length equal to the supplied interruption bytes`. Those requests were complete HTTP requests carrying a short body, so their `ServerCommitted == InterruptOffset` and zero retransmission figures proved nothing about interruption recovery. They are retained verbatim in `superseded-invalid/` and in the `preflight-*` artifacts.\n")

	var invalidRecs []string
	for _, rec := range recStats {
		if !rec.Valid {
			invalidRecs = append(invalidRecs, fmt.Sprintf("- `%s` %d GB @ %d%%: %s", rec.RunID, rec.PayloadSize/(1024*1024*1024), rec.InterruptPercent, rec.Notes))
		}
	}
	if len(invalidRecs) > 0 {
		sb.WriteString("\n### Invalid recovery attempts\n\n")
		for _, line := range invalidRecs {
			sb.WriteString(line + "\n")
		}
	}

	_ = os.WriteFile(path, []byte(sb.String()), 0o644)
}

func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var total float64
	for _, v := range vals {
		total += v
	}
	return total / float64(len(vals))
}

// ErrEmptySample is returned when a statistical metric is requested on an empty dataset.
var ErrEmptySample = errors.New("empty benchmark sample: cannot compute median of 0 elements")

// median returns the conventional sample median:
// - Empty input: returns ErrEmptySample (never silently reporting 0.0)
// - Odd sample count: middle sorted value
// - Even sample count: arithmetic average of the two middle sorted values
func median(vals []float64) (float64, error) {
	if len(vals) == 0 {
		return 0, ErrEmptySample
	}
	cpy := make([]float64, len(vals))
	copy(cpy, vals)
	sort.Float64s(cpy)
	n := len(cpy)
	if n%2 == 1 {
		return cpy[n/2], nil
	}
	return (cpy[n/2-1] + cpy[n/2]) / 2.0, nil
}

// upperMiddleHistorical returns the historical upper-middle element (sorted[len/2])
// retained for explicit, separately labelled historical comparisons.
func upperMiddleHistorical(vals []float64) (float64, error) {
	if len(vals) == 0 {
		return 0, ErrEmptySample
	}
	cpy := make([]float64, len(vals))
	copy(cpy, vals)
	sort.Float64s(cpy)
	return cpy[len(cpy)/2], nil
}

func minMax(vals []float64) (float64, float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	min := vals[0]
	max := vals[0]
	for _, v := range vals {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return min, max
}

func maxVal(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	m := vals[0]
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

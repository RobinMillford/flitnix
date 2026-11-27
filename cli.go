package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/doctor"
	"github.com/itsyamin/flitnix/internal/events"
	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/qr"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/settings"
	"github.com/itsyamin/flitnix/internal/transfer"
	"github.com/itsyamin/flitnix/internal/version"
)

// AppVersion is the display version string, sourced authoritatively from internal/version.
var AppVersion = version.String()

func stripBackgroundFlag(args []string) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	filtered := make([]string, 0, len(args))
	filtered = append(filtered, args[0])
	startHidden := false
	for _, arg := range args[1:] {
		if arg == "--background" || arg == "-background" {
			startHidden = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered, startHidden
}

// handleCLI processes command-line flags and subcommands before GUI initialization.
// If a CLI action is handled (version, help, doctor, serve), it returns handled=true
// and the process exit code. If the application should proceed to GUI mode, it returns
// handled=false and any file/directory paths passed as initial shares.
func handleCLI(args []string, stdout, stderr io.Writer) (bool, int, []string) {
	args, _ = stripBackgroundFlag(args)
	if len(args) <= 1 {
		return false, 0, nil
	}

	sub := args[1]
	switch sub {
	case "-v", "--version", "-version", "version":
		printVersion(stdout)
		return true, 0, nil

	case "-h", "--help", "-help", "help":
		printHelp(stdout)
		return true, 0, nil

	case "doctor":
		code := runDoctorCLI(args[2:], stdout, stderr)
		return true, code, nil

	case "send":
		code := runSendCLI(args[2:], stdout, stderr)
		return true, code, nil

	case "pair":
		code := runPairCLI(args[2:], stdout, stderr)
		return true, code, nil

	case "serve":
		code := runServeCLI(args[2:], stdout, stderr)
		return true, code, nil

	case "known-hosts":
		code := runKnownHostsCLI(args[2:], stdout, stderr)
		return true, code, nil

	default:
		// Check if arguments are file or directory paths to be shared immediately
		cwd, _ := os.Getwd()
		initialShares := ExtractSharePaths(args[1:], cwd, func(raw string, err error) {
			fmt.Fprintf(stderr, "flitnix: ignoring invalid path %q: %v\n", raw, err)
		})
		return false, 0, initialShares
	}
}

func printVersion(out io.Writer) {
	fmt.Fprintf(out, "Flitnix %s (%s/%s)\n", AppVersion, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(out, "Fast, private local file transfer between desktop and phone.\n")
}

func printHelp(out io.Writer) {
	fmt.Fprintf(out, "Flitnix - Fast, private local file transfer between desktop and phone\n\n")
	fmt.Fprintf(out, "Usage:\n")
	fmt.Fprintf(out, "  flitnix [files or directories to share...]\n")
	fmt.Fprintf(out, "  flitnix send [flags] <files or directories...>\n")
	fmt.Fprintf(out, "  flitnix pair [flags] <session-url>\n")
	fmt.Fprintf(out, "  flitnix known-hosts [list | remove <hostId>]\n")
	fmt.Fprintf(out, "  flitnix doctor [--json]\n")
	fmt.Fprintf(out, "  flitnix serve [-addr <host:port>] [-dir <receive-dir>]\n")
	fmt.Fprintf(out, "  flitnix version\n\n")
	fmt.Fprintf(out, "Commands:\n")
	fmt.Fprintf(out, "  send         Send files or directories to a recipient Flitnix instance\n")
	fmt.Fprintf(out, "  pair         Pair client as a cryptographically trusted device with host\n")
	fmt.Fprintf(out, "  known-hosts  Inspect or remove pinned TLS peer identities\n")
	fmt.Fprintf(out, "  doctor       Run Connection Doctor diagnostics and report network readiness\n")
	fmt.Fprintf(out, "  serve        Run headless transfer server on loopback or network address\n")
	fmt.Fprintf(out, "  version      Display version and architecture information\n")
	fmt.Fprintf(out, "  help         Show this help text\n")
}

func runDoctorCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "Output report as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	netSrc := netenv.DefaultSource()
	report := doctor.RunDiagnostics(doctor.Params{
		NetSource:     netSrc,
		ServerRunning: false,
		ActiveIP:      "",
		ActiveIface:   "",
	})

	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		if report.OverallStatus == doctor.StatusFail {
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "Flitnix Connection Doctor Diagnostic Report\n")
	fmt.Fprintf(stdout, "Timestamp: %s\n\n", report.Timestamp.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(stdout, "%-28s %-8s %s\n", "CHECK", "STATUS", "DETAILS")
	fmt.Fprintf(stdout, "%s\n", strings.Repeat("-", 78))
	for _, item := range report.Items {
		statusStr := string(item.Status)
		fmt.Fprintf(stdout, "%-28s %-8s %s\n", item.Title, strings.ToUpper(statusStr), item.Message)
		if item.Remediation != "" && item.Status != doctor.StatusPass {
			fmt.Fprintf(stdout, "  -> Fix: %s\n", item.Remediation)
		}
	}
	fmt.Fprintf(stdout, "%s\n", strings.Repeat("-", 78))
	if report.OverallStatus == doctor.StatusFail {
		fmt.Fprintf(stdout, "\nResult: Connection issues detected.\n")
		return 1
	}
	fmt.Fprintf(stdout, "\nResult: All diagnostic checks passed.\n")
	return 0
}

func runKnownHostsCLI(args []string, stdout, stderr io.Writer) int {
	kh, err := client.LoadOrCreateKnownHosts("")
	if err != nil {
		fmt.Fprintf(stderr, "Error loading known_hosts: %v\n", err)
		return ExitLocalPathError
	}
	sub := "list"
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch sub {
	case "list", "ls":
		entries := kh.List()
		if len(entries) == 0 {
			fmt.Fprintln(stdout, "No pinned peer hosts in known_hosts.")
			return ExitSuccess
		}
		for _, e := range entries {
			addr := e.LastAddress
			if addr == "" {
				addr = "-"
			}
			fmt.Fprintf(stdout, "%s  addr=%s  name=%q  key=%s\n", e.HostID, addr, e.Name, e.PublicKey)
		}
		return ExitSuccess
	case "remove", "rm", "delete":
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			fmt.Fprintln(stderr, "Usage: flitnix known-hosts remove <hostId>")
			return ExitUsageError
		}
		targetID := strings.TrimSpace(args[1])
		if _, ok := kh.Get(targetID); !ok {
			fmt.Fprintf(stderr, "Host ID %s is not present in known_hosts.\n", targetID)
			return ExitLocalPathError
		}
		if err := kh.Remove(targetID); err != nil {
			fmt.Fprintf(stderr, "Error removing host pin: %v\n", err)
			return ExitLocalPathError
		}
		fmt.Fprintf(stdout, "Removed pinned identity for host %s. Next connection will require explicit verification.\n", targetID)
		return ExitSuccess
	default:
		fmt.Fprintln(stderr, "Usage: flitnix known-hosts [list | remove <hostId>]")
		return ExitUsageError
	}
}

func runServeCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := resolveDataDir()
	addr := fs.String("addr", server.DefaultListenAddr, "listen address (host:port)")
	receiveDir := fs.String("dir", "", "directory to store received files (default: ~/Downloads/Flitnix)")
	recoveryDir := fs.String("recovery-dir", filepath.Join(dataDir, "recovery"), "durable state directory for session and upload recovery (empty disables recovery)")
	localHTTPSFlag := fs.Bool("local-https", false, "enable opt-in Local CA HTTPS alongside plain HTTP on the same port")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: *receiveDir})
	if err != nil {
		fmt.Fprintf(stderr, "flitnix serve: %v\n", err)
		return 1
	}

	hostIdentity, err := devices.LoadOrCreateHostIdentity(filepath.Join(dataDir, "host_identity.json"))
	if err != nil {
		fmt.Fprintf(stderr, "flitnix serve: failed to load host identity: %v\n", err)
		return 1
	}
	devStore, err := devices.NewStore(filepath.Join(dataDir, "trusted_devices.json"))
	if err != nil {
		fmt.Fprintf(stderr, "flitnix serve: failed to load trusted devices: %v\n", err)
		return 1
	}
	localHTTPSMgr := server.NewLocalHTTPSManager(filepath.Join(dataDir, "local_ca"))
	localHTTPSEnabled := *localHTTPSFlag
	if !localHTTPSEnabled {
		if stStore, stErr := settings.NewStore(filepath.Join(dataDir, "settings.json")); stErr == nil {
			localHTTPSEnabled = stStore.Get().LocalHTTPSEnabled
		}
	}

	sessMgr := session.NewManager(session.Config{})
	quota := transfer.NewQuotaManager(transfer.QuotaConfig{DefaultDir: store.Dir()})
	transfers := transfer.NewService(transfer.Config{Store: store, Quota: quota})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()

	pendingBatches := make(chan pendingServeBatch, 16)
	eventsHub := events.NewHub()
	defer eventsHub.Close()

	svcs := &server.Services{
		Sessions:     sessMgr,
		Transfers:    transfers,
		Coordinator:  coord,
		Registry:     reg,
		Downloads:    dls,
		Devices:      devStore,
		HostIdentity: hostIdentity,
		LocalHTTPS:   localHTTPSMgr,
		Events:       eventsHub,
		OnBatchPending: func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
			select {
			case pendingBatches <- pendingServeBatch{sessionKey: sessionKey, batch: batch, remoteAddr: remoteAddr}:
			default:
			}
		},
	}

	srv := server.New(server.Config{
		ListenAddr:        *addr,
		Services:          svcs,
		RecoveryDir:       *recoveryDir,
		LocalHTTPSEnabled: localHTTPSEnabled,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serving := make(chan error, 1)
	go func() { serving <- srv.Serve(ctx) }()

	select {
	case err := <-serving:
		if err != nil {
			fmt.Fprintf(stderr, "flitnix serve: %v\n", err)
			return 1
		}
		return 0
	case <-srv.Ready():
		sess, err := sessMgr.Create()
		if err != nil {
			fmt.Fprintf(stderr, "flitnix serve: failed to create initial session: %v\n", err)
			return 1
		}
		transferURL := srv.URL() + "/#" + sess.Token()
		qrTerm, _ := qr.Terminal(transferURL)
		fmt.Fprintf(stdout, "Flitnix Headless Server Active\n")
		if !strings.HasPrefix(*addr, "127.") && !strings.HasPrefix(*addr, "localhost") && !strings.HasPrefix(*addr, "[::1]") {
			fmt.Fprintf(stdout, "Notice: Server is bound to %s (accessible across the local area network).\n", *addr)
		} else {
			fmt.Fprintf(stdout, "Notice: Server is bound to loopback %s (local testing only).\n", *addr)
		}
		fmt.Fprintf(stdout, "Serving on: %s (TLS 1.3 peer transport active, Host ID: %s)\n", srv.URL(), hostIdentity.HostID)
		if srv.LocalHTTPSActive() {
			fmt.Fprintf(stdout, "Local HTTPS: %s (CA: %s/local-ca.crt)\n", srv.HTTPSURL(), srv.URL())
			if mdnsURL := srv.MDNSHTTPSURL(); mdnsURL != "" {
				fmt.Fprintf(stdout, "mDNS HTTPS:  %s\n", mdnsURL)
			}
		}
		fmt.Fprintf(stdout, "Receive dir: %s\n", store.Dir())
		fmt.Fprintf(stdout, "Transfer URL: %s\n\n", transferURL)
		if qrTerm != "" {
			fmt.Fprintln(stdout, qrTerm)
		}
		fmt.Fprintf(stdout, "\nPress Ctrl+C to stop.\n")

		// Run headless terminal approval loop in background
		go runServeApprovalLoop(ctx, pendingBatches, transfers, coord, eventsHub, os.Stdin, stdout)

		serveErr := <-serving
		sessMgr.Close()
		if serveErr != nil {
			fmt.Fprintf(stderr, "flitnix serve: %v\n", serveErr)
			return 1
		}
		fmt.Fprintln(stdout, "flitnix serve: stopped cleanly")
		return 0
	}
}

type pendingServeBatch struct {
	sessionKey string
	batch      transfer.BatchView
	remoteAddr string
}

func runServeApprovalLoop(ctx context.Context, pending <-chan pendingServeBatch, transfers *transfer.Service, coord *transfer.Coordinator, eventsHub *events.Hub, in io.Reader, out io.Writer) {
	scanner := bufio.NewScanner(in)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-pending:
			if !ok {
				return
			}
			// Check if batch is still pending
			current, err := coord.GetBatch(ev.sessionKey, ev.batch.ID)
			if err != nil || current.ApprovalState != transfer.ApprovalPending || current.State != transfer.BatchStatePending {
				continue
			}

			remote := ev.remoteAddr
			if h, _, splitErr := net.SplitHostPort(remote); splitErr == nil {
				remote = h
			}

			name := ev.batch.RootName
			if name == "" && len(ev.batch.Items) > 0 {
				name = ev.batch.Items[0].DisplayName
				if len(ev.batch.Items) > 1 {
					name = fmt.Sprintf("%s (+%d more)", name, len(ev.batch.Items)-1)
				}
			}

			fmt.Fprintf(out, "\n--> Incoming transfer request from %s:\n", remote)
			fmt.Fprintf(out, "    Batch ID:    %s\n", ev.batch.ID)
			fmt.Fprintf(out, "    Type:        %s\n", ev.batch.Kind)
			fmt.Fprintf(out, "    Name:        %s\n", name)
			fmt.Fprintf(out, "    Files:       %d item(s)\n", ev.batch.TotalItemCount)
			fmt.Fprintf(out, "    Total size:  %s\n", client.FormatBytes(ev.batch.DeclaredTotalBytes))
			fmt.Fprint(out, "Approve transfer? [y/N]: ")

			var approved bool
			if scanner.Scan() {
				ans := strings.TrimSpace(scanner.Text())
				if strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
					approved = true
				}
			}

			// Re-verify batch state after waiting for user input. If the sender canceled or timed out, do not approve.
			current, err = coord.GetBatch(ev.sessionKey, ev.batch.ID)
			if err != nil || current.ApprovalState != transfer.ApprovalPending || current.State != transfer.BatchStatePending {
				fmt.Fprintf(out, "--> Batch %s was canceled or expired while waiting for approval.\n", ev.batch.ID)
				continue
			}

			// Execute decision
			var contentIDs []string
			for _, item := range ev.batch.Items {
				if cid, err := coord.ItemContentID(ev.sessionKey, ev.batch.ID, item.ID); err == nil && cid != "" {
					contentIDs = append(contentIDs, cid)
				}
			}

			if approved {
				failed := false
				for _, cid := range contentIDs {
					if _, err := transfers.Decide(cid, true); err != nil {
						failed = true
						break
					}
				}
				if !failed {
					if v, err := coord.ApproveBatch(ev.sessionKey, ev.batch.ID); err == nil {
						if eventsHub != nil {
							eventsHub.PublishPersonal(ev.sessionKey, events.TypeBatchUpdated, v)
						}
						fmt.Fprintf(out, "--> Batch %s approved.\n", ev.batch.ID)
						continue
					}
				}
				// If approval failed partway, cancel
				for _, cid := range contentIDs {
					_ = transfers.Cancel(cid)
				}
				if v, err := coord.CancelBatch(ev.sessionKey, ev.batch.ID); err == nil && eventsHub != nil {
					eventsHub.PublishPersonal(ev.sessionKey, events.TypeBatchUpdated, v)
				}
				fmt.Fprintf(out, "--> Batch %s approval failed.\n", ev.batch.ID)
			} else {
				for _, cid := range contentIDs {
					_, _ = transfers.Decide(cid, false)
				}
				if v, err := coord.RejectBatch(ev.sessionKey, ev.batch.ID); err == nil && eventsHub != nil {
					eventsHub.PublishPersonal(ev.sessionKey, events.TypeBatchUpdated, v)
				}
				fmt.Fprintf(out, "--> Batch %s declined.\n", ev.batch.ID)
			}
		}
	}
}

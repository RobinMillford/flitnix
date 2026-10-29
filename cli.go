package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/itsyamin/flitnix/internal/doctor"
	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/qr"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

const AppVersion = "v0.1.0"

// handleCLI processes command-line flags and subcommands before GUI initialization.
// If a CLI action is handled (version, help, doctor, serve), it returns handled=true
// and the process exit code. If the application should proceed to GUI mode, it returns
// handled=false and any file/directory paths passed as initial shares.
func handleCLI(args []string, stdout, stderr io.Writer) (bool, int, []string) {
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

	case "serve":
		code := runServeCLI(args[2:], stdout, stderr)
		return true, code, nil

	default:
		// Check if arguments are file or directory paths to be shared immediately
		var initialShares []string
		for _, arg := range args[1:] {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			if absPath, err := filepath.Abs(arg); err == nil {
				if _, err := os.Stat(absPath); err == nil {
					initialShares = append(initialShares, absPath)
				}
			}
		}
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
	fmt.Fprintf(out, "  flitnix doctor [--json]\n")
	fmt.Fprintf(out, "  flitnix serve [-addr <host:port>] [-dir <receive-dir>]\n")
	fmt.Fprintf(out, "  flitnix version\n\n")
	fmt.Fprintf(out, "Commands:\n")
	fmt.Fprintf(out, "  doctor    Run Connection Doctor diagnostics and report network readiness\n")
	fmt.Fprintf(out, "  serve     Run headless transfer server on loopback or network address\n")
	fmt.Fprintf(out, "  version   Display version and architecture information\n")
	fmt.Fprintf(out, "  help      Show this help text\n")
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

func runServeCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", server.DefaultListenAddr, "listen address (host:port)")
	receiveDir := fs.String("dir", "", "directory to store received files (default: ~/Downloads/Flitnix)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: *receiveDir})
	if err != nil {
		fmt.Fprintf(stderr, "flitnix serve: %v\n", err)
		return 1
	}

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()

	svcs := &server.Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
	}

	srv := server.New(server.Config{
		ListenAddr: *addr,
		Services:   svcs,
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
		fmt.Fprintf(stdout, "Serving on: %s\n", srv.URL())
		fmt.Fprintf(stdout, "Receive dir: %s\n", store.Dir())
		fmt.Fprintf(stdout, "Transfer URL: %s\n\n", transferURL)
		if qrTerm != "" {
			fmt.Fprintln(stdout, qrTerm)
		}
		fmt.Fprintf(stdout, "\nPress Ctrl+C to stop.\n")

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

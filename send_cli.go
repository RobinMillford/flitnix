package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/mattn/go-isatty"
)

// Standard CLI exit codes
const (
	ExitSuccess         = 0
	ExitUsageError      = 1
	ExitLocalPathError  = 2
	ExitConnectionError = 3
	ExitAuthError       = 4
	ExitApprovalRefused = 5
	ExitTransferError   = 6
	ExitCanceled        = 130
)

func runSendCLI(args []string, stdout, stderr io.Writer) int {
	return runSendCLIWithStdin(args, os.Stdin, stdout, stderr)
}

func runSendCLIWithStdin(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)

	toURL := fs.String("to", "", "recipient pairing URL (e.g. http://192.168.1.50:43821/#<token>, or '-' for stdin)")
	tokenFlag := fs.String("token", "", "session pairing token (optional if specified in --to fragment, '-' for stdin)")
	nameOverride := fs.String("name", "", "override top-level folder name when sending a single directory")
	timeout := fs.Duration("timeout", 5*time.Minute, "maximum duration to wait for recipient approval")
	yes := fs.Bool("y", false, "skip interactive confirmation prompt")
	fs.BoolVar(yes, "yes", false, "skip interactive confirmation prompt")
	quiet := fs.Bool("quiet", false, "suppress progress indicators and non-essential output")
	jsonOutput := fs.Bool("json", false, "output results as JSON to stdout; diagnostics to stderr")

	fs.Usage = func() {
		printSendHelp(stdout)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitSuccess
		}
		return ExitUsageError
	}

	paths := fs.Args()
	if len(paths) == 0 {
		fmt.Fprintf(stderr, "Error: no files or directories specified to send.\nRun 'flitnix send --help' for usage.\n")
		return ExitUsageError
	}

	// 1. Resolve pairing URL and secret token
	rawURL := strings.TrimSpace(*toURL)
	explicitToken := strings.TrimSpace(*tokenFlag)
	if explicitToken == "" {
		explicitToken = os.Getenv("FLITNIX_TOKEN")
	}
	if rawURL == "" {
		rawURL = os.Getenv("FLITNIX_URL")
	}

	if rawURL == "-" {
		// Read URL from stdin
		urlFromStdin, err := client.ReadHiddenInput("", stdin, os.Stderr)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading pairing URL from stdin: %v\n", err)
			return ExitUsageError
		}
		rawURL = urlFromStdin
	} else if rawURL == "" {
		// Prompt interactively if on terminal, otherwise read stdin
		stdinFd := stdin.Fd()
		if isatty.IsTerminal(stdinFd) || isatty.IsCygwinTerminal(stdinFd) {
			urlFromPrompt, err := client.ReadHiddenInput("Paste recipient pairing URL (hidden): ", stdin, os.Stderr)
			if err != nil {
				fmt.Fprintf(stderr, "Error reading pairing URL: %v\n", err)
				return ExitUsageError
			}
			rawURL = urlFromPrompt
		} else {
			urlFromStdin, err := client.ReadHiddenInput("", stdin, os.Stderr)
			if err != nil {
				fmt.Fprintf(stderr, "Error reading pairing URL from stdin: %v\n", err)
				return ExitUsageError
			}
			rawURL = urlFromStdin
		}
	}

	if strings.TrimSpace(rawURL) == "" {
		fmt.Fprintln(stderr, "Error: recipient pairing URL is required.")
		return ExitUsageError
	}

	if explicitToken == "-" {
		// Read token from stdin
		tokFromStdin, err := client.ReadHiddenInput("", stdin, os.Stderr)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading pairing token from stdin: %v\n", err)
			return ExitUsageError
		}
		explicitToken = tokFromStdin
	} else if !strings.Contains(rawURL, "#") && explicitToken == "" {
		// URL provided without token fragment: prompt interactively if on terminal
		stdinFd := stdin.Fd()
		if isatty.IsTerminal(stdinFd) || isatty.IsCygwinTerminal(stdinFd) {
			tokFromPrompt, err := client.ReadHiddenInput("Paste recipient pairing token (hidden): ", stdin, os.Stderr)
			if err != nil {
				fmt.Fprintf(stderr, "Error reading pairing token: %v\n", err)
				return ExitUsageError
			}
			explicitToken = tokFromPrompt
		}
	}

	// Security notice: token passed as CLI argument can leak into process listing or shell history
	tokenInFlags := (*tokenFlag != "" && *tokenFlag != "-") || (*toURL != "" && strings.Contains(*toURL, "#"))
	if tokenInFlags && !*quiet && !*jsonOutput {
		fmt.Fprintln(stderr, "Notice: Pairing token passed via command-line arguments; this may be exposed in process listings (ps) or shell history.")
		fmt.Fprintln(stderr, "        To protect secrets, consider using interactive hidden prompt, '--to -', '--token -', or FLITNIX_TOKEN.")
	}

	// 2. Parse target information (strictly isolates token)
	target, err := client.ParseTarget(rawURL, explicitToken)
	if err != nil {
		if errors.Is(err, client.ErrMissingToken) {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return ExitAuthError
		}
		if errors.Is(err, client.ErrUnsupportedIPv6) {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return ExitUsageError
		}
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return ExitUsageError
	}

	// 3. Inspect local paths and construct batch manifest
	localManifest, err := client.BuildManifest(paths, *nameOverride)
	if err != nil {
		fmt.Fprintf(stderr, "Error preparing files: %v\n", err)
		return ExitLocalPathError
	}

	// 4. Initialize client and verify server reachability
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli := client.NewClient(target)

	pingCtx, pingCancel := context.WithTimeout(ctx, 4*time.Second)
	pingErr := cli.Ping(pingCtx)
	pingCancel()
	if pingErr != nil {
		fmt.Fprintf(stderr, "Error: cannot reach recipient at %s: %v\n", target.Host, pingErr)
		return ExitConnectionError
	}

	// 5. Display transfer plan
	if !*quiet && !*jsonOutput {
		fmt.Fprintf(stdout, "Recipient: %s\n", target.Host)
		if localManifest.Manifest.Kind == "folder" {
			fmt.Fprintf(stdout, "Folder:    %s (%d files, %s)\n", localManifest.Manifest.RootName, len(localManifest.LocalItems), client.FormatBytes(localManifest.TotalBytes))
		} else {
			fmt.Fprintf(stdout, "Files:     %d file(s) (%s)\n", len(localManifest.LocalItems), client.FormatBytes(localManifest.TotalBytes))
		}
	}

	stdinFd := stdin.Fd()
	if !*yes && !*jsonOutput && (isatty.IsTerminal(stdinFd) || isatty.IsCygwinTerminal(stdinFd)) {
		fmt.Fprint(stdout, "Proceed with transfer? [Y/n]: ")
		var resp string
		_, _ = fmt.Fscanln(stdin, &resp)
		resp = strings.TrimSpace(resp)
		if resp != "" && !strings.EqualFold(resp, "y") && !strings.EqualFold(resp, "yes") {
			fmt.Fprintln(stdout, "Transfer canceled by user.")
			return ExitSuccess
		}
	}

	// 6. Declare batch
	view, err := cli.DeclareBatch(ctx, localManifest.Manifest)
	if err != nil {
		if errors.Is(err, client.ErrUnauthorized) || errors.Is(err, client.ErrSessionExpired) {
			fmt.Fprintf(stderr, "Error: recipient rejected session token (%v)\n", err)
			return ExitAuthError
		}
		fmt.Fprintf(stderr, "Error declaring transfer batch: %v\n", err)
		return ExitConnectionError
	}

	// 7. Wait for recipient approval
	if !*quiet && !*jsonOutput {
		fmt.Fprintf(stdout, "Waiting for recipient approval (timeout: %s)... ", timeout.String())
	}

	approvalCtx, approvalCancel := context.WithTimeout(ctx, *timeout)
	approvedView, err := cli.WaitForApproval(approvalCtx, view.ID, 350*time.Millisecond)
	approvalCancel()
	if err != nil {
		if !*quiet && !*jsonOutput {
			fmt.Fprintln(stdout)
		}

		// Cancel the declared batch on recipient server to avoid leaving a lingering pending batch
		cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = cli.CancelBatch(cancelCtx, view.ID, view.Items)
		cancelCancel()

		if errors.Is(err, client.ErrRejected) {
			fmt.Fprintln(stderr, "Transfer rejected by recipient.")
			return ExitApprovalRefused
		}
		if errors.Is(err, client.ErrApprovalTimeout) {
			fmt.Fprintf(stderr, "Approval timed out after %s.\n", timeout.String())
			return ExitApprovalRefused
		}
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "\nTransfer canceled by user.")
			return ExitCanceled
		}
		if errors.Is(err, client.ErrUnauthorized) || errors.Is(err, client.ErrSessionExpired) {
			fmt.Fprintf(stderr, "Session expired or revoked on recipient.\n")
			return ExitAuthError
		}
		fmt.Fprintf(stderr, "Approval error: %v\n", err)
		return ExitConnectionError
	}

	if !*quiet && !*jsonOutput {
		fmt.Fprintln(stdout, "Approved!")
	}

	// 8. Stream batch items sequentially
	startTime := time.Now()
	tracker := client.NewSpeedTracker()
	results := make([]client.ItemResult, 0, len(approvedView.Items))
	var totalTransferred int64

	for i, item := range approvedView.Items {
		select {
		case <-ctx.Done():
			cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = cli.CancelBatch(cancelCtx, approvedView.ID, approvedView.Items)
			cancelCancel()
			fmt.Fprintln(stderr, "\nTransfer canceled.")
			return ExitCanceled
		default:
		}

		localItem := localManifest.LocalItems[i]
		fileName := item.DisplayName
		if item.RelativePath != "" {
			fileName = item.RelativePath
		}

		if !*quiet && !*jsonOutput {
			fmt.Fprintf(stdout, "[%d/%d] %s (%s)... ", i+1, len(approvedView.Items), fileName, client.FormatBytes(item.DeclaredSize))
		}

		res, streamErr := cli.StreamItem(ctx, approvedView.ID, item.ID, localItem.Path, func(written, total int64) {
			tracker.Update(totalTransferred + written)
		})

		if streamErr != nil {
			if !*quiet && !*jsonOutput {
				fmt.Fprintln(stdout, "FAILED")
			}
			if errors.Is(streamErr, context.Canceled) {
				cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = cli.CancelBatch(cancelCtx, approvedView.ID, approvedView.Items)
				cancelCancel()
				fmt.Fprintln(stderr, "\nTransfer canceled.")
				return ExitCanceled
			}
			if errors.Is(streamErr, client.ErrIntegrityMismatch) {
				fmt.Fprintf(stderr, "\nSecurity Error: %v\n", streamErr)
				return ExitTransferError
			}
			fmt.Fprintf(stderr, "\nError streaming %s: %v\n", fileName, streamErr)
			return ExitTransferError
		}

		if !*quiet && !*jsonOutput {
			fmt.Fprintf(stdout, "Done (SHA-256: %s...)\n", res.SHA256[:10])
		}

		results = append(results, res)
		totalTransferred += res.Written
	}

	elapsed := time.Since(startTime)
	avgSpeed := tracker.OverallSpeed(totalTransferred)

	// 9. Output final summary
	if *jsonOutput {
		type itemJSON struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			SHA256   string `json:"sha256"`
			Verified bool   `json:"verified"`
		}
		itemsList := make([]itemJSON, 0, len(results))
		for i, res := range results {
			name := localManifest.LocalItems[i].ManifestItem.DisplayName
			if localManifest.LocalItems[i].ManifestItem.RelativePath != "" {
				name = localManifest.LocalItems[i].ManifestItem.RelativePath
			}
			itemsList = append(itemsList, itemJSON{
				Name:     name,
				Size:     res.Written,
				SHA256:   res.SHA256,
				Verified: res.Verified,
			})
		}
		outData := map[string]any{
			"status":         "success",
			"batchId":        approvedView.ID,
			"kind":           string(approvedView.Kind),
			"totalItems":     len(results),
			"totalBytes":     totalTransferred,
			"elapsedSeconds": elapsed.Seconds(),
			"speedBps":       avgSpeed,
			"verified":       true,
			"items":          itemsList,
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(outData)
		return ExitSuccess
	}

	if !*quiet {
		fmt.Fprintf(stdout, "\nTransfer Complete: %d/%d file(s) sent successfully (%s in %s, %s).\n",
			len(results), len(approvedView.Items),
			client.FormatBytes(totalTransferred),
			elapsed.Round(10*time.Millisecond).String(),
			client.FormatSpeed(avgSpeed),
		)
		fmt.Fprintln(stdout, "All transferred files verified against recipient SHA-256 digests.")
	}

	return ExitSuccess
}

func printSendHelp(out io.Writer) {
	fmt.Fprintf(out, "Flitnix CLI Sender — Send files and directories over the local network\n\n")
	fmt.Fprintf(out, "Usage:\n")
	fmt.Fprintf(out, "  flitnix send [flags] <files or directories...>\n\n")
	fmt.Fprintf(out, "Flags:\n")
	fmt.Fprintf(out, "  --to <url>          Recipient pairing URL (e.g. http://192.168.1.50:43821/#<token>)\n")
	fmt.Fprintf(out, "                      Pass '-' to read URL from standard input.\n")
	fmt.Fprintf(out, "                      If omitted, prompt interactively for hidden input.\n")
	fmt.Fprintf(out, "  --token <secret>    Pairing token (optional if specified in --to fragment, '-' for stdin)\n")
	fmt.Fprintf(out, "  --name <string>     Folder name override when sending a single directory\n")
	fmt.Fprintf(out, "  --timeout <dur>     Approval timeout waiting for recipient (default: 5m)\n")
	fmt.Fprintf(out, "  -y, --yes           Skip confirmation prompt and proceed immediately\n")
	fmt.Fprintf(out, "  --quiet             Suppress progress indicators and non-essential output\n")
	fmt.Fprintf(out, "  --json              Output structured JSON result to stdout\n")
	fmt.Fprintf(out, "  -h, --help          Show this help text\n\n")
	fmt.Fprintf(out, "Exit Codes:\n")
	fmt.Fprintf(out, "  0    Success (all files transferred and SHA-256 verified)\n")
	fmt.Fprintf(out, "  1    Syntax or usage error\n")
	fmt.Fprintf(out, "  2    Local file error (not found, unreadable, special file, empty folder)\n")
	fmt.Fprintf(out, "  3    Connection error (recipient unreachable or connection refused)\n")
	fmt.Fprintf(out, "  4    Authorization error (invalid token or expired session)\n")
	fmt.Fprintf(out, "  5    Recipient rejected transfer or approval timed out\n")
	fmt.Fprintf(out, "  6    Transfer interrupted mid-stream or SHA-256 mismatch\n")
	fmt.Fprintf(out, "  130  Transfer canceled by user (Ctrl+C)\n\n")
	fmt.Fprintf(out, "Examples:\n")
	fmt.Fprintf(out, "  flitnix send photo.jpg\n")
	fmt.Fprintf(out, "  flitnix send --to \"http://192.168.1.50:43821/#<token>\" document.pdf\n")
	fmt.Fprintf(out, "  flitnix send --to - ./project-src/ < url.txt\n")
	fmt.Fprintf(out, "  flitnix send --to \"http://[2001:db8::1]:43821/#<token>\" file1.txt file2.txt\n")
}

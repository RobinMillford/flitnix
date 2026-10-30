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

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/mattn/go-isatty"
)

func runPairCLI(args []string, stdout, stderr io.Writer) int {
	return runPairCLIWithStdin(args, os.Stdin, stdout, stderr)
}

func runPairCLIWithStdin(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)

	toURL := fs.String("to", "", "recipient pairing URL (e.g. http://192.168.1.50:43821/#<token>, or '-' for stdin)")
	tokenFlag := fs.String("token", "", "session pairing token (optional if specified in --to fragment, '-' for stdin)")
	deviceName := fs.String("name", "", "device name to enroll under on host desktop")
	yes := fs.Bool("y", false, "skip interactive SAS confirmation prompt")
	fs.BoolVar(yes, "yes", false, "skip interactive SAS confirmation prompt")
	quiet := fs.Bool("quiet", false, "suppress progress indicators and non-essential output")
	jsonOutput := fs.Bool("json", false, "output results as JSON")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitSuccess
		}
		return ExitUsageError
	}

	rawURL := strings.TrimSpace(*toURL)
	if rawURL == "" && len(fs.Args()) > 0 {
		rawURL = strings.TrimSpace(fs.Args()[0])
	}

	explicitToken := strings.TrimSpace(*tokenFlag)
	if explicitToken == "" {
		explicitToken = os.Getenv("FLITNIX_TOKEN")
	}
	if rawURL == "" {
		rawURL = os.Getenv("FLITNIX_URL")
	}

	if rawURL == "" {
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

	target, err := client.ParseTarget(rawURL, explicitToken)
	if err != nil {
		fmt.Fprintf(stderr, "Error: invalid target URL or token: %v\n", err)
		return ExitUsageError
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clientID, err := client.LoadOrCreateClientIdentity("")
	if err != nil {
		fmt.Fprintf(stderr, "Error loading client identity: %v\n", err)
		return ExitLocalPathError
	}

	kh, err := client.LoadOrCreateKnownHosts("")
	if err != nil {
		fmt.Fprintf(stderr, "Error loading known hosts: %v\n", err)
		return ExitLocalPathError
	}

	if !*quiet && !*jsonOutput {
		fmt.Fprintf(stdout, "Initiating cryptographic pairing with host at %s...\n", target.Host)
	}

	confirmSAS := func(sas string, isPinned bool) (bool, error) {
		if !isPinned {
			// First-contact host! Neither --yes nor --json may silently trust an unpinned host.
			if *yes {
				return false, client.ErrFirstContactSASRequired
			}
			if *jsonOutput {
				return false, client.ErrInteractionRequired
			}
		} else {
			// Previously pinned host: TOFU verified; safe to bypass prompt with --yes or --json
			if *yes || *jsonOutput {
				return true, nil
			}
		}

		fmt.Fprintln(stdout, "\n========================================")
		fmt.Fprintf(stdout, " Short Authentication String (SAS): [ %s ]\n", sas)
		fmt.Fprintln(stdout, "========================================")
		if !isPinned {
			fmt.Fprintln(stdout, "First contact with this host. Verify that this exact 6-digit code")
			fmt.Fprintln(stdout, "matches the code shown on the host computer.")
		} else {
			fmt.Fprintln(stdout, "Verify that this exact 6-digit code matches the host display.")
		}
		fmt.Fprint(stdout, "Do the codes match? [y/N]: ")

		var resp string
		_, err := fmt.Fscanln(stdin, &resp)
		if err != nil {
			return false, nil
		}
		resp = strings.TrimSpace(resp)
		return strings.EqualFold(resp, "y") || strings.EqualFold(resp, "yes"), nil
	}

	res, err := client.Pair(ctx, target, *deviceName, clientID, kh, confirmSAS)
	if err != nil {
		if *jsonOutput {
			errCode := "pairing_failed"
			errMsg := err.Error()
			switch {
			case errors.Is(err, client.ErrInteractionRequired), errors.Is(err, client.ErrFirstContactSASRequired):
				errCode = "interaction_required"
				errMsg = "First-contact pairing with an unpinned host requires interactive visual SAS verification. Run flitnix pair interactively without --json or --yes."
			case errors.Is(err, client.ErrHostMismatch):
				errCode = "host_mismatch"
			case errors.Is(err, client.ErrUnauthorized):
				errCode = "unauthorized"
			case errors.Is(err, client.ErrPairingAborted):
				errCode = "pairing_aborted"
			}
			errJSON, _ := json.Marshal(map[string]any{
				"success": false,
				"error":   errCode,
				"message": errMsg,
			})
			fmt.Fprintln(stdout, string(errJSON))
			if errors.Is(err, client.ErrPairingAborted) {
				return ExitSuccess
			}
			return ExitAuthError
		}

		switch {
		case errors.Is(err, client.ErrFirstContactSASRequired):
			fmt.Fprintf(stderr, "Error: first-contact pairing requires interactive visual SAS verification to protect against active MITM attacks.\nRemove --yes or -y to verify the 6-digit SAS code interactively.\n")
			return ExitAuthError
		case errors.Is(err, client.ErrInteractionRequired):
			fmt.Fprintf(stderr, "Error: first-contact pairing requires interactive visual SAS verification. Cannot pair non-interactively with an unpinned host.\n")
			return ExitAuthError
		case errors.Is(err, client.ErrPairingAborted):
			if !*quiet {
				fmt.Fprintln(stdout, "\nPairing aborted by user (SAS mismatch or canceled).")
			}
			return ExitSuccess
		case errors.Is(err, client.ErrHostMismatch):
			fmt.Fprintf(stderr, "\nCRITICAL SECURITY ALERT: %v\n", err)
			return ExitAuthError
		case errors.Is(err, client.ErrUnauthorized):
			fmt.Fprintf(stderr, "Error: session token was rejected by host.\n")
			return ExitAuthError
		default:
			fmt.Fprintf(stderr, "Error: pairing failed: %v\n", err)
			return ExitTransferError
		}
	}

	if *jsonOutput {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return ExitSuccess
	}

	if !*quiet {
		fmt.Fprintln(stdout, "\n✓ Successfully paired device with host!")
		fmt.Fprintf(stdout, "  Device ID:       %s\n", res.DeviceID)
		fmt.Fprintf(stdout, "  Host ID:         %s\n", res.HostID)
		fmt.Fprintf(stdout, "  Verified SAS:    %s\n", res.SASDisplay)
		fmt.Fprintf(stdout, "  Pinned Key:      %s\n", res.HostPublicKey)
		fmt.Fprintln(stdout, "  Status:          Device enrolled in host's trusted devices store.")
	}

	return ExitSuccess
}

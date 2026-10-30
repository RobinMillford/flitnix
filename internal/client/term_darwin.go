//go:build darwin

package client

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/unix"
)

// ReadHiddenInput prompts the user and reads a line from in without echoing to terminal.
// If in is not a terminal, it reads a line normally from in without prompting or touching termios.
func ReadHiddenInput(prompt string, in *os.File, out *os.File) (string, error) {
	fd := in.Fd()
	if !isatty.IsTerminal(fd) && !isatty.IsCygwinTerminal(fd) {
		scanner := bufio.NewScanner(in)
		if scanner.Scan() {
			return strings.TrimSpace(scanner.Text()), nil
		}
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("unexpected EOF on stdin")
	}

	if prompt != "" {
		fmt.Fprint(out, prompt)
	}

	termios, err := unix.IoctlGetTermios(int(fd), unix.TIOCGETA)
	if err != nil {
		scanner := bufio.NewScanner(in)
		if scanner.Scan() {
			return strings.TrimSpace(scanner.Text()), nil
		}
		return "", scanner.Err()
	}

	newState := *termios
	newState.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(int(fd), unix.TIOCSETA, &newState); err != nil {
		scanner := bufio.NewScanner(in)
		if scanner.Scan() {
			return strings.TrimSpace(scanner.Text()), nil
		}
		return "", scanner.Err()
	}
	defer func() {
		_ = unix.IoctlSetTermios(int(fd), unix.TIOCSETA, termios)
		fmt.Fprintln(out)
	}()

	scanner := bufio.NewScanner(in)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text()), nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("unexpected EOF on stdin")
}

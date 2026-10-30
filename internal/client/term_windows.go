//go:build windows

package client

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/windows"
)

// ReadHiddenInput prompts the user and reads a line from in without echoing to terminal.
// If in is not a terminal, it reads a line normally from in without prompting or touching console mode.
func ReadHiddenInput(prompt string, in *os.File, out *os.File) (string, error) {
	fd := int(in.Fd())
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

	handle := windows.Handle(in.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		scanner := bufio.NewScanner(in)
		if scanner.Scan() {
			return strings.TrimSpace(scanner.Text()), nil
		}
		return "", scanner.Err()
	}

	newMode := mode &^ windows.ENABLE_ECHO_INPUT
	if err := windows.SetConsoleMode(handle, newMode); err != nil {
		scanner := bufio.NewScanner(in)
		if scanner.Scan() {
			return strings.TrimSpace(scanner.Text()), nil
		}
		return "", scanner.Err()
	}
	defer func() {
		_ = windows.SetConsoleMode(handle, mode)
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

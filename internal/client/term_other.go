//go:build !linux && !darwin && !windows

package client

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ReadHiddenInput prompts the user and reads a line from in.
// Fallback implementation for unsupported platforms reads line normally without terminal echo suppression.
func ReadHiddenInput(prompt string, in *os.File, out *os.File) (string, error) {
	if prompt != "" {
		fmt.Fprint(out, prompt)
	}
	scanner := bufio.NewScanner(in)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text()), nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("unexpected EOF on stdin")
}

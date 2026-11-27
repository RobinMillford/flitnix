//go:build linux

package clipboard

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
)

type linuxBackend struct {
	tool string // "wl" (Wayland), "xclip", "xsel", or ""
}

func newPlatformBackend() Backend {
	isWayland := os.Getenv("WAYLAND_DISPLAY") != ""
	if isWayland {
		if _, err := exec.LookPath("wl-paste"); err == nil {
			if _, err2 := exec.LookPath("wl-copy"); err2 == nil {
				return &linuxBackend{tool: "wl"}
			}
		}
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		return &linuxBackend{tool: "xclip"}
	}
	if _, err := exec.LookPath("wl-paste"); err == nil {
		if _, err2 := exec.LookPath("wl-copy"); err2 == nil {
			return &linuxBackend{tool: "wl"}
		}
	}
	if _, err := exec.LookPath("xsel"); err == nil {
		return &linuxBackend{tool: "xsel"}
	}
	return &linuxBackend{}
}

func (b *linuxBackend) Capabilities() Capabilities {
	switch b.tool {
	case "wl":
		return Capabilities{
			Supported:           true,
			BackendName:         "wayland (wl-clipboard)",
			ConcealedFilterSafe: true,
		}
	case "xclip":
		return Capabilities{
			Supported:           true,
			BackendName:         "x11 (xclip)",
			ConcealedFilterSafe: true,
		}
	case "xsel":
		return Capabilities{
			Supported:           true,
			BackendName:         "x11 (xsel)",
			ConcealedFilterSafe: false,
			UnsupportedReason:   "xsel does not expose password-manager MIME targets; install xclip or wl-clipboard for target filtering",
		}
	default:
		return Capabilities{
			Supported:           false,
			BackendName:         "none",
			ConcealedFilterSafe: false,
			UnsupportedReason:   "No supported Linux clipboard tool (wl-clipboard or xclip) found; desktop OS clipboard watching disabled",
		}
	}
}

func isConcealedLinuxTargets(targetsRaw string) bool {
	lower := strings.ToLower(targetsRaw)
	for _, hint := range []string{
		"x-kde-passwordmanagerhint",
		"application/x-password",
		"application/x-keepass",
		"x-special/gnome-copied-files",
	} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func (b *linuxBackend) Read(ctx context.Context) (Snapshot, error) {
	switch b.tool {
	case "wl":
		typesOut, err := exec.CommandContext(ctx, "wl-paste", "--list-types").Output()
		if err != nil {
			return Snapshot{}, nil
		}
		if isConcealedLinuxTargets(string(typesOut)) {
			return Snapshot{Concealed: true, Reason: "password manager hint"}, nil
		}
		out, err := exec.CommandContext(ctx, "wl-paste", "--no-newline", "--type", "text/plain;charset=utf-8").Output()
		if err != nil {
			out, err = exec.CommandContext(ctx, "wl-paste", "--no-newline").Output()
			if err != nil {
				return Snapshot{}, nil
			}
		}
		return Snapshot{Text: string(out)}, nil
	case "xclip":
		targetsOut, err := exec.CommandContext(ctx, "xclip", "-o", "-selection", "clipboard", "-t", "TARGETS").Output()
		if err == nil && isConcealedLinuxTargets(string(targetsOut)) {
			return Snapshot{Concealed: true, Reason: "password manager hint"}, nil
		}
		out, err := exec.CommandContext(ctx, "xclip", "-o", "-selection", "clipboard", "-t", "UTF8_STRING").Output()
		if err != nil {
			out, err = exec.CommandContext(ctx, "xclip", "-o", "-selection", "clipboard").Output()
			if err != nil {
				return Snapshot{}, nil
			}
		}
		return Snapshot{Text: string(out)}, nil
	case "xsel":
		out, err := exec.CommandContext(ctx, "xsel", "-ob").Output()
		if err != nil {
			return Snapshot{}, nil
		}
		return Snapshot{Text: string(out)}, nil
	default:
		return Snapshot{}, ErrDisabled
	}
}

func (b *linuxBackend) WriteText(ctx context.Context, text string) error {
	switch b.tool {
	case "wl":
		cmd := exec.CommandContext(ctx, "wl-copy", "--type", "text/plain;charset=utf-8")
		cmd.Stdin = bytes.NewBufferString(text)
		return cmd.Run()
	case "xclip":
		cmd := exec.CommandContext(ctx, "xclip", "-i", "-selection", "clipboard", "-t", "UTF8_STRING")
		cmd.Stdin = bytes.NewBufferString(text)
		return cmd.Run()
	case "xsel":
		cmd := exec.CommandContext(ctx, "xsel", "-ib")
		cmd.Stdin = bytes.NewBufferString(text)
		return cmd.Run()
	default:
		return ErrDisabled
	}
}

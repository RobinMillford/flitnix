//go:build windows

package clipboard

import (
	"context"
	"os/exec"
	"strings"
)

type windowsBackend struct{}

func newPlatformBackend() Backend {
	return &windowsBackend{}
}

func (b *windowsBackend) Capabilities() Capabilities {
	return Capabilities{
		Supported:           true,
		BackendName:         "windows (Clipboard API)",
		ConcealedFilterSafe: true,
	}
}

const windowsReadScript = `
Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$do = [System.Windows.Forms.Clipboard]::GetDataObject()
if ($null -eq $do) { exit 0 }
$formats = $do.GetFormats()
if ($formats -contains 'ExcludeClipboardContentFromMonitorProcessing') {
  Write-Output 'FLITNIX_CONCEALED'
  exit 0
}
if ($formats -contains 'CanIncludeInClipboardHistory') {
  try {
    $ms = $do.GetData('CanIncludeInClipboardHistory')
    if ($ms -is [System.IO.MemoryStream] -and $ms.Length -ge 4) {
      $buf = $ms.ToArray()
      if ([BitConverter]::ToInt32($buf, 0) -eq 0) {
        Write-Output 'FLITNIX_CONCEALED'
        exit 0
      }
    }
  } catch {}
}
if ([System.Windows.Forms.Clipboard]::ContainsText()) {
  [Console]::Out.Write([System.Windows.Forms.Clipboard]::GetText())
}
`

func (b *windowsBackend) Read(ctx context.Context) (Snapshot, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", windowsReadScript)
	out, err := cmd.Output()
	if err != nil {
		return Snapshot{}, nil
	}
	s := string(out)
	if strings.TrimSpace(s) == "FLITNIX_CONCEALED" {
		return Snapshot{Concealed: true, Reason: "Windows ExcludeClipboardContentFromMonitorProcessing"}, nil
	}
	return Snapshot{Text: s}, nil
}

func (b *windowsBackend) WriteText(ctx context.Context, text string) error {
	script := `
[Console]::InputEncoding = [System.Text.Encoding]::UTF8
$txt = [Console]::In.ReadToEnd()
Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.Clipboard]::SetText($txt)
`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

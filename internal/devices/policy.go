package devices

import (
	"fmt"
	"path/filepath"
	"strings"
)

// DefaultMaxAutoApproveBytes is the default per-file and per-batch size limit
// (100 MiB) for conditional automatic approval when a device has AutoApprove enabled.
const DefaultMaxAutoApproveBytes int64 = 100 * 1024 * 1024

// FileRiskClass categorizes an incoming file conservatively by filename extension.
type FileRiskClass string

const (
	RiskSafe       FileRiskClass = "safe"
	RiskExecutable FileRiskClass = "executable"
	RiskScript     FileRiskClass = "script"
)

// DevicePolicy defines granular conditional auto-approval rules for a paired device.
type DevicePolicy struct {
	AutoApprove         bool  `json:"autoApprove"`
	MaxAutoApproveBytes int64 `json:"maxAutoApproveBytes"` // 0 = default (100 MiB), <0 = unlimited
	AllowExecutables    bool  `json:"allowExecutables"`
	AllowScripts        bool  `json:"allowScripts"`
}

// ApprovalCandidate represents one incoming file evaluated against acceptance policy.
type ApprovalCandidate struct {
	Name string
	Size int64
}

// ApprovalDecision is the authoritative outcome of evaluating a transfer request
// against visibility state, device trust, file-risk classification, and size limits.
type ApprovalDecision struct {
	AutoApproved         bool
	RequiresManualReason string
	RiskyFiles           []string
}

var executableExtensions = map[string]struct{}{
	".exe":      {},
	".msi":      {},
	".msp":      {},
	".msu":      {},
	".com":      {},
	".scr":      {},
	".pif":      {},
	".app":      {},
	".dmg":      {},
	".pkg":      {},
	".deb":      {},
	".rpm":      {},
	".apk":      {},
	".ipa":      {},
	".jar":      {},
	".war":      {},
	".ear":      {},
	".cpl":      {},
	".msc":      {},
	".hta":      {},
	".gadget":   {},
	".lnk":      {},
	".url":      {},
	".scf":      {},
	".reg":      {},
	".dll":      {},
	".sys":      {},
	".drv":      {},
	".ocx":      {},
	".so":       {},
	".dylib":    {},
	".elf":      {},
	".out":      {},
	".run":      {},
	".appimage": {},
	".iso":      {},
	".img":      {},
	".vhd":      {},
	".vhdx":     {},
	".action":   {},
	".workflow": {},
}

var scriptExtensions = map[string]struct{}{
	".bat":         {},
	".cmd":         {},
	".vbs":         {},
	".vbe":         {},
	".js":          {},
	".jse":         {},
	".mjs":         {},
	".cjs":         {},
	".wsf":         {},
	".wsh":         {},
	".ps1":         {},
	".psm1":        {},
	".psd1":        {},
	".ps1xml":      {},
	".sh":          {},
	".bash":        {},
	".zsh":         {},
	".fish":        {},
	".csh":         {},
	".ksh":         {},
	".command":     {},
	".py":          {},
	".pyw":         {},
	".pyc":         {},
	".rb":          {},
	".pl":          {},
	".php":         {},
	".lua":         {},
	".tcl":         {},
	".applescript": {},
	".scpt":        {},
}

// ClassifyFileRisk conservatively classifies a filename by extension without
// trusting any client-supplied MIME type. Trailing spaces/dots are stripped
// before inspecting the extension so obfuscated names like "payload.exe ."
// cannot evade detection.
func ClassifyFileRisk(filename string) (FileRiskClass, string) {
	base := filepath.Base(strings.ReplaceAll(filename, `\`, "/"))
	clean := strings.TrimRight(strings.TrimSpace(base), " .")
	if clean == "" {
		return RiskSafe, ""
	}
	ext := strings.ToLower(filepath.Ext(clean))
	if ext == "" {
		return RiskSafe, ""
	}
	if _, ok := executableExtensions[ext]; ok {
		return RiskExecutable, ext
	}
	if _, ok := scriptExtensions[ext]; ok {
		return RiskScript, ext
	}
	return RiskSafe, ext
}

// EffectiveMaxBytes resolves the maximum byte threshold for auto-approval.
// Returns 0 when unlimited (<0 configured), otherwise a positive byte cap.
func (p DevicePolicy) EffectiveMaxBytes() int64 {
	if p.MaxAutoApproveBytes < 0 {
		return 0
	}
	if p.MaxAutoApproveBytes == 0 {
		return DefaultMaxAutoApproveBytes
	}
	return p.MaxAutoApproveBytes
}

// EvaluateAutoApproval decides whether an incoming transfer may be auto-approved.
// Unknown devices NEVER auto-approve. Even when a trusted device has AutoApprove
// enabled, executables, scripts, and transfers exceeding the size cap require
// explicit manual approval unless explicitly permitted by that device's policy.
func EvaluateAutoApproval(dev *Device, vis VisibilityMode, items []ApprovalCandidate) ApprovalDecision {
	if dev == nil {
		return ApprovalDecision{
			AutoApproved:         false,
			RequiresManualReason: "Unknown device requires manual approval",
		}
	}
	if vis == VisibilityHidden {
		return ApprovalDecision{
			AutoApproved:         false,
			RequiresManualReason: "Visibility is Hidden; manual approval required",
		}
	}
	if !dev.AutoApprove {
		return ApprovalDecision{
			AutoApproved:         false,
			RequiresManualReason: "Manual approval configured for this device",
		}
	}

	policy := dev.EffectivePolicy()
	maxBytes := policy.EffectiveMaxBytes()
	var totalBytes int64
	var execRisk, scriptRisk []string

	for _, item := range items {
		if item.Size > 0 {
			totalBytes += item.Size
		}
		risk, ext := ClassifyFileRisk(item.Name)
		switch risk {
		case RiskExecutable:
			if !policy.AllowExecutables {
				execRisk = append(execRisk, fmt.Sprintf("%s (%s)", item.Name, ext))
			}
		case RiskScript:
			if !policy.AllowScripts {
				scriptRisk = append(scriptRisk, fmt.Sprintf("%s (%s)", item.Name, ext))
			}
		}
		if maxBytes > 0 && item.Size > maxBytes {
			return ApprovalDecision{
				AutoApproved:         false,
				RequiresManualReason: fmt.Sprintf("File %s exceeds auto-approve size limit (%s)", item.Name, formatLimitBytes(maxBytes)),
			}
		}
	}

	if len(execRisk) > 0 {
		return ApprovalDecision{
			AutoApproved:         false,
			RequiresManualReason: fmt.Sprintf("Executable file requires explicit approval: %s", execRisk[0]),
			RiskyFiles:           execRisk,
		}
	}
	if len(scriptRisk) > 0 {
		return ApprovalDecision{
			AutoApproved:         false,
			RequiresManualReason: fmt.Sprintf("Script file requires explicit approval: %s", scriptRisk[0]),
			RiskyFiles:           scriptRisk,
		}
	}
	if maxBytes > 0 && totalBytes > maxBytes {
		return ApprovalDecision{
			AutoApproved:         false,
			RequiresManualReason: fmt.Sprintf("Total transfer size exceeds auto-approve limit (%s)", formatLimitBytes(maxBytes)),
		}
	}

	return ApprovalDecision{AutoApproved: true}
}

func formatLimitBytes(b int64) string {
	const (
		mib = 1024 * 1024
		gib = 1024 * mib
	)
	if b >= gib && b%gib == 0 {
		return fmt.Sprintf("%d GiB", b/gib)
	}
	if b >= mib {
		return fmt.Sprintf("%d MiB", b/mib)
	}
	return fmt.Sprintf("%d B", b)
}

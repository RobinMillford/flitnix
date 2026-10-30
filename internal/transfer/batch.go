package transfer

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// BatchDirection identifies which side supplies the file payload.
type BatchDirection string

const (
	DirectionPhoneToDesktop BatchDirection = "phone_to_desktop"
	DirectionDesktopToPhone BatchDirection = "desktop_to_phone"
)

// BatchKind identifies the manifest layout.
type BatchKind string

const (
	BatchKindFiles  BatchKind = "files"
	BatchKindFolder BatchKind = "folder"
)

// BatchState is the aggregate lifecycle state of a batch.
type BatchState string

const (
	BatchStateQueued       BatchState = "queued"
	BatchStatePending      BatchState = "pending"
	BatchStateApproved     BatchState = "approved"
	BatchStateTransferring BatchState = "transferring"
	BatchStateCompleted    BatchState = "completed"
	BatchStateFailed       BatchState = "failed"
	BatchStateCanceled     BatchState = "canceled"
	BatchStateRejected     BatchState = "rejected"
)

// ApprovalState records the explicit receiver decision independently from
// the payload lifecycle.
type ApprovalState string

const (
	ApprovalPending  ApprovalState = "pending"
	ApprovalApproved ApprovalState = "approved"
	ApprovalRejected ApprovalState = "rejected"
)

// ItemState is the lifecycle state of one ordered payload.
type ItemState string

const (
	ItemStateQueued       ItemState = "queued"
	ItemStatePending      ItemState = "pending"
	ItemStateApproved     ItemState = "approved"
	ItemStateTransferring ItemState = "transferring"
	ItemStateCompleted    ItemState = "completed"
	ItemStateFailed       ItemState = "failed"
	ItemStateCanceled     ItemState = "canceled"
	ItemStateRejected     ItemState = "rejected"
)

// TransferBatch is the internal batch record. SessionKey and the item
// SourcePath fields are intentionally absent from BatchView.
type TransferBatch struct {
	ID                 string
	SessionKey         string
	Direction          BatchDirection
	Kind               BatchKind
	RootName           string
	Items              []TransferItem
	TotalItemCount     int
	DeclaredTotalBytes int64
	TransferredBytes   int64
	State              BatchState
	ApprovalState      ApprovalState
	CreatedAt          time.Time
	StartedAt          time.Time
	CompletedAt        time.Time
	Error              string
}

// TransferItem is the internal ordered item record.
type TransferItem struct {
	ID           string
	BatchID      string
	Sequence     int
	RelativePath string
	DisplayName  string
	DeclaredSize int64
	Transferred  int64
	State        ItemState
	SHA256       string
	Error        string
	SourcePath   string
	ContentID    string
}

// BatchManifest is the pure control-plane input validated before a batch is
// accepted. IDs and sequences are optional for client manifests and can be
// checked once the server assigns them.
type BatchManifest struct {
	Direction BatchDirection `json:"direction"`
	Kind      BatchKind      `json:"kind"`
	RootName  string         `json:"rootName,omitempty"`
	Items     []ManifestItem `json:"items"`
}

type ManifestItem struct {
	ID           string `json:"id,omitempty"`
	Sequence     int    `json:"sequence,omitempty"`
	RelativePath string `json:"relativePath,omitempty"`
	DisplayName  string `json:"displayName,omitempty"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256,omitempty"`
}

// ManifestLimits bounds metadata retained in memory. The defaults are
// intentionally sized for useful folders rather than tiny request bodies.
type ManifestLimits struct {
	MaxItems             int
	MaxManifestPathBytes int
	MaxTotalBytes        int64
}

var DefaultManifestLimits = ManifestLimits{
	MaxItems:             5000,
	MaxManifestPathBytes: MaxRelativePathBytes,
	MaxTotalBytes:        math.MaxInt64,
}

// ValidatedManifest is the canonical, server-ready form of a manifest.
type ValidatedManifest struct {
	BatchManifest
	TotalItemCount int
	TotalBytes     int64
}

var (
	ErrInvalidBatch   = errors.New("transfer: invalid batch manifest")
	ErrInvalidBatchID = errors.New("transfer: invalid batch ID")
)

// ValidateManifest validates and canonicalizes a manifest without reading the
// filesystem or trusting a client aggregate total.
func ValidateManifest(manifest BatchManifest) (ValidatedManifest, error) {
	return ValidateManifestWithLimits(manifest, DefaultManifestLimits)
}

func ValidateManifestWithLimits(manifest BatchManifest, limits ManifestLimits) (ValidatedManifest, error) {
	if limits.MaxItems <= 0 || limits.MaxManifestPathBytes <= 0 || limits.MaxTotalBytes < 0 {
		return ValidatedManifest{}, fmt.Errorf("%w: invalid validation limits", ErrInvalidBatch)
	}
	if !validDirection(manifest.Direction) || !validKind(manifest.Kind) {
		return ValidatedManifest{}, fmt.Errorf("%w: unsupported direction or kind", ErrInvalidBatch)
	}
	if len(manifest.Items) == 0 || len(manifest.Items) > limits.MaxItems {
		return ValidatedManifest{}, fmt.Errorf("%w: item count must be between 1 and %d", ErrInvalidBatch, limits.MaxItems)
	}

	out := ValidatedManifest{BatchManifest: manifest}
	out.Items = nil
	if manifest.Kind == BatchKindFolder {
		root, err := sanitizeRootName(manifest.RootName)
		if err != nil {
			return ValidatedManifest{}, err
		}
		out.RootName = root
	} else if manifest.RootName != "" {
		return ValidatedManifest{}, fmt.Errorf("%w: root name is only valid for folder batches", ErrInvalidBatch)
	}

	seenIDs := make(map[string]struct{}, len(manifest.Items))
	seenPaths := make(map[string]struct{}, len(manifest.Items))
	hasIDs := false
	hasNonzeroSequence := false
	for _, item := range manifest.Items {
		if item.ID != "" {
			hasIDs = true
			if _, exists := seenIDs[item.ID]; exists {
				return ValidatedManifest{}, fmt.Errorf("%w: duplicate item ID %q", ErrInvalidBatch, item.ID)
			}
			seenIDs[item.ID] = struct{}{}
		}
		if item.Sequence != 0 {
			hasNonzeroSequence = true
		}
		if item.Size < 0 {
			return ValidatedManifest{}, fmt.Errorf("%w: item size must not be negative", ErrInvalidBatch)
		}
		if item.Size > limits.MaxTotalBytes-out.TotalBytes {
			return ValidatedManifest{}, fmt.Errorf("%w: declared total size overflows or exceeds the limit", ErrInvalidBatch)
		}
		out.TotalBytes += item.Size

		switch manifest.Kind {
		case BatchKindFolder:
			path, err := SanitizeRelativePath(item.RelativePath)
			if err != nil {
				return ValidatedManifest{}, err
			}
			if len(path) > limits.MaxManifestPathBytes {
				return ValidatedManifest{}, fmt.Errorf("%w: relative path exceeds manifest limit", ErrInvalidBatch)
			}
			if _, exists := seenPaths[path]; exists {
				return ValidatedManifest{}, fmt.Errorf("%w: duplicate normalized relative path %q", ErrInvalidBatch, path)
			}
			seenPaths[path] = struct{}{}
			if item.DisplayName != "" {
				if _, err := SanitizeName(item.DisplayName); err != nil {
					return ValidatedManifest{}, fmt.Errorf("%w: invalid display name: %v", ErrInvalidBatch, err)
				}
			}
			item.RelativePath = path
			item.DisplayName = path[strings.LastIndex(path, "/")+1:]
		case BatchKindFiles:
			if item.RelativePath != "" {
				return ValidatedManifest{}, fmt.Errorf("%w: flat collections must not contain relative paths", ErrInvalidBatch)
			}
			name, err := SanitizeName(item.DisplayName)
			if err != nil {
				return ValidatedManifest{}, fmt.Errorf("%w: invalid display name: %v", ErrInvalidBatch, err)
			}
			item.DisplayName = name
		}
		out.Items = append(out.Items, item)
	}
	if hasIDs || hasNonzeroSequence {
		for i, item := range out.Items {
			if item.ID == "" || item.Sequence != i {
				return ValidatedManifest{}, fmt.Errorf("%w: assigned items must have unique IDs and contiguous sequences", ErrInvalidBatch)
			}
		}
	}
	out.TotalItemCount = len(out.Items)
	return out, nil
}

func sanitizeRootName(raw string) (string, error) {
	root, err := SanitizeName(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%w: invalid folder root: %v", ErrInvalidBatch, err)
	}
	if err := validateRelativePathComponent(root); err != nil {
		return "", fmt.Errorf("%w: invalid folder root: %v", ErrInvalidBatch, err)
	}
	return root, nil
}

func validDirection(direction BatchDirection) bool {
	return direction == DirectionPhoneToDesktop || direction == DirectionDesktopToPhone
}

func validKind(kind BatchKind) bool {
	return kind == BatchKindFiles || kind == BatchKindFolder
}

// NewOpaqueID returns a cryptographically random, 128-bit opaque ID.
func NewOpaqueID() (string, error) {
	return randHex(16)
}

// BatchView and ItemView are phone-safe projections. They deliberately do not
// contain session ownership keys, bearer tokens, or absolute source paths.
type BatchView struct {
	ID                 string         `json:"id"`
	Direction          BatchDirection `json:"direction"`
	Kind               BatchKind      `json:"kind"`
	RootName           string         `json:"rootName,omitempty"`
	Items              []ItemView     `json:"items"`
	TotalItemCount     int            `json:"totalItemCount"`
	DeclaredTotalBytes int64          `json:"declaredTotalBytes"`
	TransferredBytes   int64          `json:"transferredBytes"`
	State              BatchState     `json:"state"`
	ApprovalState      ApprovalState  `json:"approvalState"`
	CreatedAt          time.Time      `json:"createdAt"`
	StartedAt          time.Time      `json:"startedAt,omitempty"`
	CompletedAt        time.Time      `json:"completedAt,omitempty"`
	Error              string         `json:"error,omitempty"`
}

type ItemView struct {
	ID           string    `json:"id"`
	BatchID      string    `json:"batchId"`
	Sequence     int       `json:"sequence"`
	RelativePath string    `json:"relativePath,omitempty"`
	DisplayName  string    `json:"displayName"`
	DeclaredSize int64     `json:"declaredSize"`
	Transferred  int64     `json:"transferred"`
	State        ItemState `json:"state"`
	SHA256       string    `json:"sha256,omitempty"`
	Error        string    `json:"error,omitempty"`
}

func (b TransferBatch) View() BatchView {
	items := make([]ItemView, 0, len(b.Items))
	for _, item := range b.Items {
		items = append(items, item.View())
	}
	return BatchView{
		ID: b.ID, Direction: b.Direction, Kind: b.Kind, RootName: b.RootName,
		Items: items, TotalItemCount: b.TotalItemCount,
		DeclaredTotalBytes: b.DeclaredTotalBytes, TransferredBytes: b.TransferredBytes,
		State: b.State, ApprovalState: b.ApprovalState, CreatedAt: b.CreatedAt,
		StartedAt: b.StartedAt, CompletedAt: b.CompletedAt, Error: b.Error,
	}
}

func (i TransferItem) View() ItemView {
	return ItemView{
		ID: i.ID, BatchID: i.BatchID, Sequence: i.Sequence,
		RelativePath: i.RelativePath, DisplayName: i.DisplayName,
		DeclaredSize: i.DeclaredSize, Transferred: i.Transferred,
		State: i.State, SHA256: i.SHA256, Error: i.Error,
	}
}

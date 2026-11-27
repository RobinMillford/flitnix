package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	stdlibRuntime "runtime"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/itsyamin/flitnix/internal/events"
	"github.com/itsyamin/flitnix/internal/history"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// HistoryRecordView projects persisted history safely to the desktop frontend
// without exposing raw local filesystem paths.
type HistoryRecordView struct {
	ID          string    `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Direction   string    `json:"direction"`
	Kind        string    `json:"kind,omitempty"`
	DisplayName string    `json:"displayName"`
	ItemCount   int       `json:"itemCount"`
	Size        int64     `json:"size"`
	Transferred int64     `json:"transferred"`
	Status      string    `json:"status"`
	DeviceID    string    `json:"deviceId,omitempty"`
	DeviceName  string    `json:"deviceName,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	Error       string    `json:"error,omitempty"`
	BatchID     string    `json:"batchId,omitempty"`
	CanOpen     bool      `json:"canOpen"`
	CanReveal   bool      `json:"canReveal"`
	CanReshare  bool      `json:"canReshare"`
}

// HistoryFilterView carries search and filter options from the UI.
type HistoryFilterView struct {
	Query     string `json:"query"`
	Direction string `json:"direction"`
	Status    string `json:"status"`
	Device    string `json:"device"`
	DateRange string `json:"dateRange"` // "", "all", "24h", "7d", "30d"
	Limit     int    `json:"limit"`
	Offset    int    `json:"offset"`
}

// HistoryQueryResultView bundles paginated history records and summary stats.
type HistoryQueryResultView struct {
	Records        []HistoryRecordView `json:"records"`
	Total          int                 `json:"total"`
	Stats          history.Stats       `json:"stats"`
	StoreSizeBytes int64               `json:"storeSizeBytes"`
}

var execHistoryCommand = func(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

var revealLinuxViaDBus = func(filePath string) bool {
	if stdlibRuntime.GOOS != "linux" || os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return false
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()

	abs, err := filepath.Abs(filePath)
	if err != nil {
		return false
	}
	u := (&url.URL{Scheme: "file", Path: abs}).String()
	call := conn.Object("org.freedesktop.FileManager1", "/org/freedesktop/FileManager1").Call(
		"org.freedesktop.FileManager1.ShowItems",
		0,
		[]string{u},
		"",
	)
	return call.Err == nil
}

// QueryTransferHistory returns filtered, paginated persisted personal transfer history.
func (a *App) QueryTransferHistory(filter HistoryFilterView) (HistoryQueryResultView, error) {
	if a.historyStore == nil {
		return HistoryQueryResultView{Records: []HistoryRecordView{}}, nil
	}
	var since time.Time
	now := time.Now().UTC()
	switch strings.ToLower(strings.TrimSpace(filter.DateRange)) {
	case "24h":
		since = now.Add(-24 * time.Hour)
	case "7d":
		since = now.Add(-7 * 24 * time.Hour)
	case "30d":
		since = now.Add(-30 * 24 * time.Hour)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 25
	}

	res := a.historyStore.Query(history.Filter{
		Query:     filter.Query,
		Direction: filter.Direction,
		Status:    filter.Status,
		Device:    filter.Device,
		Since:     since,
		Limit:     limit,
		Offset:    filter.Offset,
	})

	views := make([]HistoryRecordView, 0, len(res.Records))
	for _, rec := range res.Records {
		views = append(views, a.historyRecordView(rec))
	}
	return HistoryQueryResultView{
		Records:        views,
		Total:          res.Total,
		Stats:          res.Stats,
		StoreSizeBytes: res.StoreSizeBytes,
	}, nil
}

func (a *App) historyRecordView(rec history.Record) HistoryRecordView {
	canAct := false
	if rec.Status == history.StatusCompleted && rec.LocalPath != "" {
		if _, _, err := a.resolveSafeHistoryPath(rec); err == nil {
			canAct = true
		}
	}
	return HistoryRecordView{
		ID:          rec.ID,
		Timestamp:   rec.Timestamp,
		Direction:   rec.Direction,
		Kind:        rec.Kind,
		DisplayName: rec.DisplayName,
		ItemCount:   rec.ItemCount,
		Size:        rec.Size,
		Transferred: rec.Transferred,
		Status:      rec.Status,
		DeviceID:    rec.DeviceID,
		DeviceName:  rec.DeviceName,
		SHA256:      rec.SHA256,
		Error:       rec.Error,
		BatchID:     rec.BatchID,
		CanOpen:     canAct,
		CanReveal:   canAct,
		CanReshare:  canAct,
	}
}

// ClearTransferHistory clears all personal history metadata without deleting files on disk.
func (a *App) ClearTransferHistory() error {
	if a.historyStore == nil {
		return nil
	}
	if err := a.historyStore.Clear(); err != nil {
		return err
	}
	if a.events != nil {
		a.events.PublishDesktop(events.TypeHistoryUpdated, map[string]string{"action": "cleared"})
	}
	return nil
}

// ApplyHistoryRetentionNow prunes transfer history metadata exceeding configured
// age/count retention limits without touching any files on disk.
func (a *App) ApplyHistoryRetentionNow() (int, error) {
	if a.historyStore == nil || a.settingsStore == nil {
		return 0, nil
	}
	s := a.settingsStore.Get()
	var maxAge time.Duration
	if s.HistoryRetentionDays > 0 {
		maxAge = time.Duration(s.HistoryRetentionDays) * 24 * time.Hour
	}
	removed, err := a.historyStore.ApplyRetention(maxAge, s.HistoryMaxRecords)
	if err != nil {
		return removed, err
	}
	if removed > 0 && a.events != nil {
		a.events.PublishDesktop(events.TypeHistoryUpdated, map[string]any{
			"action":  "pruned",
			"removed": removed,
		})
	}
	return removed, nil
}

// DeleteHistoryRecord deletes one history entry without removing any user file.
func (a *App) DeleteHistoryRecord(id string) error {
	if a.historyStore == nil {
		return nil
	}
	if err := a.historyStore.Delete(id); err != nil {
		return err
	}
	if a.events != nil {
		a.events.PublishDesktop(events.TypeHistoryUpdated, map[string]string{"action": "deleted"})
	}
	return nil
}

// OpenHistoryFile opens the verified file or folder associated with a completed history entry.
func (a *App) OpenHistoryFile(id string) error {
	if a.historyStore == nil {
		return errors.New("flitnix: history store unavailable")
	}
	rec, ok := a.historyStore.Get(id)
	if !ok {
		return errors.New("flitnix: history record not found")
	}
	safePath, _, err := a.resolveSafeHistoryPath(rec)
	if err != nil {
		return err
	}
	switch stdlibRuntime.GOOS {
	case "windows":
		return execHistoryCommand("explorer.exe", safePath)
	case "darwin":
		return execHistoryCommand("open", safePath)
	default:
		return execHistoryCommand("xdg-open", safePath)
	}
}

// RevealHistoryFile highlights the file in the OS file manager where supported, or opens its parent folder.
func (a *App) RevealHistoryFile(id string) error {
	if a.historyStore == nil {
		return errors.New("flitnix: history store unavailable")
	}
	rec, ok := a.historyStore.Get(id)
	if !ok {
		return errors.New("flitnix: history record not found")
	}
	safePath, _, err := a.resolveSafeHistoryPath(rec)
	if err != nil {
		return err
	}
	switch stdlibRuntime.GOOS {
	case "windows":
		return execHistoryCommand("explorer.exe", "/select,"+safePath)
	case "darwin":
		return execHistoryCommand("open", "-R", safePath)
	default:
		if revealLinuxViaDBus(safePath) {
			return nil
		}
		parent := filepath.Dir(safePath)
		return execHistoryCommand("xdg-open", parent)
	}
}

// ReshareHistoryFile stages the file or folder from history back into Personal Transfer shared sources.
func (a *App) ReshareHistoryFile(id string) error {
	if a.historyStore == nil {
		return errors.New("flitnix: history store unavailable")
	}
	rec, ok := a.historyStore.Get(id)
	if !ok {
		return errors.New("flitnix: history record not found")
	}
	safePath, _, err := a.resolveSafeHistoryPath(rec)
	if err != nil {
		return err
	}
	_, err = a.AddDroppedSources([]string{safePath})
	return err
}

func (a *App) revealTransferByTargetID(targetID string) error {
	if a.historyStore != nil && targetID != "" {
		res := a.historyStore.Query(history.Filter{Limit: 100})
		for _, rec := range res.Records {
			if rec.BatchID == targetID || rec.TransferID == targetID || rec.ID == targetID {
				if rec.LocalPath != "" {
					if err := a.RevealHistoryFile(rec.ID); err == nil {
						return nil
					}
				}
				break
			}
		}
	}
	return a.OpenDownloadFolder()
}

func (a *App) resolveSafeHistoryPath(rec history.Record) (string, bool, error) {
	if strings.TrimSpace(rec.LocalPath) == "" {
		return "", false, errors.New("flitnix: no file path recorded for this entry")
	}
	cleaned := filepath.Clean(rec.LocalPath)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", false, fmt.Errorf("flitnix: file no longer accessible on disk: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, fmt.Errorf("flitnix: file unavailable: %w", err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", false, errors.New("flitnix: refusing to open special filesystem node")
	}

	// Incoming transfers must strictly reside within the effective Flitnix receive directory
	// after resolving all symlinks, preventing traversal or symlink-escape abuse.
	if rec.Direction == history.DirectionIncoming || rec.Direction == history.DirectionBatch {
		if a.store == nil {
			return "", false, errors.New("flitnix: receive store unavailable")
		}
		recvRoot := a.store.Dir()
		resolvedRoot, rErr := filepath.EvalSymlinks(recvRoot)
		if rErr != nil {
			resolvedRoot = filepath.Clean(recvRoot)
		}
		if !isSubpathOrSame(resolvedRoot, resolved) {
			return "", false, errors.New("flitnix: refusing to access path outside receive directory")
		}
	}
	return resolved, info.IsDir(), nil
}

func isSubpathOrSame(root, candidate string) bool {
	absRoot, err1 := filepath.Abs(root)
	absCand, err2 := filepath.Abs(candidate)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absCand)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (a *App) recordHistoryFromEvent(ev events.DesktopEvent) {
	if a.historyStore == nil {
		return
	}

	switch ev.Type {
	case events.TypeBatchUpdated:
		bv, ok := decodeEventPayload[BatchView](ev.RawPayload)
		if !ok || !isTerminalState(bv.State) {
			return
		}
		name := bv.RootName
		if name == "" && len(bv.Items) > 0 {
			name = bv.Items[0].DisplayName
		}
		localPath := a.resolveBatchPublishedPath(bv)
		devName := bv.DeviceName
		if devName == "" && bv.DeviceID != "" && a.devicesStore != nil {
			if dev, exists := a.devicesStore.Get(bv.DeviceID); exists {
				devName = dev.Name
			}
		}
		var firstSHA string
		if len(bv.Items) == 1 {
			firstSHA = bv.Items[0].SHA256
		}
		if _, added := a.historyStore.Record(history.Record{
			DedupKey:    "batch:" + bv.ID,
			BatchID:     bv.ID,
			Timestamp:   time.Now().UTC(),
			Direction:   history.DirectionBatch,
			Kind:        bv.Kind,
			DisplayName: name,
			ItemCount:   max(1, bv.TotalItemCount),
			Size:        bv.DeclaredTotalBytes,
			Transferred: bv.TransferredBytes,
			Status:      bv.State,
			DeviceID:    bv.DeviceID,
			DeviceName:  devName,
			SHA256:      firstSHA,
			Error:       bv.Error,
			LocalPath:   localPath,
		}); added && a.events != nil {
			a.events.PublishDesktop(events.TypeHistoryUpdated, map[string]string{"id": bv.ID})
		}

	case events.TypeUploadUpdated:
		uv, ok := decodeEventPayload[UploadView](ev.RawPayload)
		if !ok || !isTerminalState(uv.State) || a.requireStandalone(uv.ID) != nil {
			return
		}
		localPath := ""
		if u, exists := a.transfers.Get(uv.ID); exists {
			localPath = u.Published
		}
		devName := uv.DeviceName
		if devName == "" && uv.DeviceID != "" && a.devicesStore != nil {
			if dev, exists := a.devicesStore.Get(uv.DeviceID); exists {
				devName = dev.Name
			}
		}
		if _, added := a.historyStore.Record(history.Record{
			DedupKey:    "upload:" + uv.ID,
			TransferID:  uv.ID,
			Timestamp:   time.Now().UTC(),
			Direction:   history.DirectionIncoming,
			Kind:        "file",
			DisplayName: uv.Name,
			ItemCount:   1,
			Size:        uv.Size,
			Transferred: uv.Uploaded,
			Status:      uv.State,
			DeviceID:    uv.DeviceID,
			DeviceName:  devName,
			SHA256:      uv.SHA256,
			Error:       uv.Reason,
			LocalPath:   localPath,
		}); added && a.events != nil {
			a.events.PublishDesktop(events.TypeHistoryUpdated, map[string]string{"id": uv.ID})
		}

	case events.TypeDownloadUpdated:
		dv, ok := decodeEventPayload[DownloadView](ev.RawPayload)
		if !ok {
			return
		}
		var status string
		switch dv.State {
		case string(transfer.DownloadSent):
			status = history.StatusCompleted
		case string(transfer.DownloadFailed):
			status = history.StatusFailed
		case string(transfer.DownloadCanceled):
			status = history.StatusCanceled
		default:
			return
		}
		localPath := ""
		if a.registry != nil {
			if sf, ok := a.registry.Get(dv.FileID); ok {
				localPath = sf.Path
			} else if src, ok := a.registry.GetSource(dv.FileID); ok {
				localPath = src.Path
			}
		}
		if _, added := a.historyStore.Record(history.Record{
			DedupKey:    "download:" + dv.ID,
			TransferID:  dv.ID,
			Timestamp:   time.Now().UTC(),
			Direction:   history.DirectionOutgoing,
			Kind:        dv.Kind,
			DisplayName: dv.Name,
			ItemCount:   1,
			Size:        dv.Size,
			Transferred: dv.Written,
			Status:      status,
			SHA256:      dv.SHA256,
			Error:       dv.Reason,
			LocalPath:   localPath,
		}); added && a.events != nil {
			a.events.PublishDesktop(events.TypeHistoryUpdated, map[string]string{"id": dv.ID})
		}

	case events.TypePeerTransfer:
		job, ok := decodeEventPayload[PeerTransferJob](ev.RawPayload)
		if !ok {
			return
		}
		if job.State != history.StatusCompleted && job.State != history.StatusFailed && job.State != history.StatusCanceled {
			return
		}
		// RawPayload preserves the internal PeerTransferJob struct in memory when published directly.
		srcPath := ""
		if direct, ok := ev.RawPayload.(PeerTransferJob); ok {
			srcPath = direct.SourcePath
		}
		name := job.RootName
		if name == "" {
			name = "Desktop Transfer"
		}
		if _, added := a.historyStore.Record(history.Record{
			DedupKey:    "peer:" + job.ID,
			TransferID:  job.ID,
			Timestamp:   time.Now().UTC(),
			Direction:   history.DirectionPeerOutgoing,
			Kind:        job.Kind,
			DisplayName: name,
			ItemCount:   max(1, job.TotalItems),
			Size:        job.TotalBytes,
			Transferred: job.SentBytes,
			Status:      job.State,
			DeviceID:    job.PeerHostID,
			DeviceName:  job.PeerName,
			Error:       job.Error,
			LocalPath:   srcPath,
		}); added && a.events != nil {
			a.events.PublishDesktop(events.TypeHistoryUpdated, map[string]string{"id": job.ID})
		}
	}
}

func (a *App) resolveBatchPublishedPath(bv BatchView) string {
	if a.store == nil || a.coordinator == nil || a.transfers == nil {
		return ""
	}
	if bv.Kind == string(transfer.BatchKindFolder) && bv.RootName != "" {
		cand := filepath.Join(a.store.Dir(), bv.RootName)
		if info, err := os.Stat(cand); err == nil && info.IsDir() {
			return cand
		}
	}
	for _, key := range a.coordinator.SessionKeys() {
		b, err := a.coordinator.GetBatch(key, bv.ID)
		if err != nil {
			continue
		}
		for _, item := range b.Items {
			cid, cErr := a.coordinator.ItemContentID(key, b.ID, item.ID)
			if cErr != nil || cid == "" {
				continue
			}
			if u, ok := a.transfers.Get(cid); ok && u.Published != "" {
				if len(b.Items) == 1 {
					return u.Published
				}
				return filepath.Dir(u.Published)
			}
		}
	}
	return ""
}

func isTerminalState(st string) bool {
	switch st {
	case history.StatusCompleted, history.StatusFailed, history.StatusCanceled, history.StatusRejected, history.StatusDiscarded:
		return true
	default:
		return false
	}
}

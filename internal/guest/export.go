package guest

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNoCompletedUploads = errors.New("guest: room has no completed uploads to export")
	ErrFileChangedMidExport = errors.New("guest: file changed during export")
)

type ManifestItem struct {
	SubmissionID     string            `json:"submissionId"`
	GuestID          string            `json:"guestId"`
	GuestName        string            `json:"guestName"`
	ParticipantID    string            `json:"participantId,omitempty"`
	ParticipantName  string            `json:"participantName,omitempty"`
	OriginalFileName string            `json:"originalFileName"`
	ExportedPath     string            `json:"exportedPath"`
	SizeBytes        int64             `json:"sizeBytes"`
	SHA256           string            `json:"sha256"`
	SubmittedAt      time.Time         `json:"submittedAt"`
	FormValues       map[string]string `json:"formValues,omitempty"`
}

type Manifest struct {
	RoomID      string         `json:"roomId"`
	RoomTitle   string         `json:"roomTitle"`
	ExportedAt  time.Time      `json:"exportedAt"`
	TotalFiles  int            `json:"totalFiles"`
	TotalBytes  int64          `json:"totalBytes"`
	Submissions []ManifestItem `json:"submissions"`
}

type ExportResult struct {
	TargetPath string `json:"targetPath"`
	TotalFiles int    `json:"totalFiles"`
	TotalBytes int64  `json:"totalBytes"`
}

func sanitizeZipPath(p string) string {
	cleaned := filepath.Clean(filepath.ToSlash(p))
	cleaned = strings.TrimPrefix(cleaned, "/")
	cleaned = strings.TrimPrefix(cleaned, "../")
	for strings.HasPrefix(cleaned, "../") {
		cleaned = strings.TrimPrefix(cleaned, "../")
	}
	return cleaned
}

func ExportRoomToZip(room *Room, destZipPath string) (*ExportResult, error) {
	if room == nil {
		return nil, ErrRoomNotFound
	}

	room.mu.RLock()
	roomTitle := room.Title
	if roomTitle == "" {
		roomTitle = "Flitnix Collection"
	}
	roomID := room.ID
	roomFolder := room.RoomFolder

	// Collect completed uploads
	type itemExport struct {
		up       UploadRequest
		guest    Guest
		partName string
	}
	var toExport []itemExport
	for _, u := range room.uploads {
		if u.State == UploadCompleted {
			g, ok := room.guests[u.GuestID]
			if ok {
				partName := ""
				if u.ParticipantID != "" {
					if p, ok := room.roster[u.ParticipantID]; ok {
						partName = p.Name
					}
				}
				toExport = append(toExport, itemExport{
					up:       *u,
					guest:    *g,
					partName: partName,
				})
			}
		}
	}
	room.mu.RUnlock()

	if len(toExport) == 0 {
		return nil, ErrNoCompletedUploads
	}

	if destZipPath == "" {
		destZipPath = filepath.Join(roomFolder, fmt.Sprintf("collection-%s.zip", roomID))
	}
	absDest, err := filepath.Abs(destZipPath)
	if err != nil {
		return nil, fmt.Errorf("guest: invalid export path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(absDest), 0700); err != nil {
		return nil, fmt.Errorf("guest: cannot create export directory: %w", err)
	}

	stagingPath := absDest + ".part." + randomID()
	outFile, err := os.OpenFile(stagingPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("guest: cannot create export archive: %w", err)
	}
	defer func() {
		_ = outFile.Close()
		_ = os.Remove(stagingPath)
	}()

	zipWriter := zip.NewWriter(outFile)

	manifest := Manifest{
		RoomID:      roomID,
		RoomTitle:   roomTitle,
		ExportedAt:  time.Now(),
		TotalFiles:  len(toExport),
		Submissions: make([]ManifestItem, 0, len(toExport)),
	}

	var totalBytes int64
	for _, item := range toExport {
		guestDir := filepath.Join(roomFolder, fmt.Sprintf("%s-%s", item.guest.ID, item.guest.SafeSlug))
		srcPath := filepath.Join(guestDir, item.up.FileName)

		info, err := os.Stat(srcPath)
		if err != nil {
			_ = zipWriter.Close()
			return nil, fmt.Errorf("guest: cannot read completed file %s: %w", srcPath, err)
		}
		if info.Size() != item.up.Transferred && item.up.Transferred > 0 {
			_ = zipWriter.Close()
			return nil, fmt.Errorf("%w: file size changed (%d -> %d)", ErrFileChangedMidExport, item.up.Transferred, info.Size())
		}

		// Subfolder inside zip: ParticipantName or GuestName-GuestID
		folderName := item.partName
		if folderName == "" {
			folderName = fmt.Sprintf("%s (%s)", item.guest.Nickname, item.guest.ID)
		}
		folderName = sanitizeZipPath(folderName)
		exportedEntryPath := sanitizeZipPath(filepath.Join(folderName, item.up.FileName))

		f, err := os.Open(srcPath)
		if err != nil {
			_ = zipWriter.Close()
			return nil, fmt.Errorf("guest: open source file %s: %w", srcPath, err)
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			_ = f.Close()
			_ = zipWriter.Close()
			return nil, err
		}
		header.Name = exportedEntryPath
		header.Method = zip.Deflate

		entryWriter, err := zipWriter.CreateHeader(header)
		if err != nil {
			_ = f.Close()
			_ = zipWriter.Close()
			return nil, err
		}

		hasher := sha256.New()
		tee := io.TeeReader(f, hasher)
		written, err := io.Copy(entryWriter, tee)
		_ = f.Close()
		if err != nil {
			_ = zipWriter.Close()
			return nil, fmt.Errorf("guest: streaming file into archive: %w", err)
		}

		calcSHA := hex.EncodeToString(hasher.Sum(nil))
		if item.up.SHA256 != "" && calcSHA != item.up.SHA256 {
			_ = zipWriter.Close()
			return nil, fmt.Errorf("%w: checksum mismatch during export", ErrFileChangedMidExport)
		}

		totalBytes += written
		manifest.Submissions = append(manifest.Submissions, ManifestItem{
			SubmissionID:     item.up.ID,
			GuestID:          item.guest.ID,
			GuestName:        item.guest.Nickname,
			ParticipantID:    item.up.ParticipantID,
			ParticipantName:  item.partName,
			OriginalFileName: item.up.FileName,
			ExportedPath:     exportedEntryPath,
			SizeBytes:        written,
			SHA256:           calcSHA,
			SubmittedAt:      item.up.CompletedAt,
			FormValues:       item.up.FormValues,
		})
	}
	manifest.TotalBytes = totalBytes

	// 1. Write manifest.json
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err == nil {
		if jsonEntry, err := zipWriter.Create("manifest.json"); err == nil {
			_, _ = jsonEntry.Write(manifestJSON)
		}
	}

	// 2. Write manifest.csv
	if csvEntry, err := zipWriter.Create("manifest.csv"); err == nil {
		cw := csv.NewWriter(csvEntry)
		_ = cw.Write([]string{
			"SubmissionID", "GuestID", "GuestName", "ParticipantName",
			"OriginalFileName", "ExportedPath", "SizeBytes", "SHA256", "SubmittedAt",
		})
		for _, m := range manifest.Submissions {
			_ = cw.Write([]string{
				m.SubmissionID,
				m.GuestID,
				m.GuestName,
				m.ParticipantName,
				m.OriginalFileName,
				m.ExportedPath,
				strconv.FormatInt(m.SizeBytes, 10),
				m.SHA256,
				m.SubmittedAt.Format(time.RFC3339),
			})
		}
		cw.Flush()
	}

	if err := zipWriter.Close(); err != nil {
		return nil, fmt.Errorf("guest: closing zip writer: %w", err)
	}

	if err := outFile.Sync(); err != nil {
		return nil, fmt.Errorf("guest: sync zip file: %w", err)
	}
	if err := outFile.Close(); err != nil {
		return nil, fmt.Errorf("guest: close zip file: %w", err)
	}

	// Finalize atomic move
	if err := os.Rename(stagingPath, absDest); err != nil {
		return nil, fmt.Errorf("guest: finalize export archive: %w", err)
	}

	return &ExportResult{
		TargetPath: absDest,
		TotalFiles: len(manifest.Submissions),
		TotalBytes: totalBytes,
	}, nil
}

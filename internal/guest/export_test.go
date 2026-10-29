package guest

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExportRoomToZip(t *testing.T) {
	tmpDir := t.TempDir()
	room, err := NewRoomWithConfig(RoomConfig{
		DestinationRoot: tmpDir,
		Duration:        1 * time.Hour,
		Title:           "Classroom Project Submissions",
		CollisionPolicy: CollisionPreserveBoth,
	})
	if err != nil {
		t.Fatalf("NewRoomWithConfig: %v", err)
	}

	p1, _ := room.AddParticipant("Alice Smith", "student_001")
	p2, _ := room.AddParticipant("Bob Jones", "student_002")

	// 1. Guest 1 joins and links to Alice
	g1, err := room.JoinWithParticipant("Alice", p1.ID)
	if err != nil {
		t.Fatalf("JoinWithParticipant g1: %v", err)
	}
	if err := room.ApproveGuest(g1.ID); err != nil {
		t.Fatalf("ApproveGuest g1: %v", err)
	}

	// Guest 1 uploads a file
	content1 := []byte("Alice's final report on biology")
	up1, err := room.DeclareUploadWithMeta(g1.ID, "report.pdf", int64(len(content1)), map[string]string{
		"notes": "v1 final draft",
	})
	if err != nil {
		t.Fatalf("DeclareUpload g1: %v", err)
	}
	if err := room.ApproveUpload(up1.ID); err != nil {
		t.Fatalf("ApproveUpload g1: %v", err)
	}
	res1, err := room.StreamUpload(up1.ID, bytes.NewReader(content1))
	if err != nil {
		t.Fatalf("StreamUpload g1: %v", err)
	}
	if res1.Written != int64(len(content1)) {
		t.Fatalf("written = %d; want %d", res1.Written, len(content1))
	}

	// 2. Guest 2 joins and links to Bob
	g2, err := room.JoinWithParticipant("Bob", p2.ID)
	if err != nil {
		t.Fatalf("JoinWithParticipant g2: %v", err)
	}
	if err := room.ApproveGuest(g2.ID); err != nil {
		t.Fatalf("ApproveGuest g2: %v", err)
	}
	content2 := []byte("Bob's biology lab data")
	up2, err := room.DeclareUpload(g2.ID, "data.csv", int64(len(content2)))
	if err != nil {
		t.Fatalf("DeclareUpload g2: %v", err)
	}
	if err := room.ApproveUpload(up2.ID); err != nil {
		t.Fatalf("ApproveUpload g2: %v", err)
	}
	_, err = room.StreamUpload(up2.ID, bytes.NewReader(content2))
	if err != nil {
		t.Fatalf("StreamUpload g2: %v", err)
	}

	// Verify participant status moved to submitted
	roster := room.GetRoster()
	if roster[0].Status != StatusSubmitted || roster[1].Status != StatusSubmitted {
		t.Fatalf("expected both participants submitted, got: %+v", roster)
	}

	// 3. Export room collection to ZIP
	zipPath := filepath.Join(tmpDir, "exported-collection.zip")
	result, err := ExportRoomToZip(room, zipPath)
	if err != nil {
		t.Fatalf("ExportRoomToZip: %v", err)
	}

	if result.TotalFiles != 2 {
		t.Fatalf("expected 2 files exported, got %d", result.TotalFiles)
	}
	if result.TotalBytes != int64(len(content1)+len(content2)) {
		t.Fatalf("totalBytes = %d; want %d", result.TotalBytes, len(content1)+len(content2))
	}

	// 4. Verify ZIP contents
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer zr.Close()

	foundFiles := make(map[string][]byte)
	var manifestJSON []byte
	var manifestCSV []byte

	for _, f := range zr.File {
		// Prevent traversal
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			t.Fatalf("zip entry has illegal traversal path: %s", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip file %s: %v", f.Name, err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()

		if f.Name == "manifest.json" {
			manifestJSON = data
		} else if f.Name == "manifest.csv" {
			manifestCSV = data
		} else {
			foundFiles[f.Name] = data
		}
	}

	if len(foundFiles) != 2 {
		t.Fatalf("expected 2 data files in zip, got %d: %+v", len(foundFiles), foundFiles)
	}
	if len(manifestJSON) == 0 {
		t.Fatal("manifest.json missing in zip")
	}
	if len(manifestCSV) == 0 {
		t.Fatal("manifest.csv missing in zip")
	}

	// Check manifest JSON content
	var mf Manifest
	if err := json.Unmarshal(manifestJSON, &mf); err != nil {
		t.Fatalf("parse manifest.json: %v", err)
	}
	if mf.TotalFiles != 2 || len(mf.Submissions) != 2 {
		t.Fatalf("manifest submissions count mismatch: %+v", mf)
	}
	if mf.RoomTitle != "Classroom Project Submissions" {
		t.Fatalf("manifest room title mismatch: %s", mf.RoomTitle)
	}

	// Verify entries match original data
	matchedP1 := false
	matchedP2 := false
	for entryPath, data := range foundFiles {
		if strings.HasSuffix(entryPath, "report.pdf") && bytes.Equal(data, content1) {
			matchedP1 = true
		}
		if strings.HasSuffix(entryPath, "data.csv") && bytes.Equal(data, content2) {
			matchedP2 = true
		}
	}
	if !matchedP1 || !matchedP2 {
		t.Fatalf("files in zip did not match expected content: %+v", foundFiles)
	}
}

func TestExportEmptyFails(t *testing.T) {
	tmpDir := t.TempDir()
	room, _ := NewRoom(tmpDir, 1*time.Hour)

	_, err := ExportRoomToZip(room, filepath.Join(tmpDir, "out.zip"))
	if !errors.Is(err, ErrNoCompletedUploads) {
		t.Fatalf("expected ErrNoCompletedUploads, got %v", err)
	}
}

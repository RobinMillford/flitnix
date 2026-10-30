package guest

import (
	"path/filepath"
	"testing"
)

func TestTemplateStoreCRUDAndPresets(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "templates.json")

	store, err := NewTemplateStore(storePath)
	if err != nil {
		t.Fatalf("NewTemplateStore: %v", err)
	}

	// 1. Initial store should load default presets
	list := store.List()
	if len(list) < 5 {
		t.Fatalf("expected at least 5 default presets, got %d", len(list))
	}

	// 2. Create custom template
	custom := Template{
		Name:            "CS 50 Final Project",
		Title:           "CS 50 Deliverables",
		Description:     "Submit code archive and PDF report",
		DurationMinutes: 180,
		CollisionPolicy: CollisionPreserveBoth,
		FormSchema: []FormField{
			{ID: "group_num", Label: "Group Number", Type: "text", Required: true, MaxLength: 10},
		},
		ExpectedParticipants: []ExpectedParticipant{
			{Name: "Team Alpha", Identifier: "team_01"},
			{Name: "Team Beta", Identifier: "team_02"},
		},
	}

	if err := store.Save(custom); err != nil {
		t.Fatalf("Save custom template: %v", err)
	}

	// Reload from disk to verify persistence
	store2, err := NewTemplateStore(storePath)
	if err != nil {
		t.Fatalf("reload template store: %v", err)
	}

	found := false
	var savedID string
	for _, tpl := range store2.List() {
		if tpl.Name == "CS 50 Final Project" {
			found = true
			savedID = tpl.ID
			if len(tpl.ExpectedParticipants) != 2 {
				t.Fatalf("expected 2 expected participants, got %d", len(tpl.ExpectedParticipants))
			}
			if len(tpl.FormSchema) != 1 {
				t.Fatalf("expected 1 form field, got %d", len(tpl.FormSchema))
			}
		}
	}
	if !found {
		t.Fatal("custom template not found after reload")
	}

	// 3. Duplicate template
	dup, err := store2.Duplicate(savedID, "CS 50 Fall 2026")
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if dup.ID == savedID || dup.Name != "CS 50 Fall 2026" {
		t.Fatalf("unexpected duplicated template: %+v", dup)
	}

	// 4. Delete template
	if err := store2.Delete(dup.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := store2.Get(dup.ID); ok {
		t.Fatal("expected deleted template to be gone")
	}
}

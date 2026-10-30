package guest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrTemplateNotFound = errors.New("guest: template not found")
	ErrInvalidTemplate  = errors.New("guest: invalid template")
)

type Template struct {
	ID                   string                `json:"id"`
	Name                 string                `json:"name"`
	Title                string                `json:"title"`
	Description          string                `json:"description,omitempty"`
	DurationMinutes      int                   `json:"durationMinutes"`
	CollisionPolicy      CollisionPolicy       `json:"collisionPolicy"`
	DestinationRoot      string                `json:"destinationRoot,omitempty"`
	FormSchema           []FormField           `json:"formSchema,omitempty"`
	ExpectedParticipants []ExpectedParticipant `json:"expectedParticipants,omitempty"`
	Version              int                   `json:"version"`
	CreatedAt            time.Time             `json:"createdAt"`
	UpdatedAt            time.Time             `json:"updatedAt"`
}

type TemplateStore struct {
	mu        sync.RWMutex
	filePath  string
	templates map[string]*Template
}

func NewTemplateStore(filePath string) (*TemplateStore, error) {
	ts := &TemplateStore{
		filePath:  filePath,
		templates: make(map[string]*Template),
	}

	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0700); err != nil {
			return nil, fmt.Errorf("guest: cannot create template directory: %w", err)
		}
		if err := ts.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Non-fatal if corrupted; fallback to presets
			_ = err
		}
	}

	// If empty, initialize with default presets
	if len(ts.templates) == 0 {
		for _, p := range DefaultPresets() {
			t := Template{
				ID:              p.ID,
				Name:            p.Name,
				Title:           p.Title,
				Description:     p.Description,
				DurationMinutes: p.DurationMinutes,
				CollisionPolicy: p.CollisionPolicy,
				FormSchema:      p.FormSchema,
				Version:         1,
				CreatedAt:       time.Now(),
				UpdatedAt:       time.Now(),
			}
			ts.templates[t.ID] = &t
		}
		if filePath != "" {
			_ = ts.persistLocked()
		}
	}

	return ts, nil
}

func (s *TemplateStore) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}

	var list []Template
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}

	for i := range list {
		t := list[i]
		if t.ID != "" && t.Name != "" {
			s.templates[t.ID] = &t
		}
	}
	return nil
}

func (s *TemplateStore) persistLocked() error {
	if s.filePath == "" {
		return nil
	}

	list := make([]Template, 0, len(s.templates))
	for _, t := range s.templates {
		list = append(list, *t)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := s.filePath + ".tmp." + randomID()
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmpFile, s.filePath)
}

func (s *TemplateStore) List() []Template {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Template, 0, len(s.templates))
	for _, t := range s.templates {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *TemplateStore) Get(id string) (*Template, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.templates[id]
	if !ok {
		return nil, false
	}
	copy := *t
	return &copy, true
}

func (s *TemplateStore) Save(t Template) error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return fmt.Errorf("%w: template name is required", ErrInvalidTemplate)
	}
	t.Title = strings.TrimSpace(t.Title)
	if t.Title == "" {
		t.Title = t.Name
	}
	if t.DurationMinutes <= 0 {
		t.DurationMinutes = 60
	}
	if t.CollisionPolicy == "" {
		t.CollisionPolicy = CollisionPreserveBoth
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if t.ID == "" {
		id, err := randomHex(6)
		if err != nil {
			return err
		}
		t.ID = id
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	t.Version = 1

	s.templates[t.ID] = &t
	return s.persistLocked()
}

func (s *TemplateStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.templates[id]; !ok {
		return ErrTemplateNotFound
	}
	delete(s.templates, id)
	return s.persistLocked()
}

func (s *TemplateStore) Duplicate(id string, newName string) (*Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	orig, ok := s.templates[id]
	if !ok {
		return nil, ErrTemplateNotFound
	}

	newName = strings.TrimSpace(newName)
	if newName == "" {
		newName = orig.Name + " (Copy)"
	}

	newID, err := randomHex(6)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	dup := *orig
	dup.ID = newID
	dup.Name = newName
	dup.CreatedAt = now
	dup.UpdatedAt = now

	s.templates[newID] = &dup
	if err := s.persistLocked(); err != nil {
		delete(s.templates, newID)
		return nil, err
	}
	return &dup, nil
}

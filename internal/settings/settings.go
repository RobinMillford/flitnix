package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// CurrentSchemaVersion is the current version of the settings file schema.
	CurrentSchemaVersion = 3

	// Allowed themes
	ThemeSystem = "system"
	ThemeDark   = "dark"
	ThemeLight  = "light"

	// Allowed visibility modes (matching devices.VisibilityMode)
	VisibilityHidden            = "hidden"
	VisibilityTrustedOnly       = "trusted"
	VisibilityTemporaryEveryone = "temporary_everyone"
	VisibilityEveryone          = "everyone"
)

var (
	ErrInvalidTheme = errors.New("settings: invalid theme; must be 'system', 'dark', or 'light'")
)

// Settings represents persistent user preferences.
type Settings struct {
	Version                int       `json:"version"`
	Theme                  string    `json:"theme"`
	Locale                 string    `json:"locale"`
	DownloadDir            string    `json:"downloadDir"`
	AutoStartSession       bool      `json:"autoStartSession"`
	PreserveModTimes       bool      `json:"preserveModTimes"`
	PreferredInterface     string    `json:"preferredInterface"`
	VisibilityMode         string    `json:"visibilityMode"`
	VisibilityPreviousMode string    `json:"visibilityPreviousMode"`
	VisibilityExpiresAt    time.Time `json:"visibilityExpiresAt,omitempty"`
	UpdatedAt              time.Time `json:"updatedAt"`
}

// DefaultSettings returns the baseline configuration for Flitnix.
func DefaultSettings() Settings {
	return Settings{
		Version:                CurrentSchemaVersion,
		Theme:                  ThemeSystem,
		Locale:                 "system",
		DownloadDir:            "", // empty indicates default (~/Downloads/Flitnix)
		AutoStartSession:       false,
		PreserveModTimes:       true,
		PreferredInterface:     "",
		VisibilityMode:         VisibilityTrustedOnly,
		VisibilityPreviousMode: VisibilityTrustedOnly,
		UpdatedAt:              time.Now().UTC(),
	}
}

// Store manages thread-safe, atomic persistence of settings.
type Store struct {
	mu       sync.RWMutex
	filePath string
	current  Settings
}

// NewStore loads settings from filePath, or initializes defaults if missing or corrupted.
func NewStore(filePath string) (*Store, error) {
	s := &Store{
		filePath: filePath,
		current:  DefaultSettings(),
	}

	if filePath == "" {
		return s, nil
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return nil, fmt.Errorf("settings: failed to create directory: %w", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Clean initial state: not created yet
			return s, nil
		}
		return nil, fmt.Errorf("settings: failed to read file: %w", err)
	}

	if len(data) == 0 {
		return s, nil
	}

	loaded := DefaultSettings()
	if err := json.Unmarshal(data, &loaded); err != nil {
		// Corrupted file recovery: back up corrupted file and fallback to defaults
		corruptBackup := fmt.Sprintf("%s.corrupt.%s", filePath, time.Now().UTC().Format("20060102150405"))
		_ = os.Rename(filePath, corruptBackup)
		fmt.Fprintf(os.Stderr, "flitnix: settings corrupted, backed up to %s; restored defaults\n", corruptBackup)
		return s, nil
	}

	// Schema migration hook for future versions
	if loaded.Version < CurrentSchemaVersion {
		loaded = migrateSettings(loaded)
	}

	loaded = ValidateAndNormalize(loaded)
	s.current = loaded
	return s, nil
}

// ValidateAndNormalize cleans and checks settings fields.
func ValidateAndNormalize(s Settings) Settings {
	s.Version = CurrentSchemaVersion

	theme := strings.ToLower(strings.TrimSpace(s.Theme))
	switch theme {
	case ThemeDark, ThemeLight:
		s.Theme = theme
	default:
		s.Theme = ThemeSystem
	}

	s.Locale = strings.TrimSpace(s.Locale)
	if s.Locale == "" {
		s.Locale = "system"
	}

	s.DownloadDir = strings.TrimSpace(s.DownloadDir)
	if s.DownloadDir != "" {
		s.DownloadDir = filepath.Clean(s.DownloadDir)
	}

	s.PreferredInterface = strings.TrimSpace(s.PreferredInterface)

	prevVis := strings.ToLower(strings.TrimSpace(s.VisibilityPreviousMode))
	switch prevVis {
	case VisibilityHidden, VisibilityTrustedOnly:
		s.VisibilityPreviousMode = prevVis
	default:
		s.VisibilityPreviousMode = VisibilityTrustedOnly
	}

	vis := strings.ToLower(strings.TrimSpace(s.VisibilityMode))
	switch vis {
	case VisibilityHidden, VisibilityTrustedOnly:
		s.VisibilityMode = vis
		s.VisibilityPreviousMode = vis
		s.VisibilityExpiresAt = time.Time{}
	case VisibilityEveryone:
		s.VisibilityMode = vis
		s.VisibilityExpiresAt = time.Time{}
	case VisibilityTemporaryEveryone:
		if s.VisibilityExpiresAt.IsZero() || !s.VisibilityExpiresAt.After(time.Now().UTC()) {
			// Expired temporary open session must never become active again after restart.
			s.VisibilityMode = s.VisibilityPreviousMode
			s.VisibilityExpiresAt = time.Time{}
		} else {
			s.VisibilityMode = vis
		}
	default:
		s.VisibilityMode = VisibilityTrustedOnly
		s.VisibilityExpiresAt = time.Time{}
	}

	return s
}

// migrateSettings migrates older configuration schemas to current schema.
func migrateSettings(old Settings) Settings {
	migrated := old
	if old.Version < 3 {
		if migrated.VisibilityMode == "" {
			migrated.VisibilityMode = VisibilityTrustedOnly
		}
		if migrated.VisibilityPreviousMode == "" {
			migrated.VisibilityPreviousMode = VisibilityTrustedOnly
		}
	}
	migrated.Version = CurrentSchemaVersion
	return migrated
}

// Get returns a copy of the current settings.
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Save validates and atomically writes settings to disk.
func (s *Store) Save(settings Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	valid := ValidateAndNormalize(settings)
	valid.UpdatedAt = time.Now().UTC()

	if s.filePath != "" {
		data, err := json.MarshalIndent(valid, "", "  ")
		if err != nil {
			return fmt.Errorf("settings: failed to marshal: %w", err)
		}

		tmpPath := s.filePath + ".tmp"
		if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
			return fmt.Errorf("settings: failed to write temporary file: %w", err)
		}
		if err := os.Rename(tmpPath, s.filePath); err != nil {
			return fmt.Errorf("settings: failed to commit file: %w", err)
		}
	}

	s.current = valid
	return nil
}

// Reset restores settings to default values and commits to disk.
func (s *Store) Reset() (Settings, error) {
	defaults := DefaultSettings()
	if err := s.Save(defaults); err != nil {
		return Settings{}, err
	}
	return defaults, nil
}

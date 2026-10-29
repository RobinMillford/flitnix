package guest

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

type RoomState string

const (
	RoomStarting RoomState = "starting"
	RoomActive   RoomState = "active"
	RoomExpiring RoomState = "expiring"
	RoomExpired  RoomState = "expired"
	RoomEnded    RoomState = "ended"
)

type GuestState string

const (
	GuestPending  GuestState = "pending"
	GuestApproved GuestState = "approved"
	GuestRejected GuestState = "rejected"
	GuestRevoked  GuestState = "revoked"
)

type UploadState string

const (
	UploadPendingApproval UploadState = "pending"
	UploadApproved        UploadState = "approved"
	UploadRejected        UploadState = "rejected"
	UploadUploading       UploadState = "uploading"
	UploadCompleted       UploadState = "completed"
	UploadFailed          UploadState = "failed"
	UploadCanceled        UploadState = "canceled"
)

var (
	ErrRoomNotFound         = errors.New("guest: room not found")
	ErrRoomInactive         = errors.New("guest: room is not active")
	ErrRoomExpired          = errors.New("guest: room has expired")
	ErrRoomEnded            = errors.New("guest: room has ended")
	ErrInvalidSecret        = errors.New("guest: invalid invitation secret")
	ErrGuestNotFound        = errors.New("guest: guest not found")
	ErrGuestNotApproved     = errors.New("guest: guest is not approved")
	ErrGuestRevoked         = errors.New("guest: guest has been revoked")
	ErrGuestRejected        = errors.New("guest: guest was rejected")
	ErrUploadNotFound       = errors.New("guest: upload request not found")
	ErrUploadNotApproved    = errors.New("guest: upload request not approved")
	ErrUploadAlreadyDecided = errors.New("guest: upload request already decided")
	ErrUnauthorized         = errors.New("guest: unauthorized")
	ErrRoomFull             = errors.New("guest: room capacity reached")
	ErrFileConflict         = errors.New("guest: file already exists and collision policy rejects overwrite")
)

type CollisionPolicy string

const (
	CollisionPreserveBoth CollisionPolicy = "preserve_both"
	CollisionReject       CollisionPolicy = "reject"
)

type ParticipantStatus string

const (
	StatusNotJoined         ParticipantStatus = "not_joined"
	StatusJoined            ParticipantStatus = "joined"
	StatusAwaitingUpload    ParticipantStatus = "awaiting_upload"
	StatusUploadRequested   ParticipantStatus = "upload_requested"
	StatusAwaitingApproval  ParticipantStatus = "awaiting_approval"
	StatusUploading         ParticipantStatus = "uploading"
	StatusSubmitted         ParticipantStatus = "submitted"
	StatusRejected          ParticipantStatus = "rejected"
	StatusFailed            ParticipantStatus = "failed"
	StatusCanceled          ParticipantStatus = "canceled"
	StatusMissingAtDeadline ParticipantStatus = "missing_at_deadline"
)

type FormField struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Type      string   `json:"type"` // "text", "textarea", "select"
	Required  bool     `json:"required"`
	Options   []string `json:"options,omitempty"`
	MaxLength int      `json:"maxLength,omitempty"`
}

type ExpectedParticipant struct {
	ID              string            `json:"id"`
	Identifier      string            `json:"identifier"`
	Name            string            `json:"name"`
	AssignedGuestID string            `json:"assignedGuestId,omitempty"`
	Status          ParticipantStatus `json:"status"`
	SubmissionCount int               `json:"submissionCount"`
	LastActivity    time.Time         `json:"lastActivity,omitempty"`
}

type Preset struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Title           string          `json:"title"`
	Description     string          `json:"description"`
	DurationMinutes int             `json:"durationMinutes"`
	CollisionPolicy CollisionPolicy `json:"collisionPolicy"`
	FormSchema      []FormField     `json:"formSchema,omitempty"`
}

func DefaultPresets() []Preset {
	return []Preset{
		{
			ID:              "classroom_assignment",
			Name:            "Classroom Assignment",
			Title:           "Classroom Assignment Submission",
			Description:     "Submit your assignment files. Provide your student ID and notes.",
			DurationMinutes: 120,
			CollisionPolicy: CollisionPreserveBoth,
			FormSchema: []FormField{
				{ID: "student_id", Label: "Student ID", Type: "text", Required: true, MaxLength: 30},
				{ID: "assignment_num", Label: "Assignment # / Topic", Type: "text", Required: true, MaxLength: 50},
				{ID: "notes", Label: "Submission Notes", Type: "textarea", Required: false, MaxLength: 200},
			},
		},
		{
			ID:              "photo_collection",
			Name:            "Photo Collection",
			Title:           "Event Photo Collection",
			Description:     "Upload full-resolution event photos and video clips.",
			DurationMinutes: 180,
			CollisionPolicy: CollisionPreserveBoth,
			FormSchema: []FormField{
				{ID: "caption", Label: "Caption / Group Name", Type: "text", Required: false, MaxLength: 100},
			},
		},
		{
			ID:              "meeting_documents",
			Name:            "Meeting Documents",
			Title:           "Meeting Document Drop",
			Description:     "Drop meeting handouts, slide decks, and discussion notes.",
			DurationMinutes: 90,
			CollisionPolicy: CollisionPreserveBoth,
			FormSchema: []FormField{
				{ID: "presenter", Label: "Presenter / Author", Type: "text", Required: true, MaxLength: 50},
				{ID: "agenda_item", Label: "Agenda Item", Type: "text", Required: false, MaxLength: 50},
			},
		},
		{
			ID:              "team_submission",
			Name:            "Team Submission",
			Title:           "Team Deliverable Submission",
			Description:     "Upload final project deliverables and assets.",
			DurationMinutes: 240,
			CollisionPolicy: CollisionPreserveBoth,
			FormSchema: []FormField{
				{ID: "team_name", Label: "Team Name", Type: "text", Required: true, MaxLength: 50},
				{ID: "deliverable_version", Label: "Deliverable Version", Type: "text", Required: false, MaxLength: 20},
			},
		},
		{
			ID:              "general_file_drop",
			Name:            "General File Drop",
			Title:           "Flitnix Guest Drop",
			Description:     "Quick temporary drop box for general file collection.",
			DurationMinutes: 60,
			CollisionPolicy: CollisionPreserveBoth,
		},
	}
}

type RoomConfig struct {
	DestinationRoot      string                `json:"destinationRoot"`
	Duration             time.Duration         `json:"duration"`
	Title                string                `json:"title"`
	Description          string                `json:"description,omitempty"`
	Deadline             *time.Time            `json:"deadline,omitempty"`
	CollisionPolicy      CollisionPolicy       `json:"collisionPolicy"`
	FormSchema           []FormField           `json:"formSchema,omitempty"`
	ExpectedParticipants []ExpectedParticipant `json:"expectedParticipants,omitempty"`
}

const (
	MaxGuestsPerRoom = 256
)

var slugInvalidChars = regexp.MustCompile(`[^a-z0-9_-]+`)

func SanitizeNickname(name string) (display string, slug string) {
	name = strings.TrimSpace(name)
	// Remove control characters
	var b strings.Builder
	for _, r := range name {
		if r >= 32 && r != 127 {
			b.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		cleaned = "Guest"
	}
	runes := []rune(cleaned)
	if len(runes) > 30 {
		cleaned = string(runes[:30])
	}
	display = cleaned

	lower := strings.ToLower(display)
	slug = slugInvalidChars.ReplaceAllString(lower, "-")
	slug = strings.Trim(slug, "-_")
	if slug == "" {
		slug = "guest"
	}
	if len(slug) > 24 {
		slug = slug[:24]
	}
	return display, slug
}

type Guest struct {
	ID            string     `json:"id"`
	Nickname      string     `json:"nickname"`
	SafeSlug      string     `json:"safeSlug"`
	ParticipantID string     `json:"participantId,omitempty"`
	Token         string     `json:"-"`
	TokenHash     string     `json:"-"`
	State         GuestState `json:"state"`
	CreatedAt     time.Time  `json:"createdAt"`
	DecidedAt     time.Time  `json:"decidedAt,omitempty"`

	store    *transfer.Store
	transfer *transfer.Service
}

type UploadRequest struct {
	ID               string            `json:"id"`
	GuestID          string            `json:"guestId"`
	GuestName        string            `json:"guestName"`
	ParticipantID    string            `json:"participantId,omitempty"`
	FileName         string            `json:"fileName"`
	Size             int64             `json:"size"`
	State            UploadState       `json:"state"`
	CreatedAt        time.Time         `json:"createdAt"`
	DecidedAt        time.Time         `json:"decidedAt,omitempty"`
	StartedAt        time.Time         `json:"startedAt,omitempty"`
	CompletedAt      time.Time         `json:"completedAt,omitempty"`
	Transferred      int64             `json:"transferred"`
	Speed            float64           `json:"speed"`
	SHA256           string            `json:"sha256,omitempty"`
	PublishedPath    string            `json:"publishedPath,omitempty"`
	Error            string            `json:"error,omitempty"`
	FormValues       map[string]string `json:"formValues,omitempty"`
	internalUploadID string
}

type RoomView struct {
	ID              string                `json:"id"`
	Date            string                `json:"date"`
	DestinationRoot string                `json:"destinationRoot"`
	RoomFolder      string                `json:"roomFolder"`
	Title           string                `json:"title"`
	Description     string                `json:"description,omitempty"`
	Deadline        *time.Time            `json:"deadline,omitempty"`
	CollisionPolicy CollisionPolicy       `json:"collisionPolicy"`
	State           RoomState             `json:"state"`
	DurationSeconds int                   `json:"durationSeconds"`
	CreatedAt       time.Time             `json:"createdAt"`
	ExpiresAt       time.Time             `json:"expiresAt"`
	InviteSecret    string                `json:"inviteSecret,omitempty"`
	InviteURL       string                `json:"inviteUrl,omitempty"`
	FormSchema      []FormField           `json:"formSchema,omitempty"`
	Roster          []ExpectedParticipant `json:"roster,omitempty"`
	HasRoster       bool                  `json:"hasRoster"`
}

type Room struct {
	ID              string
	Date            string
	InviteSecret    string
	DestinationRoot string
	RoomFolder      string
	Duration        time.Duration
	CreatedAt       time.Time
	ExpiresAt       time.Time
	State           RoomState

	Title           string
	Description     string
	Deadline        *time.Time
	CollisionPolicy CollisionPolicy
	FormSchema      []FormField

	mu            sync.RWMutex
	guests        map[string]*Guest
	guestsByToken map[string]*Guest
	uploads       map[string]*UploadRequest
	roster        map[string]*ExpectedParticipant
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = io.ReadFull(rand.Reader, b)
	return hex.EncodeToString(b)
}

func randomHex(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func randomBase64URL(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func NewRoom(destinationRoot string, duration time.Duration) (*Room, error) {
	return NewRoomWithConfig(RoomConfig{
		DestinationRoot: destinationRoot,
		Duration:        duration,
	})
}

func NewRoomWithConfig(cfg RoomConfig) (*Room, error) {
	destRoot := cfg.DestinationRoot
	if destRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return nil, fmt.Errorf("guest: cannot resolve default home directory: %w", err)
		}
		destRoot = filepath.Join(home, "Downloads", "Flitnix")
	}
	absDest, err := filepath.Abs(destRoot)
	if err != nil {
		return nil, fmt.Errorf("guest: invalid destination root %q: %w", destRoot, err)
	}

	roomID, err := randomHex(6) // 12 characters
	if err != nil {
		return nil, err
	}
	inviteSecret, err := randomBase64URL(24) // 32 characters
	if err != nil {
		return nil, err
	}

	now := time.Now()
	dateStr := now.Format("2006-01-02")
	roomFolder := filepath.Join(absDest, "Flitnix Guest Drop", fmt.Sprintf("%s-%s", dateStr, roomID))

	dur := cfg.Duration
	if dur <= 0 {
		dur = 1 * time.Hour
	}

	title := strings.TrimSpace(cfg.Title)
	if title == "" {
		title = "Flitnix Guest Drop"
	}

	collisionPolicy := cfg.CollisionPolicy
	if collisionPolicy == "" {
		collisionPolicy = CollisionPreserveBoth
	}

	r := &Room{
		ID:              roomID,
		Date:            dateStr,
		InviteSecret:    inviteSecret,
		DestinationRoot: absDest,
		RoomFolder:      roomFolder,
		Duration:        dur,
		CreatedAt:       now,
		ExpiresAt:       now.Add(dur),
		State:           RoomActive,
		Title:           title,
		Description:     strings.TrimSpace(cfg.Description),
		Deadline:        cfg.Deadline,
		CollisionPolicy: collisionPolicy,
		FormSchema:      cfg.FormSchema,
		guests:          make(map[string]*Guest),
		guestsByToken:   make(map[string]*Guest),
		uploads:         make(map[string]*UploadRequest),
		roster:          make(map[string]*ExpectedParticipant),
	}

	for _, ep := range cfg.ExpectedParticipants {
		if ep.ID == "" {
			ep.ID = randomID()
		}
		if ep.Status == "" {
			ep.Status = StatusNotJoined
		}
		pCopy := ep
		r.roster[ep.ID] = &pCopy
	}

	return r, nil
}

func (r *Room) CheckExpiry() RoomState {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// If deadline has passed, update unsubmitted roster participants
	if r.Deadline != nil && now.After(*r.Deadline) {
		for _, p := range r.roster {
			if p.Status == StatusNotJoined || p.Status == StatusJoined || p.Status == StatusAwaitingUpload {
				p.Status = StatusMissingAtDeadline
			}
		}
	}

	if r.State == RoomEnded || r.State == RoomExpired {
		return r.State
	}

	if !now.Before(r.ExpiresAt) {
		r.State = RoomExpired
		r.revokeAllLocked("room expired")
		return RoomExpired
	}

	// Within 5 minutes or 10% of duration
	expiringWindow := 5 * time.Minute
	if r.Duration < expiringWindow*2 {
		expiringWindow = r.Duration / 10
	}
	if r.ExpiresAt.Sub(now) <= expiringWindow {
		r.State = RoomExpiring
	} else {
		r.State = RoomActive
	}
	return r.State
}

func (r *Room) View() RoomView {
	r.mu.RLock()
	defer r.mu.RUnlock()

	roster := make([]ExpectedParticipant, 0, len(r.roster))
	for _, p := range r.roster {
		roster = append(roster, *p)
	}
	sort.Slice(roster, func(i, j int) bool {
		return roster[i].Name < roster[j].Name
	})

	formSchema := make([]FormField, len(r.FormSchema))
	copy(formSchema, r.FormSchema)

	return RoomView{
		ID:              r.ID,
		Date:            r.Date,
		DestinationRoot: r.DestinationRoot,
		RoomFolder:      r.RoomFolder,
		Title:           r.Title,
		Description:     r.Description,
		Deadline:        r.Deadline,
		CollisionPolicy: r.CollisionPolicy,
		State:           r.State,
		DurationSeconds: int(r.Duration.Seconds()),
		CreatedAt:       r.CreatedAt,
		ExpiresAt:       r.ExpiresAt,
		InviteSecret:    r.InviteSecret,
		FormSchema:      formSchema,
		Roster:          roster,
		HasRoster:       len(r.roster) > 0,
	}
}

func (r *Room) End() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State == RoomEnded {
		return
	}
	r.State = RoomEnded
	r.revokeAllLocked("room ended by host")
}

func (r *Room) revokeAllLocked(reason string) {
	for _, g := range r.guests {
		if g.State == GuestApproved || g.State == GuestPending {
			g.State = GuestRevoked
			if g.transfer != nil {
				g.transfer.CancelSession("guest-" + g.ID)
			}
		}
	}
	for _, u := range r.uploads {
		if u.State == UploadPendingApproval || u.State == UploadApproved || u.State == UploadUploading {
			u.State = UploadCanceled
			u.Error = reason
		}
	}
}

func (r *Room) ValidateInviteSecret(secret string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.State != RoomActive && r.State != RoomExpiring {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.InviteSecret), []byte(secret)) != 1 {
		return false
	}
	return true
}

func (r *Room) Join(nickname string) (*Guest, error) {
	return r.JoinWithParticipant(nickname, "")
}

func (r *Room) JoinWithParticipant(nickname, participantID string) (*Guest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State != RoomActive && r.State != RoomExpiring {
		return nil, ErrRoomInactive
	}
	if len(r.guests) >= MaxGuestsPerRoom {
		return nil, ErrRoomFull
	}

	disp, slug := SanitizeNickname(nickname)

	if participantID != "" {
		p, ok := r.roster[participantID]
		if !ok {
			return nil, errors.New("guest: expected participant not found")
		}
		if p.AssignedGuestID != "" {
			if existingG, exists := r.guests[p.AssignedGuestID]; exists && existingG.State != GuestRevoked && existingG.State != GuestRejected {
				return nil, errors.New("guest: participant identity is already in use")
			}
		}
		if disp == "Guest" && p.Name != "" {
			disp, slug = SanitizeNickname(p.Name)
		}
	}

	guestID, err := randomHex(6)
	if err != nil {
		return nil, err
	}
	token, err := randomBase64URL(32)
	if err != nil {
		return nil, err
	}

	g := &Guest{
		ID:            guestID,
		Nickname:      disp,
		SafeSlug:      slug,
		ParticipantID: participantID,
		Token:         token,
		TokenHash:     hashToken(token),
		State:         GuestPending,
		CreatedAt:     time.Now(),
	}

	if participantID != "" {
		if p, ok := r.roster[participantID]; ok {
			p.AssignedGuestID = guestID
			p.Status = StatusJoined
			p.LastActivity = time.Now()
		}
	}

	r.guests[g.ID] = g
	r.guestsByToken[g.Token] = g
	return g, nil
}

func (r *Room) GetGuest(id string) (*Guest, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	g, ok := r.guests[id]
	if !ok {
		return nil, ErrGuestNotFound
	}
	return g, nil
}

func (r *Room) ValidateGuestToken(token string) (*Guest, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.State == RoomEnded {
		return nil, ErrRoomEnded
	}
	if r.State == RoomExpired {
		return nil, ErrRoomExpired
	}

	g, ok := r.guestsByToken[token]
	if !ok {
		return nil, ErrUnauthorized
	}
	switch g.State {
	case GuestRevoked:
		return nil, ErrGuestRevoked
	case GuestRejected:
		return nil, ErrGuestRejected
	default:
		return g, nil
	}
}

func (r *Room) ApproveGuest(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State != RoomActive && r.State != RoomExpiring {
		return ErrRoomInactive
	}
	g, ok := r.guests[id]
	if !ok {
		return ErrGuestNotFound
	}
	if g.State == GuestApproved {
		return nil
	}

	// Prepare isolated directory for guest
	guestDir := filepath.Join(r.RoomFolder, fmt.Sprintf("%s-%s", g.ID, g.SafeSlug))
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: guestDir})
	if err != nil {
		return fmt.Errorf("guest: cannot create store for guest %s: %w", id, err)
	}

	g.store = store
	g.transfer = transfer.NewService(transfer.Config{Store: store})
	g.State = GuestApproved
	g.DecidedAt = time.Now()

	if g.ParticipantID != "" {
		if p, ok := r.roster[g.ParticipantID]; ok {
			p.Status = StatusAwaitingUpload
			p.LastActivity = time.Now()
		}
	}
	return nil
}

func (r *Room) RejectGuest(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	g, ok := r.guests[id]
	if !ok {
		return ErrGuestNotFound
	}
	g.State = GuestRejected
	g.DecidedAt = time.Now()

	if g.ParticipantID != "" {
		if p, ok := r.roster[g.ParticipantID]; ok {
			p.Status = StatusRejected
			p.LastActivity = time.Now()
		}
	}

	for _, u := range r.uploads {
		if u.GuestID == id && (u.State == UploadPendingApproval || u.State == UploadApproved || u.State == UploadUploading) {
			u.State = UploadCanceled
			u.Error = "guest rejected by host"
		}
	}
	return nil
}

func (r *Room) RevokeGuest(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	g, ok := r.guests[id]
	if !ok {
		return ErrGuestNotFound
	}
	g.State = GuestRevoked
	g.DecidedAt = time.Now()

	if g.ParticipantID != "" {
		if p, ok := r.roster[g.ParticipantID]; ok {
			p.Status = StatusCanceled
			p.LastActivity = time.Now()
		}
	}

	if g.transfer != nil {
		g.transfer.CancelSession("guest-" + g.ID)
	}

	for _, u := range r.uploads {
		if u.GuestID == id && (u.State == UploadPendingApproval || u.State == UploadApproved || u.State == UploadUploading) {
			u.State = UploadCanceled
			u.Error = "guest revoked by host"
		}
	}
	return nil
}

func (r *Room) DeclareUpload(guestID, fileName string, size int64) (*UploadRequest, error) {
	return r.DeclareUploadWithMeta(guestID, fileName, size, nil)
}

func (r *Room) DeclareUploadWithMeta(guestID, fileName string, size int64, formValues map[string]string) (*UploadRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State != RoomActive && r.State != RoomExpiring {
		return nil, ErrRoomInactive
	}
	g, ok := r.guests[guestID]
	if !ok {
		return nil, ErrGuestNotFound
	}
	if g.State != GuestApproved {
		return nil, ErrGuestNotApproved
	}

	sanitizedName, err := transfer.SanitizeName(fileName)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, transfer.ErrInvalidUpload
	}

	// Validate form fields if schema configured
	for _, field := range r.FormSchema {
		val := strings.TrimSpace(formValues[field.ID])
		if field.Required && val == "" {
			return nil, fmt.Errorf("guest: form field %q is required", field.Label)
		}
		if field.MaxLength > 0 && len([]rune(val)) > field.MaxLength {
			return nil, fmt.Errorf("guest: form field %q exceeds max length (%d)", field.Label, field.MaxLength)
		}
		if field.Type == "select" && len(field.Options) > 0 && val != "" {
			valid := false
			for _, opt := range field.Options {
				if opt == val {
					valid = true
					break
				}
			}
			if !valid {
				return nil, fmt.Errorf("guest: invalid selection for %q", field.Label)
			}
		}
	}

	// Check CollisionReject policy
	if r.CollisionPolicy == CollisionReject {
		guestDir := filepath.Join(r.RoomFolder, fmt.Sprintf("%s-%s", g.ID, g.SafeSlug))
		targetPath := filepath.Join(guestDir, sanitizedName)
		if _, err := os.Stat(targetPath); err == nil {
			return nil, ErrFileConflict
		}
		for _, existing := range r.uploads {
			if existing.GuestID == guestID && existing.FileName == sanitizedName && existing.State != UploadFailed && existing.State != UploadCanceled && existing.State != UploadRejected {
				return nil, ErrFileConflict
			}
		}
	}

	upID, err := randomHex(8)
	if err != nil {
		return nil, err
	}

	// Declare in guest's transfer service
	internalUp, err := g.transfer.Declare("guest-"+g.ID, sanitizedName, size)
	if err != nil {
		return nil, err
	}

	req := &UploadRequest{
		ID:               upID,
		GuestID:          g.ID,
		GuestName:        g.Nickname,
		ParticipantID:    g.ParticipantID,
		FileName:         sanitizedName,
		Size:             size,
		State:            UploadPendingApproval,
		CreatedAt:        time.Now(),
		FormValues:       formValues,
		internalUploadID: internalUp.ID,
	}

	if g.ParticipantID != "" {
		if p, ok := r.roster[g.ParticipantID]; ok {
			p.Status = StatusUploadRequested
			p.LastActivity = time.Now()
		}
	}

	r.uploads[upID] = req
	return req, nil
}

func (r *Room) GetUpload(uploadID string) (*UploadRequest, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	u, ok := r.uploads[uploadID]
	if !ok {
		return nil, ErrUploadNotFound
	}
	return u, nil
}

func (r *Room) ApproveUpload(uploadID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State != RoomActive && r.State != RoomExpiring {
		return ErrRoomInactive
	}
	u, ok := r.uploads[uploadID]
	if !ok {
		return ErrUploadNotFound
	}
	if u.State != UploadPendingApproval {
		return ErrUploadAlreadyDecided
	}

	g, ok := r.guests[u.GuestID]
	if !ok || g.State != GuestApproved {
		return ErrGuestNotApproved
	}

	_, err := g.transfer.Decide(u.internalUploadID, true)
	if err != nil {
		return err
	}

	u.State = UploadApproved
	u.DecidedAt = time.Now()
	return nil
}

func (r *Room) RejectUpload(uploadID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	u, ok := r.uploads[uploadID]
	if !ok {
		return ErrUploadNotFound
	}
	if u.State != UploadPendingApproval {
		return ErrUploadAlreadyDecided
	}

	g, ok := r.guests[u.GuestID]
	if ok && g.transfer != nil {
		_, _ = g.transfer.Decide(u.internalUploadID, false)
	}

	u.State = UploadRejected
	u.DecidedAt = time.Now()
	return nil
}

func (r *Room) StreamUpload(uploadID string, body io.Reader) (*transfer.ContentResult, error) {
	r.mu.Lock()
	if r.State != RoomActive && r.State != RoomExpiring {
		r.mu.Unlock()
		return nil, ErrRoomInactive
	}
	u, ok := r.uploads[uploadID]
	if !ok {
		r.mu.Unlock()
		return nil, ErrUploadNotFound
	}
	if u.State != UploadApproved {
		r.mu.Unlock()
		return nil, ErrUploadNotApproved
	}

	g, ok := r.guests[u.GuestID]
	if !ok || g.State != GuestApproved {
		r.mu.Unlock()
		return nil, ErrGuestNotApproved
	}

	u.State = UploadUploading
	u.StartedAt = time.Now()
	sessionKey := "guest-" + g.ID
	internalUpID := u.internalUploadID
	service := g.transfer
	r.mu.Unlock()

	// Stream without holding room lock
	res, err := service.SubmitContent(internalUpID, sessionKey, body, func(up transfer.Upload) {
		r.mu.Lock()
		u.Transferred = up.Uploaded
		u.Speed = up.Speed
		r.mu.Unlock()
	})

	r.mu.Lock()
	defer r.mu.Unlock()

	if err != nil {
		if errors.Is(err, transfer.ErrCanceled) || u.State == UploadCanceled {
			u.State = UploadCanceled
			u.Error = "upload canceled"
			return nil, fmt.Errorf("%w: upload canceled", transfer.ErrCanceled)
		}
		if r.State == RoomEnded || r.State == RoomExpired {
			u.State = UploadCanceled
			u.Error = "room ended during transfer"
			return nil, ErrRoomEnded
		}
		u.State = UploadFailed
		u.Error = err.Error()
		if u.ParticipantID != "" {
			if p, ok := r.roster[u.ParticipantID]; ok {
				p.Status = StatusFailed
				p.LastActivity = time.Now()
			}
		}
		return nil, err
	}

	// File was committed to disk; preserve completed state
	u.State = UploadCompleted
	u.CompletedAt = time.Now()
	u.Transferred = res.Written
	u.SHA256 = res.SHA256
	u.PublishedPath = res.Path

	if u.ParticipantID != "" {
		if p, ok := r.roster[u.ParticipantID]; ok {
			p.SubmissionCount++
			p.Status = StatusSubmitted
			p.LastActivity = time.Now()
		}
	}
	return &res, nil
}

func (r *Room) AddParticipant(name, identifier string) (*ExpectedParticipant, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("guest: participant name is required")
	}
	id, err := randomHex(6)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	p := &ExpectedParticipant{
		ID:           id,
		Identifier:   strings.TrimSpace(identifier),
		Name:         name,
		Status:       StatusNotJoined,
		LastActivity: time.Now(),
	}
	r.roster[id] = p
	return p, nil
}

func (r *Room) RemoveParticipant(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.roster[id]; !ok {
		return errors.New("guest: participant not found")
	}
	delete(r.roster, id)
	return nil
}

func (r *Room) UpdateParticipant(id, name, identifier string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, ok := r.roster[id]
	if !ok {
		return errors.New("guest: participant not found")
	}
	if name != "" {
		p.Name = strings.TrimSpace(name)
	}
	if identifier != "" {
		p.Identifier = strings.TrimSpace(identifier)
	}
	p.LastActivity = time.Now()
	return nil
}

func (r *Room) GetRoster() []ExpectedParticipant {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ExpectedParticipant, 0, len(r.roster))
	for _, p := range r.roster {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func (r *Room) LinkGuestToParticipant(guestID, participantID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	g, ok := r.guests[guestID]
	if !ok {
		return ErrGuestNotFound
	}
	p, ok := r.roster[participantID]
	if !ok {
		return errors.New("guest: participant not found")
	}
	p.AssignedGuestID = guestID
	if p.SubmissionCount > 0 {
		p.Status = StatusSubmitted
	} else if g.State == GuestApproved {
		p.Status = StatusAwaitingUpload
	} else {
		p.Status = StatusJoined
	}
	p.LastActivity = time.Now()
	g.ParticipantID = participantID
	return nil
}

func (r *Room) ListGuests() []Guest {
	r.mu.RLock()
	defer r.mu.RUnlock()

	res := make([]Guest, 0, len(r.guests))
	for _, g := range r.guests {
		res = append(res, *g)
	}
	return res
}

func (r *Room) ListUploads() []UploadRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()

	res := make([]UploadRequest, 0, len(r.uploads))
	for _, u := range r.uploads {
		res = append(res, *u)
	}
	return res
}

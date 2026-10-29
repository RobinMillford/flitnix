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
)

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
	ID        string     `json:"id"`
	Nickname  string     `json:"nickname"`
	SafeSlug  string     `json:"safeSlug"`
	Token     string     `json:"-"`
	TokenHash string     `json:"-"`
	State     GuestState `json:"state"`
	CreatedAt time.Time  `json:"createdAt"`
	DecidedAt time.Time  `json:"decidedAt,omitempty"`

	store    *transfer.Store
	transfer *transfer.Service
}

type UploadRequest struct {
	ID               string      `json:"id"`
	GuestID          string      `json:"guestId"`
	GuestName        string      `json:"guestName"`
	FileName         string      `json:"fileName"`
	Size             int64       `json:"size"`
	State            UploadState `json:"state"`
	CreatedAt        time.Time   `json:"createdAt"`
	DecidedAt        time.Time   `json:"decidedAt,omitempty"`
	StartedAt        time.Time   `json:"startedAt,omitempty"`
	CompletedAt      time.Time   `json:"completedAt,omitempty"`
	Transferred      int64       `json:"transferred"`
	Speed            float64     `json:"speed"`
	SHA256           string      `json:"sha256,omitempty"`
	Error            string      `json:"error,omitempty"`
	internalUploadID string
}

type RoomView struct {
	ID              string    `json:"id"`
	Date            string    `json:"date"`
	DestinationRoot string    `json:"destinationRoot"`
	RoomFolder      string    `json:"roomFolder"`
	State           RoomState `json:"state"`
	DurationSeconds int       `json:"durationSeconds"`
	CreatedAt       time.Time `json:"createdAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
	InviteSecret    string    `json:"inviteSecret,omitempty"`
	InviteURL       string    `json:"inviteUrl,omitempty"`
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

	mu            sync.RWMutex
	guests        map[string]*Guest
	guestsByToken map[string]*Guest
	uploads       map[string]*UploadRequest
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
	if destinationRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return nil, fmt.Errorf("guest: cannot resolve default home directory: %w", err)
		}
		destinationRoot = filepath.Join(home, "Downloads", "Flitnix")
	}
	absDest, err := filepath.Abs(destinationRoot)
	if err != nil {
		return nil, fmt.Errorf("guest: invalid destination root %q: %w", destinationRoot, err)
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

	if duration <= 0 {
		duration = 1 * time.Hour
	}

	return &Room{
		ID:              roomID,
		Date:            dateStr,
		InviteSecret:    inviteSecret,
		DestinationRoot: absDest,
		RoomFolder:      roomFolder,
		Duration:        duration,
		CreatedAt:       now,
		ExpiresAt:       now.Add(duration),
		State:           RoomActive,
		guests:          make(map[string]*Guest),
		guestsByToken:   make(map[string]*Guest),
		uploads:         make(map[string]*UploadRequest),
	}, nil
}

func (r *Room) CheckExpiry() RoomState {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State == RoomEnded || r.State == RoomExpired {
		return r.State
	}

	now := time.Now()
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

	return RoomView{
		ID:              r.ID,
		Date:            r.Date,
		DestinationRoot: r.DestinationRoot,
		RoomFolder:      r.RoomFolder,
		State:           r.State,
		DurationSeconds: int(r.Duration.Seconds()),
		CreatedAt:       r.CreatedAt,
		ExpiresAt:       r.ExpiresAt,
		InviteSecret:    r.InviteSecret,
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
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.State != RoomActive && r.State != RoomExpiring {
		return nil, ErrRoomInactive
	}
	if len(r.guests) >= MaxGuestsPerRoom {
		return nil, ErrRoomFull
	}

	guestID, err := randomHex(6)
	if err != nil {
		return nil, err
	}
	token, err := randomBase64URL(32)
	if err != nil {
		return nil, err
	}

	disp, slug := SanitizeNickname(nickname)
	g := &Guest{
		ID:        guestID,
		Nickname:  disp,
		SafeSlug:  slug,
		Token:     token,
		TokenHash: hashToken(token),
		State:     GuestPending,
		CreatedAt: time.Now(),
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
		FileName:         sanitizedName,
		Size:             size,
		State:            UploadPendingApproval,
		CreatedAt:        time.Now(),
		internalUploadID: internalUp.ID,
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

	if u.State == UploadCanceled {
		return nil, fmt.Errorf("%w: upload canceled", transfer.ErrCanceled)
	}

	if r.State == RoomEnded || r.State == RoomExpired {
		u.State = UploadCanceled
		u.Error = "room ended during transfer"
		return nil, ErrRoomEnded
	}

	if err != nil {
		u.State = UploadFailed
		u.Error = err.Error()
		return nil, err
	}

	u.State = UploadCompleted
	u.CompletedAt = time.Now()
	u.Transferred = res.Written
	u.SHA256 = res.SHA256
	return &res, nil
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

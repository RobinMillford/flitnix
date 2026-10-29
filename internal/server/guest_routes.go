package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/audit"
	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/transfer"
)

const guestCookieName = "flitnix_guest_token"

func guestTokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(guestCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(auth, bearerPrefix) {
		return strings.TrimSpace(auth[len(bearerPrefix):])
	}
	return ""
}

func newGuestAPIMux(services *Services, stall time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /guest-api/ping", handleDiagnosticsPing(services))
	mux.HandleFunc("POST /guest-api/ping", handleDiagnosticsPing(services))
	mux.HandleFunc("GET /guest-api/room-info", handleGuestRoomInfo(services))
	mux.HandleFunc("POST /guest-api/join", handleGuestJoin(services))
	mux.HandleFunc("GET /guest-api/status", handleGuestStatus(services))
	mux.HandleFunc("POST /guest-api/uploads", handleGuestUploadDeclare(services))
	mux.HandleFunc("GET /guest-api/uploads/{id}", handleGuestUploadStatus(services))
	mux.HandleFunc("PUT /guest-api/uploads/{id}/content", handleGuestUploadContent(services, stall))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if services != nil {
			services.RecordContact("guest_api")
		}
		mux.ServeHTTP(w, r)
	})
}

type guestRoomInfoResponse struct {
	ID          string                      `json:"id"`
	Title       string                      `json:"title"`
	Description string                      `json:"description,omitempty"`
	Deadline    *time.Time                  `json:"deadline,omitempty"`
	ExpiresAt   time.Time                   `json:"expiresAt"`
	FormSchema  []guest.FormField           `json:"formSchema,omitempty"`
	Roster      []guest.ExpectedParticipant `json:"roster,omitempty"`
	HasRoster   bool                        `json:"hasRoster"`
}

func handleGuestRoomInfo(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svcs.GuestManager == nil {
			writeError(w, http.StatusNotFound, "guest room service unavailable")
			return
		}

		room := svcs.GuestManager.ActiveRoom()
		if room == nil {
			writeError(w, http.StatusUnauthorized, "no active guest room")
			return
		}

		secret := strings.TrimSpace(r.URL.Query().Get("secret"))
		if secret == "" {
			secret = strings.TrimSpace(r.Header.Get("X-Guest-Secret"))
		}
		if !room.ValidateInviteSecret(secret) {
			writeError(w, http.StatusUnauthorized, "invalid invitation secret")
			return
		}

		v := room.View()
		writeJSON(w, http.StatusOK, guestRoomInfoResponse{
			ID:          v.ID,
			Title:       v.Title,
			Description: v.Description,
			Deadline:    v.Deadline,
			ExpiresAt:   v.ExpiresAt,
			FormSchema:  v.FormSchema,
			Roster:      v.Roster,
			HasRoster:   v.HasRoster,
		})
	}
}

type guestJoinRequest struct {
	Secret        string `json:"secret"`
	Nickname      string `json:"nickname"`
	ParticipantID string `json:"participantId,omitempty"`
}

type guestJoinResponse struct {
	GuestID  string `json:"guestId"`
	Token    string `json:"token"`
	Nickname string `json:"nickname"`
	State    string `json:"state"`
}

func handleGuestJoin(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svcs.GuestManager == nil {
			writeError(w, http.StatusNotFound, "guest room service unavailable")
			return
		}

		room := svcs.GuestManager.ActiveRoom()
		if room == nil {
			writeError(w, http.StatusUnauthorized, "no active guest room")
			return
		}

		var req guestJoinRequest
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid join request body")
			return
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid join JSON")
			return
		}

		req.Secret = strings.TrimSpace(req.Secret)
		if !room.ValidateInviteSecret(req.Secret) {
			writeError(w, http.StatusUnauthorized, "invalid invitation secret")
			return
		}

		var g *guest.Guest
		if req.ParticipantID != "" {
			g, err = room.JoinWithParticipant(req.Nickname, req.ParticipantID)
		} else {
			g, err = room.Join(req.Nickname)
		}
		if err != nil {
			if errors.Is(err, guest.ErrRoomInactive) {
				writeError(w, http.StatusUnauthorized, "guest room is not active")
				return
			}
			if errors.Is(err, guest.ErrRoomFull) {
				writeError(w, http.StatusTooManyRequests, "guest room capacity reached")
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		if svcs.Audit != nil {
			svcs.Audit.Record(audit.Entry{
				EventType: audit.EventGuestJoined,
				RoomID:    room.ID,
				GuestID:   g.ID,
				GuestName: g.Nickname,
				Outcome:   audit.OutcomeSuccess,
				Details:   fmt.Sprintf("Guest %s requested entry", g.Nickname),
			})
		}

		// Set HttpOnly cookie
		maxAge := int(time.Until(room.ExpiresAt).Seconds())
		if maxAge < 0 {
			maxAge = 0
		}
		http.SetCookie(w, &http.Cookie{
			Name:     guestCookieName,
			Value:    g.Token,
			Path:     "/guest-api/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   maxAge,
		})

		writeJSON(w, http.StatusCreated, guestJoinResponse{
			GuestID:  g.ID,
			Token:    g.Token,
			Nickname: g.Nickname,
			State:    string(g.State),
		})
	}
}

type guestStatusResponse struct {
	GuestID       string    `json:"guestId,omitempty"`
	Nickname      string    `json:"nickname,omitempty"`
	State         string    `json:"state"`
	Error         string    `json:"error,omitempty"`
	RoomState     string    `json:"roomState,omitempty"`
	RoomExpiresAt time.Time `json:"roomExpiresAt,omitempty"`
}

func handleGuestStatus(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := guestTokenFromRequest(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}

		if svcs.GuestManager == nil {
			writeError(w, http.StatusNotFound, "guest room service unavailable")
			return
		}

		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			switch {
			case errors.Is(err, guest.ErrGuestRevoked):
				writeJSON(w, http.StatusOK, guestStatusResponse{State: "revoked", Error: "guest_revoked"})
				return
			case errors.Is(err, guest.ErrGuestRejected):
				writeJSON(w, http.StatusOK, guestStatusResponse{State: "rejected", Error: "guest_rejected"})
				return
			case errors.Is(err, guest.ErrRoomEnded):
				writeJSON(w, http.StatusOK, guestStatusResponse{State: "ended", Error: "room_ended"})
				return
			case errors.Is(err, guest.ErrRoomExpired):
				writeJSON(w, http.StatusOK, guestStatusResponse{State: "expired", Error: "room_expired"})
				return
			default:
				writeError(w, http.StatusUnauthorized, "invalid guest credential")
				return
			}
		}

		writeJSON(w, http.StatusOK, guestStatusResponse{
			GuestID:       g.ID,
			Nickname:      g.Nickname,
			State:         string(g.State),
			RoomState:     string(room.State),
			RoomExpiresAt: room.ExpiresAt,
		})
	}
}

type guestUploadDeclareRequest struct {
	Name       string            `json:"name"`
	Size       int64             `json:"size"`
	FormValues map[string]string `json:"formValues,omitempty"`
}

type guestUploadResponse struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Size     int64             `json:"size"`
	State    string            `json:"state"`
	Uploaded int64             `json:"uploaded,omitempty"`
	Speed    float64           `json:"speed,omitempty"`
	Error    string            `json:"error,omitempty"`
	SHA256   string            `json:"sha256,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func handleGuestUploadDeclare(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := guestTokenFromRequest(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}

		if svcs.GuestManager == nil {
			writeError(w, http.StatusNotFound, "guest room service unavailable")
			return
		}

		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		if g.State != guest.GuestApproved {
			writeError(w, http.StatusForbidden, "guest is not approved for upload")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "reading metadata")
			return
		}

		var meta guestUploadDeclareRequest
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&meta); err != nil {
			writeError(w, http.StatusBadRequest, "invalid metadata JSON")
			return
		}

		up, err := room.DeclareUploadWithMeta(g.ID, meta.Name, meta.Size, meta.FormValues)
		if err != nil {
			if errors.Is(err, transfer.ErrInvalidFilename) || errors.Is(err, transfer.ErrInvalidUpload) {
				writeError(w, http.StatusBadRequest, "invalid filename or size")
				return
			}
			if errors.Is(err, guest.ErrFileConflict) {
				writeError(w, http.StatusConflict, "a file with this name already exists in your submission")
				return
			}
			if errors.Is(err, transfer.ErrNoCapacity) {
				writeError(w, http.StatusTooManyRequests, "upload capacity reached")
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		if svcs.Audit != nil {
			svcs.Audit.Record(audit.Entry{
				EventType: audit.EventUploadDeclared,
				RoomID:    room.ID,
				GuestID:   g.ID,
				GuestName: g.Nickname,
				FileName:  up.FileName,
				FileSize:  up.Size,
				Outcome:   audit.OutcomeSuccess,
				Details:   fmt.Sprintf("Upload declared: %s (%d bytes)", up.FileName, up.Size),
			})
		}

		writeJSON(w, http.StatusCreated, guestUploadResponse{
			ID:       up.ID,
			Name:     up.FileName,
			Size:     up.Size,
			State:    string(up.State),
			Metadata: up.FormValues,
		})
	}
}

func handleGuestUploadStatus(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := guestTokenFromRequest(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}

		if svcs.GuestManager == nil {
			writeError(w, http.StatusNotFound, "guest room service unavailable")
			return
		}

		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		uploadID := r.PathValue("id")
		up, err := room.GetUpload(uploadID)
		if err != nil || up.GuestID != g.ID {
			// Cross-guest isolation: Return 404 if upload belongs to another guest!
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		writeJSON(w, http.StatusOK, guestUploadResponse{
			ID:       up.ID,
			Name:     up.FileName,
			Size:     up.Size,
			State:    string(up.State),
			Uploaded: up.Transferred,
			Speed:    up.Speed,
			Error:    up.Error,
			SHA256:   up.SHA256,
		})
	}
}

func handleGuestUploadContent(svcs *Services, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := guestTokenFromRequest(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}

		if svcs.GuestManager == nil {
			writeError(w, http.StatusNotFound, "guest room service unavailable")
			return
		}

		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		uploadID := r.PathValue("id")
		up, err := room.GetUpload(uploadID)
		if err != nil || up.GuestID != g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		if up.State != guest.UploadApproved {
			writeError(w, http.StatusBadRequest, "upload request is not approved")
			return
		}

		body := io.Reader(r.Body)
		if rc := http.NewResponseController(w); rc != nil && stall > 0 {
			body = &stallBodyReader{
				ReadCloser: r.Body,
				rc:         rc,
				stall:      stall,
			}
		}

		res, err := room.StreamUpload(uploadID, body)
		if err != nil {
			if svcs.Audit != nil {
				svcs.Audit.Record(audit.Entry{
					EventType: audit.EventUploadFailed,
					RoomID:    room.ID,
					GuestID:   g.ID,
					GuestName: g.Nickname,
					FileName:  up.FileName,
					Outcome:   audit.OutcomeFailed,
					Details:   fmt.Sprintf("Upload failed: %v", err),
				})
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		if svcs.Audit != nil {
			svcs.Audit.Record(audit.Entry{
				EventType: audit.EventUploadCompleted,
				RoomID:    room.ID,
				GuestID:   g.ID,
				GuestName: g.Nickname,
				FileName:  up.FileName,
				FileSize:  res.Written,
				SHA256:    res.SHA256,
				Outcome:   audit.OutcomeSuccess,
				Details:   fmt.Sprintf("Upload completed: %s (%d bytes)", up.FileName, res.Written),
			})
		}

		writeJSON(w, http.StatusOK, uploadResultJSON{
			Written: res.Written,
			SHA256:  res.SHA256,
		})
	}
}

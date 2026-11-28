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
	"github.com/itsyamin/flitnix/internal/events"
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

func newGuestAPIMux(services *Services, stall, downloadStall time.Duration, grants *DownloadGrantStore, shutdownCh <-chan struct{}) http.Handler {
	mux := http.NewServeMux()
	pinLimiter := newPINLimiter()
	mux.HandleFunc("GET /guest-api/events", handleGuestEvents(services, shutdownCh))
	mux.HandleFunc("GET /guest-api/ping", handleDiagnosticsPing(services))
	mux.HandleFunc("POST /guest-api/ping", handleDiagnosticsPing(services))
	mux.HandleFunc("GET /guest-api/room-info", handleGuestRoomInfo(services))
	mux.HandleFunc("POST /guest-api/join", handleGuestJoin(services, pinLimiter))
	mux.HandleFunc("GET /guest-api/status", handleGuestStatus(services))
	mux.HandleFunc("POST /guest-api/uploads", handleGuestUploadDeclare(services))
	mux.HandleFunc("GET /guest-api/uploads/{id}", handleGuestUploadStatus(services))
	mux.HandleFunc("PUT /guest-api/uploads/{id}/content", handleGuestUploadContent(services, stall))
	mux.HandleFunc("GET /guest-api/files", handleGuestFilesList(services))
	mux.HandleFunc("GET /guest-api/sources", handleGuestSourcesList(services))
	mux.HandleFunc("GET /guest-api/sources/{id}/items", handleGuestSourceItems(services))
	mux.HandleFunc("POST /guest-api/download-grants", handleGuestCreateDownloadGrant(services, grants))
	mux.HandleFunc("GET /guest-api/download-progress/{id}", handleGuestDownloadProgress(services, grants))
	dlHandler := serveGuestArchiveDownload(services, grants, downloadStall)
	mux.HandleFunc("GET /guest-api/downloads/{id}/content", dlHandler)
	mux.HandleFunc("HEAD /guest-api/downloads/{id}/content", dlHandler)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if services != nil {
			services.RecordContact("guest_api")
		}
		mux.ServeHTTP(w, r)
	})
}

type guestRoomInfoResponse struct {
	ID                  string                      `json:"id"`
	Title               string                      `json:"title"`
	Description         string                      `json:"description,omitempty"`
	Deadline            *time.Time                  `json:"deadline,omitempty"`
	ExpiresAt           time.Time                   `json:"expiresAt"`
	Mode                guest.RoomMode              `json:"mode"`
	AllowsUpload        bool                        `json:"allowsUpload"`
	AllowsDownload      bool                        `json:"allowsDownload"`
	RequireJoinApproval bool                        `json:"requireJoinApproval"`
	PINRequired         bool                        `json:"pinRequired"`
	MaxDownloads        int                         `json:"maxDownloads"`
	RemainingDownloads  int                         `json:"remainingDownloads"`
	FormSchema          []guest.FormField           `json:"formSchema,omitempty"`
	Roster              []guest.ExpectedParticipant `json:"roster,omitempty"`
	HasRoster           bool                        `json:"hasRoster"`
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
			ID:                  v.ID,
			Title:               v.Title,
			Description:         v.Description,
			Deadline:            v.Deadline,
			ExpiresAt:           v.ExpiresAt,
			Mode:                v.Mode,
			AllowsUpload:        v.AllowsUpload,
			AllowsDownload:      v.AllowsDownload,
			RequireJoinApproval: v.RequireJoinApproval,
			PINRequired:         v.PINRequired,
			MaxDownloads:        v.MaxDownloads,
			RemainingDownloads:  v.RemainingDownloads,
			FormSchema:          v.FormSchema,
			Roster:              v.Roster,
			HasRoster:           v.HasRoster,
		})
	}
}

type guestJoinRequest struct {
	Secret        string `json:"secret"`
	Nickname      string `json:"nickname"`
	ParticipantID string `json:"participantId,omitempty"`
	Pin           string `json:"pin,omitempty"`
}

// guestJoinResponse is the join acknowledgement shown to the guest page. It
// deliberately carries no credential: the guest token is issued only as the
// HttpOnly flitnix_guest_token cookie, so guest JavaScript never holds it and
// an XSS on the guest page cannot exfiltrate a long-lived room credential.
type guestJoinResponse struct {
	GuestID  string `json:"guestId"`
	Nickname string `json:"nickname"`
	State    string `json:"state"`
}

func handleGuestJoin(svcs *Services, pinLimiter *deviceSessionLimiter) http.HandlerFunc {
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

		ip := clientIPFromRequest(r)
		if allowed, retryAfter := pinLimiter.checkAllowed(ip); !allowed {
			writeRateLimit(w, retryAfter)
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

		if room.PINRequired() && !room.VerifyPIN(req.Pin) {
			pinLimiter.recordFailure(ip)
			if allowed, retryAfter := pinLimiter.checkAllowed(ip); !allowed {
				writeRateLimit(w, retryAfter)
				return
			}
			if strings.TrimSpace(req.Pin) == "" {
				writeErrorCode(w, http.StatusForbidden, "PIN required to join this room", "pin_required")
				return
			}
			writeErrorCode(w, http.StatusForbidden, "Incorrect room PIN", "invalid_pin")
			return
		}
		pinLimiter.clear(ip)

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

		// The guest credential is issued as an HttpOnly cookie and nowhere else.
		maxAge := int(time.Until(room.ExpiresAt).Seconds())
		if maxAge < 0 {
			maxAge = 0
		}
		http.SetCookie(w, &http.Cookie{
			Name:     guestCookieName,
			Value:    g.Token,
			Path:     "/guest-api/",
			HttpOnly: true,
			// Lax, not Strict: the guest page is reached from a QR/link that may
			// be a top-level cross-site navigation, and Strict would drop the
			// cookie on that first hop and strand an approved guest. Lax still
			// withholds the cookie from cross-site subresource requests and from
			// cross-site POSTs, so every state-changing guest call stays
			// protected.
			SameSite: http.SameSiteLaxMode,
			// Secure follows the actual TLS state of this connection. Nothing is
			// inferred from X-Forwarded-* headers: a client can forge those, and
			// trusting them would either silently weaken the cookie or break
			// plaintext LAN transfers. This matches the download-grant cookie.
			Secure: r.TLS != nil,
			MaxAge: maxAge,
		})

		svcs.EmitGuest(room.ID, g.ID, events.TypeGuestRoomUpdated, map[string]any{
			"action":   "guest_joined",
			"roomId":   room.ID,
			"guestId":  g.ID,
			"nickname": g.Nickname,
			"state":    string(g.State),
		})

		writeJSON(w, http.StatusCreated, guestJoinResponse{
			GuestID:  g.ID,
			Nickname: g.Nickname,
			State:    string(g.State),
		})
	}
}

type guestStatusResponse struct {
	GuestID             string         `json:"guestId,omitempty"`
	Nickname            string         `json:"nickname,omitempty"`
	State               string         `json:"state"`
	Error               string         `json:"error,omitempty"`
	RoomState           string         `json:"roomState,omitempty"`
	RoomExpiresAt       time.Time      `json:"roomExpiresAt,omitempty"`
	Mode                guest.RoomMode `json:"mode,omitempty"`
	AllowsUpload        bool           `json:"allowsUpload"`
	AllowsDownload      bool           `json:"allowsDownload"`
	RequireJoinApproval bool           `json:"requireJoinApproval"`
	MaxDownloads        int            `json:"maxDownloads"`
	RemainingDownloads  int            `json:"remainingDownloads"`
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

		v := room.View()
		writeJSON(w, http.StatusOK, guestStatusResponse{
			GuestID:             g.ID,
			Nickname:            g.Nickname,
			State:               string(g.State),
			RoomState:           string(v.State),
			RoomExpiresAt:       v.ExpiresAt,
			Mode:                v.Mode,
			AllowsUpload:        v.AllowsUpload,
			AllowsDownload:      v.AllowsDownload,
			RequireJoinApproval: v.RequireJoinApproval,
			MaxDownloads:        v.MaxDownloads,
			RemainingDownloads:  v.RemainingDownloads,
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
		if !room.AllowsUpload() {
			writeErrorCode(w, http.StatusForbidden, "uploads are disabled in download-only rooms", "room_download_only")
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
			if errors.Is(err, transfer.ErrInsufficientStorage) {
				writeInsufficientStorage(w)
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

		resp := guestUploadResponse{
			ID:       up.ID,
			Name:     up.FileName,
			Size:     up.Size,
			State:    string(up.State),
			Metadata: up.FormValues,
		}
		svcs.EmitGuest(room.ID, g.ID, events.TypeGuestUploadUpdated, resp)
		writeJSON(w, http.StatusCreated, resp)
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
		if !room.AllowsUpload() {
			writeErrorCode(w, http.StatusForbidden, "uploads are disabled in download-only rooms", "room_download_only")
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

		if svcs.Inhibitor != nil {
			pLease, pErr := svcs.Inhibitor.Acquire("Receiving guest upload " + up.FileName)
			if pErr != nil && svcs.Diagnose != nil {
				svcs.Diagnose("power inhibitor acquire failed: %v", pErr)
			} else if pLease != nil {
				defer pLease.Release()
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
			if curUp, uErr := room.GetUpload(uploadID); uErr == nil {
				svcs.EmitGuest(room.ID, g.ID, events.TypeGuestUploadUpdated, guestUploadResponse{
					ID:       curUp.ID,
					Name:     curUp.FileName,
					Size:     curUp.Size,
					State:    string(curUp.State),
					Uploaded: curUp.Transferred,
					Error:    curUp.Error,
				})
			}
			if errors.Is(err, transfer.ErrInsufficientStorage) {
				writeInsufficientStorage(w)
				return
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

		svcs.EmitGuest(room.ID, g.ID, events.TypeGuestUploadUpdated, guestUploadResponse{
			ID:       up.ID,
			Name:     up.FileName,
			Size:     up.Size,
			State:    string(guest.UploadCompleted),
			Uploaded: res.Written,
			SHA256:   res.SHA256,
		})

		writeJSON(w, http.StatusOK, uploadResultJSON{
			Written: res.Written,
			SHA256:  res.SHA256,
		})
	}
}

func requireApprovedGuestDownload(w http.ResponseWriter, r *http.Request, svcs *Services) (*guest.Room, *guest.Guest, bool) {
	token := guestTokenFromRequest(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing guest credential")
		return nil, nil, false
	}
	if svcs == nil || svcs.GuestManager == nil {
		writeError(w, http.StatusNotFound, "guest room service unavailable")
		return nil, nil, false
	}
	room, g, err := svcs.GuestManager.ValidateGuest(token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, nil, false
	}
	if g.State != guest.GuestApproved {
		writeError(w, http.StatusForbidden, "guest is not approved")
		return nil, nil, false
	}
	if !room.AllowsDownload() {
		writeErrorCode(w, http.StatusForbidden, "downloads are disabled in upload-only rooms", "room_upload_only")
		return nil, nil, false
	}
	return room, g, true
}

func handleGuestFilesList(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		room, _, ok := requireApprovedGuestDownload(w, r, svcs)
		if !ok {
			return
		}
		files := room.SharedRegistry().List()
		out := make([]fileJSON, len(files))
		for i, sf := range files {
			out[i] = toFileJSON(sf)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleGuestSourcesList(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		room, _, ok := requireApprovedGuestDownload(w, r, svcs)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, buildSourcesList(room.SharedRegistry()))
	}
}

func handleGuestSourceItems(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		room, _, ok := requireApprovedGuestDownload(w, r, svcs)
		if !ok {
			return
		}
		serveSourceItemsFromRegistry(w, r, room.SharedRegistry())
	}
}

func handleGuestCreateDownloadGrant(svcs *Services, grants *DownloadGrantStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		room, g, ok := requireApprovedGuestDownload(w, r, svcs)
		if !ok {
			return
		}
		if grants == nil {
			writeError(w, http.StatusServiceUnavailable, "download grants unavailable")
			return
		}
		var req createGrantRequest
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024)).Decode(&req) != nil {
			writeError(w, http.StatusBadRequest, "invalid source selection")
			return
		}
		sources, errCode, errMsg := resolveGrantSources(room.SharedRegistry(), req)
		if errCode != 0 {
			writeError(w, errCode, errMsg)
			return
		}
		grant, progress, err := grants.createForGuest(room, g.ID, sources, grantFilename(sources))
		if err != nil {
			if errors.Is(err, errDownloadLimit) {
				writeErrorCode(w, http.StatusGone, "download limit reached for this room", "download_limit_reached")
				return
			}
			if errors.Is(err, errRoomBusy) {
				writeError(w, http.StatusTooManyRequests, err.Error())
				return
			}
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     grantCookieName,
			Value:    grant.Secret,
			Path:     "/guest-api/downloads/" + grant.ID + "/content",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   int(grantLifetime.Seconds()),
			Secure:   r.TLS != nil,
		})
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, createGrantResponse{
			DownloadURL: "/guest-api/downloads/" + grant.ID + "/content",
			ProgressID:  progress.ID,
			Archive:     grant.Archive,
			Filename:    grant.Filename,
		})
	}
}

func handleGuestDownloadProgress(svcs *Services, grants *DownloadGrantStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		room, g, ok := requireApprovedGuestDownload(w, r, svcs)
		if !ok {
			return
		}
		writeDownloadProgress(w, grants, r.PathValue("id"), guestGrantSessionKey(room.ID, g.ID))
	}
}

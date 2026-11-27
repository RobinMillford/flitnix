package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/events"
	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/session"
)

const (
	defaultMaxEventStreamTTL = 15 * time.Minute
	eventKeepaliveInterval   = 15 * time.Second
	eventWriteTimeout        = 20 * time.Second
)

func validateEventRequestOrigin(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if origin == "null" {
			return false
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || !equalHostPort(u.Host, r.Host) {
			return false
		}
	} else if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
		u, err := url.Parse(referer)
		if err != nil || u.Host == "" || !equalHostPort(u.Host, r.Host) {
			return false
		}
	}
	return true
}

func writeSSEFrame(w http.ResponseWriter, flusher http.Flusher, rc *http.ResponseController, frame string) error {
	if rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout))
	}
	if _, err := fmt.Fprint(w, frame); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func formatSSEEvent(id uint64, eventType string, data []byte) string {
	var b strings.Builder
	if id > 0 {
		fmt.Fprintf(&b, "id: %d\n", id)
	}
	if eventType != "" {
		fmt.Fprintf(&b, "event: %s\n", eventType)
	}
	b.WriteString("data: ")
	if len(data) == 0 {
		b.WriteString("{}")
	} else {
		b.Write(data)
	}
	b.WriteString("\n\n")
	return b.String()
}

func handlePersonalEvents(svcs *Services, shutdownCh <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validateEventRequestOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin event subscription forbidden")
			return
		}
		sess := sessionFrom(r)
		if sess == nil || svcs == nil || svcs.Events == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if sess.Kind() == session.KindDevice && svcs.Devices != nil {
			if _, ok := svcs.Devices.Get(sess.DeviceID()); !ok {
				writeErrorCode(w, http.StatusUnauthorized, "device revoked", "device_revoked")
				return
			}
		}

		sub, err := svcs.Events.SubscribePersonal(sess.Key())
		if err != nil {
			if errors.Is(err, events.ErrTooManyStreams) {
				writeError(w, http.StatusTooManyRequests, "too many active event streams")
				return
			}
			writeError(w, http.StatusServiceUnavailable, "event stream unavailable")
			return
		}
		defer sub.Close()

		ttl := defaultMaxEventStreamTTL
		if rem := time.Until(sess.ExpiresAt()); rem > 0 && rem < ttl {
			ttl = rem
		} else if rem <= 0 {
			writeUnauthorized(w, session.ErrExpired)
			return
		}

		streamSSELoop(w, r, sub, shutdownCh, ttl, func() bool {
			if sess.Status() != session.StatusActive {
				return false
			}
			if sess.Kind() == session.KindDevice && svcs.Devices != nil {
				if _, ok := svcs.Devices.Get(sess.DeviceID()); !ok {
					return false
				}
			}
			return true
		})
	}
}

func handleGuestEvents(svcs *Services, shutdownCh <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validateEventRequestOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin event subscription forbidden")
			return
		}
		if svcs == nil || svcs.GuestManager == nil || svcs.Events == nil {
			writeError(w, http.StatusNotFound, "guest event stream unavailable")
			return
		}
		token := guestTokenFromRequest(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}
		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			switch {
			case errors.Is(err, guest.ErrGuestRevoked):
				writeErrorCode(w, http.StatusUnauthorized, "guest revoked", "guest_revoked")
			case errors.Is(err, guest.ErrGuestRejected):
				writeErrorCode(w, http.StatusUnauthorized, "guest rejected", "guest_rejected")
			case errors.Is(err, guest.ErrRoomEnded):
				writeErrorCode(w, http.StatusUnauthorized, "room ended", "room_ended")
			case errors.Is(err, guest.ErrRoomExpired):
				writeErrorCode(w, http.StatusUnauthorized, "room expired", "room_expired")
			default:
				writeError(w, http.StatusUnauthorized, "invalid guest credential")
			}
			return
		}

		sub, err := svcs.Events.SubscribeGuest(room.ID, g.ID)
		if err != nil {
			if errors.Is(err, events.ErrTooManyStreams) {
				writeError(w, http.StatusTooManyRequests, "too many active event streams")
				return
			}
			writeError(w, http.StatusServiceUnavailable, "event stream unavailable")
			return
		}
		defer sub.Close()

		ttl := defaultMaxEventStreamTTL
		if rem := time.Until(room.ExpiresAt); rem > 0 && rem < ttl {
			ttl = rem
		} else if rem <= 0 {
			writeErrorCode(w, http.StatusUnauthorized, "room expired", "room_expired")
			return
		}

		streamSSELoop(w, r, sub, shutdownCh, ttl, func() bool {
			_, _, vErr := svcs.GuestManager.ValidateGuest(token)
			return vErr == nil
		})
	}
}

func streamSSELoop(
	w http.ResponseWriter,
	r *http.Request,
	sub *events.Subscription,
	shutdownCh <-chan struct{},
	ttl time.Duration,
	stillValid func() bool,
) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	rc := http.NewResponseController(w)

	readyData, _ := json.Marshal(map[string]any{
		"ts": time.Now().UnixMilli(),
	})
	if err := writeSSEFrame(w, flusher, rc, formatSSEEvent(0, "ready", readyData)); err != nil {
		return
	}

	if ttl <= 0 {
		ttl = defaultMaxEventStreamTTL
	}
	expireTimer := time.NewTimer(ttl)
	defer expireTimer.Stop()

	keepalive := time.NewTicker(eventKeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-shutdownCh:
			data, _ := json.Marshal(map[string]string{"reason": "server_stopping"})
			_ = writeSSEFrame(w, flusher, rc, formatSSEEvent(0, "reconnect", data))
			return
		case <-sub.Done():
			code := sub.CloseReason()
			if code == "" {
				code = "session_ended"
			}
			data, _ := json.Marshal(map[string]string{"code": code})
			_ = writeSSEFrame(w, flusher, rc, formatSSEEvent(0, events.TypeSessionEnded, data))
			return
		case <-expireTimer.C:
			data, _ := json.Marshal(map[string]string{"reason": "connection_expired"})
			_ = writeSSEFrame(w, flusher, rc, formatSSEEvent(0, "reconnect", data))
			return
		case <-sub.Resync():
			data, _ := json.Marshal(map[string]string{"reason": "queue_overflow"})
			if err := writeSSEFrame(w, flusher, rc, formatSSEEvent(0, "resync", data)); err != nil {
				return
			}
		case <-keepalive.C:
			if stillValid != nil && !stillValid() {
				data, _ := json.Marshal(map[string]string{"code": "unauthorized"})
				_ = writeSSEFrame(w, flusher, rc, formatSSEEvent(0, events.TypeSessionEnded, data))
				return
			}
			if err := writeSSEFrame(w, flusher, rc, ": keepalive\n\n"); err != nil {
				return
			}
		case ev := <-sub.C():
			raw, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if err := writeSSEFrame(w, flusher, rc, formatSSEEvent(ev.ID, ev.Type, raw)); err != nil {
				return
			}
		}
	}
}

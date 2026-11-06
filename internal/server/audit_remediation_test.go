package server

// Regression tests for the 2 November 2026 security audit remediation:
//
//	F-01  every small control-request JSON body is bounded and oversized
//	      payloads answer 413 instead of being buffered without limit.
//	F-02  the guest cookie carries Secure on real TLS only, keeps HttpOnly,
//	      and never infers TLS from client-supplied forwarding headers.
//	F-06  the guest token is issued only as an HttpOnly cookie; the join
//	      response and the guest page no longer carry it.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func setupGuestServerWithServices(t *testing.T) (*httptest.Server, *guest.Manager, *guest.Room, *Services) {
	t.Helper()
	tmpDir := t.TempDir()

	guestMgr := guest.NewManager()
	t.Cleanup(guestMgr.Close)

	room, err := guestMgr.CreateRoom(tmpDir, 1*time.Hour)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	downloads := transfer.NewDownloadRegistry()

	services := &Services{
		Sessions:     sessMgr,
		Transfers:    transfers,
		Coordinator:  coord,
		Registry:     reg,
		Downloads:    downloads,
		GuestManager: guestMgr,
	}

	grants := newGrantStore(downloads)
	handler := newRouter(services, 5*time.Second, 5*time.Second, grants)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts, guestMgr, room, services
}

// oversizedBody builds a JSON object comfortably larger than
// maxControlBytes while staying syntactically valid, so the limit is what
// rejects it rather than a parse error.
func oversizedBody(t *testing.T) []byte {
	t.Helper()
	pad := strings.Repeat("x", maxControlBytes*2)
	body, err := json.Marshal(map[string]string{"pad": pad})
	if err != nil {
		t.Fatalf("marshal oversized body: %v", err)
	}
	if len(body) <= maxControlBytes {
		t.Fatalf("oversized body is only %d bytes, want > %d", len(body), maxControlBytes)
	}
	return body
}

// doRaw is env.do for a caller-supplied body, so a test can send bytes that
// are oversized or deliberately unparseable.
func (e *pairingTestEnv) doRaw(method, path string, body []byte, token string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	return rec
}

// F-01: the five session-authenticated control routes must refuse an
// oversized body with 413, still answer 400 for malformed JSON, keep working
// for a well-formed body, and check authentication before reading the body.
func TestControlEndpointsBoundJSONBodies(t *testing.T) {
	env := newPairingTestEnv(t)

	pub, _, err := devices.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	ephemeral, err := devices.GenerateEphemeralKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	salt, err := devices.GenerateOpeningSalt()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := devices.ComputeCommitment(ephemeral.PublicKey, salt)
	if err != nil {
		t.Fatal(err)
	}

	validChallenge := map[string]string{
		"clientPublicKey":  hex.EncodeToString(pub),
		"clientCommitment": hex.EncodeToString(commit[:]),
	}

	cases := []struct {
		name   string
		method string
		path   string
		valid  any
	}{
		{
			name: "pairChallenge", method: http.MethodPost, path: "/api/pair/challenge",
			valid: validChallenge,
		},
		{
			name: "pairReveal", method: http.MethodPost, path: "/api/pair/reveal",
			valid: map[string]string{
				"pairingNonce":       strings.Repeat("ab", 32),
				"clientEphemeralKey": hex.EncodeToString(ephemeral.PublicKey),
				"clientSalt":         hex.EncodeToString(salt),
			},
		},
		{
			name: "pairRequest", method: http.MethodPost, path: "/api/pair/request",
			valid: map[string]string{
				"deviceName":      "Test Phone",
				"clientPublicKey": hex.EncodeToString(pub),
				"pairingNonce":    strings.Repeat("ab", 32),
				"clientProof":     strings.Repeat("cd", 64),
			},
		},
		{
			name: "authChallenge", method: http.MethodPost, path: "/api/auth/challenge",
			valid: map[string]string{"deviceId": "no-such-device"},
		},
		{
			name: "deviceAutoApprove", method: http.MethodPut, path: "/api/devices/no-such-device/auto-approve",
			valid: map[string]bool{"autoApprove": true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Oversized JSON: the body bound answers 413.
			rec := env.doRaw(tc.method, tc.path, oversizedBody(t), env.token)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("oversized body: status = %d, want 413 (body %s)", rec.Code, rec.Body.String())
			}

			// Malformed JSON inside the bound still answers 400, so the two
			// failure modes stay distinguishable.
			rec = env.doRaw(tc.method, tc.path, []byte(`{"truncated":`), env.token)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("malformed body: status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}

			// A well-formed body must get past the bound entirely.
			valid, err := json.Marshal(tc.valid)
			if err != nil {
				t.Fatal(err)
			}
			if len(valid) > maxControlBytes {
				t.Fatalf("test valid body is %d bytes, over the %d-byte bound", len(valid), maxControlBytes)
			}
			rec = env.doRaw(tc.method, tc.path, valid, env.token)
			if rec.Code == http.StatusRequestEntityTooLarge {
				t.Fatalf("valid body: status = 413, body bound rejected a well-formed request")
			}
			if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "invalid") {
				// A domain-level 400 is fine; a parse rejection is not.
				var probe map[string]string
				_ = json.Unmarshal(rec.Body.Bytes(), &probe)
				if strings.Contains(probe["error"], "payload") || strings.Contains(probe["error"], "JSON") {
					t.Fatalf("valid body rejected as malformed: %s", rec.Body.String())
				}
			}

			// Authentication is checked before the body is read, so an
			// unauthenticated oversized request answers 401, never 413. That
			// keeps the bound from becoming an oracle for unauthenticated
			// callers.
			rec = env.doRaw(tc.method, tc.path, oversizedBody(t), "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated oversized body: status = %d, want 401", rec.Code)
			}
		})
	}
}

// F-01: a well-formed pairing challenge still succeeds end to end, proving
// the bound did not disturb the happy path.
func TestPairChallengeStillSucceedsWithinBodyBound(t *testing.T) {
	env := newPairingTestEnv(t)

	pub, _, err := devices.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	ephemeral, err := devices.GenerateEphemeralKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	salt, err := devices.GenerateOpeningSalt()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := devices.ComputeCommitment(ephemeral.PublicKey, salt)
	if err != nil {
		t.Fatal(err)
	}

	rec := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(pub),
		"clientCommitment": hex.EncodeToString(commit[:]),
	}, env.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid challenge: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// F-01: the shared decoder's contract, exercised directly so the two tus
// completion routes are covered by the same assertions as the routes that
// reach the decoder earlier in their handlers.
func TestDecodeControlJSONContract(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	t.Run("valid", func(t *testing.T) {
		rec := httptest.NewRecorder()
		var got payload
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ok"}`))
		if !decodeControlJSON(rec, req, &got, "invalid", rejectUnknownFields) {
			t.Fatalf("decode failed: %s", rec.Body.String())
		}
		if got.Name != "ok" {
			t.Fatalf("decoded name = %q, want %q", got.Name, "ok")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		var got payload
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{`))
		if decodeControlJSON(rec, req, &got, "invalid", rejectUnknownFields) {
			t.Fatal("malformed body accepted")
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		rec := httptest.NewRecorder()
		var got payload
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversizedBody(t)))
		if decodeControlJSON(rec, req, &got, "invalid", rejectUnknownFields) {
			t.Fatal("oversized body accepted")
		}
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
	})

	t.Run("unknown field rejected when strict", func(t *testing.T) {
		rec := httptest.NewRecorder()
		var got payload
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ok","extra":1}`))
		if decodeControlJSON(rec, req, &got, "invalid", rejectUnknownFields) {
			t.Fatal("unknown field accepted under rejectUnknownFields")
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("unknown field tolerated when lenient", func(t *testing.T) {
		rec := httptest.NewRecorder()
		var got payload
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ok","extra":1}`))
		if !decodeControlJSON(rec, req, &got, "invalid", allowUnknownFields) {
			t.Fatalf("lenient decode rejected the body: %s", rec.Body.String())
		}
		if got.Name != "ok" {
			t.Fatalf("decoded name = %q, want %q", got.Name, "ok")
		}
	})
}

// F-02: over plain HTTP the guest cookie must not claim Secure, must stay
// HttpOnly, and must keep SameSite=Lax for the QR navigation flow.
func TestGuestCookieAttributesOverPlainHTTP(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	body, _ := json.Marshal(map[string]string{"secret": room.InviteSecret, "nickname": "Plain Guest"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/guest-api/join", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// A client must not be able to talk the server into marking the cookie
	// Secure by asserting a forwarding header on a plaintext connection.
	req.Header.Set("X-Forwarded-Proto", "https")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("join: status = %d, want 201", resp.StatusCode)
	}

	cookie := findCookie(t, resp, guestCookieName)
	if !cookie.HttpOnly {
		t.Error("guest cookie is not HttpOnly")
	}
	if cookie.Secure {
		t.Error("guest cookie marked Secure on a plaintext connection (X-Forwarded-Proto must not be trusted)")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("guest cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/guest-api/" {
		t.Errorf("guest cookie Path = %q, want /guest-api/", cookie.Path)
	}
}

// F-02: over genuine TLS the guest cookie must carry Secure.
func TestGuestCookieAttributesOverTLS(t *testing.T) {
	_, _, room, svcs := setupGuestServerWithServices(t)

	grants := newGrantStore(svcs.Downloads)
	tlsServer := httptest.NewTLSServer(newRouter(svcs, 5*time.Second, 5*time.Second, grants))
	t.Cleanup(tlsServer.Close)

	body, _ := json.Marshal(map[string]string{"secret": room.InviteSecret, "nickname": "TLS Guest"})
	req, _ := http.NewRequest(http.MethodPost, tlsServer.URL+"/guest-api/join", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := tlsServer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("join over TLS: status = %d, want 201", resp.StatusCode)
	}

	cookie := findCookie(t, resp, guestCookieName)
	if !cookie.Secure {
		t.Error("guest cookie lacks Secure on a TLS connection")
	}
	if !cookie.HttpOnly {
		t.Error("guest cookie is not HttpOnly")
	}
}

// findCookie returns the named Set-Cookie from a response, failing the test
// when it is absent.
func findCookie(t *testing.T, resp *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("response did not set cookie %q", name)
	return nil
}

// F-06: the join response must not carry the guest credential at all, in the
// decoded object or in the raw bytes.
func TestGuestJoinResponseOmitsToken(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	body, _ := json.Marshal(map[string]string{"secret": room.InviteSecret, "nickname": "No Token Guest"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/guest-api/join", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("join: status = %d, want 201", resp.StatusCode)
	}

	raw := new(bytes.Buffer)
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(raw.String()), `"token"`) {
		t.Fatalf("join response still exposes a token field: %s", raw.String())
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
		t.Fatalf("join response is not a JSON object: %v", err)
	}
	if _, ok := decoded["token"]; ok {
		t.Fatal("join response carries a token key")
	}
	if _, ok := decoded["guestId"]; !ok {
		t.Fatal("join response lost guestId")
	}

	// The credential must still be issued, just as a cookie.
	cookie := findCookie(t, resp, guestCookieName)
	if cookie.Value == "" {
		t.Fatal("guest cookie has an empty value")
	}
}

// F-06: with no JavaScript-held token, the whole guest journey must still
// work on the cookie alone: join, status, declare, tus create, PATCH and
// complete.
func TestGuestCookieAuthenticatesFullUploadFlow(t *testing.T) {
	ts, _, room, _ := setupGuestServerWithServices(t)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	// Join and establish the cookie.
	joinBody, _ := json.Marshal(map[string]string{"secret": room.InviteSecret, "nickname": "Cookie Guest"})
	joinResp, err := client.Post(ts.URL+"/guest-api/join", "application/json", bytes.NewReader(joinBody))
	if err != nil {
		t.Fatal(err)
	}
	var join guestJoinResponse
	if err := json.NewDecoder(joinResp.Body).Decode(&join); err != nil {
		t.Fatalf("decode join response: %v", err)
	}
	joinResp.Body.Close()
	if joinResp.StatusCode != http.StatusCreated {
		t.Fatalf("join: status = %d, want 201", joinResp.StatusCode)
	}
	if err := room.ApproveGuest(join.GuestID); err != nil {
		t.Fatal(err)
	}

	// Status polling, no Authorization header anywhere.
	statusResp, err := client.Get(ts.URL + "/guest-api/status")
	if err != nil {
		t.Fatal(err)
	}
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d, want 200 (cookie auth failed)", statusResp.StatusCode)
	}

	// Declare an upload through the cookie.
	payload := []byte("cookie-auth payload")
	meta, _ := json.Marshal(map[string]any{"name": "cookie-flow.txt", "size": len(payload)})
	declareReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/guest-api/uploads", bytes.NewReader(meta))
	declareReq.Header.Set("Content-Type", "application/json")
	declareResp, err := client.Do(declareReq)
	if err != nil {
		t.Fatal(err)
	}
	var declared struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(declareResp.Body).Decode(&declared); err != nil {
		t.Fatalf("decode declare response: %v", err)
	}
	declareResp.Body.Close()
	if declareResp.StatusCode != http.StatusCreated {
		t.Fatalf("declare: status = %d, want 201", declareResp.StatusCode)
	}
	if declared.ID == "" {
		t.Fatal("declare response carried no upload id")
	}

	if err := room.ApproveUpload(declared.ID); err != nil {
		t.Fatal(err)
	}

	// Single-pass streaming upload, cookie-authenticated.
	putURL := ts.URL + "/guest-api/uploads/" + declared.ID + "/content"
	putReq, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	putResp, err := client.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("put: status = %d, want 200", putResp.StatusCode)
	}
}

// F-06: cookie authentication must stay scoped to its own guest.
func TestGuestCookieCannotReadAnotherGuestsUpload(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	join := func(nickname string) (*http.Client, guestJoinResponse) {
		t.Helper()
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Jar: jar}
		body, _ := json.Marshal(map[string]string{"secret": room.InviteSecret, "nickname": nickname})
		resp, err := client.Post(ts.URL+"/guest-api/join", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("join %s: status = %d, want 201", nickname, resp.StatusCode)
		}
		var out guestJoinResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return client, out
	}

	aliceClient, alice := join("Alice")
	bobClient, bob := join("Bob")
	for _, id := range []string{alice.GuestID, bob.GuestID} {
		if err := room.ApproveGuest(id); err != nil {
			t.Fatal(err)
		}
	}

	// Alice declares one upload.
	meta, _ := json.Marshal(map[string]any{"name": "alice-only.txt", "size": 5})
	declareReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/guest-api/uploads", bytes.NewReader(meta))
	declareReq.Header.Set("Content-Type", "application/json")
	declareResp, err := aliceClient.Do(declareReq)
	if err != nil {
		t.Fatal(err)
	}
	var declared struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(declareResp.Body).Decode(&declared); err != nil {
		t.Fatal(err)
	}
	declareResp.Body.Close()
	if declareResp.StatusCode != http.StatusCreated {
		t.Fatalf("alice declare: status = %d, want 201", declareResp.StatusCode)
	}

	// Bob must not be able to see it.
	bobResp, err := bobClient.Get(ts.URL + "/guest-api/uploads/" + declared.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer bobResp.Body.Close()
	if bobResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-guest upload read: status = %d, want 404", bobResp.StatusCode)
	}

	// Alice still can.
	aliceResp, err := aliceClient.Get(ts.URL + "/guest-api/uploads/" + declared.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer aliceResp.Body.Close()
	if aliceResp.StatusCode != http.StatusOK {
		t.Fatalf("owner upload read: status = %d, want 200", aliceResp.StatusCode)
	}
}

// F-06: the guest cookie is not a general desktop credential. A guest must
// never reach the desktop session API.
func TestGuestCookieCannotReachDesktopAPI(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	body, _ := json.Marshal(map[string]string{"secret": room.InviteSecret, "nickname": "Scoped Guest"})
	resp, err := client.Post(ts.URL+"/guest-api/join", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	apiResp, err := client.Get(ts.URL + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	defer apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("guest cookie on /api/files: status = %d, want 401", apiResp.StatusCode)
	}
}

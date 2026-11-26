package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestAppShortCodeFlowAndSessionRevocation(t *testing.T) {
	tempConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempConfig)

	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() { app.shutdown(context.Background()) })
	ctx := context.Background()
	app.startup(ctx)

	if err := app.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	sess, err := app.sessions.Create()
	if err != nil {
		t.Fatalf("sessions.Create: %v", err)
	}

	sc := app.GetSessionShortCode(sess.Key())
	if !sc.Active || len(sc.Code) != 6 {
		t.Fatalf("GetSessionShortCode = %+v", sc)
	}

	reg, err := app.RegenerateSessionShortCode(sess.Key())
	if err != nil || !reg.Active {
		t.Fatalf("RegenerateSessionShortCode: %+v err=%v", reg, err)
	}

	// Redeem over real HTTP server on loopback.
	body, _ := json.Marshal(map[string]string{"code": reg.FormattedCode})
	resp, err := http.Post("http://"+app.ServerAddress()+"/api/connect-code", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/connect-code: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/connect-code status = %d, want 200", resp.StatusCode)
	}
	var tokenResp struct {
		Token      string `json:"token"`
		SessionKey string `json:"sessionKey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		t.Fatalf("decode connect-code response: %v", err)
	}
	if _, err := app.sessions.Validate(tokenResp.Token); err != nil {
		t.Fatalf("redeemed token should be valid: %v", err)
	}

	// Ending parent QR session must also revoke the child session minted via 6-digit code.
	if err := app.EndSession(sess.Key()); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if _, err := app.sessions.Validate(tokenResp.Token); err == nil {
		t.Fatalf("child session from short code should be revoked when parent session ends")
	}
}

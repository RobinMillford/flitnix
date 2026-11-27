package main

import (
	"testing"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/notify"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestHandleNotificationActionApprovesAndRevalidates(t *testing.T) {
	a := newIsolatedApp(t)

	sess, err := a.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	up, err := a.transfers.DeclareBatchItem(sess.Key(), "", "", "hello.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := a.coordinator.CreateBatch(sess.Key(), transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{DisplayName: "hello.txt", Size: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.coordinator.SetItemContentID(sess.Key(), batch.ID, batch.Items[0].ID, up.ID); err != nil {
		t.Fatal(err)
	}

	// Hidden visibility mode refuses notification approval rather than mutating state.
	if _, err := a.SetVisibilityMode(string(devices.VisibilityHidden)); err != nil {
		t.Fatal(err)
	}
	a.handleNotificationAction(notify.ActionInvocation{
		Kind:     notify.ActionApproveBatch,
		TargetID: batch.ID,
	})
	cur, err := a.coordinator.GetBatch(sess.Key(), batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ApprovalState != transfer.ApprovalPending {
		t.Fatalf("expected batch still pending when visibility is hidden, got %s", cur.ApprovalState)
	}

	// Restoring trusted visibility permits validated interactive batch approval.
	if _, err := a.SetVisibilityMode(string(devices.VisibilityTrustedOnly)); err != nil {
		t.Fatal(err)
	}
	a.handleNotificationAction(notify.ActionInvocation{
		Kind:     notify.ActionApproveBatch,
		TargetID: batch.ID,
	})
	cur, err = a.coordinator.GetBatch(sess.Key(), batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ApprovalState != transfer.ApprovalApproved {
		t.Fatalf("expected batch approved, got %s", cur.ApprovalState)
	}

	// Pairing action never approves pairing directly; only restores UI safely.
	a.windowHidden.Store(true)
	a.handleNotificationAction(notify.ActionInvocation{
		Kind:     notify.ActionReviewPairing,
		TargetID: "req-1",
	})
	if a.windowHidden.Load() {
		t.Fatal("expected ReviewPairing action to restore window without approving pairing")
	}
}

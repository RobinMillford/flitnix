package main

import (
	"context"
	"testing"
)

func TestAppDiscoveryLifecycleAndEndpoints(t *testing.T) {
	app := newIsolatedApp(t)
	t.Cleanup(func() { app.shutdown(context.Background()) })

	ep := app.discoveryLocalEndpoint()
	if ep.DeviceName == "" || ep.OS == "" || ep.Port <= 0 {
		t.Fatalf("unexpected discoveryLocalEndpoint: %+v", ep)
	}

	peers := app.ListNearbyPeers()
	if peers == nil {
		t.Fatalf("ListNearbyPeers returned nil slice")
	}

	// Ensure TriggerNearbyScan never panics even before startup.
	if scanned := app.TriggerNearbyScan(); scanned == nil {
		t.Fatalf("TriggerNearbyScan returned nil slice")
	}
}

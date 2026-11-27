package events

import (
	"testing"
	"time"
)

func BenchmarkHubPublishProgressWithSubscriber(b *testing.B) {
	hub := NewHub()
	defer hub.Close()

	sub, err := hub.SubscribePersonal("sess-bench")
	if err != nil {
		b.Fatal(err)
	}
	defer sub.Close()

	// Drain subscriber asynchronously to simulate an active SSE client.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-sub.Done():
				return
			case <-sub.Resync():
			case <-sub.C():
			}
		}
	}()

	payload := map[string]any{"id": "up-1", "uploaded": 1048576, "state": "uploading"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if hub.AllowProgress("up-1", 200*time.Millisecond) {
			hub.PublishPersonal("sess-bench", TypeUploadProgress, payload)
		}
	}
	b.StopTimer()
	sub.Close()
	<-done
}

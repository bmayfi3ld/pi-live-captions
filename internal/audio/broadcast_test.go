package audio

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBroadcastReachesSubscriber is the happy path: bytes fed to Run come out
// of a subscription unchanged.
func TestBroadcastReachesSubscriber(t *testing.T) {
	b := NewBroadcaster(nil)
	ch, unsub := b.Subscribe()
	defer unsub()

	want := bytes.Repeat([]byte("mp3"), 1000)
	go b.Run(context.Background(), bytes.NewReader(want))

	var got []byte
	deadline := time.After(2 * time.Second)
	for len(got) < len(want) {
		select {
		case c := <-ch:
			got = append(got, c...)
		case <-deadline:
			t.Fatalf("got %d of %d bytes before timeout", len(got), len(want))
		}
	}
	if !bytes.Equal(got, want) {
		t.Error("subscriber received different bytes than were fed in")
	}
}

// TestSlowSubscriberIsDroppedNotBlocking is the invariant the whole thing
// exists to protect: the ffmpeg producing these bytes is also producing the
// PCM the recognizer needs, so a listener that stops reading must be dropped
// rather than stalling the read loop.
func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	var drops atomic.Int64
	b := NewBroadcaster(nil)
	b.SetCallbacks(func() { drops.Add(1) }, nil)
	_, unsub := b.Subscribe() // never read
	defer unsub()

	done := make(chan struct{})
	go func() {
		b.Run(context.Background(), bytes.NewReader(make([]byte, 100*audioChunk)))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked on a subscriber that never reads")
	}
	if drops.Load() == 0 {
		t.Error("no drops counted for a subscriber that never read")
	}
}

// TestCloseAndUnsubscribeAreSafeUnderSend covers the shutdown race: only a
// sender may close a channel, so publish, unsubscribe and Close must not be
// able to close one twice or send on a closed one.
func TestCloseAndUnsubscribeAreSafeUnderSend(t *testing.T) {
	b := NewBroadcaster(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsub := b.Subscribe()
			go func() {
				for range ch {
				}
			}()
			time.Sleep(time.Millisecond)
			unsub()
			unsub() // idempotent
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			b.publish([]byte("x"))
		}
	}()

	time.Sleep(5 * time.Millisecond)
	b.Close()
	b.Close() // idempotent
	wg.Wait()

	// A subscription taken after Close must be closed, not left hanging.
	ch, _ := b.Subscribe()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("subscribing after Close yielded data")
		}
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Error("channel from a closed broadcaster was never closed")
	}
}

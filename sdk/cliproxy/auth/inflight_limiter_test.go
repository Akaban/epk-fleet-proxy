package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestPerAuthInFlightLimiter_CapAndQueue(t *testing.T) {
	l := &perAuthInFlightLimiter{}
	l.setLimit(2)

	r1, err := l.acquire(context.Background(), "cred1")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	r2, err := l.acquire(context.Background(), "cred1")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}

	third := make(chan struct{})
	go func() {
		r3, errAcquire := l.acquire(context.Background(), "cred1")
		if errAcquire != nil {
			t.Errorf("acquire 3: %v", errAcquire)
			close(third)
			return
		}
		close(third)
		r3()
	}()

	select {
	case <-third:
		t.Fatal("third acquire proceeded past the cap without a release")
	case <-time.After(50 * time.Millisecond):
	}

	r1()
	select {
	case <-third:
	case <-time.After(2 * time.Second):
		t.Fatal("third acquire still blocked after a slot was released")
	}

	// A different credential is not affected by cred1 saturation.
	rOther, err := l.acquire(context.Background(), "cred2")
	if err != nil {
		t.Fatalf("acquire cred2: %v", err)
	}
	rOther()
	r2()
}

func TestPerAuthInFlightLimiter_ContextAbortWhileQueued(t *testing.T) {
	l := &perAuthInFlightLimiter{}
	l.setLimit(1)

	release, err := l.acquire(context.Background(), "cred1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx, "cred1"); err == nil {
		t.Fatal("queued acquire should fail when its context ends")
	}
}

func TestPerAuthInFlightLimiter_DisabledAndIdempotentRelease(t *testing.T) {
	l := &perAuthInFlightLimiter{}
	// limit 0 = unlimited no-op
	for i := 0; i < 100; i++ {
		release, err := l.acquire(context.Background(), "cred1")
		if err != nil {
			t.Fatalf("unlimited acquire: %v", err)
		}
		release()
	}

	l.setLimit(1)
	r1, err := l.acquire(context.Background(), "cred1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	r1()
	r1() // double release must not free a phantom slot
	r2, err := l.acquire(context.Background(), "cred1")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	blocked := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, errAcquire := l.acquire(ctx, "cred1")
		blocked <- errAcquire
	}()
	if errAcquire := <-blocked; errAcquire == nil {
		t.Fatal("cap of 1 not enforced after a double release")
	}
	r2()
}

func TestHoldSlotThroughStream_ReleasesOnChannelClose(t *testing.T) {
	released := make(chan struct{})
	src := make(chan cliproxyexecutor.StreamChunk)
	sr := &cliproxyexecutor.StreamResult{Chunks: src}
	wrapped := holdSlotThroughStream(sr, func() { close(released) }, nil)

	go func() {
		src <- cliproxyexecutor.StreamChunk{Payload: []byte("a")}
		src <- cliproxyexecutor.StreamChunk{Payload: []byte("b")}
		close(src)
	}()

	var got []string
	for chunk := range wrapped.Chunks {
		got = append(got, string(chunk.Payload))
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("chunks not forwarded intact: %v", got)
	}
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("slot not released after the stream closed")
	}
}

func TestHoldSlotThroughStream_NilStreamReleasesImmediately(t *testing.T) {
	released := false
	holdSlotThroughStream(nil, func() { released = true }, nil)
	if !released {
		t.Fatal("nil stream must release the slot immediately")
	}
}

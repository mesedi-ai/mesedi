package api

// Contract test for DrainDispatches: it must not return while a
// dispatch goroutine is still running. The original failure mode was
// timing-dependent (a test's store Close raced a live dispatch and
// panicked only on CI's slower runner), so this test removes timing
// from the equation: a stub store blocks the dispatch on a channel,
// and the test asserts Drain stays blocked until the channel is
// released. A regression that unhooks the WaitGroup makes Drain
// return while the dispatch is provably still inside the store call,
// failing this test on every run, not one run in fifty.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

type blockingDispatchStore struct {
	store.Store
	entered chan struct{} // closed when the dispatch reaches the store
	release chan struct{} // dispatch blocks until this closes
}

func (s *blockingDispatchStore) GetFailureGroupByClassSignature(
	_ context.Context, _, _, _ string,
) (*store.FailureGroup, error) {
	close(s.entered)
	<-s.release
	return nil, store.ErrNotFound
}

func (s *blockingDispatchStore) ListEnabledProjectWebhooks(
	_ context.Context, _ string,
) ([]*store.ProjectWebhook, error) {
	return nil, nil
}

func TestDrainDispatchesWaitsForInflightDispatch(t *testing.T) {
	st := &blockingDispatchStore{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := &Handlers{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  st,
	}

	h.dispatchFailureGroupCreated("proj_drain", "cost_velocity", "cost_$10+|tenant:x", "")

	// The dispatch is provably in flight once it enters the store.
	select {
	case <-st.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch goroutine never reached the store")
	}

	drained := make(chan struct{})
	go func() {
		h.DrainDispatches()
		close(drained)
	}()

	// Drain must still be blocked: the dispatch is sitting inside
	// the store call.
	select {
	case <-drained:
		t.Fatal("DrainDispatches returned while a dispatch was still " +
			"in flight; the WaitGroup is not tracking dispatch goroutines")
	case <-time.After(100 * time.Millisecond):
	}

	close(st.release)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainDispatches never returned after the dispatch finished")
	}
}

// TestDrainDispatchesContextHonorsDeadline: the shutdown-path variant
// must give up when its context does, even with a dispatch stuck,
// and must return cleanly once dispatches finish.
func TestDrainDispatchesContextHonorsDeadline(t *testing.T) {
	st := &blockingDispatchStore{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := &Handlers{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  st,
	}
	h.dispatchFailureGroupCreated("proj_drain_ctx", "cost_velocity", "cost_$1+|tenant:y", "")
	<-st.entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.DrainDispatchesContext(ctx); err == nil {
		t.Fatal("DrainDispatchesContext returned nil with a dispatch still " +
			"blocked; the shutdown deadline was not honored")
	}

	close(st.release)
	if err := h.DrainDispatchesContext(context.Background()); err != nil {
		t.Fatalf("DrainDispatchesContext after release: %v", err)
	}
}

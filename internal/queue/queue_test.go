package queue

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"videoremix/internal/download"
	"videoremix/internal/recipe"
)

// fakeDispatcher succeeds or fails based on the recipe id, without ffmpeg.
type fakeDispatcher struct {
	mu     sync.Mutex
	failN  int // fail this many times before succeeding (per job)
	counts map[recipe.RecipeID]int
}

func (d *fakeDispatcher) Dispatch(job Job) (RenderResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.counts == nil {
		d.counts = map[recipe.RecipeID]int{}
	}
	d.counts[job.RecipeID]++
	if d.counts[job.RecipeID] <= d.failN {
		// transient failure to exercise retry
		return RenderResult{}, fmt.Errorf("transient boom")
	}
	return RenderResult{OutputPath: "out/" + string(job.RecipeID) + ".mp4"}, nil
}

func TestEnqueueBatchCompletes(t *testing.T) {
	store := NewMemoryJobStore()
	reporter := NewChannelReporter()
	svc := NewService(store, &fakeDispatcher{}, reporter, DefaultRetryPolicy(), 4)
	defer svc.Shutdown()

	recipes := []recipe.Recipe{
		{ID: "r1", Status: recipe.StatusFrozen},
		{ID: "r2", Status: recipe.StatusFrozen},
		{ID: "r3", Status: recipe.StatusFrozen},
	}
	batch, err := svc.EnqueueBatch(recipes, PriorityNormal)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if !waitForDone(t, svc, batch, 3*time.Second) {
		t.Fatal("batch did not complete in time")
	}
	snap, _ := svc.BatchProgress(batch)
	if snap.Completed != 3 {
		t.Fatalf("expected 3 completed, got %+v", snap)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	store := NewMemoryJobStore()
	reporter := NewChannelReporter()
	svc := NewService(store, &fakeDispatcher{failN: 2}, reporter, DefaultRetryPolicy(), 2)
	defer svc.Shutdown()

	batch, err := svc.EnqueueBatch([]recipe.Recipe{{ID: "retryme", Status: recipe.StatusFrozen}}, PriorityNormal)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !waitForDone(t, svc, batch, 5*time.Second) {
		t.Fatal("retry batch did not complete")
	}
	snap, _ := svc.BatchProgress(batch)
	if snap.Completed != 1 {
		t.Fatalf("expected 1 completed after retries, got %+v", snap)
	}
}

func TestTerminalErrorDeadLetters(t *testing.T) {
	store := NewMemoryJobStore()
	reporter := NewChannelReporter()
	// dispatcher returns a permanent error -> should dead-letter immediately.
	perm := &permDispatcher{}
	svc := NewService(store, perm, reporter, DefaultRetryPolicy(), 2)
	defer svc.Shutdown()

	batch, _ := svc.EnqueueBatch([]recipe.Recipe{{ID: "bad", Status: recipe.StatusFrozen}}, PriorityNormal)
	if !waitForDone(t, svc, batch, 3*time.Second) {
		t.Fatal("dead-letter batch did not settle")
	}
	snap, _ := svc.BatchProgress(batch)
	if snap.Failed != 1 {
		t.Fatalf("expected 1 failed/dead-lettered, got %+v", snap)
	}
	if perm.calls != 1 {
		t.Fatalf("permanent error must not be retried, got %d calls", perm.calls)
	}
}

type permDispatcher struct {
	mu    sync.Mutex
	calls int
}

func (d *permDispatcher) Dispatch(job Job) (RenderResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return RenderResult{}, fmt.Errorf("%w: cannot render", download.ErrPermanent)
}

func waitForDone(t *testing.T, svc *Service, batch BatchID, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap, err := svc.BatchProgress(batch)
		if err == nil && snap.Total > 0 && snap.Done() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

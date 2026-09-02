package inference

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testQueue(t *testing.T, global int) *Queue {
	t.Helper()
	addr := os.Getenv("DAIKI_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("DAIKI_TEST_REDIS_ADDR not set")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.FlushDB(context.Background()).Err(); _ = client.Close() })
	return NewQueue(client, QueueConfig{GlobalLimit: global, PerPrincipalLimit: 1, WorkloadLimits: map[Workload]int{WorkloadFast: global, WorkloadDeep: 1}, LeaseTTL: 600 * time.Millisecond, MaxWait: 800 * time.Millisecond, PollInterval: 20 * time.Millisecond})
}
func TestQueueGlobalAndPerPrincipalConcurrency(t *testing.T) {
	q := testQueue(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := q.Acquire(ctx, "a", "user:1", WorkloadFast, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release(context.Background())
	c, err := q.Acquire(ctx, "c", "user:2", WorkloadFast, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release(context.Background())
	result := make(chan *Ticket, 1)
	errs := make(chan error, 1)
	go func() {
		ticket, err := q.Acquire(ctx, "b", "user:1", WorkloadFast, 5)
		if err != nil {
			errs <- err
			return
		}
		result <- ticket
	}()
	select {
	case <-result:
		t.Fatal("second request from same principal acquired concurrently")
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(120 * time.Millisecond):
	}
	a.Release(context.Background())
	select {
	case b := <-result:
		b.Release(context.Background())
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("queued request did not acquire after release")
	}
}
func TestQueueCancellationRemovesWaitingRequest(t *testing.T) {
	q := testQueue(t, 1)
	held, err := q.Acquire(context.Background(), "held", "user:1", WorkloadFast, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := q.Acquire(ctx, "waiting", "user:2", WorkloadFast, 5); done <- err }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	err = <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	snap, err := q.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Queued[WorkloadFast] != 0 {
		t.Fatalf("cancelled request still queued: %#v", snap)
	}
}
func TestQueueLeaseRenewsUntilRelease(t *testing.T) {
	q := testQueue(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticket, err := q.Acquire(ctx, "long", "user:1", WorkloadFast, 5)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond)
	snap, err := q.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.GlobalActive != 1 {
		t.Fatalf("lease was not renewed: %#v", snap)
	}
	ticket.Release(context.Background())
	time.Sleep(30 * time.Millisecond)
	snap, err = q.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.GlobalActive != 0 {
		t.Fatalf("lease was not released: %#v", snap)
	}
}

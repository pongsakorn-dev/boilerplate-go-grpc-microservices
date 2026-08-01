package interceptor

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"

	"github.com/example/gomicro/internal/platform/apperr"
)

var testInfo = &grpc.UnaryServerInfo{FullMethod: "/order.v1.OrderService/CreateOrder"}

func TestAdmissionShedsExcessLoadImmediately(t *testing.T) {
	t.Parallel()

	const limit = 2

	release := make(chan struct{})
	entered := make(chan struct{}, limit)

	intercept := Admission(limit)
	handler := func(ctx context.Context, req any) (any, error) {
		entered <- struct{}{}
		<-release
		return "ok", nil
	}

	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = intercept(context.Background(), nil, testInfo, handler)
		}()
	}

	// Wait until both slots are genuinely occupied before testing the third.
	for range limit {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("handlers did not start")
		}
	}

	// The third request must be REJECTED, not queued.
	//
	// Queueing is the failure mode this interceptor exists to prevent: a queued request
	// holds memory while its caller's deadline burns down, and it is usually abandoned by
	// the time it reaches the front. A fast rejection lets the caller retry elsewhere or
	// fail visibly.
	start := time.Now()
	_, err := intercept(context.Background(), nil, testInfo, handler)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the third concurrent request was admitted; the limit is not enforced")
	}
	if kind := apperr.KindOf(err); kind != apperr.KindResourceExhausted {
		t.Errorf("kind = %v, want ResourceExhausted", kind)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("rejection took %v -- it queued instead of shedding", elapsed)
	}

	// RetryInfo must be attached, or a well-behaved client retries immediately and the
	// retries themselves become the load.
	appErr, ok := apperr.From(err)
	if !ok {
		t.Fatal("not an apperr.Error")
	}
	var found bool
	for _, d := range appErr.Details {
		if _, ok := d.(*errdetails.RetryInfo); ok {
			found = true
		}
	}
	if !found {
		t.Error("no RetryInfo detail; clients have no backoff hint and will hot-retry")
	}

	close(release)
	wg.Wait()
}

// TestAdmissionReleasesSlotOnPanic is the leak this interceptor is most likely to have.
//
// Without a deferred release, every panicking handler permanently consumes a slot. After
// `limit` panics the service rejects everything forever, and nothing in the logs points at
// admission control -- it looks like sustained overload with no traffic.
func TestAdmissionReleasesSlotOnPanic(t *testing.T) {
	t.Parallel()

	intercept := Admission(1)

	panicking := func(ctx context.Context, req any) (any, error) { panic("boom") }
	ok := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	func() {
		defer func() { _ = recover() }()
		_, _ = intercept(context.Background(), nil, testInfo, panicking)
	}()

	// The single slot must be available again.
	resp, err := intercept(context.Background(), nil, testInfo, ok)
	if err != nil {
		t.Fatalf("the slot leaked after a panic: %v", err)
	}
	if resp != "ok" {
		t.Errorf("resp = %v, want ok", resp)
	}
}

// TestAdmissionRejectsAlreadyExpiredRequests matters most under load.
//
// When a queue builds, the requests reaching the front are the oldest, and their callers
// have usually given up. Executing them burns a slot and a database connection to produce a
// response nobody reads -- which deepens the queue that caused the problem.
func TestAdmissionRejectsAlreadyExpiredRequests(t *testing.T) {
	t.Parallel()

	intercept := Admission(10)

	var called bool
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := intercept(ctx, nil, testInfo, handler)

	if err == nil {
		t.Fatal("an already-expired request was admitted")
	}
	if called {
		t.Error("the handler ran for a request whose caller had already given up")
	}
	if kind := apperr.KindOf(err); kind != apperr.KindResourceExhausted {
		t.Errorf("kind = %v, want ResourceExhausted", kind)
	}
}

func TestAdmissionAllowsSequentialTraffic(t *testing.T) {
	t.Parallel()

	// A concurrency limit must not behave like a rate limit: N sequential requests through
	// a limit of 1 must all succeed.
	intercept := Admission(1)
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	for i := range 50 {
		if _, err := intercept(context.Background(), nil, testInfo, handler); err != nil {
			t.Fatalf("sequential request %d was rejected: %v", i, err)
		}
	}
}

func TestAdmissionTreatsZeroLimitAsOne(t *testing.T) {
	t.Parallel()

	// A misconfigured limit of 0 must not wedge the service into rejecting everything.
	intercept := Admission(0)
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	if _, err := intercept(context.Background(), nil, testInfo, handler); err != nil {
		t.Errorf("a zero limit rejected all traffic: %v", err)
	}
}

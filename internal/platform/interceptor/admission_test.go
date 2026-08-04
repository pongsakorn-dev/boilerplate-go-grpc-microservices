package interceptor

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"

	"github.com/example/gomicro/internal/platform/apperr"
	"github.com/example/gomicro/internal/platform/observability"
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

// TestTheLivenessProbeIsNotShed is the difference between a survivable overload and a
// rolling outage.
//
// grpcapi registers the health server on the same grpc.Server as the business methods --
// correctly, because Kubernetes' native grpc: probe dials the port that actually serves
// traffic. That means Health/Check goes through this interceptor like anything else, and
// with no exemption it was answered with ResourceExhausted the moment the service saturated.
//
// Follow that through. The liveness probe fails, so the kubelet RESTARTS the pod. A replica
// disappears from a service that was already at its limit, its share of the traffic moves to
// the pods that remain, and the next one saturates. A load shedder that kills the thing
// proving you are alive converts overload into cascading failure -- and the restarts look
// like the cause rather than the consequence, so the incident points at the wrong thing.
//
// The test fills every slot for real rather than reaching into the limiter, and asserts both
// halves: the probe is answered, AND an ordinary method is still being shed. The second half
// is what stops this passing vacuously if the limit ever fails to engage.
func TestTheLivenessProbeIsNotShed(t *testing.T) {
	t.Parallel()

	const limit = 2

	release := make(chan struct{})
	entered := make(chan struct{}, limit)

	intercept := Admission(limit)
	blocking := func(ctx context.Context, req any) (any, error) {
		entered <- struct{}{}
		<-release
		return "ok", nil
	}

	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = intercept(context.Background(), nil, testInfo, blocking)
		}()
	}
	for range limit {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("handlers did not start")
		}
	}
	defer func() {
		close(release)
		wg.Wait()
	}()

	// Half one: the limiter really is full, so the exemption below is not vacuous.
	if _, err := intercept(context.Background(), nil, testInfo, blocking); err == nil {
		t.Fatal("an ordinary method was admitted past a full limiter, so this test proves nothing")
	}

	// Half two: the probe is answered anyway.
	healthInfo := &grpc.UnaryServerInfo{FullMethod: observability.HealthCheckMethod}
	served := func(ctx context.Context, req any) (any, error) { return "SERVING", nil }

	resp, err := intercept(context.Background(), nil, healthInfo, served)
	if err != nil {
		t.Fatalf("the liveness probe was shed: %v.\n\n"+
			"The kubelet reads that as a dead pod and restarts it -- removing a replica from "+
			"a service that is already overloaded, and moving its traffic onto the pods that "+
			"are next to fall over.", err)
	}
	if resp != "SERVING" {
		t.Errorf("resp = %v, want the handler's own answer", resp)
	}
}

// TestTheProbeExemptionDoesNotLeakToOtherMethods is the other side of the exemption.
//
// An exemption written as a prefix match, or extended to "anything on the health service",
// would be a free pass an attacker can name. This pins it to the one method the kubelet
// actually calls.
func TestTheProbeExemptionDoesNotLeakToOtherMethods(t *testing.T) {
	t.Parallel()

	intercept := Admission(1)

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	blocking := func(ctx context.Context, req any) (any, error) {
		entered <- struct{}{}
		<-release
		return "ok", nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = intercept(context.Background(), nil, testInfo, blocking)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not start")
	}
	defer func() {
		close(release)
		wg.Wait()
	}()

	// Neighbouring methods on the same service, and a lookalike, all stay subject to the limit.
	for _, method := range []string{
		"/grpc.health.v1.Health/Watch",
		"/grpc.health.v1.Health/CheckSomethingElse",
		"/grpc.health.v1.HealthAdmin/Check",
		"/order.v1.OrderService/Check",
	} {
		t.Run(method, func(t *testing.T) {
			info := &grpc.UnaryServerInfo{FullMethod: method}
			if _, err := intercept(context.Background(), nil, info, blocking); err == nil {
				t.Errorf("%s was admitted past a full limiter.\n\n"+
					"Only %s is exempt. A broader match is a shed-control bypass that any "+
					"caller can select by choosing a method name.", method, observability.HealthCheckMethod)
			}
		})
	}
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

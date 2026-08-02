package client_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/example/gomicro/internal/platform/client"
)

// TestABudgetIsReservedFromTheCallersDeadline is the behaviour a service mesh cannot provide.
//
// A mesh can enforce a timeout on the call. It cannot arrange for YOUR handler to still be
// running when that timeout fires -- and that is the whole value. Spend the caller's entire
// remaining deadline on the upstream and, when the upstream is slow, your handler is cancelled
// at the same instant: no log line naming the culprit, no metric, and a caller who learns only
// that something, somewhere, was slow.
//
// Measured at the CALLEE, because that is the only place the propagated deadline is real.
func TestABudgetIsReservedFromTheCallersDeadline(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t)

	const callerBudget = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
	defer cancel()

	if err := invoke(ctx, conn, "order.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("call: %v", err)
	}

	got := up.observedCalls()[0]
	if !got.hadDeadline {
		t.Fatal("the upstream received no deadline at all, so nothing was budgeted")
	}

	// "LESS THAN THE WHOLE" IS NOT THE ASSERTION, and writing it that way was a real mistake
	// caught by sabotage: with the reserve deleted entirely this test still passed, because
	// the deadline measured at the callee is always a fraction of a millisecond short of the
	// caller's simply from transit. A test that cannot tell a 10% reserve from a 0.05% one is
	// not testing the reserve.
	//
	// 10% of 2s is 200ms; transit over bufconn is microseconds. Anything above 95% means the
	// budget did not happen.
	const ceiling = callerBudget * 95 / 100
	if got.deadline > ceiling {
		t.Errorf("the upstream was given %s of a %s budget (more than %s).\n\n"+
			"When it is slow, the caller's deadline and this service's expire together and "+
			"the handler is cancelled before it can record which dependency was responsible.",
			got.deadline, callerBudget, ceiling)
	}

	// And the reserve is a slice, not most of it.
	if got.deadline < callerBudget/2 {
		t.Errorf("the upstream was given only %s of a %s budget; the reserve is meant to be a "+
			"slice, not most of it", got.deadline, callerBudget)
	}
}

// TestACallWithNoDeadlineGetsTheDefault covers the case that otherwise runs forever.
//
// An outbound RPC with no deadline holds a goroutine and a connection until the upstream feels
// like answering, which under load is indistinguishable from never.
func TestACallWithNoDeadlineGetsTheDefault(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t, func(o *client.Options) { o.DefaultTimeout = 3 * time.Second })

	if err := invoke(context.Background(), conn, "order.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("call: %v", err)
	}

	got := up.observedCalls()[0]
	if !got.hadDeadline {
		t.Fatal("a call made with no deadline reached the upstream with no deadline; it would " +
			"hold a goroutine and a connection for as long as the upstream stayed silent")
	}
	if got.deadline > 3*time.Second {
		t.Errorf("deadline = %s, want no more than the 3s default", got.deadline)
	}
}

// TestACallIsRefusedWhenTooLittleTimeRemains asserts the call is not merely doomed but never
// made.
//
// Dialling anyway spends a connection, a goroutine, and a complete upstream handler on an
// answer already certain to arrive too late -- and the upstream cannot tell, so it does the
// entire job, including its database work, before discovering nobody is listening. Under the
// load that causes the tight deadlines in the first place, that is the difference between a
// slow dependency and a collapsed one.
func TestACallIsRefusedWhenTooLittleTimeRemains(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t, func(o *client.Options) { o.MinBudget = 500 * time.Millisecond })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := invoke(ctx, conn, "order.v1.OrderService/GetOrder")
	if err == nil {
		t.Fatal("a call with 50ms left against a 500ms minimum budget succeeded")
	}

	if n := up.count(); n != 0 {
		t.Errorf("the upstream handled %d calls; it should never have been dialled.\n\n"+
			"The point of a minimum budget is to spend nothing -- no connection, no upstream "+
			"handler -- on an answer that cannot arrive in time.", n)
	}

	// And it must be legible as OUR decision, not an upstream fault. A metric that counts
	// this as an upstream timeout blames the wrong service during an incident.
	var ce *client.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error is not a *client.Error: %T %v", err, err)
	}
	if ce.Reason != "BUDGET_EXHAUSTED" {
		t.Errorf("Reason = %q, want BUDGET_EXHAUSTED so the cause is unambiguous", ce.Reason)
	}
	if ce.Code != codes.DeadlineExceeded {
		t.Errorf("Code = %s, want DeadlineExceeded", ce.Code)
	}
}

// TestAnExpiredDeadlineIsRefusedRatherThanSent is the degenerate case of the same rule.
func TestAnExpiredDeadlineIsRefusedRatherThanSent(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	if err := invoke(ctx, conn, "order.v1.OrderService/GetOrder"); err == nil {
		t.Fatal("a call with an already-expired deadline was sent")
	}
	if n := up.count(); n != 0 {
		t.Errorf("the upstream handled %d calls with an already-expired deadline", n)
	}
}

// TestTheBudgetDoesNotExtendTheCallersDeadline guards the direction of the arithmetic.
//
// context.WithTimeout takes the EARLIER of the two deadlines, so a budget larger than what
// remains is silently a no-op -- and a test that only checks "the call succeeded" cannot tell
// the difference. This one gives the client a default far larger than the caller's deadline
// and asserts the caller's still wins.
func TestTheBudgetDoesNotExtendTheCallersDeadline(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t, func(o *client.Options) { o.DefaultTimeout = time.Hour })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := invoke(ctx, conn, "order.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("call: %v", err)
	}

	if got := up.observedCalls()[0].deadline; got > time.Second {
		t.Errorf("the upstream was given %s when the caller allowed 1s; a budget must only "+
			"ever shorten", got)
	}
}

// TestASlowUpstreamFailsInsideTheHandler is the payoff, stated as an outcome.
//
// The caller allows 600ms; the upstream takes longer than that. With a reserve, the upstream
// call fails first and this code is still running to observe it. Without one, both deadlines
// fire together and there is nothing left to observe with.
func TestASlowUpstreamFailsInsideTheHandler(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	up.blockEachCallFor(2 * time.Second)
	conn := up.dial(t)

	const callerBudget = 600 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
	defer cancel()

	start := time.Now()
	err := invoke(ctx, conn, "order.v1.OrderService/GetOrder")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a call to an upstream slower than the budget succeeded")
	}
	if ctx.Err() != nil {
		t.Errorf("the caller's context expired too (after %s).\n\n"+
			"That is the failure this reserve exists to prevent: the handler is cancelled at "+
			"the same instant as its upstream call, so it cannot log which dependency was "+
			"slow or return anything better than a bare deadline error.", elapsed)
	}
	if elapsed >= callerBudget {
		t.Errorf("the call took %s of a %s budget; it should have given up sooner", elapsed, callerBudget)
	}
}

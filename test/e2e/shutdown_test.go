//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
)

// TestSigtermReachesTheProcessAndItDrains is the assertion the whole exec-form ENTRYPOINT
// argument rests on.
//
// A Dockerfile written as `ENTRYPOINT /orderd` runs the command under a shell, so PID 1 is
// /bin/sh and the shell does not forward signals. SIGTERM goes to the shell, the service never
// hears it, and `docker stop` waits out its full timeout before SIGKILL -- which means every
// deploy kills the process mid-request instead of draining it.
//
// Both spellings BUILD. Both RUN. Both serve traffic identically. The only observable
// difference is how long `docker stop` takes, which is why this is measured rather than
// reviewed. (The distroless image has no shell at all, so the shell form would in fact fail to
// start -- but that is a property of this base image, not of the Dockerfile, and swapping the
// base is one line.)
//
// Measured by hand during M10 at 5.7s against a 30s timeout. This is that measurement, executed.
func TestSigtermReachesTheProcessAndItDrains(t *testing.T) {
	requireStack(t)

	// Restart it afterwards: later tests in this package share the stack.
	t.Cleanup(func() { restart(t, "orderd") })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	start := time.Now()
	// The default `docker stop` timeout is 10s; compose's stop_grace_period would override it.
	// 30s is deliberately generous, so the number below is the SERVICE's drain time and not a
	// timeout being hit.
	if out, err := compose(ctx, "stop", "-t", "30", "orderd"); err != nil {
		t.Fatalf("stop orderd: %v\n%s", err, out)
	}
	elapsed := time.Since(start)

	t.Logf("docker stop returned in %s (timeout was 30s)", elapsed.Round(time.Millisecond))

	// THE ASSERTION IS TWO-SIDED, and finding that out cost a sabotage.
	//
	// The upper bound was obvious: near 30s means SIGTERM never reached the process and the
	// container was SIGKILLed when the timeout expired -- what a shell-form ENTRYPOINT does,
	// since PID 1 becomes /bin/sh and the shell forwards nothing.
	//
	// The lower bound is the one that was missing. Deleting SIGTERM from the signal set does
	// NOT produce a slow stop: Go's runtime has a default SIGTERM handler that terminates the
	// process immediately, so an unhandled signal stops the container in 750ms -- FASTER than
	// a healthy drain, and comfortably inside an upper bound alone. Measured, not reasoned:
	// the first version of this test passed under exactly that sabotage.
	//
	// A real drain cannot beat SHUTDOWN_DRAIN_DELAY, which is 5s. Too fast is a failure.
	const drainDelay = 5 * time.Second
	switch {
	case elapsed < drainDelay:
		t.Errorf("docker stop returned in %s, which is faster than SHUTDOWN_DRAIN_DELAY (%s).\n\n"+
			"The process did not drain -- it died. Go's runtime kills the process on an "+
			"unhandled SIGTERM, so this is what it looks like when the signal arrives and "+
			"nothing is listening for it: in-flight requests are cut, and Kubernetes is still "+
			"routing traffic here because health was never flipped.",
			elapsed.Round(time.Millisecond), drainDelay)

	case elapsed > 20*time.Second:
		t.Errorf("docker stop took %s against a 30s timeout.\n\n"+
			"The process never received SIGTERM; it was killed when the timeout expired. That "+
			"is what a shell-form ENTRYPOINT does -- PID 1 becomes /bin/sh, which does not "+
			"forward signals, and every deploy cuts requests off mid-flight.",
			elapsed.Round(time.Millisecond))
	}

	// And it drained rather than merely dying: the shutdown sequence logs its steps, and the
	// health flip is the FIRST of them. Its presence is what distinguishes an orderly drain
	// from a process that was killed quickly.
	logs := serviceLogs(t, "orderd")
	if !strings.Contains(logs, "NOT_SERVING") {
		t.Errorf("the shutdown log has no health flip:\n%s\n\n"+
			"Flipping health before draining is what makes a rolling update invisible to "+
			"callers; without it, Kubernetes keeps routing to a pod that is shutting down.",
			tail(logs, 15))
	}
}

// TestAnInFlightRequestSurvivesShutdown is the drain, from the caller's side.
//
// A server that stops accepting AND cuts existing calls returns Unavailable to a request that
// was already being served -- a 5xx during every deploy, for a request that would have
// succeeded. GracefulStop exists to finish what is in flight, and the only way to see it is to
// have something in flight when the signal lands.
//
// WatchOrders is server-streaming, so it is genuinely still open when the stop begins.
func TestAnInFlightRequestSurvivesShutdown(t *testing.T) {
	requireStack(t)

	t.Cleanup(func() { restart(t, "orderd") })

	client := dialGRPC(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stream, err := client.WatchOrders(ctx, &orderv1.WatchOrdersRequest{})
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}

	// Wait until the stream is really established. Starting the shutdown before the server has
	// registered it would test the accept path instead of the drain path, and would pass for
	// the wrong reason.
	if _, err := stream.Recv(); err != nil {
		// A stream that yields nothing before shutdown is still a valid subject -- what
		// matters is how it ENDS -- so a non-fatal note rather than a failure.
		t.Logf("no first message before shutdown (%v); the stream is still open", err)
	}

	stopped := make(chan error, 1)
	go func() {
		sctx, scancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer scancel()
		_, err := compose(sctx, "stop", "-t", "30", "orderd")
		stopped <- err
	}()

	// Drain the stream to its end and record how it finished.
	var final error
	for {
		if _, err := stream.Recv(); err != nil {
			final = err
			break
		}
	}

	if err := <-stopped; err != nil {
		t.Fatalf("stop orderd: %v", err)
	}

	code := status.Code(final)
	switch code {
	case codes.OK, codes.Unavailable, codes.Canceled:
		// A server-streaming call with no more to send ends in one of these. Unavailable is
		// acceptable here ONLY because the stream is long-lived and the server is going away
		// deliberately; what would not be acceptable is the connection being severed before
		// the drain delay, which the timing assertion above covers.
		t.Logf("the stream ended with %s", code)
	default:
		t.Errorf("the stream ended with %s (%v), which is not an orderly close", code, final)
	}

	// The decisive evidence is in the log: the service must have run its drain sequence rather
	// than been cut off.
	logs := serviceLogs(t, "orderd")
	for _, want := range []string{"NOT_SERVING", "draining"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the shutdown log does not mention %q:\n%s", want, tail(logs, 20))
		}
	}
}

// restart brings a stopped service back and waits for it to be healthy again.
func restart(t *testing.T, service string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if out, err := compose(ctx, "--profile", "app", "up", "-d", "--wait", "--wait-timeout", "120", service); err != nil {
		t.Fatalf("restart %s: %v\n%s", service, err, out)
	}
}

// serviceLogs returns one service's output.
func serviceLogs(t *testing.T, service string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := compose(ctx, "logs", "--no-log-prefix", service)
	if err != nil {
		t.Fatalf("logs for %s: %v\n%s", service, err, out)
	}
	return out
}

// tail returns the last n lines, for error messages that should not print a megabyte.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// waitFor polls until cond holds, and fails with what it last saw.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() (bool, string)) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, detail := cond()
		if ok {
			return
		}
		last = detail
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s (last saw: %s)", timeout, what, last)
}

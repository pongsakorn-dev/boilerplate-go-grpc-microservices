// Package eventstest runs a real NATS server inside the test process.
//
// WHY THIS IS IN THE DEFAULT TIER, when testcontainers is not.
//
// test/tiers_test.go keeps Docker-dependent packages out of `go test ./...`, and until this
// package existed that list also named nats-server -- on the assumption that it was "large".
// Measured on this machine, with a warm standard library:
//
//	compile the nats-server tree, cold   8.0s   (once, then cached)
//	rebuild the test binary, warm        0.7s
//	test binary size                      24 MB
//
// Eight seconds once is not the same kind of cost as needing a Docker daemon, and the two
// should not share a tier. Putting broker tests behind the integration tag would have meant
// that anyone without Docker never ran them -- which is precisely backwards, since running
// them without Docker is the entire reason to embed the server.
//
// So the rule changed and the measurement is recorded here rather than in a commit message.
package eventstest

import (
	"context"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Server is a running in-process NATS server with JetStream enabled.
type Server struct {
	URL string
	NC  *nats.Conn
	JS  jetstream.JetStream
}

// Start runs a server on a random free port and connects a client to it.
//
// Everything is torn down through t.Cleanup, in an order that matters on Windows -- see the
// comment on the shutdown below.
func Start(t *testing.T) *Server {
	t.Helper()

	// t.TempDir() is called BEFORE the shutdown cleanup is registered, so its removal is
	// registered first and therefore runs LAST (cleanups are LIFO). Windows cannot delete a
	// directory while a handle to it is open, so a server still holding its store would make
	// the temp-dir cleanup fail the test with a message about the filesystem rather than
	// about NATS.
	storeDir := t.TempDir()

	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // -1 asks the OS for a free port, so parallel packages never collide
		JetStream: true,
		StoreDir:  storeDir,
		NoLog:     true,
		NoSigs:    true, // a test binary must keep its own signal handling
	}

	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("start the embedded nats server: %v", err)
	}
	go srv.Start()

	if !srv.ReadyForConnections(15 * time.Second) {
		srv.Shutdown()
		t.Fatal("the embedded nats server did not become ready within 15s")
	}

	t.Cleanup(func() {
		srv.Shutdown()
		// WaitForShutdown is not optional on Windows: Shutdown only signals, and returning
		// before the store files are closed leaves open handles that make removing the temp
		// directory fail.
		srv.WaitForShutdown()
	})

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect to the embedded nats server: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	return &Server{URL: srv.ClientURL(), NC: nc, JS: js}
}

// Messages returns every message currently stored on subject, oldest first.
//
// It reads through an ephemeral consumer rather than the stream API so it sees exactly what a
// subscriber would, including headers.
func (s *Server) Messages(t *testing.T, stream, subject string) []jetstream.Msg {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := s.JS.Stream(ctx, stream)
	if err != nil {
		t.Fatalf("open stream %q: %v", stream, err)
	}

	cons, err := st.CreateConsumer(ctx, jetstream.ConsumerConfig{
		FilterSubject: subject,
		AckPolicy:     jetstream.AckNonePolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("create a reader for %q: %v", subject, err)
	}

	info, err := cons.Info(ctx)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	pending := int(info.NumPending)
	if pending == 0 {
		return nil
	}

	batch, err := cons.Fetch(pending, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetch from %q: %v", subject, err)
	}

	var out []jetstream.Msg
	for m := range batch.Messages() {
		out = append(out, m)
	}
	if err := batch.Error(); err != nil {
		t.Fatalf("read %q: %v", subject, err)
	}
	return out
}

// Count returns how many messages are stored on subject.
func (s *Server) Count(t *testing.T, stream, subject string) int {
	t.Helper()
	return len(s.Messages(t, stream, subject))
}

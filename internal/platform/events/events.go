// Package events publishes the outbox to NATS JetStream and consumes it back.
//
// It implements outbox.Publisher, which is the only seam between the relay and any broker.
// Nothing in internal/platform/outbox changes because this package exists.
//
// # The tenant is not in the subject, and that was measured rather than assumed
//
// The obvious subject layout is {prefix}.{tenant}.{event_type}, so that a consumer can filter
// one tenant's events with "events.acme.>". It is what most examples show. It is wrong here,
// and the reason is not hypothetical:
//
//	tenant_id "acme.com"  ->  subject "events.acme.com.order.created"
//	a consumer filtering  ->  "events.acme.>"
//	                          RECEIVES IT.
//
// A dot in a tenant id is not an attack. "acme.com" is an ordinary tenant id, and any
// identity provider that mints domain-shaped or email-shaped tenants produces one on the first
// customer. NATS subjects are dot-delimited with no escaping, so the tenant silently becomes
// two tokens and lands inside a DIFFERENT tenant's subtree. publisher_test.go reproduces exactly
// that against a real embedded server.
//
// Rejecting such tenants at publish time is worse, not better: the relay would fail forever on
// a row it can never publish, blocking every event behind it, for a customer whose only crime
// was having a domain name.
//
// So the subject carries only the event type, and the tenant travels as a header and inside
// the payload. Per-tenant filtering, if a fork needs it, is a per-tenant STREAM or a filter on
// the header -- both of which handle a dot correctly.
//
// # What this package guarantees
//
//	✓ A publish is not reported successful until JetStream has acknowledged storing it.
//	  The relay marks an outbox row published only after that ack, so a broker outage
//	  leaves rows unpublished rather than losing them.
//	✓ Relay-side duplicates collapse. Every message carries Nats-Msg-Id, and JetStream drops
//	  a repeat within NATS_DUPLICATE_WINDOW.
//	✗ Duplicates are NOT eliminated. A republish after the window is a new message. The
//	  consumer deduplicates with processed_events, in the same transaction as its effect,
//	  which is the boundary that actually holds.
//	✗ Ordering is NOT guaranteed. See the note in internal/platform/outbox: two relays claim
//	  disjoint batches concurrently, so events arrive out of order.
package events

import (
	"fmt"
	"strings"

	"github.com/example/gomicro/internal/platform/outbox"
)

// Header names carried on every published message.
//
// Prefixed with X- and spelled out, because these are read by operators staring at
// `nats stream view` during an incident, not only by this consumer.
const (
	HeaderTenantID    = "X-Tenant-Id"
	HeaderEventType   = "X-Event-Type"
	HeaderAggregateID = "X-Aggregate-Id"
	HeaderOccurredAt  = "X-Occurred-At"

	// HeaderDLQReason explains why a dead-lettered message was given up on. Without it, a
	// DLQ is a pile of messages with no indication of what went wrong.
	HeaderDLQReason = "X-Dlq-Reason"

	// HeaderDLQDeliveries records how many attempts were made before giving up, which is how
	// you tell a poison message (1 attempt, terminated immediately) from a message that
	// exhausted its retries against a broken dependency (MaxDeliver attempts).
	HeaderDLQDeliveries = "X-Dlq-Deliveries"
)

// Permanent marks err as unretryable, and IsPermanent reports whether it is.
//
// Re-exported from outbox rather than reimplemented, so a Handler author marking a poison
// message and a Publisher marking an unsendable one are using the same thing. The definition
// lives with the Publisher interface it belongs to; these two lines exist so writing a
// projection does not require importing the outbox.
var (
	Permanent   = outbox.Permanent
	IsPermanent = outbox.IsPermanent
)

// Subject builds the subject an event is published to.
//
// The event type becomes subject tokens directly, so "order.created" with prefix "events"
// gives "events.order.created" -- a hierarchy consumers can filter on ("events.order.>")
// without this package inventing a second naming scheme.
//
// Every token is validated. A subject NATS considers invalid fails the publish, and a subject
// containing a wildcard token is accepted by the server and then matched by filters that were
// never meant to see it. Both are permanent: the same event type will produce the same subject
// on every retry until a human changes the code.
func Subject(prefix, eventType string) (string, error) {
	if err := validTokens("subject prefix", prefix); err != nil {
		return "", Permanent(err)
	}
	if err := validTokens("event type", eventType); err != nil {
		return "", Permanent(err)
	}
	return prefix + "." + eventType, nil
}

// validTokens checks a dot-delimited subject fragment.
func validTokens(what, s string) error {
	if s == "" {
		return fmt.Errorf("%s is empty", what)
	}
	for _, token := range strings.Split(s, ".") {
		if token == "" {
			// "order..created" -- an empty token. The server answers a publish to such a
			// subject with "no response from stream", which is the SAME error a missing
			// stream produces, so the real cause is invisible unless it is caught here.
			return fmt.Errorf("%s %q has an empty token", what, s)
		}
		for _, r := range token {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			default:
				return fmt.Errorf("%s %q contains %q, which is not allowed in a subject token "+
					"(letters, digits, underscore and hyphen only)", what, s, r)
			}
		}
	}
	return nil
}

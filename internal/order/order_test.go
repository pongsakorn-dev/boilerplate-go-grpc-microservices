package order_test

import (
	"testing"

	"github.com/example/gomicro/internal/order"
)

// TestStatusRoundTripsThroughItsName is the guard for a persistence decision.
//
// The database stores the status NAME, not the numeric iota. That choice is what makes
// inserting a new status in the middle of the const block a safe edit instead of a silent
// reinterpretation of every existing row -- but it only holds while String and ParseStatus
// remain exact inverses.
//
// Nothing else would catch a drift. Adding a status and forgetting the ParseStatus arm
// compiles, passes every unit test, writes rows happily, and fails only when something reads
// one back -- in production, on the row a customer just created.
func TestStatusRoundTripsThroughItsName(t *testing.T) {
	t.Parallel()

	statuses := order.AllStatuses()
	if len(statuses) == 0 {
		t.Fatal("AllStatuses is empty, so this guard would pass forever")
	}

	seen := make(map[string]order.Status, len(statuses))
	for _, want := range statuses {
		name := want.String()

		if name == "UNSPECIFIED" {
			t.Errorf("%d stringifies to UNSPECIFIED but is in AllStatuses; it would be stored "+
				"as the zero value and read back as a different status", want)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("statuses %d and %d both stringify to %q, so the stored name is ambiguous",
				prev, want, name)
		}
		seen[name] = want

		got, err := order.ParseStatus(name)
		if err != nil {
			t.Errorf("ParseStatus(%q) failed: %v\n\n"+
				"A status String() can produce but ParseStatus cannot read means rows written "+
				"by this binary cannot be read by it.", name, err)
			continue
		}
		if got != want {
			t.Errorf("round trip of %d via %q produced %d", want, name, got)
		}
	}
}

// TestParseStatusRejectsUnknownNames matters because the alternative is silent misreading.
//
// A row holding a status this binary does not know -- written by a newer version, or by a
// failed rollback -- must surface as an error. Defaulting it to UNSPECIFIED, or to PENDING,
// would quietly change the meaning of somebody's order.
func TestParseStatusRejectsUnknownNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"REFUNDED", "pending", "Pending", "3", "PENDING "} {
		if _, err := order.ParseStatus(name); err == nil {
			t.Errorf("ParseStatus(%q) succeeded; an unrecognised status must not be guessed at", name)
		}
	}
}

// TestParseStatusAcceptsTheZeroValue keeps the empty string readable.
//
// A nullable or defaulted column reads back as "", and that is legitimately "no status set"
// rather than corruption.
func TestParseStatusAcceptsTheZeroValue(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "UNSPECIFIED"} {
		got, err := order.ParseStatus(name)
		if err != nil {
			t.Errorf("ParseStatus(%q) failed: %v", name, err)
		}
		if got != order.StatusUnspecified {
			t.Errorf("ParseStatus(%q) = %v, want StatusUnspecified", name, got)
		}
	}
}

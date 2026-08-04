package order_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/order"
)

func usd(t *testing.T, units int64, nanos int32) order.Money {
	t.Helper()
	m, err := order.NewMoney("USD", units, nanos)
	if err != nil {
		t.Fatalf("NewMoney(USD, %d, %d): %v", units, nanos, err)
	}
	return m
}

// This is the single most important test in the package.
//
// Money in a float is the canonical silent data-corruption bug: float64 cannot represent
// 0.10, so summing line items drifts by fractions of a cent that reconcile fine in a unit
// test and wrongly at month end. No integration test catches it, because the drift is
// smaller than any assertion anyone thinks to write.
//
// So instead of testing for the symptom, this forbids the cause outright, at the AST
// level. If someone adds a float to the money path, the build goes red with a file and
// line number.
func TestNoFloatingPointInMoneyPath(t *testing.T) {
	t.Parallel()

	// parser.ParseFile over an explicit file list rather than parser.ParseDir: ParseDir is
	// deprecated as of Go 1.25 because it ignores build tags when grouping files into
	// packages. Listing the files here keeps the guard dependency-free and makes the
	// exclusion rule visible.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	scanned := 0

	for _, entry := range entries {
		name := entry.Name()
		// Skip test files: this very file mentions "float64" as a string literal, and
		// fixtures are allowed to be looser than the code they guard.
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++

		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == "float32" || id.Name == "float64" {
				t.Errorf("%s: %s appears in the domain package. Money must never touch "+
					"floating point -- use Money's integer arithmetic instead.",
					fset.Position(id.Pos()), id.Name)
			}
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("scanned no source files -- the guard would silently pass forever")
	}
}

func TestMoneyNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		units      int64
		nanos      int32
		wantUnits  int64
		wantNanos  int32
		wantErrIs  error
		skipVerify bool
	}{
		{name: "already normal", units: 19, nanos: 990000000, wantUnits: 19, wantNanos: 990000000},
		{name: "zero", units: 0, nanos: 0, wantUnits: 0, wantNanos: 0},
		{name: "negative amount", units: -19, nanos: -990000000, wantUnits: -19, wantNanos: -990000000},
		{name: "mixed signs fold to a consistent value", units: 1, nanos: -500000000, wantUnits: 0, wantNanos: 500000000},
		{name: "negative units, positive nanos", units: -1, nanos: 500000000, wantUnits: 0, wantNanos: -500000000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := order.NewMoney("USD", tc.units, tc.nanos)
			if err != nil {
				t.Fatalf("NewMoney: %v", err)
			}
			if got.Units != tc.wantUnits || got.Nanos != tc.wantNanos {
				t.Errorf("got {%d, %d}, want {%d, %d}", got.Units, got.Nanos, tc.wantUnits, tc.wantNanos)
			}
		})
	}
}

func TestMoneyValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    order.Money
		want error
	}{
		{"valid", order.Money{CurrencyCode: "USD", Units: 1, Nanos: 500000000}, nil},
		{"lowercase currency", order.Money{CurrencyCode: "usd"}, order.ErrInvalidCurrencyCode},
		{"two letter currency", order.Money{CurrencyCode: "US"}, order.ErrInvalidCurrencyCode},
		{"empty currency", order.Money{}, order.ErrInvalidCurrencyCode},
		{"digits in currency", order.Money{CurrencyCode: "US1"}, order.ErrInvalidCurrencyCode},
		{"nanos too large", order.Money{CurrencyCode: "USD", Nanos: 1000000000}, order.ErrInvalidMoney},
		{"sign disagreement", order.Money{CurrencyCode: "USD", Units: 1, Nanos: -1}, order.ErrInvalidMoney},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.m.Validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMoneyArithmetic(t *testing.T) {
	t.Parallel()

	t.Run("add carries nanos into units", func(t *testing.T) {
		t.Parallel()
		got, err := usd(t, 0, 700000000).Add(usd(t, 0, 400000000))
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if got.Units != 1 || got.Nanos != 100000000 {
			t.Errorf("0.70 + 0.40 = {%d, %d}, want {1, 100000000}", got.Units, got.Nanos)
		}
	})

	// The exact case float64 gets wrong: 0.10 has no finite binary representation, so
	// ten of them summed in float is 0.9999999999999999.
	t.Run("ten times ten cents is exactly one unit", func(t *testing.T) {
		t.Parallel()
		total := order.Zero("USD")
		for i := 0; i < 10; i++ {
			var err error
			total, err = total.Add(usd(t, 0, 100000000))
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		if total.Units != 1 || total.Nanos != 0 {
			t.Errorf("sum = {%d, %d}, want exactly {1, 0}", total.Units, total.Nanos)
		}
	})

	t.Run("subtract crossing zero", func(t *testing.T) {
		t.Parallel()
		got, err := usd(t, 1, 0).Sub(usd(t, 1, 500000000))
		if err != nil {
			t.Fatalf("Sub: %v", err)
		}
		if got.Units != 0 || got.Nanos != -500000000 {
			t.Errorf("1.00 - 1.50 = {%d, %d}, want {0, -500000000}", got.Units, got.Nanos)
		}
	})

	t.Run("multiply by a quantity", func(t *testing.T) {
		t.Parallel()
		got, err := usd(t, 19, 990000000).Mul(3)
		if err != nil {
			t.Fatalf("Mul: %v", err)
		}
		if got.Units != 59 || got.Nanos != 970000000 {
			t.Errorf("19.99 * 3 = {%d, %d}, want {59, 970000000}", got.Units, got.Nanos)
		}
	})

	t.Run("multiply by zero", func(t *testing.T) {
		t.Parallel()
		got, err := usd(t, 19, 990000000).Mul(0)
		if err != nil {
			t.Fatalf("Mul: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("got %s, want zero", got)
		}
	})

	// No implicit conversion, ever. An exchange rate has a timestamp and a source; an
	// arithmetic helper that invents one is a financial bug with a friendly API.
	t.Run("mixing currencies is an error, not a conversion", func(t *testing.T) {
		t.Parallel()
		eur, err := order.NewMoney("EUR", 1, 0)
		if err != nil {
			t.Fatalf("NewMoney: %v", err)
		}
		if _, err := usd(t, 1, 0).Add(eur); !errors.Is(err, order.ErrCurrencyMismatch) {
			t.Errorf("Add: got %v, want ErrCurrencyMismatch", err)
		}
		if _, err := usd(t, 1, 0).Sub(eur); !errors.Is(err, order.ErrCurrencyMismatch) {
			t.Errorf("Sub: got %v, want ErrCurrencyMismatch", err)
		}
		if _, err := usd(t, 1, 0).Compare(eur); !errors.Is(err, order.ErrCurrencyMismatch) {
			t.Errorf("Compare: got %v, want ErrCurrencyMismatch", err)
		}
	})

	t.Run("overflow is detected, not wrapped", func(t *testing.T) {
		t.Parallel()
		huge := order.Money{CurrencyCode: "USD", Units: 9_000_000_000}
		if _, err := huge.Mul(1000); !errors.Is(err, order.ErrMoneyOverflow) {
			t.Errorf("Mul: got %v, want ErrMoneyOverflow", err)
		}
	})
}

func TestMoneyCompare(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b order.Money
		want int
	}{
		{"less", usdOrPanic(1, 0), usdOrPanic(2, 0), -1},
		{"equal", usdOrPanic(1, 500000000), usdOrPanic(1, 500000000), 0},
		{"greater", usdOrPanic(2, 0), usdOrPanic(1, 999999999), 1},
		{"nanos decide", usdOrPanic(1, 1), usdOrPanic(1, 2), -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.a.Compare(tc.b)
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			if got != tc.want {
				t.Errorf("Compare(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestMoneyString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		m    order.Money
		want string
	}{
		{usdOrPanic(19, 990000000), "19.99 USD"},
		{usdOrPanic(0, 0), "0.00 USD"},
		{usdOrPanic(-5, -500000000), "-5.50 USD"},
		{usdOrPanic(1, 0), "1.00 USD"},
	}
	for _, tc := range cases {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func usdOrPanic(units int64, nanos int32) order.Money {
	m, err := order.NewMoney("USD", units, nanos)
	if err != nil {
		panic(err)
	}
	return m
}

// TestUnitsBoundLeavesRoomForNanos pins an off-by-one that silently wrapped int64.
//
// maxUnits was (MaxInt64 / NanosPerUnit) = 9223372036, and its comment said it "bounds Units so
// that Units*NanosPerUnit cannot overflow int64". That is true of the MULTIPLICATION and false
// of the expression it actually guards: totalNanos computes Units*NanosPerUnit + Nanos.
//
//	maxUnits * NanosPerUnit  = 9223372036000000000
//	MaxInt64                 = 9223372036854775807
//	headroom for Nanos       =          854775807
//	Nanos may legally reach  =          999999999
//
// So for Units == maxUnits and Nanos above 854775807, totalNanos wrapped negative -- silently,
// with no error -- and Add, Sub, Compare and the ordering they imply all returned nonsense for
// a Money value that Validate had already accepted. Wrapping in a MONEY type is the failure this
// whole integral representation exists to prevent, so it is worth a named test.
func TestUnitsBoundLeavesRoomForNanos(t *testing.T) {
	t.Parallel()

	// The largest value the type accepts. If the bound is right, this is representable and
	// arithmetic on it is monotonic; if it is wrong, the sum wraps to a negative number.
	extreme, err := order.NewMoney("USD", order.MaxUnits(), 999_999_999)
	if err != nil {
		t.Fatalf("the maximum representable Money was rejected: %v", err)
	}

	one, err := order.NewMoney("USD", 0, 1)
	if err != nil {
		t.Fatalf("NewMoney(0,1): %v", err)
	}

	// Compare is implemented over totalNanos, so a wrap shows up as the largest value in the
	// type comparing LESS than one nanounit.
	got, err := extreme.Compare(one)
	if err != nil {
		t.Fatalf("comparing the maximum Money returned an error: %v", err)
	}
	if got != 1 {
		t.Errorf("the maximum Money compares %d against 0.000000001, want 1.\n\n"+
			"totalNanos overflowed int64 and wrapped negative, so every comparison and every "+
			"sum involving a near-maximum amount is silently wrong -- in the type that exists "+
			"precisely so money arithmetic cannot be silently wrong.", got)
	}

	// And adding to it must be refused rather than wrapping.
	if _, err := extreme.Add(one); err == nil {
		t.Error("adding to the maximum Money succeeded; overflow must be an error, not a wrap")
	}
}

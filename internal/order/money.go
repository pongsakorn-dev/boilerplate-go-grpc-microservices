package order

import (
	"errors"
	"fmt"
	"strings"
)

// NanosPerUnit is the number of nano-units in one whole currency unit.
const NanosPerUnit = 1_000_000_000

// maxUnits bounds Units so that Units*NanosPerUnit cannot overflow int64.
// At roughly 9.2 billion whole units this is far past any real order total, but the
// bound is checked rather than assumed -- silent wraparound in a money type is the
// exact class of bug this package exists to prevent.
const maxUnits = (1<<63 - 1) / NanosPerUnit

var (
	// ErrCurrencyMismatch is returned when arithmetic mixes two currencies. There is
	// deliberately no implicit conversion: an exchange rate is a business decision with
	// a timestamp, not something an arithmetic helper should invent.
	ErrCurrencyMismatch = errors.New("currency mismatch")

	// ErrMoneyOverflow is returned when a result cannot be represented.
	ErrMoneyOverflow = errors.New("money overflow")

	// ErrInvalidCurrencyCode is returned for anything that is not three uppercase letters.
	ErrInvalidCurrencyCode = errors.New("invalid currency code")

	// ErrInvalidMoney is returned when Units and Nanos disagree in sign, or Nanos is
	// outside (-NanosPerUnit, NanosPerUnit).
	ErrInvalidMoney = errors.New("invalid money")
)

// Money is an exact decimal amount, mirroring the google.type.Money shape.
//
// The representation is deliberately integral. A float64 has 53 bits of mantissa and
// cannot represent 0.10 exactly, so summing a hundred line items in float drifts by
// amounts that reconcile fine in tests and wrongly in production. money_test.go contains
// an AST assertion that float64 appears nowhere in this package.
//
// The zero value is a valid zero amount with no currency; use NewMoney to construct.
type Money struct {
	// CurrencyCode is an ISO-4217 three-letter code, e.g. "USD".
	CurrencyCode string

	// Units is the whole-unit part. For "USD", 1 Unit is 1 dollar.
	Units int64

	// Nanos is the fractional part in units of 10^-9, always in the open interval
	// (-NanosPerUnit, NanosPerUnit) and always the same sign as Units.
	Nanos int32
}

// NewMoney builds a normalized Money and validates it.
func NewMoney(currencyCode string, units int64, nanos int32) (Money, error) {
	m := Money{CurrencyCode: currencyCode, Units: units, Nanos: nanos}.normalized()
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	return m, nil
}

// Zero returns a zero amount in the given currency.
func Zero(currencyCode string) Money {
	return Money{CurrencyCode: strings.ToUpper(currencyCode)}
}

// Validate reports whether m is a well-formed amount.
func (m Money) Validate() error {
	if !validCurrencyCode(m.CurrencyCode) {
		return fmt.Errorf("%w: %q", ErrInvalidCurrencyCode, m.CurrencyCode)
	}
	if m.Nanos <= -NanosPerUnit || m.Nanos >= NanosPerUnit {
		return fmt.Errorf("%w: nanos %d out of range", ErrInvalidMoney, m.Nanos)
	}
	if (m.Units > 0 && m.Nanos < 0) || (m.Units < 0 && m.Nanos > 0) {
		return fmt.Errorf("%w: units %d and nanos %d disagree in sign", ErrInvalidMoney, m.Units, m.Nanos)
	}
	if m.Units > maxUnits || m.Units < -maxUnits {
		return fmt.Errorf("%w: units %d", ErrMoneyOverflow, m.Units)
	}
	return nil
}

// Add returns m+o. Both operands must share a currency.
func (m Money) Add(o Money) (Money, error) { return m.combine(o, false) }

// Sub returns m-o. Both operands must share a currency.
func (m Money) Sub(o Money) (Money, error) { return m.combine(o, true) }

func (m Money) combine(o Money, negate bool) (Money, error) {
	if err := m.requireSameCurrency(o); err != nil {
		return Money{}, err
	}
	a, err := m.totalNanos()
	if err != nil {
		return Money{}, err
	}
	b, err := o.totalNanos()
	if err != nil {
		return Money{}, err
	}
	if negate {
		b = -b
	}
	sum := a + b
	// Signed overflow is undefined-ish territory in most languages and merely wrong in
	// Go; detect it by checking that the sign of the result is consistent with the operands.
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrMoneyOverflow, m, o)
	}
	return fromTotalNanos(m.CurrencyCode, sum)
}

// Mul returns m*n. Used for line totals (unit price times quantity).
func (m Money) Mul(n int64) (Money, error) {
	total, err := m.totalNanos()
	if err != nil {
		return Money{}, err
	}
	if n != 0 {
		product := total * n
		if product/n != total {
			return Money{}, fmt.Errorf("%w: %s * %d", ErrMoneyOverflow, m, n)
		}
		return fromTotalNanos(m.CurrencyCode, product)
	}
	return Zero(m.CurrencyCode), nil
}

// Neg returns -m.
func (m Money) Neg() Money {
	return Money{CurrencyCode: m.CurrencyCode, Units: -m.Units, Nanos: -m.Nanos}
}

// IsZero reports whether m is exactly zero, regardless of currency.
func (m Money) IsZero() bool { return m.Units == 0 && m.Nanos == 0 }

// IsNegative reports whether m is strictly less than zero.
func (m Money) IsNegative() bool { return m.Units < 0 || m.Nanos < 0 }

// Compare returns -1, 0 or +1. It errors on a currency mismatch rather than returning a
// meaningless ordering.
func (m Money) Compare(o Money) (int, error) {
	if err := m.requireSameCurrency(o); err != nil {
		return 0, err
	}
	a, err := m.totalNanos()
	if err != nil {
		return 0, err
	}
	b, err := o.totalNanos()
	if err != nil {
		return 0, err
	}
	switch {
	case a < b:
		return -1, nil
	case a > b:
		return 1, nil
	default:
		return 0, nil
	}
}

// String renders the amount for humans and logs. It is NOT a wire format and NOT a
// parsing contract -- the wire format is the proto message.
func (m Money) String() string {
	neg := ""
	units, nanos := m.Units, m.Nanos
	if units < 0 || nanos < 0 {
		neg = "-"
		units, nanos = -units, -nanos
	}
	frac := strings.TrimRight(fmt.Sprintf("%09d", nanos), "0")
	if frac == "" {
		return fmt.Sprintf("%s%d.00 %s", neg, units, m.CurrencyCode)
	}
	if len(frac) < 2 {
		frac += "0"
	}
	return fmt.Sprintf("%s%d.%s %s", neg, units, frac, m.CurrencyCode)
}

func (m Money) requireSameCurrency(o Money) error {
	if m.CurrencyCode != o.CurrencyCode {
		return fmt.Errorf("%w: %q vs %q", ErrCurrencyMismatch, m.CurrencyCode, o.CurrencyCode)
	}
	return nil
}

func (m Money) totalNanos() (int64, error) {
	if m.Units > maxUnits || m.Units < -maxUnits {
		return 0, fmt.Errorf("%w: units %d", ErrMoneyOverflow, m.Units)
	}
	return m.Units*NanosPerUnit + int64(m.Nanos), nil
}

func fromTotalNanos(currencyCode string, total int64) (Money, error) {
	m := Money{
		CurrencyCode: currencyCode,
		Units:        total / NanosPerUnit,
		Nanos:        int32(total % NanosPerUnit),
	}
	return m, nil
}

// normalized folds out-of-range nanos into units and makes the two agree in sign.
func (m Money) normalized() Money {
	m.CurrencyCode = strings.ToUpper(m.CurrencyCode)
	m.Units += int64(m.Nanos) / NanosPerUnit
	m.Nanos %= NanosPerUnit
	switch {
	case m.Units > 0 && m.Nanos < 0:
		m.Units--
		m.Nanos += NanosPerUnit
	case m.Units < 0 && m.Nanos > 0:
		m.Units++
		m.Nanos -= NanosPerUnit
	}
	return m
}

func validCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

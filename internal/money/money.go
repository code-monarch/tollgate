// Package money represents amounts as integer minor units + a currency code.
// There is deliberately no float constructor: money is never a float in Tollgate
// (docs/03-data-model.md, invariant 1).
package money

import (
	"errors"
	"fmt"
	"strconv"
)

// Amount is a quantity of a currency's minor units (e.g. 10000 == 0.01 USDC at
// 6 decimals).
type Amount struct {
	Minor    int64
	Currency string
}

// New builds an Amount from minor units.
func New(minor int64, currency string) Amount {
	return Amount{Minor: minor, Currency: currency}
}

// Parse reads a base-10 integer string of minor units. It rejects decimals,
// signs, blank input and blank currency — the only accepted form is a
// non-negative integer.
func Parse(s, currency string) (Amount, error) {
	if s == "" {
		return Amount{}, errors.New("money: empty amount")
	}
	if currency == "" {
		return Amount{}, errors.New("money: empty currency")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return Amount{}, fmt.Errorf("money: invalid minor units %q: %w", s, err)
	}
	if n < 0 {
		return Amount{}, fmt.Errorf("money: negative amount %q", s)
	}
	return Amount{Minor: n, Currency: currency}, nil
}

// String renders the minor-unit integer (no currency, no decimal point).
func (a Amount) String() string { return strconv.FormatInt(a.Minor, 10) }

func (a Amount) sameCurrency(b Amount) error {
	if a.Currency != b.Currency {
		return fmt.Errorf("money: currency mismatch %s vs %s", a.Currency, b.Currency)
	}
	return nil
}

// Add returns a+b, erroring on currency mismatch.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	return Amount{Minor: a.Minor + b.Minor, Currency: a.Currency}, nil
}

// GTE reports whether a >= b, erroring on currency mismatch.
func (a Amount) GTE(b Amount) (bool, error) {
	if err := a.sameCurrency(b); err != nil {
		return false, err
	}
	return a.Minor >= b.Minor, nil
}

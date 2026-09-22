package hdfcmail

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"hdfc2ff/internal/money"
)

// ErrNoBalance is returned when a message is not a balance notice.
var ErrNoBalance = errors.New("not an HDFC balance notice")

// BalanceNotice is HDFC stating the balance of an account at a point in time.
//
// These are not transactions and must never be imported as such, but they are
// the bank's own ground truth: comparing one against the balance implied by
// the ledger catches drift that no per-transaction check can see.
type BalanceNotice struct {
	Amount int64 // minor units (paise)
	AsOf   time.Time
	Last4  string
}

// balancePattern matches the notice wording:
//
//	Available balance in your account ending XX1234 is Rs. INR 19,329.74
//	as on 15-JAN-26.
//
// Note the doubled currency prefix and the XX in front of the account digits.
var balancePattern = regexp.MustCompile(
	`Available balance in your account ending (?P<last4>[Xx*\s]*\d{4})\s+is\s+` +
		`Rs\.?\s*(?:INR\s*)?(?P<amount>[\d,]+(?:\.\d{1,2})?)\s+as on\s+` +
		`(?P<date>\d{1,2}-[A-Za-z]{3}-\d{2,4})`)

var balanceDateLayouts = []string{"02-Jan-06", "02-Jan-2006", "2-Jan-06", "2-Jan-2006"}

// ParseBalance extracts a balance notice from a message body. It returns
// ErrNoBalance when the message is not one, which is the common case.
func ParseBalance(subject, body string, loc *time.Location) (*BalanceNotice, error) {
	flat := strings.Join(strings.Fields(body), " ")

	for _, haystack := range []string{flat, body} {
		m := balancePattern.FindStringSubmatch(haystack)
		if m == nil {
			continue
		}

		amount, err := money.ParseMinor(subexp(balancePattern, m, "amount"))
		if err != nil {
			return nil, fmt.Errorf("balance notice: %w", err)
		}
		asOf, err := ParseDate(subexp(balancePattern, m, "date"), loc, balanceDateLayouts...)
		if err != nil {
			return nil, fmt.Errorf("balance notice: %w", err)
		}

		last4 := strings.TrimLeft(subexp(balancePattern, m, "last4"), "Xx* ")
		return &BalanceNotice{Amount: amount, AsOf: asOf, Last4: last4}, nil
	}
	return nil, fmt.Errorf("%w (subject %q)", ErrNoBalance, subject)
}

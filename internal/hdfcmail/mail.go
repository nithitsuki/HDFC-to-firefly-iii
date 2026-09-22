// Package hdfcmail turns raw HDFC Bank transaction-alert emails into
// structured transactions.
//
// HDFC sends several distinct alert templates (UPI, debit card, credit card)
// from a single address, and the body wording differs per template. Each
// template is described declaratively in parse.go so that new formats can be
// added without touching the surrounding logic.
package hdfcmail

import (
	"fmt"
	"strings"
	"time"

	"hdfc2ff/internal/money"
)

// Kind identifies which HDFC alert template a message came from.
type Kind string

const (
	KindUPIDebit  Kind = "upi_debit"
	KindUPICredit Kind = "upi_credit"
)

// String returns the stable machine-readable name of the kind.
func (k Kind) String() string { return string(k) }

// Transaction is one parsed alert, in bank-agnostic terms.
//
// Amount is always positive and stored in minor units (paise) so that no
// floating-point value ever touches the money path. Direction is implied by
// Kind: a "debit" is money leaving the account, a "credit" is money arriving.
type Transaction struct {
	Kind    Kind
	Amount  int64 // minor units (paise); always positive
	Date    time.Time
	Last4   string // last four digits of the account or card the alert is about
	Payee   string // merchant, VPA, or counterparty
	Ref     string // UPI reference number or RRN, when the email carries one
	Subject string
}

// AmountString renders the amount as a decimal string with two places, which
// is the form the Firefly III API expects.
func (t Transaction) AmountString() string {
	return money.FormatMinor(t.Amount)
}

// Direction reports whether money left the account.
func (t Transaction) IsDebit() bool {
	return t.Kind == KindUPIDebit
}

// ParseAmount converts an Indian-formatted money string into minor units.
// It is a thin wrapper over money.ParseMinor, kept because the amount rules
// are part of this package's contract.
func ParseAmount(s string) (int64, error) {
	return money.ParseMinor(s)
}

// ParseDate tries each layout in turn and returns the first that matches.
// Times are interpreted in the bank's timezone, which the caller supplies.
func ParseDate(s string, loc *time.Location, layouts ...string) (time.Time, error) {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised date %q (tried %d layouts)", s, len(layouts))
}

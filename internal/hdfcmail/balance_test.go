package hdfcmail

import (
	"errors"
	"testing"
	"time"
)

func TestParseBalance(t *testing.T) {
	loc := testLoc(t)

	// The real wording, from testdata/samples/balance-notice.eml. Note the
	// doubled currency prefix and the XX before the account digits.
	const body = "Dear Customer, Available balance in your account ending XX1234 " +
		"is Rs. INR 12,345.67 as on 15-JAN-26. The balance in the account does not " +
		"include the uncleared cheque amount, if any."

	got, err := ParseBalance("View: Account update for your HDFC Bank A/c", body, loc)
	if err != nil {
		t.Fatalf("ParseBalance: %v", err)
	}
	if got.Amount != 1234567 {
		t.Errorf("Amount = %d, want 1234567", got.Amount)
	}
	if got.Last4 != "1234" {
		t.Errorf("Last4 = %q, want %q", got.Last4, "1234")
	}
	want := time.Date(2026, 1, 15, 0, 0, 0, 0, loc)
	if !got.AsOf.Equal(want) {
		t.Errorf("AsOf = %v, want %v", got.AsOf, want)
	}
}

func TestParseBalanceRejectsTransactions(t *testing.T) {
	loc := testLoc(t)
	cases := []struct{ name, subject, body string }{
		{
			name:    "upi debit",
			subject: "❗  You have done a UPI txn. Check details!",
			body:    "Rs.100.00 is debited from your account ending 1234 towards VPA 1234567890@bank (Example Merchant) on 05-01-26.",
		},
		{
			name:    "upi credit",
			subject: "View: Account update for your HDFC Bank A/c",
			body:    "We're writing to inform you that Rs.20.00 has been successfully credited to your HDFC Bank account ending in 1234.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseBalance(tc.subject, tc.body, loc); !errors.Is(err, ErrNoBalance) {
				t.Errorf("expected ErrNoBalance, got %v", err)
			}
		})
	}
}

// TestBalanceNoticeIsNotATransaction is the inverse guard: a balance notice
// must never be importable, or it would create a phantom transaction.
func TestBalanceNoticeIsNotATransaction(t *testing.T) {
	loc := testLoc(t)
	const body = "Available balance in your account ending XX1234 is Rs. INR 12,345.67 as on 15-JAN-26."
	if _, err := Parse("View: Account update for your HDFC Bank A/c", body, time.Now(), loc); !errors.Is(err, ErrNoMatch) {
		t.Errorf("a balance notice must not parse as a transaction, got %v", err)
	}
}

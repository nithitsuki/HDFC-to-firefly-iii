package hdfcmail

import (
	"errors"
	"testing"
	"time"
)

func testLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load Asia/Kolkata: %v", err)
	}
	return loc
}

// TestParseTemplates exercises the template table and the plumbing around it.
//
// These inputs are synthetic and exist to test the engine — amount parsing,
// date layouts, payee selection, reference extraction. They are not evidence
// that a real HDFC email parses; the golden tests over testdata/samples are
// what validate real formats.
func TestParseTemplates(t *testing.T) {
	loc := testLoc(t)
	fallback := time.Date(2026, 1, 7, 20, 0, 0, 0, loc)

	cases := []struct {
		name        string
		subject     string
		body        string
		wantKind    Kind
		wantAmount  int64
		wantLast4   string
		wantPayee   string
		wantRef     string
		wantDate    string // yyyy-mm-dd
		wantIsDebit bool
	}{
		{
			// The verified real-world wording, seen in testdata/samples.
			name:        "upi debit, current wording",
			subject:     "❗  You have done a UPI txn. Check details!",
			body:        "Dear Customer, Greetings from HDFC Bank! Rs.100.00 is debited from your account ending 1234 towards VPA 1234567890@bank (Example Merchant) on 05-01-26. UPI transaction reference no.: 234567890123. If you did not authorize this transaction, please report it immediately.",
			wantKind:    KindUPIDebit,
			wantAmount:  10000,
			wantLast4:   "1234",
			wantPayee:   "Example Merchant",
			wantRef:     "234567890123",
			wantDate:    "2026-01-05",
			wantIsDebit: true,
		},
		{
			// A merchant name containing brackets, as real alerts do.
			name:        "upi debit, nested brackets in merchant name",
			subject:     "❗  You have done a UPI txn. Check details!",
			body:        "Rs.50.00 is debited from your account ending 1234 towards VPA vendor@bank (Example Vendor(T)) on 07-01-26. UPI transaction reference no.: 456789012345.",
			wantKind:    KindUPIDebit,
			wantAmount:  5000,
			wantLast4:   "1234",
			wantPayee:   "Example Vendor(T)",
			wantRef:     "456789012345",
			wantDate:    "2026-01-07",
			wantIsDebit: true,
		},
		{
			// The verified credit wording, seen in testdata/samples. Note there
			// is no date in the body, so the fallback (the email Date header)
			// supplies it.
			name:        "upi credit, current wording",
			subject:     "View: Account update for your HDFC Bank A/c",
			body:        "We're writing to inform you that Rs.20.00 has been successfully credited to your HDFC Bank account ending in 1234. b. Sender: Example Sender (VPA: sender@bank) c. UPI Reference No.: 345678901234",
			wantKind:    KindUPICredit,
			wantAmount:  2000,
			wantLast4:   "1234",
			wantPayee:   "Example Sender",
			wantRef:     "345678901234",
			wantDate:    "2026-01-07", // from the fallback, not the body
			wantIsDebit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.subject, tc.body, fallback, loc)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Amount != tc.wantAmount {
				t.Errorf("Amount = %d, want %d", got.Amount, tc.wantAmount)
			}
			if got.Last4 != tc.wantLast4 {
				t.Errorf("Last4 = %q, want %q", got.Last4, tc.wantLast4)
			}
			if got.Payee != tc.wantPayee {
				t.Errorf("Payee = %q, want %q", got.Payee, tc.wantPayee)
			}
			if got.Ref != tc.wantRef {
				t.Errorf("Ref = %q, want %q", got.Ref, tc.wantRef)
			}
			if d := got.Date.Format("2006-01-02"); d != tc.wantDate {
				t.Errorf("Date = %s, want %s", d, tc.wantDate)
			}
			if got.IsDebit() != tc.wantIsDebit {
				t.Errorf("IsDebit() = %v, want %v", got.IsDebit(), tc.wantIsDebit)
			}
		})
	}
}

// TestCardAlertsAreSkipped pins a deliberate scope decision.
//
// Debit card and credit card alerts are not handled: no real sample of either
// wording has been captured, and a guessed template is how this parser ended up
// matching nothing real in the first place. Such a message must be skipped, not
// mis-imported, until a sample exists to calibrate against.
func TestCardAlertsAreSkipped(t *testing.T) {
	loc := testLoc(t)
	cases := []struct{ name, subject, body string }{
		{
			name:    "debit card",
			subject: "View: Account update for your HDFC Bank A/c",
			body:    "HDFC Bank Debit Card ending 5678 for Rs 1,499.00 at AMAZON PAY INDIA on 20-09-2026 18:42:11.",
		},
		{
			name:    "credit card",
			subject: "Update on your HDFC Bank Credit Card",
			body:    "HDFC Bank Credit Card ending 9012 for Rs 799.00 at NETFLIX on 19-09-2026 09:15:00.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.subject, tc.body, time.Now(), loc)
			if !errors.Is(err, ErrNoMatch) {
				t.Fatalf("expected ErrNoMatch for an uncalibrated card alert, got %v", err)
			}
		})
	}
}

// TestBalanceNoticeIsNotACredit is the counterpart to the credit template.
//
// HDFC's credit alerts and its balance notices share the subject "View:
// Account update for your HDFC Bank A/c", so the template cannot key on the
// subject. If the body expression were loosened even slightly, a balance
// notice would import as a spurious deposit.
func TestBalanceNoticeIsNotACredit(t *testing.T) {
	loc := testLoc(t)
	_, err := Parse(
		"View: Account update for your HDFC Bank A/c",
		"Dear Customer, Available balance in your account ending XX1234 is Rs. INR 19,329.74 as on 15-JAN-26. The balance in the account does not include the uncleared cheque amount, if any.",
		time.Now(), loc,
	)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("a balance notice must not parse as a transaction, got %v", err)
	}
}

// TestParseNonTransactionAlert pins the behaviour that matters most for
// day-to-day noise: HDFC sends plenty of mail from the same address that is
// not a transaction, and those must not be reported as failures.
func TestParseNonTransactionAlert(t *testing.T) {
	loc := testLoc(t)
	_, err := Parse(
		"Important: Your HDFC Bank Credit Card statement is ready",
		"Dear Customer, your credit card statement for the period is now available. Total amount due: Rs 12,345.00.",
		time.Now(), loc,
	)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch for a statement email, got %v", err)
	}
}

func TestParseCollapsesHTMLWhitespace(t *testing.T) {
	loc := testLoc(t)
	// Body text as it arrives after HTML has been flattened: the sentence is
	// split across lines, which prose templates must still match.
	body := "Dear Customer,\n\nRs.100.00 is debited from your\naccount ending 1234\ntowards VPA 1234567890@bank (Example Merchant)\non 05-01-26.\n"

	got, err := Parse("❗  You have done a UPI txn. Check details!", body, time.Now(), loc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Amount != 10000 || got.Last4 != "1234" || got.Payee != "Example Merchant" {
		t.Errorf("unexpected parse: %+v", got)
	}
}

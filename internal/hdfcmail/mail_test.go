package hdfcmail

import (
	"strings"
	"testing"
	"time"
)

func TestParseAmount(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "100.00", want: 10000},
		{in: "1,234.56", want: 123456},
		{in: "Rs.1,234.56", want: 123456},
		{in: "Rs 99", want: 9900},
		{in: "INR 0.99", want: 99},
		{in: "1,00,000.00", want: 10000000}, // Indian lakh grouping
		{in: "12.5", want: 1250},
		{in: "  42.00  ", want: 4200},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseAmount(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseAmount(%q): expected an error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAmount(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAmount(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestAmountString(t *testing.T) {
	cases := []struct {
		minor int64
		want  string
	}{
		{10000, "100.00"},
		{99, "0.99"},
		{123456, "1234.56"},
		{10000000, "100000.00"},
	}
	for _, tc := range cases {
		got := Transaction{Amount: tc.minor}.AmountString()
		if got != tc.want {
			t.Errorf("AmountString(%d) = %q, want %q", tc.minor, got, tc.want)
		}
	}
}

func TestCleanPayee(t *testing.T) {
	cases := []struct{ in, want string }{
		{"merchant@bank (SWIGGY)", "SWIGGY"},
		{"merchant@bank", "merchant@bank"},
		{"  AMAZON PAY INDIA  ", "AMAZON PAY INDIA"},
		// Trailing sentence punctuation is stripped so that "NETFLIX" and
		// "NETFLIX." cannot become two separate Firefly accounts.
		{"VPA 1234@paytm (Zomato Ltd.)", "Zomato Ltd"},
		{"NETFLIX.", "NETFLIX"},
		{"", ""},
		{"(only parens)", "only parens"},
	}
	for _, tc := range cases {
		if got := CleanPayee(tc.in); got != tc.want {
			t.Errorf("CleanPayee(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	const src = `<html><head><style>p{color:red}</style></head><body>
		<p>Dear Customer,</p>
		<p>Rs.100.00 has been debited from account **1234<br>to VPA merchant@bank on 07-01-26.</p>
		<div><span>Thanks</span></div>
	</body></html>`

	got := HTMLToText(src)
	for _, want := range []string{
		"Dear Customer,",
		"Rs.100.00 has been debited from account **1234",
		"to VPA merchant@bank on 07-01-26.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("HTMLToText output missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "color:red") {
		t.Errorf("HTMLToText leaked <style> content:\n%s", got)
	}
}

func TestParseDate(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	got, err := ParseDate("07-01-26", loc, "02-01-06")
	if err != nil {
		t.Fatalf("ParseDate: %v", err)
	}
	want := time.Date(2026, 1, 7, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("ParseDate = %v, want %v", got, want)
	}

	if _, err := ParseDate("not a date", loc, "02-01-06"); err == nil {
		t.Error("ParseDate: expected an error for unparseable input")
	}
}

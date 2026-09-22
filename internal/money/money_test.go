package money

import "testing"

func TestParseMinor(t *testing.T) {
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
		{in: "₹250.75", want: 25075},
		// Firefly III returns long fractions.
		{in: "100.000000000000", want: 10000},
		{in: "309.670000000000", want: 30967},
		{in: "9876.540000000000", want: 987654},
		{in: "-42.50", want: -4250},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "-", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseMinor(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseMinor(%q): expected an error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMinor(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMinor(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFormatMinor(t *testing.T) {
	cases := []struct {
		minor int64
		want  string
	}{
		{10000, "100.00"},
		{99, "0.99"},
		{123456, "1234.56"},
		{10000000, "100000.00"},
		{-4250, "-42.50"},
		{0, "0.00"},
	}
	for _, tc := range cases {
		if got := FormatMinor(tc.minor); got != tc.want {
			t.Errorf("FormatMinor(%d) = %q, want %q", tc.minor, got, tc.want)
		}
	}
}

// TestRoundTrip guards the property that matters: a value parsed and formatted
// again is unchanged.
func TestRoundTrip(t *testing.T) {
	for _, s := range []string{"0.00", "0.01", "100.00", "309.67", "9876.54", "-42.50"} {
		minor, err := ParseMinor(s)
		if err != nil {
			t.Fatalf("ParseMinor(%q): %v", s, err)
		}
		if got := FormatMinor(minor); got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
	}
}

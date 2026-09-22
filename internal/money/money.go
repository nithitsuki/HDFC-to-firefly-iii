// Package money represents currency amounts as integer minor units (paise)
// so that no floating-point value ever touches the money path.
package money

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseMinor converts a decimal money string into minor units.
//
// It accepts the forms that appear in bank emails and in Firefly III's API:
// "100.00", "1,234.56", "Rs.1,234.56", "INR 0.99", "100.000000000000", and a
// leading minus sign. Indian lakh grouping ("1,00,000.00") needs no special
// handling because commas are simply removed.
func ParseMinor(s string) (int64, error) {
	s = strings.TrimSpace(s)
	for _, prefix := range []string{"Rs.", "Rs", "INR", "₹"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
	}
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0, fmt.Errorf("money: empty amount")
	}

	negative := false
	if strings.HasPrefix(s, "-") {
		negative = true
		s = s[1:]
	}
	if s == "" {
		return 0, fmt.Errorf("money: amount is only a sign")
	}

	// Split on the decimal point and handle each side as an integer, so that
	// nothing is ever routed through a float. Firefly returns long fractions
	// like "100.000000000000", which are truncated to two places.
	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if !hasFrac {
		fracPart = "00"
	}
	switch len(fracPart) {
	case 0:
		fracPart = "00"
	case 1:
		fracPart += "0"
	default:
		fracPart = fracPart[:2]
	}

	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: parse %q: %w", s, err)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: parse %q: %w", s, err)
	}

	minor := whole*100 + frac
	if negative {
		minor = -minor
	}
	return minor, nil
}

// FormatMinor renders minor units as a decimal string with two places, which
// is the form the Firefly III API expects.
func FormatMinor(minor int64) string {
	sign := ""
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	return sign + strconv.FormatInt(minor/100, 10) + "." + fmt.Sprintf("%02d", minor%100)
}

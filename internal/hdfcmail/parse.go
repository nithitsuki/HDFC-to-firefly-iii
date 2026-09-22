package hdfcmail

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrNoMatch is returned when a message looks like an HDFC alert but no
// template matched its body. Callers record these as skipped rather than
// failed: HDFC sends many non-transaction alerts from the same address, and
// the fix for a genuine miss is a new template entry, not a retry.
var ErrNoMatch = errors.New("no HDFC alert template matched the message body")

// template describes one HDFC alert wording.
//
// The body expression must use these named capture groups:
//
//	amount  (required) money string, e.g. "1,234.56"
//	last4   (required) last four digits of the account or card
//	date    (optional) transaction date, parsed with dateLayouts
//	payee   (optional) VPA, merchant, or counterparty identifier
//	name    (optional) human-readable name, preferred over payee
//
// When date is absent the email's Date header is used instead. HDFC's credit
// alerts carry no date in the body at all, so this is the normal path for them
// rather than a fallback.
//
// HDFC puts the counterparty and the reference number in their own sentences,
// with punctuation that varies independently of the movement wording, so those
// are matched by the patterns in aux and merged into the result.
type template struct {
	kind        Kind
	subjectHint *regexp.Regexp
	body        *regexp.Regexp
	aux         []*regexp.Regexp
	dateLayouts []string
}

// dateLayouts covers the formats HDFC uses across its templates.
var upiDateLayouts = []string{"02-01-06", "02-01-2006", "02/01/06", "02/01/2006"}

// merchantName matches a parenthesised merchant name, tolerating one level of
// nested brackets. HDFC emits names like "Example Vendor(T)".
const merchantName = `[^()]*(?:\([^()]*\)[^()]*)*`

var (
	// upiRef matches the reference sentence on both debit and credit alerts.
	upiRef = regexp.MustCompile(`(?i)UPI (?:transaction )?reference no\.?:?\s*(?P<ref>\d+)`)

	// creditSender matches "Sender: NAME (VPA: handle@bank)" on credit alerts.
	// The name comes first and the VPA is in brackets — the opposite order to
	// the debit wording, where the VPA leads and the merchant name follows.
	creditSender = regexp.MustCompile(`(?i)Sender:\s*(?P<name>[^(]{0,80}?)\s*\(VPA:\s*(?P<payee>[^)]+)\)`)
)

// templates is the calibration surface for this package.
//
// Every entry here was written against a real message captured in
// testdata/samples. Nothing is guessed: a wording that has not been seen is
// left unhandled, which shows up as a skip rather than as a wrong entry.
var templates = []template{
	// VERIFIED against 12 real alerts, Sep 2026:
	//   Rs.100.00 is debited from your account ending 1234 towards VPA
	//   1234567890@bank (Example Merchant) on 05-01-26.
	//   UPI transaction reference no.: 234567890123.
	{
		kind:        KindUPIDebit,
		subjectHint: regexp.MustCompile(`(?i)UPI txn`),
		body: regexp.MustCompile(
			`Rs\.?\s*(?P<amount>[\d,]+(?:\.\d{1,2})?)\s+is debited from (?:your )?account ending (?P<last4>\d{4})` +
				`(?:\s+(?:to|towards) VPA\s+(?P<payee>\S+?))?` +
				`(?:\s*\((?P<name>` + merchantName + `)\))?` +
				`\s+on\s+(?P<date>\d{2}[-/]\d{2}[-/]\d{2,4})`),
		aux:         []*regexp.Regexp{upiRef},
		dateLayouts: upiDateLayouts,
	},

	// VERIFIED against 3 real alerts, Sep 2026:
	//   We're writing to inform you that Rs.20.00 has been successfully
	//   credited to your HDFC Bank account ending in 1234.
	//   b. Sender: Example Sender (VPA: sender@bank)
	//   c. UPI Reference No.: 345678901234
	//
	// Note there is no date in the body, so the email's Date header is used.
	// Note also that the subject is shared with balance notices, which this
	// body expression deliberately does not match.
	{
		kind:        KindUPICredit,
		subjectHint: regexp.MustCompile(`(?i)account update`),
		body: regexp.MustCompile(
			`Rs\.?\s*(?P<amount>[\d,]+(?:\.\d{1,2})?)\s+has been successfully credited to (?:your )?HDFC Bank account ending in (?P<last4>\d{4})`),
		aux: []*regexp.Regexp{upiRef, creditSender},
	},
}

// Parse turns the subject and body of one HDFC alert into a Transaction.
//
// fallback is used as the transaction time when a template does not capture a
// date, which is the normal case for credit alerts. It should be the email's
// Date header. loc is the timezone the bank writes its timestamps in, normally
// Asia/Kolkata.
func Parse(subject, body string, fallback time.Time, loc *time.Location) (*Transaction, error) {
	flat := strings.Join(strings.Fields(body), " ")

	// Prefer templates whose subject hint matches, but do not require it: HDFC
	// reuses subjects across templates and sometimes changes them outright.
	var candidates []template
	for _, t := range templates {
		if t.subjectHint != nil && t.subjectHint.MatchString(subject) {
			candidates = append(candidates, t)
		}
	}
	candidates = append(candidates, templates...)

	for _, t := range candidates {
		for _, haystack := range []string{flat, body} {
			m := t.body.FindStringSubmatch(haystack)
			if m == nil {
				continue
			}
			tx, err := t.build(m, haystack, fallback, loc, subject)
			if err != nil {
				return nil, err
			}
			return tx, nil
		}
	}
	return nil, fmt.Errorf("%w (subject %q)", ErrNoMatch, subject)
}

// build assembles a Transaction from a successful regex match.
func (t template) build(m []string, haystack string, fallback time.Time, loc *time.Location, subject string) (*Transaction, error) {
	amountRaw := subexp(t.body, m, "amount")
	amount, err := ParseAmount(amountRaw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.kind, err)
	}
	if amount <= 0 {
		return nil, fmt.Errorf("%s: non-positive amount %q", t.kind, amountRaw)
	}

	// A credit alert carries no date in the body, so the email's Date header
	// is the normal source of the transaction time, not a last resort.
	when := fallback
	if raw := subexp(t.body, m, "date"); raw != "" {
		parsed, err := ParseDate(raw, loc, t.dateLayouts...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.kind, err)
		}
		when = parsed
	}

	aux := t.auxGroups(haystack)

	// A human-readable name beats a raw VPA handle. On a debit the merchant
	// name follows the VPA in brackets; on a credit the sender's name comes
	// first and the VPA is in brackets.
	payee := firstNonEmpty(
		aux["name"],
		subexp(t.body, m, "name"),
		CleanPayee(aux["payee"]),
		CleanPayee(subexp(t.body, m, "payee")),
	)

	return &Transaction{
		Kind:    t.kind,
		Amount:  amount,
		Date:    when,
		Last4:   subexp(t.body, m, "last4"),
		Payee:   payee,
		Ref:     aux["ref"],
		Subject: subject,
	}, nil
}

// auxGroups searches the auxiliary patterns and returns the named groups they
// captured, first match winning. None of them are required.
func (t template) auxGroups(haystack string) map[string]string {
	out := map[string]string{}
	for _, re := range t.aux {
		m := re.FindStringSubmatch(haystack)
		if m == nil {
			continue
		}
		for i, name := range re.SubexpNames() {
			if name == "" || i >= len(m) || out[name] != "" {
				continue
			}
			out[name] = strings.TrimSpace(m[i])
		}
	}
	return out
}

// subexp returns the named capture group, or "" when it is absent or did not
// participate in the match.
func subexp(re *regexp.Regexp, m []string, name string) string {
	for i, n := range re.SubexpNames() {
		if n == name && i < len(m) {
			return strings.TrimSpace(m[i])
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// CleanPayee tidies a merchant or VPA string for use as an account name. If a
// parenthesised name is present it is preferred, since "merchant@bank (SWIGGY)"
// should become "SWIGGY" rather than the handle.
func CleanPayee(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if start := strings.Index(s, "("); start >= 0 {
		if end := strings.LastIndex(s, ")"); end > start {
			if inner := strings.TrimSpace(s[start+1 : end]); inner != "" {
				s = inner
			}
		}
	}
	s = strings.Trim(s, " .,*-")
	return strings.Join(strings.Fields(s), " ")
}

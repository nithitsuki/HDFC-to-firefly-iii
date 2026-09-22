// Package hdfcmail_test drives the parser from real .eml files.
//
// This is the calibration loop for new HDFC wordings. Drop a sample into
// testdata/samples/ and run:
//
//	go test ./internal/hdfcmail/ -run TestSamples -v
//
// The test decodes the message exactly as production does and reports what it
// parsed. Once the output is correct, save it as a .json file beside the .eml
// and the test starts asserting on it.
//
// A sample that must never be imported — a balance notice, a statement — gets
// a .json containing {"skip": true} instead, which pins the fact that it is
// deliberately not handled.
package hdfcmail_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

	"hdfc2ff/internal/hdfcmail"
)

// expectation mirrors the fields worth pinning for a sample.
type expectation struct {
	// Skip means the message must not parse as a transaction.
	Skip bool `json:"skip,omitempty"`
	// BalanceMinor, when set, means the sample must parse as a balance notice
	// carrying this amount, and must still not parse as a transaction.
	BalanceMinor *int64 `json:"balance_minor,omitempty"`

	Kind        string `json:"kind,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	Date        string `json:"date,omitempty"`
	Last4       string `json:"last4,omitempty"`
	Payee       string `json:"payee,omitempty"`
	Ref         string `json:"ref,omitempty"`
}

const samplesDir = "../../testdata/samples"

func TestSamples(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load Asia/Kolkata: %v", err)
	}

	paths, err := filepath.Glob(filepath.Join(samplesDir, "*.eml"))
	if err != nil {
		t.Fatalf("glob samples: %v", err)
	}
	if len(paths) == 0 {
		t.Skipf("no .eml samples in %s yet — see the README section \"Calibrating the parser\"", samplesDir)
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			subject, body, date := decodeSample(t, path)
			if date.IsZero() {
				date = time.Now()
			}

			// Mirrors gmailclient.Message.Parse, which passes the email's Date
			// header as the fallback. Credit alerts carry no date in the body,
			// so for them this is the only source of the transaction time.
			tx, err := hdfcmail.Parse(subject, body, date, loc)

			goldenPath := trimExt(path) + ".json"
			want, readErr := os.ReadFile(goldenPath)
			if os.IsNotExist(readErr) {
				// No expectation yet: report the parse so it can be reviewed,
				// then written out as the golden file.
				if err != nil {
					t.Logf("%s parses as: NO MATCH (%v)\nsubject: %s", filepath.Base(path), err, subject)
					return
				}
				got := describe(tx)
				enc, _ := json.MarshalIndent(got, "", "  ")
				t.Logf("parsed %s (no %s yet):\n%s", filepath.Base(path), filepath.Base(goldenPath), enc)
				return
			}
			if readErr != nil {
				t.Fatalf("read %s: %v", goldenPath, readErr)
			}

			var wantExp expectation
			if err := json.Unmarshal(want, &wantExp); err != nil {
				t.Fatalf("parse %s: %v", goldenPath, err)
			}

			if wantExp.BalanceMinor != nil {
				notice, berr := hdfcmail.ParseBalance(subject, body, loc)
				if berr != nil {
					t.Errorf("expected %s to parse as a balance notice: %v", filepath.Base(path), berr)
				} else if notice.Amount != *wantExp.BalanceMinor {
					t.Errorf("balance notice amount = %d, want %d", notice.Amount, *wantExp.BalanceMinor)
				}
				if err == nil {
					t.Errorf("a balance notice must not parse as a transaction, got a %s", tx.Kind)
				}
				return
			}

			if wantExp.Skip {
				if err == nil {
					t.Errorf("expected %s to be skipped, but it parsed as a %s transaction",
						filepath.Base(path), tx.Kind)
				} else if !errors.Is(err, hdfcmail.ErrNoMatch) {
					t.Errorf("expected %s to be skipped with ErrNoMatch, got %v", filepath.Base(path), err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Parse failed for %s\nsubject: %s\nbody:\n%s\nerror: %v",
					path, subject, body, err)
			}

			got := describe(tx)
			if got != wantExp {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				t.Errorf("parse mismatch for %s\ngot:\n%s\nwant:\n%s",
					filepath.Base(path), gotJSON, string(want))
			}
		})
	}
}

func describe(tx *hdfcmail.Transaction) expectation {
	return expectation{
		Kind:        tx.Kind.String(),
		AmountMinor: tx.Amount,
		Date:        tx.Date.Format("2006-01-02"),
		Last4:       tx.Last4,
		Payee:       tx.Payee,
		Ref:         tx.Ref,
	}
}

// decodeSample reads a .eml file the same way gmailclient does, so the test
// exercises the real MIME path rather than a simplified one.
func decodeSample(t *testing.T, path string) (subject, body string, date time.Time) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	entity, err := message.Read(bytes.NewReader(raw))
	if entity == nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if err != nil && !message.IsUnknownCharset(err) {
		t.Fatalf("parse %s: %v", path, err)
	}
	hdr := mail.NewReader(entity).Header
	if s, err := hdr.Subject(); err == nil {
		subject = s
	}
	if d, err := hdr.Date(); err == nil {
		date = d
	}
	return subject, hdfcmail.Body(entity), date
}

func trimExt(p string) string { return p[:len(p)-len(filepath.Ext(p))] }

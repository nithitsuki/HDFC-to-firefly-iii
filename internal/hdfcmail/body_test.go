package hdfcmail

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

// mailSubject decodes the Subject header, including RFC 2047 encoded words.
func mailSubject(e *message.Entity) (string, error) {
	return mail.NewReader(e).Header.Subject()
}

// TestBodyHTMLOnlyMultipart is a regression test for a bug that produced an
// empty body for every real HDFC alert.
//
// HDFC sends multipart/alternative carrying a single text/html part and no
// text/plain part. Body() originally walked the MIME tree twice — once looking
// for text/plain, once for text/html — but MultipartReader returns a reader
// over the entity's body, and reading it consumes that body. The second walk
// therefore saw nothing. Synthetic fixtures with a text/plain part passed,
// which is exactly why the bug survived the first round of tests.
func TestBodyHTMLOnlyMultipart(t *testing.T) {
	const raw = `From: HDFC Bank InstaAlerts <alerts@hdfcbank.bank.in>
Subject: =?UTF-8?q?=E2=9D=97__You_have_done_a_UPI_txn._Check_details!?=
Date: Mon, 05 Jan 2026 13:12:22 +0530
MIME-Version: 1.0
Content-Type: multipart/alternative;
 boundary=BOUNDARY123

--BOUNDARY123
Content-Transfer-Encoding: quoted-printable
Content-Type: text/html; charset=UTF-8

<html><body>
<td class=3D"td">Dear Customer,<br><br>Greetings from HDFC Bank!<br><br>Rs.100.=
00 is debited from your account ending 1234 towards VPA 1234567890@bank (Examp=
le Merchant) on 05-01-26.<br><br>UPI transaction reference no.: 234567=
890123.<br><br>Warm regards,<br>HDFC Bank</td>
</body></html>

--BOUNDARY123--
`

	entity, err := message.Read(strings.NewReader(raw))
	if entity == nil {
		t.Fatalf("message.Read returned no entity: %v", err)
	}

	body := Body(entity)
	if body == "" {
		t.Fatal("Body returned empty for an HTML-only multipart message")
	}
	for _, want := range []string{
		"Rs.100.00 is debited from your account ending 1234",
		"towards VPA 1234567890@bank (Example Merchant) on 05-01-26.",
		"UPI transaction reference no.: 234567890123.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Content-Type") || strings.Contains(body, "<td") {
		t.Errorf("body leaked markup:\n%s", body)
	}
}

// TestParseHTMLOnlyAlert runs the same real-world shape through the parser.
func TestParseHTMLOnlyAlert(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load Asia/Kolkata: %v", err)
	}

	const raw = `From: HDFC Bank InstaAlerts <alerts@hdfcbank.bank.in>
Subject: =?UTF-8?q?=E2=9D=97__You_have_done_a_UPI_txn._Check_details!?=
Date: Mon, 05 Jan 2026 13:12:22 +0530
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary=B

--B
Content-Type: text/html; charset=UTF-8

<html><body><td>Rs.100.00 is debited from your account ending 1234 towards
VPA 1234567890@bank (Example Merchant) on 05-01-26.<br>UPI transaction
reference no.: 234567890123.</td></body></html>

--B--
`

	entity, err := message.Read(strings.NewReader(raw))
	if entity == nil {
		t.Fatalf("message.Read returned no entity: %v", err)
	}
	subject, _ := mailSubject(entity)

	tx, err := Parse(subject, Body(entity), time.Now(), loc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tx.Kind != KindUPIDebit {
		t.Errorf("Kind = %q, want %q", tx.Kind, KindUPIDebit)
	}
	if tx.Amount != 10000 {
		t.Errorf("Amount = %d, want 10000", tx.Amount)
	}
	if tx.Last4 != "1234" {
		t.Errorf("Last4 = %q, want %q", tx.Last4, "1234")
	}
	if tx.Payee != "Example Merchant" {
		t.Errorf("Payee = %q, want %q", tx.Payee, "Example Merchant")
	}
	if tx.Ref != "234567890123" {
		t.Errorf("Ref = %q, want %q", tx.Ref, "234567890123")
	}
	if d := tx.Date.Format("2006-01-02"); d != "2026-01-05" {
		t.Errorf("Date = %s, want 2026-01-05", d)
	}
}

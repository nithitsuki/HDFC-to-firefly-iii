package firefly

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// accountTransactionsResponse is the exact wire shape of
// GET /api/v1/accounts/{id}/transactions, captured from a live instance.
//
// Each group wraps its splits under "attributes". An earlier version of
// FindByExternalID was written against a reshaped view of this endpoint where
// the splits sat directly on the group, which silently produced an empty slice
// and made every duplicate check report "not found". That failure looks exactly
// like "no duplicate exists", so this fixture exists to keep the shape honest.
const accountTransactionsResponse = `{
  "data": [
    {
      "type": "transactions",
      "id": "48",
      "attributes": {
        "created_at": "2026-01-07T20:30:01+05:30",
        "transactions": [
          {
            "transaction_journal_id": "48",
            "type": "withdrawal",
            "date": "2026-01-07T00:00:00+05:30",
            "amount": "30.000000000000",
            "description": "UPI to Example Payee",
            "external_id": "123456789012"
          }
        ]
      },
      "links": {}
    },
    {
      "type": "transactions",
      "id": "1",
      "attributes": {
        "created_at": "2024-09-27T00:00:00+05:30",
        "transactions": [
          {
            "transaction_journal_id": "1",
            "type": "opening balance",
            "date": "2024-09-27T00:00:00+05:30",
            "amount": "9876.540000000000",
            "description": "Initial balance",
            "external_id": null
          }
        ]
      },
      "links": {}
    }
  ]
}`

func TestFindByExternalID(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(accountTransactionsResponse))
	}))
	defer srv.Close()

	c := New(srv.URL, "token")
	date := time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)

	found, err := c.FindByExternalID(context.Background(), "3", date, "123456789012")
	if err != nil {
		t.Fatalf("FindByExternalID: %v", err)
	}
	if !found {
		t.Error("expected the real external_id to be found; the response shape is probably wrong")
	}

	// A transaction with no external_id must not match anything.
	found, err = c.FindByExternalID(context.Background(), "3", date, "does-not-exist")
	if err != nil {
		t.Fatalf("FindByExternalID: %v", err)
	}
	if found {
		t.Error("expected an unknown external_id not to be found")
	}

	// An empty identifier is never a match, and must not even hit the network.
	found, err = c.FindByExternalID(context.Background(), "3", date, "")
	if err != nil || found {
		t.Errorf("empty external_id: got found=%v err=%v, want false, nil", found, err)
	}

	if !strings.Contains(gotPath, "/accounts/3/transactions") {
		t.Errorf("path = %q, want it to contain /accounts/3/transactions", gotPath)
	}
	// The date window is widened by a day either side so a timezone
	// difference cannot hide an existing transaction.
	for _, want := range []string{"start=2026-01-06", "end=2026-01-08"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query = %q, want it to contain %q", gotQuery, want)
		}
	}
}

func TestCreateTreatsDuplicateAsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"Duplicate transaction","errors":{"transactions":["Duplicate"]}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "token").Create(context.Background(), Transaction{
		Type: "withdrawal", Date: "2026-01-07T00:00:00+05:30", Amount: "30.00", Description: "test",
	})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("got %v, want ErrDuplicate", err)
	}
}

func TestCreateSurfacesOtherErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"The given data was invalid.","errors":{"amount":["required"]}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "token").Create(context.Background(), Transaction{
		Type: "withdrawal", Date: "2026-01-07T00:00:00+05:30", Amount: "30.00", Description: "test",
	})
	if err == nil {
		t.Fatal("expected an error for a malformed transaction")
	}
	if errors.Is(err, ErrDuplicate) {
		t.Error("a malformed transaction must not be reported as a duplicate")
	}
}

func TestNewAppendsAPIPath(t *testing.T) {
	cases := map[string]string{
		"https://firefly.example.com":         "https://firefly.example.com/api/v1",
		"https://firefly.example.com/":        "https://firefly.example.com/api/v1",
		"https://firefly.example.com/api/v1":  "https://firefly.example.com/api/v1",
		"https://firefly.example.com/api/v1/": "https://firefly.example.com/api/v1",
	}
	for in, want := range cases {
		if got := New(in, "t").baseURL; got != want {
			t.Errorf("New(%q).baseURL = %q, want %q", in, got, want)
		}
	}
}

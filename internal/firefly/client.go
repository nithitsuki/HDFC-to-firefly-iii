// Package firefly is a small client for the Firefly III REST API.
//
// Only the endpoints this service needs are implemented: creating a
// transaction, and reading an account to validate configuration at startup.
package firefly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hdfc2ff/internal/money"
)

// ErrDuplicate is returned when Firefly III rejects a transaction because an
// identical one already exists. It is a normal outcome, not a failure: it
// means a message was imported twice.
var ErrDuplicate = errors.New("firefly: transaction already exists")

// Client talks to one Firefly III instance.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

// New returns a client for the given instance. baseURL may be given with or
// without the trailing /api/v1.
func New(baseURL, token string) *Client {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.HasSuffix(base, "/api/v1") {
		base += "/api/v1"
	}
	return &Client{
		baseURL: base,
		token:   token,
		hc:      &http.Client{Timeout: 30 * time.Second},
	}
}

// Transaction is a single Firefly III transaction journal entry.
//
// Firefly resolves accounts by name when an ID is not supplied, and creates
// any that do not exist. That is what lets a new UPI merchant become an
// expense account without a separate provisioning step.
type Transaction struct {
	Type            string   `json:"type"` // withdrawal, deposit, or transfer
	Date            string   `json:"date"` // RFC 3339
	Amount          string   `json:"amount"`
	Description     string   `json:"description"`
	CurrencyCode    string   `json:"currency_code,omitempty"`
	SourceID        string   `json:"source_id,omitempty"`
	SourceName      string   `json:"source_name,omitempty"`
	DestinationID   string   `json:"destination_id,omitempty"`
	DestinationName string   `json:"destination_name,omitempty"`
	ExternalID      string   `json:"external_id,omitempty"`
	Notes           string   `json:"notes,omitempty"`
	Tags            []string `json:"tags,omitempty"`
}

type createRequest struct {
	ErrorIfDuplicateHash bool          `json:"error_if_duplicate_hash"`
	ApplyRules           bool          `json:"apply_rules"`
	FireWebhooks         bool          `json:"fire_webhooks"`
	Transactions         []Transaction `json:"transactions"`
}

type createResponse struct {
	Data struct {
		ID         string `json:"id"`
		Attributes struct {
			GroupTitle string `json:"group_title"`
			SplitTitle string `json:"title"`
		} `json:"attributes"`
	} `json:"data"`
}

// Create posts one transaction and returns the Firefly transaction-group ID.
//
// Duplicate detection is delegated to Firefly via error_if_duplicate_hash,
// which hashes the journal fields; a repeat of the same import therefore
// surfaces as ErrDuplicate rather than creating a second entry.
func (c *Client) Create(ctx context.Context, tx Transaction) (string, error) {
	body, err := json.Marshal(createRequest{
		ErrorIfDuplicateHash: true,
		ApplyRules:           true,
		FireWebhooks:         true,
		Transactions:         []Transaction{tx},
	})
	if err != nil {
		return "", fmt.Errorf("firefly: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/transactions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("firefly: build request: %w", err)
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("firefly: post transaction: %w", err)
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnprocessableEntity && looksLikeDuplicate(payload) {
		return "", ErrDuplicate
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("firefly: post transaction: status %d: %s", resp.StatusCode, summarise(payload))
	}

	var out createResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", fmt.Errorf("firefly: decode response: %w", err)
	}
	return out.Data.ID, nil
}

// Account is the subset of a Firefly III account this service cares about.
type Account struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	AccountRole  string `json:"account_role"`
	CurrencyCode string `json:"currency_code"`
	Active       bool   `json:"active"`
}

// GetAccount fetches one account, used to validate the configured account map
// before any mail is processed.
func (c *Client) GetAccount(ctx context.Context, id string) (*Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/accounts/"+id, nil)
	if err != nil {
		return nil, fmt.Errorf("firefly: build request: %w", err)
	}
	c.authorize(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("firefly: get account %s: %w", id, err)
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("firefly: get account %s: status %d: %s", id, resp.StatusCode, summarise(payload))
	}

	var out struct {
		Data struct {
			ID         string  `json:"id"`
			Attributes Account `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("firefly: decode account: %w", err)
	}
	acct := out.Data.Attributes
	acct.ID = out.Data.ID
	return &acct, nil
}

// accountSplit is one transaction journal as returned by the account
// transactions endpoint.
type accountSplit struct {
	Type            string  `json:"type"`
	Date            string  `json:"date"`
	Amount          string  `json:"amount"`
	Description     string  `json:"description"`
	ExternalID      *string `json:"external_id"`
	SourceID        string  `json:"source_id"`
	DestinationID   string  `json:"destination_id"`
	SourceName      string  `json:"source_name"`
	DestinationName string  `json:"destination_name"`
}

// listAccountTransactions returns every transaction touching an account
// between start and end inclusive, following pagination. A zero start means no
// lower bound.
//
// The response shape matters: each group wraps its splits under "attributes".
// Getting this wrong fails silently, because an unmatched field just leaves the
// slice empty and every lookup then reports "not found" — which looks exactly
// like "no duplicate exists". This is written from the wire format, not from a
// tool that reshapes it.
func (c *Client) listAccountTransactions(ctx context.Context, accountID string, start, end time.Time, page, limit int) ([]accountSplit, error) {
	q := url.Values{}
	q.Set("end", end.Format("2006-01-02"))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("page", strconv.Itoa(page))
	if !start.IsZero() {
		q.Set("start", start.Format("2006-01-02"))
	}
	full := fmt.Sprintf("%s/accounts/%s/transactions?%s", c.baseURL, url.PathEscape(accountID), q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("firefly: build request: %w", err)
	}
	c.authorize(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("firefly: list account transactions: %w", err)
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("firefly: list account transactions: status %d: %s", resp.StatusCode, summarise(payload))
	}

	var out struct {
		Data []struct {
			Attributes struct {
				Transactions []accountSplit `json:"transactions"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("firefly: decode account transactions: %w", err)
	}

	var splits []accountSplit
	for _, group := range out.Data {
		splits = append(splits, group.Attributes.Transactions...)
	}
	return splits, nil
}

// FindByExternalID reports whether a transaction carrying the given external ID
// already exists on the account, near the given date.
//
// This reads the external_id field straight off the account's transactions
// rather than going through the search endpoint. Firefly silently ignores
// unknown search fields, so a search returning nothing is indistinguishable
// from a search that was never understood, and that is not a foundation to
// build duplicate protection on.
func (c *Client) FindByExternalID(ctx context.Context, accountID string, date time.Time, externalID string) (bool, error) {
	if externalID == "" {
		return false, nil
	}

	// Widen by a day either side so a timezone difference cannot hide an
	// existing transaction.
	splits, err := c.listAccountTransactions(ctx, accountID,
		date.AddDate(0, 0, -1), date.AddDate(0, 0, 1), 1, 200)
	if err != nil {
		return false, err
	}
	for _, split := range splits {
		if split.ExternalID != nil && *split.ExternalID == externalID {
			return true, nil
		}
	}
	return false, nil
}

// balanceEpoch is the lower bound used when computing a balance. The Firefly
// API rejects an end without a start, and rejects a start on or before
// 1970-01-02, so this stands in for "from the beginning of the ledger".
var balanceEpoch = time.Date(1971, 1, 1, 0, 0, 0, 0, time.UTC)

// BalanceAt returns the balance the account's transactions imply as of the end
// of the given date, in minor units.
//
// It is computed from the transactions rather than read from the account's
// current_balance, because the point is to compare the ledger against the
// bank's own statement at a past date.
func (c *Client) BalanceAt(ctx context.Context, accountID string, asOf time.Time) (int64, error) {
	const pageSize = 100

	var total int64
	for page := 1; ; page++ {
		splits, err := c.listAccountTransactions(ctx, accountID, balanceEpoch, asOf, page, pageSize)
		if err != nil {
			return 0, err
		}
		if len(splits) == 0 {
			break
		}
		for _, split := range splits {
			minor, err := money.ParseMinor(split.Amount)
			if err != nil {
				return 0, fmt.Errorf("firefly: transaction %q: %w", split.Description, err)
			}
			// A transaction is signed from this account's point of view.
			switch {
			case split.SourceID == accountID:
				total -= minor
			case split.DestinationID == accountID:
				total += minor
			}
		}
		if len(splits) < pageSize {
			break
		}
	}
	return total, nil
}

// Ping checks that the instance is reachable and the token is accepted.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/about", nil)
	if err != nil {
		return fmt.Errorf("firefly: build request: %w", err)
	}
	c.authorize(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("firefly: ping: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("firefly: token rejected (status %d): check FIREFLY_TOKEN", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("firefly: ping: status %d: %s", resp.StatusCode, summarise(payload))
	}
	return nil
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
}

// looksLikeDuplicate reports whether a 422 body is Firefly's duplicate-hash
// rejection rather than a genuinely malformed transaction.
func looksLikeDuplicate(payload []byte) bool {
	return strings.Contains(strings.ToLower(string(payload)), "duplicate")
}

// summarise trims an error body to something readable in a log line.
func summarise(payload []byte) string {
	s := strings.Join(strings.Fields(string(payload)), " ")
	const max = 400
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

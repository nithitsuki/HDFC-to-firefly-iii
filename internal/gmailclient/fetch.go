// Package gmailclient reads HDFC alert messages from a Gmail mailbox over IMAP.
//
// It authenticates with a Gmail app password, which requires 2-Step
// Verification on the account. Messages are fetched with PEEK so that reading
// them does not mark them as seen in the user's mailbox.
package gmailclient

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

	"hdfc2ff/internal/hdfcmail"
)

// DefaultAddr is Gmail's IMAPS endpoint.
const DefaultAddr = "imap.gmail.com:993"

const (
	// dialTimeout bounds establishing the connection.
	dialTimeout = 30 * time.Second
	// DefaultTimeout bounds a whole fetch. The IMAP client exposes no read or
	// write deadline, so without this a stalled server would wedge the service
	// indefinitely.
	DefaultTimeout = 90 * time.Second
)

// Message is one fetched alert, already decoded to text.
type Message struct {
	UID       uint32
	MessageID string
	Subject   string
	Date      time.Time
	Body      string

	// Raw is the original RFC822 message, kept so that the dump command can
	// write real .eml fixtures for parser calibration.
	Raw []byte
}

// Kind parses this message into a transaction.
func (m Message) Parse(loc *time.Location) (*hdfcmail.Transaction, error) {
	return hdfcmail.Parse(m.Subject, m.Body, m.Date, loc)
}

// Fetcher connects to one IMAP mailbox.
type Fetcher struct {
	Addr    string
	User    string
	Pass    string
	Mailbox string
	Timeout time.Duration
}

// New returns a Fetcher with sensible defaults filled in.
func New(addr, user, pass, mailbox string) *Fetcher {
	if addr == "" {
		addr = DefaultAddr
	}
	if mailbox == "" {
		mailbox = "INBOX"
	}
	return &Fetcher{Addr: addr, User: user, Pass: pass, Mailbox: mailbox, Timeout: DefaultTimeout}
}

// Fetch returns alert messages from the given sender, received on or after
// since, with a UID greater than afterUID. Results are ordered by UID so that
// transactions are imported oldest first.
//
// A single connection is opened and closed per call: polling every couple of
// minutes is well within Gmail's IMAP limits, and it avoids holding a socket
// open across long idle periods.
func (f *Fetcher) Fetch(ctx context.Context, since time.Time, from string, afterUID uint32) ([]Message, error) {
	return f.fetch(ctx, func(c *imapclient.Client) (imap.UIDSet, error) {
		criteria := &imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: from}},
		}
		if !since.IsZero() {
			criteria.Since = since
		}
		data, err := c.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return nil, fmt.Errorf("imap: search: %w", err)
		}

		var set imap.UIDSet
		for _, uid := range data.AllUIDs() {
			if uint32(uid) > afterUID {
				set.AddNum(uid)
			}
		}
		return set, nil
	})
}

// FetchUIDs returns specific messages by UID, regardless of age or sender.
// This backs the replay and import commands, which must reach messages that
// have since fallen outside the normal lookback window.
func (f *Fetcher) FetchUIDs(ctx context.Context, uids []uint32) ([]Message, error) {
	return f.fetch(ctx, func(*imapclient.Client) (imap.UIDSet, error) {
		var set imap.UIDSet
		for _, uid := range uids {
			set.AddNum(imap.UID(uid))
		}
		return set, nil
	})
}

// fetch runs a connect-select-collect cycle, letting the caller decide which
// UIDs to retrieve.
func (f *Fetcher) fetch(ctx context.Context, selectUIDs func(*imapclient.Client) (imap.UIDSet, error)) ([]Message, error) {
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, err := imapclient.DialTLS(f.Addr, &imapclient.Options{
		Dialer: &net.Dialer{Timeout: dialTimeout},
	})
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", f.Addr, err)
	}

	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { c.Close() }) }
	defer closeConn()

	// Watchdog: closing the connection unblocks whatever the client is waiting
	// on, so an expired context or a cancelled run cannot leave the service
	// hanging on a stalled server.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			closeConn()
		case <-done:
		}
	}()

	msgs, err := f.run(c, selectUIDs)
	if err != nil {
		// Report cancellation as such rather than as a mysterious socket
		// error, since the caller may want to treat it as a clean shutdown.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return msgs, nil
}

// run performs the IMAP conversation on an established connection.
func (f *Fetcher) run(c *imapclient.Client, selectUIDs func(*imapclient.Client) (imap.UIDSet, error)) ([]Message, error) {
	if err := c.Login(f.User, f.Pass).Wait(); err != nil {
		return nil, fmt.Errorf("imap: login as %s: %w (an app password is required when 2-Step Verification is on)", f.User, err)
	}
	// Note the closure: Logout() writes to the wire the moment it is called, so
	// `defer c.Logout().Wait()` would log out immediately rather than on return.
	defer func() { c.Logout().Wait() }()

	if _, err := c.Select(f.Mailbox, nil).Wait(); err != nil {
		return nil, fmt.Errorf("imap: select %s: %w", f.Mailbox, err)
	}

	set, err := selectUIDs(c)
	if err != nil {
		return nil, err
	}
	if len(set) == 0 {
		return nil, nil
	}

	fetched, err := c.Fetch(set, &imap.FetchOptions{
		UID:          true,
		Envelope:     true,
		InternalDate: true,
		BodySection:  []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: fetch: %w", err)
	}

	out := make([]Message, 0, len(fetched))
	for _, buf := range fetched {
		raw := bodyBytes(buf)
		if raw == nil {
			continue
		}
		msg, err := decode(uint32(buf.UID), raw)
		if err != nil {
			// One unreadable message must not abort the whole poll. The caller
			// records it against the UID so it can be inspected later.
			msg = Message{
				UID:     uint32(buf.UID),
				Date:    buf.InternalDate,
				Raw:     raw,
				Subject: fmt.Sprintf("(undecodable: %v)", err),
			}
		}
		out = append(out, msg)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// bodyBytes extracts the full RFC822 message requested by FetchOptions.
func bodyBytes(buf *imapclient.FetchMessageBuffer) []byte {
	for _, section := range buf.BodySection {
		s := section.Section
		if s != nil && s.Specifier == imap.PartSpecifierNone && len(s.Part) == 0 {
			return section.Bytes
		}
	}
	if len(buf.BodySection) > 0 {
		return buf.BodySection[0].Bytes
	}
	return nil
}

// decode parses a raw RFC822 message into the fields this service needs.
func decode(uid uint32, raw []byte) (Message, error) {
	entity, err := message.Read(bytes.NewReader(raw))
	if entity == nil {
		return Message{}, fmt.Errorf("parse message: %w", err)
	}
	if err != nil && !message.IsUnknownCharset(err) {
		return Message{}, fmt.Errorf("parse message: %w", err)
	}

	// mail.NewReader supplies the RFC 2047 decoding that message.Entity does
	// not, so encoded subjects arrive as readable text.
	hdr := mail.NewReader(entity).Header

	msg := Message{UID: uid, Body: hdfcmail.Body(entity), Raw: raw}
	if subject, err := hdr.Subject(); err == nil {
		msg.Subject = subject
	}
	if id, err := hdr.MessageID(); err == nil {
		msg.MessageID = id
	}
	if date, err := hdr.Date(); err == nil {
		msg.Date = date
	}
	return msg, nil
}

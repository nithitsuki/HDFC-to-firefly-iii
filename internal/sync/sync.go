// Package sync polls Gmail, parses HDFC alerts, and records them in Firefly III.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"hdfc2ff/internal/config"
	"hdfc2ff/internal/firefly"
	"hdfc2ff/internal/gmailclient"
	"hdfc2ff/internal/hdfcmail"
	"hdfc2ff/internal/store"
)

// Syncer wires the mail source, the bank parser, and the ledger together.
type Syncer struct {
	cfg   *config.Config
	ff    *firefly.Client
	store *store.Store
	log   *slog.Logger
}

// New returns a Syncer.
func New(cfg *config.Config, ff *firefly.Client, st *store.Store, log *slog.Logger) *Syncer {
	return &Syncer{cfg: cfg, ff: ff, store: st, log: log}
}

// Verify prepares for a run: it releases messages left pending by an interrupted
// run, checks the Firefly connection, and confirms every configured account
// exists. Failing here is cheap; failing midway through an import is not.
func (s *Syncer) Verify(ctx context.Context) error {
	if released, err := s.store.ReleasePending(); err != nil {
		return err
	} else if released > 0 {
		s.log.Warn("released messages left pending by an interrupted run", "count", released,
			"hint", "run `hdfc2ff replay` to retry them")
	}

	if s.cfg.DryRun {
		s.log.Warn("dry run: skipping Firefly connectivity check")
		return nil
	}
	if err := s.ff.Ping(ctx); err != nil {
		return err
	}
	for last4, id := range s.cfg.AccountMap {
		acct, err := s.ff.GetAccount(ctx, id)
		if err != nil {
			return fmt.Errorf("account map %s=%s: %w", last4, id, err)
		}
		s.log.Info("account mapped", "last4", last4, "firefly_id", acct.ID, "name", acct.Name, "type", acct.Type)
	}
	return nil
}

// Run performs one poll-and-import cycle and records it as a healthy poll.
func (s *Syncer) Run(ctx context.Context) error {
	if err := s.run(ctx); err != nil {
		return err
	}
	if err := s.store.MarkPollOK(time.Now()); err != nil {
		// Health bookkeeping must not fail the poll itself.
		s.log.Warn("could not record poll health", "error", err)
	}
	return nil
}

// fetcher returns a client for the configured mailbox.
func (s *Syncer) fetcher() *gmailclient.Fetcher {
	return gmailclient.New("", s.cfg.GmailAddress, s.cfg.GmailPassword, s.cfg.GmailMailbox)
}

// run performs one poll-and-import cycle.
func (s *Syncer) run(ctx context.Context) error {
	afterUID, err := s.lastUID()
	if err != nil {
		return err
	}

	since := time.Now().Add(-s.cfg.Lookback)
	msgs, err := s.fetcher().Fetch(ctx, since, s.cfg.GmailFrom, afterUID)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		s.log.Debug("no new alerts")
		return nil
	}
	s.log.Info("fetched alerts", "count", len(msgs))

	var imported int
	for _, msg := range msgs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		done, err := s.handle(ctx, msg, false)
		if err != nil {
			// A single bad message must not stop the rest of the batch.
			s.log.Error("message failed", "uid", msg.UID, "subject", msg.Subject, "error", err)
		}
		if done {
			imported++
		}
		// The watermark only moves in a real run. A dry run must not consume
		// messages that a real run should still import.
		if !s.cfg.DryRun {
			if err := s.setLastUID(msg.UID + 1); err != nil {
				return err
			}
		}
	}
	s.log.Info("poll complete", "imported", imported, "fetched", len(msgs))
	return nil
}

// ImportByUID fetches and processes specific messages by UID, bypassing both
// the UID watermark and the lookback window. This is for backfilling a message
// that predates the window, or one that was skipped for a reason since fixed.
//
// The duplicate guards still apply: a message already imported is recognised by
// its external_id in Firefly and will not be created twice.
func (s *Syncer) ImportByUID(ctx context.Context, uids []uint32) (int, error) {
	msgs, err := s.fetcher().FetchUIDs(ctx, uids)
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, fmt.Errorf("no messages found for UID(s) %v", uids)
	}

	var imported int
	for _, msg := range msgs {
		done, err := s.handle(ctx, msg, false)
		if err != nil {
			s.log.Error("import failed", "uid", msg.UID, "subject", msg.Subject, "error", err)
			continue
		}
		if done {
			imported++
		}
	}
	return imported, nil
}

// finish records an outcome, unless this is a dry run. A dry run is a preview:
// it must not consume messages, or a real run afterwards would find nothing.
func (s *Syncer) finish(rec store.Record) error {
	if s.cfg.DryRun {
		return nil
	}
	return s.store.Finish(rec)
}

// handle processes one message. It reports whether the message resulted in a
// transaction reaching Firefly III.
//
// force bypasses the claim, which is what a dry-run replay needs: it must
// re-examine messages whose records are still in the store.
func (s *Syncer) handle(ctx context.Context, msg gmailclient.Message, force bool) (bool, error) {
	key := msg.MessageID
	if key == "" {
		// Some messages lack a Message-ID header; the UID is stable per mailbox
		// and is enough to keep the record unique.
		key = fmt.Sprintf("uid:%d", msg.UID)
	}

	rec := store.Record{
		MessageID:   key,
		UID:         msg.UID,
		Subject:     msg.Subject,
		ProcessedAt: time.Now(),
	}

	// Claim the message before doing any work. This is a single atomic insert,
	// so a timer firing while a manual run is in flight cannot double-import.
	if !force && !s.cfg.DryRun {
		claimed, err := s.store.Claim(rec)
		if err != nil {
			return false, err
		}
		if !claimed {
			s.log.Debug("already claimed", "uid", msg.UID, "message_id", key)
			return false, nil
		}
	}

	tx, err := msg.Parse(s.cfg.Timezone)
	if err != nil {
		if errors.Is(err, hdfcmail.ErrNoMatch) {
			// HDFC sends plenty of non-transaction alerts from this address
			// (logins, statements, beneficiary changes). Not matching is an
			// expected outcome, not a failure, so it is recorded as skipped
			// and left out of a plain replay.
			rec.Status = store.StatusSkipped
			rec.Detail = "not a transaction alert: " + err.Error()
			if saveErr := s.finish(rec); saveErr != nil {
				return false, saveErr
			}
			s.log.Debug("skipped: not a transaction alert", "uid", msg.UID, "subject", msg.Subject)
			return false, nil
		}
		rec.Status = store.StatusFailed
		rec.Detail = err.Error()
		if saveErr := s.finish(rec); saveErr != nil {
			return false, saveErr
		}
		return false, err
	}
	rec.Kind = tx.Kind.String()
	rec.AmountMinor = tx.Amount
	rec.OccurredAt = tx.Date

	accountID, ok := s.cfg.AccountMap[tx.Last4]
	if !ok {
		// Not an account we have been told about. Record and move on rather
		// than guessing: posting to the wrong account silently corrupts the
		// ledger, and the fix is a one-line config change.
		rec.Status = store.StatusSkipped
		rec.Detail = fmt.Sprintf("no account mapped for last4 %q", tx.Last4)
		if err := s.finish(rec); err != nil {
			return false, err
		}
		s.log.Warn("skipped: unmapped account", "last4", tx.Last4, "amount", tx.AmountString(), "payee", tx.Payee)
		return false, nil
	}

	entry := s.buildEntry(tx, accountID, msg)

	if s.cfg.DryRun {
		s.log.Info("dry run", "type", entry.Type, "date", entry.Date, "amount", entry.Amount,
			"description", entry.Description, "source", entry.SourceID,
			"destination", entry.DestinationName, "external_id", entry.ExternalID)
		return false, nil
	}

	// Ask Firefly whether this transaction is already there. The local claim
	// covers the normal case; this covers a lost database, a replay after a
	// crash between creating the transaction and recording it, and the fact
	// that Firefly's own content hash changes if the payload format ever does.
	exists, err := s.ff.FindByExternalID(ctx, accountID, tx.Date, entry.ExternalID)
	if err != nil {
		rec.Status = store.StatusFailed
		rec.Detail = "duplicate check failed: " + err.Error()
		if saveErr := s.finish(rec); saveErr != nil {
			return false, saveErr
		}
		return false, err
	}
	if exists {
		rec.Status = store.StatusDuplicate
		rec.Detail = fmt.Sprintf("external_id %s already exists on account %s", entry.ExternalID, accountID)
		if err := s.finish(rec); err != nil {
			return false, err
		}
		s.log.Info("already in Firefly, not importing again",
			"external_id", entry.ExternalID, "amount", entry.Amount, "description", entry.Description)
		return false, nil
	}

	id, err := s.ff.Create(ctx, entry)
	switch {
	case errors.Is(err, firefly.ErrDuplicate):
		rec.Status = store.StatusDuplicate
		rec.Detail = "Firefly III reported an existing identical transaction"
		if err := s.finish(rec); err != nil {
			return false, err
		}
		s.log.Info("duplicate, ignored", "description", entry.Description, "amount", entry.Amount)
		return false, nil

	case err != nil:
		rec.Status = store.StatusFailed
		rec.Detail = err.Error()
		if saveErr := s.finish(rec); saveErr != nil {
			return false, saveErr
		}
		return false, err

	default:
		rec.Status = store.StatusImported
		rec.FireflyID = id
		if err := s.finish(rec); err != nil {
			return false, err
		}
		s.log.Info("imported", "amount", entry.Amount, "description", entry.Description, "firefly_id", id)
		return true, nil
	}
}

// buildEntry converts a parsed alert into a Firefly III transaction.
//
// Withdrawals point at an account ID and name the payee as the destination, so
// Firefly creates an expense account the first time a given merchant appears.
// Deposits are the mirror image.
func (s *Syncer) buildEntry(tx *hdfcmail.Transaction, accountID string, msg gmailclient.Message) firefly.Transaction {
	entry := firefly.Transaction{
		Date:         tx.Date.Format(time.RFC3339),
		Amount:       tx.AmountString(),
		CurrencyCode: s.cfg.Currency,
		Tags:         append(append([]string{}, s.cfg.Tags...), tx.Kind.String()),
		Notes:        buildNotes(tx, msg),
	}

	payee := tx.Payee
	if payee == "" {
		payee = "Unknown"
	}

	if tx.IsDebit() {
		entry.Type = "withdrawal"
		entry.SourceID = accountID
		entry.DestinationName = payee
		entry.Description = "UPI to " + payee
	} else {
		entry.Type = "deposit"
		entry.SourceName = payee
		entry.DestinationID = accountID
		entry.Description = "UPI received from " + payee
	}

	// A UPI reference number is the most stable identity Firefly can display,
	// and the value the duplicate check matches on. Falling back to the
	// Message-ID keeps every entry identifiable.
	entry.ExternalID = tx.Ref
	if entry.ExternalID == "" {
		entry.ExternalID = msg.MessageID
	}
	return entry
}

// buildNotes keeps the provenance with the transaction. When a merchant name
// looks wrong, the original wording is right there in Firefly to compare.
func buildNotes(tx *hdfcmail.Transaction, msg gmailclient.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Imported automatically from an HDFC Bank alert email.\n\n")
	fmt.Fprintf(&b, "Kind:      %s\n", tx.Kind)
	fmt.Fprintf(&b, "Amount:    INR %s\n", tx.AmountString())
	fmt.Fprintf(&b, "Date:      %s\n", tx.Date.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "Account:   ****%s\n", tx.Last4)
	if tx.Payee != "" {
		fmt.Fprintf(&b, "Payee:     %s\n", tx.Payee)
	}
	if tx.Ref != "" {
		fmt.Fprintf(&b, "Reference: %s\n", tx.Ref)
	}
	if msg.MessageID != "" {
		fmt.Fprintf(&b, "Message-ID: %s\n", msg.MessageID)
	}
	fmt.Fprintf(&b, "Subject:   %s\n", msg.Subject)

	if body := strings.TrimSpace(msg.Body); body != "" {
		fmt.Fprintf(&b, "\n--- original email ---\n%s\n", body)
	}
	return b.String()
}

// Replay re-processes messages that did not import, fetching them by UID so
// that age and the lookback window are irrelevant. It returns how many messages
// were retried.
//
// Skipped messages are worth retrying too: they are the ones set aside for an
// unmapped account or an unrecognised wording, both of which are fixed by a
// config or template change rather than by the message itself.
func (s *Syncer) Replay(ctx context.Context, limit int, statuses []store.Status) (int, error) {
	seen := map[string]bool{}
	var pending []store.Record
	for _, status := range statuses {
		recs, err := s.store.ByStatus(status, limit)
		if err != nil {
			return 0, err
		}
		for _, r := range recs {
			if seen[r.MessageID] {
				continue
			}
			seen[r.MessageID] = true
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		s.log.Info("nothing to replay")
		return 0, nil
	}

	uids := make([]uint32, 0, len(pending))
	ids := make([]string, 0, len(pending))
	for _, r := range pending {
		if r.UID == 0 {
			continue
		}
		uids = append(uids, r.UID)
		ids = append(ids, r.MessageID)
	}
	if len(uids) == 0 {
		return 0, fmt.Errorf("no replayable messages: %d record(s) have no UID", len(pending))
	}

	// Clear the records so the normal claim path can take them again. A dry run
	// leaves them alone, and bypasses the claim instead.
	if !s.cfg.DryRun {
		if err := s.store.Delete(ids...); err != nil {
			return 0, err
		}
	}

	msgs, err := s.fetcher().FetchUIDs(ctx, uids)
	if err != nil {
		return 0, err
	}

	for _, msg := range msgs {
		if _, err := s.handle(ctx, msg, s.cfg.DryRun); err != nil {
			s.log.Error("replay failed again", "uid", msg.UID, "subject", msg.Subject, "error", err)
		}
	}
	s.log.Info("replay complete", "retried", len(msgs))
	return len(msgs), nil
}

// lastUID returns the highest UID already imported, so polls can skip messages
// that have already been considered.
func (s *Syncer) lastUID() (uint32, error) {
	v, err := s.store.GetMeta(metaLastUID)
	if err != nil {
		return 0, err
	}
	if v == "" {
		return 0, nil
	}
	var uid uint32
	if _, err := fmt.Sscanf(v, "%d", &uid); err != nil {
		return 0, fmt.Errorf("store: bad %s value %q: %w", metaLastUID, v, err)
	}
	return uid, nil
}

func (s *Syncer) setLastUID(uid uint32) error {
	return s.store.SetMeta(metaLastUID, fmt.Sprint(uid))
}

const metaLastUID = "last_uid"

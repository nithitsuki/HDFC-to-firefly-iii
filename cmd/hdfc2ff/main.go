// Command hdfc2ff imports HDFC Bank transaction-alert emails from Gmail into
// Firefly III.
//
// Usage:
//
//	hdfc2ff run          poll Gmail continuously (default)
//	hdfc2ff once         run a single poll and exit, for cron or a systemd timer
//	hdfc2ff status       show what has been imported, skipped, and failed
//	hdfc2ff dump         show what the parser makes of recent real messages
//	hdfc2ff import       import specific messages by UID, for backfilling
//	hdfc2ff verify       compare the ledger against the bank's balance notices
//	hdfc2ff healthcheck  exit non-zero if polls have stopped succeeding
//	hdfc2ff replay       retry messages that did not import
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed the timezone database so containers need no tzdata

	"hdfc2ff/internal/config"
	"hdfc2ff/internal/firefly"
	"hdfc2ff/internal/gmailclient"
	"hdfc2ff/internal/hdfcmail"
	"hdfc2ff/internal/money"
	"hdfc2ff/internal/store"
	"hdfc2ff/internal/sync"
)

// maxBackoff caps how long a persistently failing service waits between polls.
const maxBackoff = 30 * time.Minute

func main() {
	os.Exit(realMain())
}

func realMain() int {
	cmd, args := splitCommand(os.Args[1:])
	if cmd == "" {
		cmd = "run"
	}

	fs := flag.NewFlagSet("hdfc2ff", flag.ExitOnError)
	fs.Usage = usage
	envFile := fs.String("env", ".env", "path to the env file holding credentials")
	once := fs.Bool("once", false, "run a single poll and exit")
	dryRun := fs.Bool("dry-run", false, "parse and report without writing to Firefly III")
	verbose := fs.Bool("verbose", false, "log at debug level")
	limit := fs.Int("limit", 50, "maximum records to list, dump, or replay")
	save := fs.String("save", "", "with dump: directory to write real .eml fixtures into")
	replayStatus := fs.String("status", "failed", "with replay: which outcomes to retry (failed, skipped, all)")
	uidFlag := fs.String("uid", "", "with import: comma-separated message UIDs to import")
	fromFlag := fs.String("from", "", "with dump: override the sender filter, e.g. -from hdfc")
	lookback := fs.Duration("lookback", 30*24*time.Hour, "with verify: how far back to look for a balance notice")
	maxAge := fs.Duration("max-age", 15*time.Minute, "with healthcheck: how stale the last successful poll may be")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument(s): %s\n\n", strings.Join(fs.Args(), " "))
		usage()
		return 2
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*envFile)
	if err != nil {
		log.Error("configuration", "error", err)
		return 1
	}
	if *dryRun {
		cfg.DryRun = true
	}

	switch cmd {
	case "status":
		return cmdStatus(cfg, log, *limit)
	case "healthcheck":
		return cmdHealthcheck(cfg, *maxAge)
	case "dump":
		return cmdDump(cfg, log, *limit, *save, *fromFlag)
	case "import":
		return cmdImport(cfg, log, *uidFlag)
	case "verify":
		return cmdVerify(cfg, log, *lookback)
	case "replay":
		return cmdReplay(cfg, log, *limit, *replayStatus)
	case "once":
		return cmdRun(cfg, log, true)
	case "run":
		return cmdRun(cfg, log, *once)
	case "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		return 2
	}
}

// nextDelay returns how long to wait before the next poll. A healthy service
// polls at the configured interval; a failing one backs off, so that a Gmail or
// Firefly outage does not turn into a hot retry loop.
func nextDelay(base time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return base
	}
	delay := base
	for i := 0; i < failures; i++ {
		if delay >= maxBackoff {
			return maxBackoff
		}
		delay *= 2
	}
	if delay > maxBackoff {
		return maxBackoff
	}
	return delay
}

// cmdRun polls continuously, or once when once is set.
func cmdRun(cfg *config.Config, log *slog.Logger, once bool) int {
	if err := cfg.Validate(); err != nil {
		log.Error("configuration", "error", err)
		return 1
	}
	st, ff, err := open(cfg)
	if err != nil {
		log.Error("startup", "error", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := sync.New(cfg, ff, st, log)
	if err := s.Verify(ctx); err != nil {
		log.Error("cannot reach Firefly III", "error", err)
		return 1
	}

	failures := 0
	poll := func() {
		// A panic in one poll must not take the service down with it.
		defer func() {
			if r := recover(); r != nil {
				failures++
				log.Error("poll panicked; service continues",
					"panic", r, "consecutive_failures", failures, "stack", string(debug.Stack()))
			}
		}()

		err := s.Run(ctx)
		switch {
		case err == nil:
			if failures > 0 {
				log.Info("recovered", "after_consecutive_failures", failures)
			}
			failures = 0

		case ctx.Err() != nil:
			// Shutting down: a cancelled context is not a failure.
			log.Debug("poll interrupted by shutdown", "error", err)

		default:
			failures++
			log.Error("poll failed", "error", err, "consecutive_failures", failures,
				"retry_in", nextDelay(cfg.PollInterval, failures).String())
		}
	}

	poll()
	if once {
		return 0
	}

	log.Info("polling", "interval", cfg.PollInterval.String(), "max_backoff", maxBackoff.String())
	for {
		timer := time.NewTimer(nextDelay(cfg.PollInterval, failures))
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Info("shutting down")
			return 0
		case <-timer.C:
		}
		poll()
	}
}

// cmdHealthcheck reports whether polls are still succeeding. It needs no
// credentials, only the local database, so it works when Gmail or Firefly are
// unreachable — which is exactly when it matters.
func cmdHealthcheck(cfg *config.Config, maxAge time.Duration) int {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unhealthy: open store: %v\n", err)
		return 1
	}
	defer st.Close()

	last, err := st.LastPollOK()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unhealthy: read store: %v\n", err)
		return 1
	}
	if last.IsZero() {
		fmt.Println("unhealthy: no successful poll recorded yet")
		return 1
	}

	age := time.Since(last)
	if age > maxAge {
		fmt.Printf("unhealthy: last successful poll was %s ago, limit %s\n", age.Round(time.Second), maxAge)
		return 1
	}
	fmt.Printf("ok: last successful poll %s ago\n", age.Round(time.Second))
	return 0
}

// cmdStatus reports local state. It deliberately needs no credentials beyond
// the database path, so it works even when Gmail or Firefly are unreachable.
func cmdStatus(cfg *config.Config, log *slog.Logger, limit int) int {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("open store", "error", err)
		return 1
	}
	defer st.Close()

	counts, err := st.Counts()
	if err != nil {
		log.Error("read store", "error", err)
		return 1
	}
	if len(counts) == 0 {
		fmt.Println("No messages processed yet.")
		return 0
	}

	fmt.Println("Messages by outcome:")
	for _, status := range []store.Status{
		store.StatusImported, store.StatusDuplicate, store.StatusIgnored,
		store.StatusDryRun, store.StatusSkipped, store.StatusFailed,
		store.StatusPending,
	} {
		if n := counts[string(status)]; n > 0 {
			fmt.Printf("  %-10s %d\n", status, n)
		}
	}

	for _, status := range []store.Status{store.StatusFailed, store.StatusSkipped} {
		recs, err := st.ByStatus(status, limit)
		if err != nil {
			log.Error("read store", "error", err)
			return 1
		}
		if len(recs) == 0 {
			continue
		}
		fmt.Printf("\nMost recent %s:\n", status)
		for _, r := range recs {
			fmt.Printf("  uid=%-6d %-28s %s\n    %s\n", r.UID, r.Kind, truncate(r.Subject, 60), truncate(r.Detail, 160))
		}
	}
	return 0
}

// cmdDump reports what the parser makes of recent real messages, and can save
// them as .eml fixtures for calibration. It never writes to Firefly III.
//
// This is the command to reach for when a new HDFC wording appears: run it,
// look at what the parser produced, then add a template to hdfcmail.
func cmdDump(cfg *config.Config, log *slog.Logger, limit int, saveDir, from string) int {
	if err := cfg.ValidateGmail(); err != nil {
		log.Error("configuration", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sender := cfg.GmailFrom
	if from != "" {
		sender = from
	}

	since := time.Now().Add(-cfg.Lookback)
	msgs, err := gmailclient.New("", cfg.GmailAddress, cfg.GmailPassword, cfg.GmailMailbox).
		Fetch(ctx, since, sender, 0)
	if err != nil {
		log.Error("fetch", "error", err)
		return 1
	}
	if len(msgs) == 0 {
		fmt.Printf("No messages from %s in the last %s.\n", sender, cfg.Lookback)
		fmt.Println("Raise LOOKBACK in .env to search further back.")
		return 0
	}

	// Keep the most recent messages when there are more than the limit.
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}

	fmt.Printf("%d message(s) from %s in the last %s\n", len(msgs), sender, cfg.Lookback)

	for _, msg := range msgs {
		fmt.Println(strings.Repeat("─", 72))
		fmt.Printf("UID %d  %s\n", msg.UID, msg.Date.Format("2006-01-02 15:04:05 MST"))
		fmt.Printf("Subject: %s\n", msg.Subject)

		if notice, err := hdfcmail.ParseBalance(msg.Subject, msg.Body, cfg.Timezone); err == nil {
			fmt.Printf("BALANCE account ending %s = %s as on %s\n",
				notice.Last4, money.FormatMinor(notice.Amount), notice.AsOf.Format("2006-01-02"))
		} else if tx, err := msg.Parse(cfg.Timezone); err == nil {
			fmt.Printf("PARSED  kind=%s amount=%s date=%s last4=%s payee=%q ref=%q\n",
				tx.Kind, tx.AmountString(), tx.Date.Format("2006-01-02"), tx.Last4, tx.Payee, tx.Ref)
		} else if errors.Is(err, hdfcmail.ErrNoMatch) {
			fmt.Println("SKIP    no template matched (recorded as skipped, not failed)")
		} else {
			fmt.Printf("FAILED  %v\n", err)
		}

		if saveDir != "" {
			path, err := writeFixture(saveDir, msg)
			if err != nil {
				log.Error("save fixture", "uid", msg.UID, "error", err)
				return 1
			}
			fmt.Printf("Saved   %s\n", path)
		}
	}
	return 0
}

// cmdImport fetches and imports specific messages by UID. This is for
// backfilling a message that predates the lookback window.
func cmdImport(cfg *config.Config, log *slog.Logger, uidFlag string) int {
	if err := cfg.Validate(); err != nil {
		log.Error("configuration", "error", err)
		return 1
	}
	uids, err := parseUIDs(uidFlag)
	if err != nil {
		log.Error("bad -uid", "error", err)
		return 2
	}

	st, ff, err := open(cfg)
	if err != nil {
		log.Error("startup", "error", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := sync.New(cfg, ff, st, log)
	if err := s.Verify(ctx); err != nil {
		log.Error("cannot reach Firefly III", "error", err)
		return 1
	}

	imported, err := s.ImportByUID(ctx, uids)
	if err != nil {
		log.Error("import", "error", err)
		return 1
	}
	log.Info("import complete", "imported", imported, "requested", len(uids))
	if imported == 0 {
		return 0
	}
	return 0
}

// cmdVerify compares the balance implied by the ledger against the bank's own
// balance notices.
//
// This is the only independent check available. Per-transaction duplicate
// detection cannot see drift: a transaction that was never recorded at all is
// invisible to it. A balance notice is the bank stating what the number should
// be, so a mismatch localises the problem to a date range.
//
// It exits non-zero when the figures disagree, so it can be used as a check.
func cmdVerify(cfg *config.Config, log *slog.Logger, lookback time.Duration) int {
	if err := cfg.Validate(); err != nil {
		log.Error("configuration", "error", err)
		return 1
	}

	st, ff, err := open(cfg)
	if err != nil {
		log.Error("startup", "error", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := ff.Ping(ctx); err != nil {
		log.Error("cannot reach Firefly III", "error", err)
		return 1
	}

	msgs, err := gmailclient.New("", cfg.GmailAddress, cfg.GmailPassword, cfg.GmailMailbox).
		Fetch(ctx, time.Now().Add(-lookback), cfg.GmailFrom, 0)
	if err != nil {
		log.Error("fetch", "error", err)
		return 1
	}

	var latest *hdfcmail.BalanceNotice
	for _, msg := range msgs {
		notice, err := hdfcmail.ParseBalance(msg.Subject, msg.Body, cfg.Timezone)
		if err != nil {
			continue
		}
		if latest == nil || notice.AsOf.After(latest.AsOf) {
			latest = notice
		}
	}
	if latest == nil {
		fmt.Printf("No balance notice from %s in the last %s.\n", cfg.GmailFrom, lookback)
		fmt.Println("Raise -lookback to search further back.")
		return 1
	}

	accountID, ok := cfg.AccountMap[latest.Last4]
	if !ok {
		fmt.Printf("The balance notice is for account ending %s, which is not in ACCOUNT_MAP.\n", latest.Last4)
		return 1
	}

	ledger, err := ff.BalanceAt(ctx, accountID, latest.AsOf)
	if err != nil {
		log.Error("compute ledger balance", "error", err)
		return 1
	}

	diff := ledger - latest.Amount
	fmt.Printf("Account ending %s, as on %s\n", latest.Last4, latest.AsOf.Format("02 Jan 2006"))
	fmt.Printf("  bank says:   %s\n", money.FormatMinor(latest.Amount))
	fmt.Printf("  ledger says: %s\n", money.FormatMinor(ledger))
	fmt.Printf("  difference:  %s\n", money.FormatMinor(diff))

	if diff != 0 {
		verb := "higher than"
		if diff < 0 {
			verb = "lower than"
		}
		abs := diff
		if abs < 0 {
			abs = -abs
		}
		fmt.Printf("\nThe ledger is %s %s the bank's figure.\n", money.FormatMinor(abs), verb)
		fmt.Println("A difference means a transaction is missing from, or extra in, the ledger.")
		return 1
	}

	fmt.Println("\nThe ledger matches the bank.")
	return 0
}

// cmdReplay retries messages that did not import on an earlier run.
func cmdReplay(cfg *config.Config, log *slog.Logger, limit int, status string) int {
	if err := cfg.Validate(); err != nil {
		log.Error("configuration", "error", err)
		return 1
	}
	statuses, err := parseReplayStatus(status)
	if err != nil {
		log.Error("bad -status", "error", err)
		return 2
	}
	st, ff, err := open(cfg)
	if err != nil {
		log.Error("startup", "error", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := sync.New(cfg, ff, st, log)
	if err := s.Verify(ctx); err != nil {
		log.Error("cannot reach Firefly III", "error", err)
		return 1
	}
	if _, err := s.Replay(ctx, limit, statuses); err != nil {
		log.Error("replay", "error", err)
		return 1
	}
	return 0
}

// parseReplayStatus maps the -status flag to the outcomes to retry.
func parseReplayStatus(s string) ([]store.Status, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "failed":
		return []store.Status{store.StatusFailed}, nil
	case "skipped":
		return []store.Status{store.StatusSkipped}, nil
	case "all":
		return []store.Status{store.StatusFailed, store.StatusSkipped}, nil
	default:
		return nil, fmt.Errorf("unknown status %q, want failed, skipped, or all", s)
	}
}

// parseUIDs reads the -uid flag, which accepts "1259" or "1259,1260".
func parseUIDs(s string) ([]uint32, error) {
	var out []uint32
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%q is not a message UID: %w", part, err)
		}
		out = append(out, uint32(n))
	}
	if len(out) == 0 {
		return nil, errors.New("no UIDs given, e.g. -uid 1259")
	}
	return out, nil
}

// valueFlags lists the flags that consume the following argument, so that a
// command word can be told apart from a flag value.
var valueFlags = map[string]bool{
	"env": true, "limit": true, "save": true, "status": true,
	"uid": true, "lookback": true, "max-age": true, "from": true,
}

// splitCommand finds the command word wherever it appears among the arguments
// and returns it alongside the remaining arguments.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `hdfc2ff -dry-run once` would otherwise leave "once" unparsed, drop it, and
// silently start the long-running daemon instead of doing a single poll.
func splitCommand(args []string) (cmd string, rest []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]

		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}

		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			name := strings.TrimLeft(a, "-")
			if strings.ContainsRune(name, '=') {
				continue // value is attached, nothing to consume
			}
			if valueFlags[name] && i+1 < len(args) {
				rest = append(rest, args[i+1])
				i++
			}
			continue
		}

		if cmd == "" {
			cmd = a
			continue
		}
		rest = append(rest, a)
	}
	return cmd, rest
}

func open(cfg *config.Config) (*store.Store, *firefly.Client, error) {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	return st, firefly.New(cfg.FireflyURL, cfg.FireflyToken), nil
}

// writeFixture saves the raw RFC822 message so the golden test can replay it.
func writeFixture(dir string, msg gmailclient.Message) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%03d-%s.eml", msg.UID, slugify(msg.Subject))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, msg.Raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// slugify reduces a subject to something safe for a file name.
func slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "message"
	}
	if len(out) > 60 {
		out = strings.Trim(out[:60], "-")
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func usage() {
	fmt.Fprint(os.Stderr, `hdfc2ff imports HDFC Bank alert emails from Gmail into Firefly III.

Commands:
  run          poll Gmail continuously (default)
  once         run a single poll and exit, for cron or a systemd timer
  status       show what has been imported, skipped, and failed
  dump         show what the parser makes of recent real messages
  import       import specific messages by UID, for backfilling
  verify       compare the ledger against the bank's balance notices
  healthcheck  exit non-zero if polls have stopped succeeding
  replay       retry messages that did not import

Flags:
  -env string       path to the env file holding credentials (default ".env")
  -once             run a single poll and exit
  -dry-run          parse and report without writing to Firefly III
  -verbose          log at debug level
  -limit int        maximum records to list, dump, or replay (default 50)
  -save string      with dump: directory to write real .eml fixtures into
  -status string    with replay: failed, skipped, or all (default "failed")
  -uid string       with import: comma-separated message UIDs
  -from string      with dump: override the sender filter, for example -from hdfc
  -lookback dur     with verify: how far back to look for a balance notice
  -max-age dur      with healthcheck: staleness limit (default 15m)

Examples:
  hdfc2ff -dry-run -verbose once    parse new alerts, write nothing
  hdfc2ff dump                      show how recent alerts parse
  hdfc2ff dump -save testdata/samples
                                    capture real emails as test fixtures
  hdfc2ff import -uid 1259          backfill one message by UID
  hdfc2ff verify                    check the ledger against the bank
  hdfc2ff once                      one poll, for a cron job
  hdfc2ff status                    summarise local state
  hdfc2ff replay -status all        retry everything that did not import
`)
}

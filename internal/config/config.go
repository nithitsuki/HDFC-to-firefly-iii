// Package config loads and validates this service's settings.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved service configuration.
type Config struct {
	GmailAddress  string
	GmailPassword string // Gmail app password
	GmailMailbox  string
	GmailFrom     string

	FireflyURL   string
	FireflyToken string
	Currency     string

	DBPath       string
	PollInterval time.Duration
	Lookback     time.Duration
	DryRun       bool
	Tags         []string
	Timezone     *time.Location

	// AccountMap maps the last four digits of an HDFC account or card to a
	// Firefly III account ID. Example: {"1234": "3", "5678": "9"}.
	AccountMap map[string]string
}

// Load reads configuration from the process environment, after merging in an
// optional .env file. Real environment variables always win over the file.
func Load(envFile string) (*Config, error) {
	values, err := readEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	get := func(key string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		return values[key]
	}

	cfg := &Config{
		GmailAddress:  get("GMAIL_ADDRESS"),
		GmailPassword: get("GMAIL_APP_PASSWORD"),
		GmailMailbox:  defaultTo(get("GMAIL_MAILBOX"), "INBOX"),
		GmailFrom:     defaultTo(get("GMAIL_FROM"), "alerts@hdfcbank.bank.in"),
		FireflyURL:    get("FIREFLY_URL"),
		FireflyToken:  get("FIREFLY_TOKEN"),
		Currency:      defaultTo(get("FIREFLY_CURRENCY"), "INR"),
		DBPath:        defaultTo(get("DB_PATH"), "hdfc2ff.db"),
		Timezone:      time.Local,
		Tags:          splitList(get("FIREFLY_TAGS"), []string{"hdfc-import"}),
	}

	if cfg.PollInterval, err = parseDuration(get("POLL_INTERVAL"), 2*time.Minute); err != nil {
		return nil, fmt.Errorf("POLL_INTERVAL: %w", err)
	}
	if cfg.Lookback, err = parseDuration(get("LOOKBACK"), 72*time.Hour); err != nil {
		return nil, fmt.Errorf("LOOKBACK: %w", err)
	}
	if cfg.DryRun, err = strconv.ParseBool(defaultTo(get("DRY_RUN"), "false")); err != nil {
		return nil, fmt.Errorf("DRY_RUN: %w", err)
	}
	if tz := get("TIMEZONE"); tz != "" && tz != "Local" {
		if cfg.Timezone, err = time.LoadLocation(tz); err != nil {
			return nil, fmt.Errorf("TIMEZONE: %w", err)
		}
	}
	if cfg.AccountMap, err = parseAccountMap(get("ACCOUNT_MAP")); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ValidateGmail reports the settings needed to reach the mailbox. Commands
// that only read mail, such as dump, skip the Firefly requirements.
func (c *Config) ValidateGmail() error {
	var missing []string
	if c.GmailAddress == "" {
		missing = append(missing, "GMAIL_ADDRESS")
	}
	if c.GmailPassword == "" {
		missing = append(missing, "GMAIL_APP_PASSWORD")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required setting(s): %s\nsee .env.example for the full list", strings.Join(missing, ", "))
	}
	return nil
}

// Validate reports the settings that are required to actually run an import.
// Commands that only read local state (status) skip this.
func (c *Config) Validate() error {
	var missing []string
	if c.GmailAddress == "" {
		missing = append(missing, "GMAIL_ADDRESS")
	}
	if c.GmailPassword == "" {
		missing = append(missing, "GMAIL_APP_PASSWORD")
	}
	if c.FireflyURL == "" {
		missing = append(missing, "FIREFLY_URL")
	}
	if !c.DryRun && c.FireflyToken == "" {
		missing = append(missing, "FIREFLY_TOKEN")
	}
	if len(c.AccountMap) == 0 {
		missing = append(missing, "ACCOUNT_MAP")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required setting(s): %s\nsee .env.example for the full list", strings.Join(missing, ", "))
	}
	return nil
}

// TagList returns the tags to attach to created transactions.
func (c *Config) TagList() []string { return c.Tags }

// parseAccountMap reads "1234=3,5678=9" into a map of last-four to account ID.
func parseAccountMap(s string) (map[string]string, error) {
	out := map[string]string{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		last4, id, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("ACCOUNT_MAP: %q is not in last4=accountID form", pair)
		}
		last4, id = strings.TrimSpace(last4), strings.TrimSpace(id)
		if len(last4) != 4 {
			return nil, fmt.Errorf("ACCOUNT_MAP: %q must be the last four digits of the account", last4)
		}
		if id == "" {
			return nil, fmt.Errorf("ACCOUNT_MAP: %q has no account ID", pair)
		}
		out[last4] = id
	}
	return out, nil
}

// readEnvFile parses a minimal KEY=VALUE file. It is deliberately small: a
// missing file is not an error, and quoting is only lightly supported.
func readEnvFile(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key != "" {
			out[key] = value
		}
	}
	return out, sc.Err()
}

func parseDuration(s string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", d)
	}
	return d, nil
}

func defaultTo(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func splitList(v string, fallback []string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

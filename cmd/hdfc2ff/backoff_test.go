package main

import (
	"testing"
	"time"
)

// TestNextDelay pins the backoff behaviour: a healthy service polls at its
// configured interval, and a failing one backs off rather than hot-looping.
func TestNextDelay(t *testing.T) {
	const base = 2 * time.Minute

	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 2 * time.Minute},
		{-1, 2 * time.Minute},
		{1, 4 * time.Minute},
		{2, 8 * time.Minute},
		{3, 16 * time.Minute},
		{4, 30 * time.Minute}, // capped
		{10, 30 * time.Minute},
		{1000, 30 * time.Minute}, // must not overflow into a negative delay
	}
	for _, tc := range cases {
		if got := nextDelay(base, tc.failures); got != tc.want {
			t.Errorf("nextDelay(%s, %d) = %s, want %s", base, tc.failures, got, tc.want)
		}
	}
}

// TestNextDelayNeverNegative guards the property that matters most: a delay
// used to arm a timer must never be zero or negative, or the loop spins.
func TestNextDelayNeverNegative(t *testing.T) {
	for _, base := range []time.Duration{time.Millisecond, time.Second, time.Minute, time.Hour} {
		for failures := 0; failures < 64; failures++ {
			if got := nextDelay(base, failures); got <= 0 {
				t.Fatalf("nextDelay(%s, %d) = %s, must be positive", base, failures, got)
			}
		}
	}
}

func TestParseUIDs(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []uint32
		wantErr bool
	}{
		{name: "single", in: "1259", want: []uint32{1259}},
		{name: "multiple", in: "1259,1260", want: []uint32{1259, 1260}},
		{name: "spaces", in: " 1259 , 1260 ", want: []uint32{1259, 1260}},
		{name: "empty", in: "", wantErr: true},
		{name: "only commas", in: ",,", wantErr: true},
		{name: "not a number", in: "abc", wantErr: true},
		{name: "negative", in: "-1", wantErr: true},
		{name: "too large", in: "4294967296", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUIDs(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseUIDs(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseUIDs(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseUIDs(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseUIDs(%q)[%d] = %d, want %d", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestSplitCommandHandlesValueFlags keeps the command-word detection honest for
// the flags added alongside the new commands.
func TestSplitCommandHandlesValueFlags(t *testing.T) {
	cases := []struct {
		args    []string
		wantCmd string
	}{
		{[]string{"-uid", "1259", "import"}, "import"},
		{[]string{"import", "-uid", "1259"}, "import"},
		{[]string{"-lookback", "720h", "verify"}, "verify"},
		{[]string{"-max-age", "5m", "healthcheck"}, "healthcheck"},
		{[]string{"-status", "all", "replay"}, "replay"},
		{[]string{"-uid=1259", "import"}, "import"},
	}
	for _, tc := range cases {
		if got, _ := splitCommand(tc.args); got != tc.wantCmd {
			t.Errorf("splitCommand(%v) = %q, want %q", tc.args, got, tc.wantCmd)
		}
	}
}

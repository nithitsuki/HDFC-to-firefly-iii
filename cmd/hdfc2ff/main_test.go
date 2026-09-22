package main

import (
	"reflect"
	"testing"
)

// TestSplitCommand guards against a bug where a command word appearing after
// flags was swallowed by flag parsing, silently starting the long-running
// daemon instead of running the requested command once.
func TestSplitCommand(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCmd  string
		wantRest []string
	}{
		{
			name:     "command first",
			args:     []string{"once"},
			wantCmd:  "once",
			wantRest: []string{},
		},
		{
			name:     "flags before command",
			args:     []string{"-dry-run", "-verbose", "once"},
			wantCmd:  "once",
			wantRest: []string{"-dry-run", "-verbose"},
		},
		{
			name:     "value flag before command",
			args:     []string{"-env", "prod.env", "once"},
			wantCmd:  "once",
			wantRest: []string{"-env", "prod.env"},
		},
		{
			name:     "attached value flag",
			args:     []string{"-env=prod.env", "once"},
			wantCmd:  "once",
			wantRest: []string{"-env=prod.env"},
		},
		{
			name:     "command first, value flag after",
			args:     []string{"dump", "-save", "testdata/samples"},
			wantCmd:  "dump",
			wantRest: []string{"-save", "testdata/samples"},
		},
		{
			name:     "limit before command",
			args:     []string{"-limit", "10", "status"},
			wantCmd:  "status",
			wantRest: []string{"-limit", "10"},
		},
		{
			name:     "no command",
			args:     []string{"-verbose"},
			wantCmd:  "",
			wantRest: []string{"-verbose"},
		},
		{
			name:     "no arguments",
			args:     []string{},
			wantCmd:  "",
			wantRest: []string{},
		},
		{
			name:     "run with once flag",
			args:     []string{"run", "-once"},
			wantCmd:  "run",
			wantRest: []string{"-once"},
		},
		{
			name:     "double dash stops command detection",
			args:     []string{"--", "-not-a-flag"},
			wantCmd:  "",
			wantRest: []string{"-not-a-flag"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, rest := splitCommand(tc.args)
			if cmd != tc.wantCmd {
				t.Errorf("cmd = %q, want %q", cmd, tc.wantCmd)
			}
			if len(rest) == 0 && len(tc.wantRest) == 0 {
				return
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

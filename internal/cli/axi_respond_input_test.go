package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestAxiRespondInputRejectsBeforeDaemon(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	blank := writeIntentFile(t, " \n\t")
	for _, tc := range []struct {
		name  string
		args  []string
		input string
		want  string
	}{
		{"instructions conflict", []string{"--instructions=", "--instructions-file", missing}, "", "mutually exclusive"},
		{"finding conflict", []string{"--add-finding=", "--add-finding-file="}, "", "mutually exclusive"},
		{"missing guidance", []string{"--instructions-file", missing}, "", "read --instructions-file"},
		{"missing finding", []string{"--add-finding-file", missing}, "", "read --add-finding-file"},
		{"empty path", []string{"--instructions-file="}, "", "requires a file path"},
		{"blank file", []string{"--instructions-file", blank}, "", "must not be empty"},
		{"empty stdin", []string{"--instructions", "-"}, "", "must not be empty"},
		{"blank stdin finding", []string{"--add-finding", "-"}, " \n", "must not be empty"},
		{"two stdin", []string{"--instructions", "-", "--add-finding", "-"}, "", "only one respond input may read stdin"},
		{"invalid utf8", []string{"--instructions-file", writeIntentFile(t, "\xff")}, "", "must be valid UTF-8"},
		{"nonregular", []string{"--instructions-file", t.TempDir()}, "", "regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newAxiRespondCmd()
			cmd.SetArgs(append([]string{"--action", "fix", "--findings", "R1"}, tc.args...))
			cmd.SetIn(strings.NewReader(tc.input))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.Execute()
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("error %v, output %s; want %q", err, out.String(), tc.want)
			}
		})
	}
}

func TestAxiRespondInputPreservedThroughIPC(t *testing.T) {
	guidance := "  keep `backticks` and 'quotes'\nnext line\n"
	finding := "{\"description\":\"fix `literal` and \\\"quotes\\\"\\nnext\",\"action\":\"auto-fix\"}\n"
	for _, tc := range []struct {
		name     string
		file     bool
		guidance bool
	}{
		{"instructions file", true, true}, {"instructions stdin", false, true},
		{"finding file", true, false}, {"finding stdin", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ipc.RespondParams
			var responded atomic.Bool
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				if err := json.Unmarshal(raw, &got); err != nil {
					return nil, err
				}
				responded.Store(true)
				return &ipc.RespondResult{OK: true}, nil
			}})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
				if responded.Load() {
					return fx.completed(), nil
				}
				return fx.awaiting(), nil
			})
			flag, text := "--add-finding", finding
			args := []string{"--action", "fix", "--findings", "R1", "--wait", "3s"}
			if tc.guidance {
				flag, text = "--instructions", guidance
			}
			if tc.file {
				path := filepath.Join(t.TempDir(), "input.txt")
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, flag+"-file", path)
			} else {
				args = append(args, flag, "-")
			}
			cmd := newAxiRespondCmd()
			cmd.SetArgs(args)
			cmd.SetIn(strings.NewReader(text))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("respond: %v\n%s", err, out.String())
			}
			if tc.guidance {
				if got.Instructions["R1"] != guidance {
					t.Fatalf("guidance = %q, want %q", got.Instructions["R1"], guidance)
				}
			} else if len(got.AddedFindings) != 1 || got.AddedFindings[0].Description != "fix `literal` and \"quotes\"\nnext" {
				t.Fatalf("added findings = %+v", got.AddedFindings)
			}
		})
	}
}

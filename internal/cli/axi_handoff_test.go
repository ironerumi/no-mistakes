package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestRenderDriveResultHandoff(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	run := &ipc.RunInfo{ID: "handed-off", Status: types.RunHandedOff}
	if err := renderDriveResult(cmd, run, false); err != nil {
		t.Fatalf("handoff returned failure: %v", err)
	}
	for _, want := range []string{"outcome: handed-off", "PR remains open", "not merged or cancelled"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
}

func TestAxiHandoff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  types.RunStatus
		refusal error
		success bool
	}{
		{"green", types.RunHandedOff, nil, true},
		{"not-green", types.RunRunning, errors.New("handoff requires an open PR with checks-passed; use axi run to wait for green CI, or axi abort to discard the run"), false},
		{"different-terminal-outcome", types.RunCancelled, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newStopQuiescenceFixture(t, ipc.MethodHandoffRun, func(context.Context, int) (*ipc.RunInfo, error) {
				return &ipc.RunInfo{ID: "run-quiesce", Branch: "feature/abort", Status: tc.status}, nil
			}, tc.refusal)
			out, err := executeCmd("axi", "handoff")
			t.Logf("handoff CLI output:\n%s", out)
			if tc.success {
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"handed_off: true", "run_status: handed_off"} {
					if !strings.Contains(out, want) {
						t.Fatalf("missing %q: %s", want, out)
					}
				}
			} else {
				if err == nil {
					t.Fatal("expected refusal")
				}
				if strings.Contains(out, "handed_off: true") {
					t.Fatal("refusal claimed handoff")
				}
				if !strings.Contains(out, "axi ") {
					t.Fatalf("missing guidance: %s", out)
				}
			}
		})
	}
	if outcomeFor(string(types.RunHandedOff)) != "handed-off" {
		t.Fatal("handoff outcome missing")
	}
}

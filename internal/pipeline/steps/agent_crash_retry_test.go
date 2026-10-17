package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestTestStep_AgentCrashRetryExitThenSuccess(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "crash-then-valid", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("agent exited: exit status 1")
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.AgentCrashRetries = 1
	executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, nil)
	if err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err != nil || calls != 2 {
		t.Fatalf("Test crash did not recover: calls=%d error=%v", calls, err)
	}
}

// Exercise the real parser and executor together, not only a marked mock error.
func TestDocumentStep_AgentCrashRetryMalformedOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retries    int
		alwaysBad  bool
		wantCalls  int
		wantFailed bool
	}{
		{"then valid", 1, false, 2, false},
		{"exhausted", 1, true, 2, true},
		{"disabled", 0, false, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "malformed-output", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls == 1 || tc.alwaysBad {
					return &agent.Result{Output: json.RawMessage(`{not json`)}, nil
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"documentation is current"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Config.AgentCrashRetries = tc.retries
			executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&DocumentStep{}}, nil)
			err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
			if (err != nil) != tc.wantFailed || calls != tc.wantCalls {
				t.Fatalf("malformed output: calls=%d error=%v; want calls=%d failed=%v", calls, err, tc.wantCalls, tc.wantFailed)
			}
			steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil || steps[0].FailureCause != nil {
				t.Fatalf("parser rejection became a manual continuation cause: steps=%+v error=%v", steps, err)
			}
		})
	}
}

func TestTestStep_AgentCrashRetryRefusesMovedEffectiveBase(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "base-moved", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		gitCmd(t, dir, "update-ref", "refs/heads/main", head)
		return nil, errors.New("agent exited: exit status 1")
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.AgentCrashRetries = 1
	executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, nil)
	if err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil || calls != 1 {
		t.Fatalf("moved effective base reused: calls=%d error=%v", calls, err)
	} else if !strings.Contains(err.Error(), "effective validation base changed") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
}

func TestTestStep_AgentCrashRetryKeepsCommandFailureTerminal(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "crashed-evidence-agent", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return nil, errors.New("agent exited: exit status 1")
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{
		Test: "git diff --exit-code --no-index base.txt feature.txt",
	})
	sctx.Config.AgentCrashRetries = 1
	executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, nil)
	if err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil || calls != 1 {
		t.Fatalf("known command failure retried: calls=%d error=%v", calls, err)
	}
}

func TestDocumentStep_AgentCrashRetryPermanentFailure(t *testing.T) {
	for _, message := range []string{"401 unauthorized", "insufficient quota"} {
		t.Run(message, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "permanent-error", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return nil, errors.New(message)
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Config.AgentCrashRetries = 1
			executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&DocumentStep{}}, nil)
			if err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil || calls != 1 {
				t.Fatalf("permanent failure retried: calls=%d error=%v", calls, err)
			}
		})
	}
}

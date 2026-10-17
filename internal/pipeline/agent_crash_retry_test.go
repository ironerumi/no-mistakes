package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_AgentCrashRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retries    int
		failures   int
		wantCalls  int
		wantFailed bool
	}{
		{"crash then success", 1, 1, 2, false},
		{"exhausted", 1, 2, 2, true},
		{"three retry bound", 3, 4, 4, true},
		{"default unchanged", 0, 1, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			database, p, _, repo := setupTest(t)
			work := t.TempDir()
			initGitRepo(t, work)
			head, err := git.HeadSHA(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "crash", head, head)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			ag := &hangingAgent{name: "crashing", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls <= tc.failures {
					return nil, errors.New("agent process exited: exit status 1")
				}
				return &agent.Result{}, nil
			}}
			before := newPassStep(types.StepReview)
			after := newPassStep(types.StepLint)
			crash := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				_, err := sctx.RunAgent(agent.RunOpts{})
				return &StepOutcome{}, err
			}}
			executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: tc.retries}, ag, []Step{before, crash, after}, nil)
			events := collectEvents(executor)
			execErr := executor.Execute(context.Background(), run, repo, work)
			if (execErr != nil) != tc.wantFailed || calls != tc.wantCalls {
				t.Fatalf("Execute = %v, calls = %d; want failed=%v calls=%d", execErr, calls, tc.wantFailed, tc.wantCalls)
			}
			if before.callCount() != 1 || after.callCount() != boolInt(!tc.wantFailed) {
				t.Fatalf("earlier/later calls = %d/%d", before.callCount(), after.callCount())
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil || len(steps) != 3 || steps[0].Status != types.StepStatusCompleted {
				t.Fatalf("completed step lost: %+v, %v", steps, err)
			}
			failedEvents := 0
			for _, event := range events.all() {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusFailed) {
					failedEvents++
				}
			}
			if failedEvents != boolInt(tc.wantFailed) {
				t.Fatalf("intermediate crash emitted a terminal event: %d", failedEvents)
			}
			if tc.wantFailed {
				const wantError = "step test failed: agent process exited: exit status 1"
				if execErr.Error() != wantError || errString(executor, run.ID) != wantError {
					t.Fatalf("failure output changed: %v / %s", execErr, errString(executor, run.ID))
				}
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func errString(executor *Executor, runID string) string {
	run, _ := executor.db.GetRun(runID)
	if run != nil && run.Error != nil {
		return *run.Error
	}
	return ""
}

func TestExecutor_AgentCrashRetryRefusesChangedProof(t *testing.T) {
	for _, change := range []string{"head", "run head", "base", "config", "evidence", "evidence contents", "completed evidence", "dirty", "ignored", "initial dirty"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			database, p, _, repo := setupTest(t)
			work := t.TempDir()
			initGitRepo(t, work)
			head, err := git.HeadSHA(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "proof", head, head)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{AgentCrashRetries: 1}
			before := newPassStep(types.StepReview)
			calls := 0
			crash := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				switch change {
				case "head":
					execGit(t, work, "commit", "--allow-empty", "-m", "agent commit")
				case "run head":
					if err := database.UpdateRunHeadSHA(run.ID, "changed"); err != nil {
						t.Fatal(err)
					}
				case "base":
					sctx.Run.BaseSHA = "changed"
				case "config":
					cfg.TrustedConfigSHA = "changed"
				case "evidence", "evidence contents":
					if err := os.MkdirAll(sctx.EvidenceDir, 0o700); err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, sctx.EvidenceDir, "partial.txt", "partial evidence")
				case "completed evidence":
					steps, err := database.GetStepsByRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.SetStepFindings(steps[0].ID, `{"findings":[]}`); err != nil {
						t.Fatal(err)
					}
				case "dirty":
					writeTestFile(t, work, "README.md", "unfinished")
				case "ignored":
					execGit(t, work, "config", "core.excludesFile", filepath.Join(work, ".git", "local-ignore"))
					writeTestFile(t, filepath.Join(work, ".git"), "local-ignore", "partial.txt\n")
					writeTestFile(t, work, "partial.txt", "unfinished")
				}
				return nil, AgentInvocationFailed(errors.New("agent process crashed"))
			}}
			if change == "initial dirty" {
				writeTestFile(t, work, "README.md", "already dirty")
			}
			executor := NewExecutor(database, p, cfg, nil, []Step{before, crash}, nil)
			if change == "evidence contents" {
				evidence := executor.runEvidenceDir(run.ID)
				if err := os.MkdirAll(evidence, 0o700); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, evidence, "partial.txt", "earlier evidence")
			}
			if err := executor.Execute(context.Background(), run, repo, work); err == nil || calls != 1 {
				t.Fatalf("changed proof retried: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestExecutor_AgentCrashRetryDoesNotRetryVerdicts(t *testing.T) {
	t.Parallel()
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepTest, `{"findings":[{"id":"test-1","severity":"error","description":"live scenario failed","action":"auto-fix"}],"verdict":"no-go"}`)
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 3}, nil, []Step{step}, nil)
	executor.onEvent = func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			if err := executor.Respond(types.StepTest, types.ActionAbort, nil); err != nil {
				t.Error(err)
			}
		}
	}
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil || step.callCount() != 1 {
		t.Fatalf("verdict retried: calls=%d err=%v", step.callCount(), err)
	}
}

func TestCrashRetryPhases(t *testing.T) {
	for _, name := range []types.StepName{types.StepIntent, types.StepRebase, types.StepPush, types.StepPR, types.StepCI, "gate.test.check"} {
		if crashRetryStep(name) {
			t.Fatalf("unsafe phase %s can be replayed", name)
		}
	}
	for _, phase := range []string{"fix", "answer", "fix-answer", "coverage"} {
		t.Run(phase, func(t *testing.T) {
			sctx := &StepContext{
				Fixing:            phase == "fix" || phase == "fix-answer",
				FinalizingAnswers: phase == "answer" || phase == "fix-answer",
			}
			if phase == "coverage" {
				sctx.CoverageContinuation = &db.StepRound{}
			}
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				if phase != "fix" {
					if err := sctx.CheckAgentCrashRetryBase("advanced"); err != nil {
						return nil, err
					}
				}
				return nil, AgentInvocationFailed(errors.New("crash"))
			}}
			executor := &Executor{crashRetryBaseSHA: "original"}
			budget := 3
			_, err := executor.executeWithCrashRetry(step, sctx, &budget)
			if err == nil || calls != 1 || budget != 3 {
				t.Fatalf("%s replayed: calls=%d budget=%d err=%v", phase, calls, budget, err)
			}
			if phase != "fix" && !strings.Contains(err.Error(), "effective validation base changed") {
				t.Fatalf("%s ignored the changed effective base: %v", phase, err)
			}
		})
	}
}

func TestExecutor_ReviewContinuationAfterDefaultBranchAdvanceFailsClosed(t *testing.T) {
	base := "default-before"
	executor := &Executor{crashRetryBaseSHA: base}
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		return nil, sctx.CheckAgentCrashRetryBase("default-after")
	}}
	sctx := &StepContext{FinalizingAnswers: true}
	budget := 1
	_, err := executor.executeWithCrashRetry(step, sctx, &budget)
	if err == nil || !strings.Contains(err.Error(), "effective validation base changed") || calls != 1 || budget != 1 {
		t.Fatalf("review continuation crossed the advanced default branch: calls=%d budget=%d err=%v", calls, budget, err)
	}
}

func seedRecoveredTestGate(t *testing.T, database *db.DB, run *db.Run) *db.Run {
	t.Helper()
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(review.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	testStep, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(testStep.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"test-1","severity":"warning","description":"operator decision","action":"ask-user"}]}`
	if err := database.ParkStepForApproval(run.ID, testStep.ID, types.StepStatusAwaitingApproval, 0, 0, &findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(testStep.ID, 1, "initial", &findings, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepDocument); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestExecutor_AgentCrashRetryDisabledAfterRecovery(t *testing.T) {
	database, p, run, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	run = seedRecoveredTestGate(t, database, run)
	calls := 0
	document := &adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		return nil, AgentInvocationFailed(errors.New("agent crashed"))
	}}
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 1}, nil, []Step{newPassStep(types.StepReview), newPassStep(types.StepTest), document}, nil)
	executor.onEvent = func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			if err := executor.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
				t.Error(err)
			}
		}
	}
	err := executor.Resume(context.Background(), run, repo, work)
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "step document failed: agent crashed") {
		t.Fatalf("recovered crash was not an ordinary single failure: calls=%d err=%v", calls, err)
	}
}

func TestExecutor_RecoveredContinuationDoesNotFalseFailBaseFence(t *testing.T) {
	database, p, run, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	run = seedRecoveredTestGate(t, database, run)
	calls := 0
	document := &adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 1}, nil, []Step{newPassStep(types.StepReview), newPassStep(types.StepTest), document}, nil)
	executor.onEvent = func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			if err := executor.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
				t.Error(err)
			}
		}
	}
	if err := executor.Resume(context.Background(), run, repo, work); err != nil || calls != 1 {
		t.Fatalf("recovered successful continuation failed: calls=%d err=%v", calls, err)
	}
}

func TestAgentCrashRetryPinsTheEffectiveBase(t *testing.T) {
	base := ""
	first := &StepContext{crashRetryBaseSHA: &base}
	second := &StepContext{crashRetryBaseSHA: &base}
	if err := first.CheckAgentCrashRetryBase("original"); err != nil {
		t.Fatal(err)
	}
	if err := second.CheckAgentCrashRetryBase("original"); err != nil {
		t.Fatal(err)
	}
	if err := second.CheckAgentCrashRetryBase("changed"); err == nil {
		t.Fatal("later validation reused completed steps on a different effective base")
	}
	if err := (&StepContext{}).CheckAgentCrashRetryBase("changed"); err != nil {
		t.Fatal("unconfigured base resolution changed")
	}
}

func TestRetryableAgentCrash(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		configured bool
		want       bool
	}{
		{"exit", AgentInvocationFailed(errors.New("exit status 1")), false, true},
		{"unparseable", AgentInvocationFailed(errors.New("invalid JSON")), false, true},
		{"verdict failure", errors.New("tests failed"), false, false},
		{"git error", errors.New("git exited: exit status 1"), false, false},
		{"cancel", AgentInvocationFailed(context.Canceled), false, false},
		{"parent deadline", AgentInvocationFailed(context.DeadlineExceeded), false, false},
		{"auth", AgentInvocationFailed(errors.New("provider authentication failed")), false, false},
		{"login", AgentInvocationFailed(errors.New("login required")), false, false},
		{"api key", AgentInvocationFailed(errors.New("invalid API key")), false, false},
		{"credit", AgentInvocationFailed(errors.New("insufficient credit balance")), false, false},
		{"forbidden", AgentInvocationFailed(errors.New("403 forbidden")), false, false},
		{"quota", AgentInvocationFailed(errors.New("insufficient_quota")), false, false},
		{"claude schema exhaustion", AgentInvocationFailed(errors.New("error_max_structured_output_retries")), false, false},
		{"default hang", AgentInvocationFailed(&agentInvocationError{bound: "silent budget", cause: ErrAgentTimeout, adapter: context.Canceled}), false, true},
		{"quota beyond bounded report", AgentInvocationFailed(&agentInvocationError{message: "timed out; bounded adapter report", bound: "silent budget", cause: ErrAgentTimeout, adapter: errors.New(strings.Repeat("x", 500) + " insufficient_quota")}), false, false},
		{"configured timeout", AgentInvocationFailed(&agentInvocationError{bound: "silent budget", cause: ErrAgentTimeout}), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableAgentCrash(context.Background(), tc.err, tc.configured); got != tc.want {
				t.Fatalf("retry = %v want %v", got, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if retryableAgentCrash(ctx, AgentInvocationFailed(errors.New("crash")), false) {
		t.Fatal("cancelled run retried")
	}
}

func TestExecutor_AgentCrashRetryUsesGeneratedDefaultConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "global.yaml")
	config.EnsureDefaultGlobalConfig(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "agent_crash_retries: 0", "agent_crash_retries: 1", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	cfg.AgentTimeout = 20 * time.Millisecond
	if cfg.AgentCrashRetries != 1 || cfg.AgentTimeoutsConfigured {
		t.Fatalf("generated defaults changed retry eligibility: retries=%d configured=%t", cfg.AgentCrashRetries, cfg.AgentTimeoutsConfigured)
	}

	database, p, _, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	head, err := git.HeadSHA(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "generated-default-hang", head, head)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ag := &hangingAgent{name: "generated-default-hang", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &agent.Result{}, nil
	}}
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, err := sctx.RunAgent(agent.RunOpts{})
		return &StepOutcome{}, err
	}}
	if err := NewExecutor(database, p, cfg, ag, []Step{step}, nil).Execute(context.Background(), run, repo, work); err != nil || calls != 2 {
		t.Fatalf("generated default-budget hang = %v, calls=%d; want one retry and two calls", err, calls)
	}
}

func TestExecutor_AgentCrashRetryHang(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "default budget", true: "operator budget"}[configured], func(t *testing.T) {
			t.Parallel()
			database, p, _, repo := setupTest(t)
			work := t.TempDir()
			initGitRepo(t, work)
			head, err := git.HeadSHA(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "hang", head, head)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			ag := &hangingAgent{name: "hang", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls == 1 {
					<-ctx.Done() // the invocation's own cancellation proves the hang ended
					return nil, ctx.Err()
				}
				return &agent.Result{}, nil
			}}
			step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				_, err := sctx.RunAgent(agent.RunOpts{})
				return &StepOutcome{}, err
			}}
			cfg := &config.Config{AgentCrashRetries: 1, AgentTimeout: time.Second, AgentTimeoutsConfigured: configured}
			executor := NewExecutor(database, p, cfg, ag, []Step{step}, nil)
			err = executor.Execute(context.Background(), run, repo, work)
			if (err != nil) != configured || calls != 2-boolInt(configured) {
				t.Fatalf("calls=%d err=%v configured=%v", calls, err, configured)
			}
		})
	}
}

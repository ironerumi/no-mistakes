package daemon

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ciMonitorStep stands in for the CI step: it records that the monitor was
// entered and finishes the step so the recovered run can complete.
type ciMonitorStep struct {
	entered atomic.Int32
}

func (s *ciMonitorStep) Name() types.StepName { return types.StepCI }
func (s *ciMonitorStep) Execute(_ *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.entered.Add(1)
	return &pipeline.StepOutcome{}, nil
}

// seedRestartedRun builds the database a daemon leaves behind when it stops
// while a run is executing: a running run on a PR whose first step is
// completed and whose second step (named by inFlight) is running. The pushed
// head is recorded and the worktree exists, as for a run that reached CI.
func seedRestartedRun(t *testing.T, p *paths.Paths, d *db.DB, repoID string, inFlight types.StepName) *db.Run {
	t.Helper()
	repo, headSHA := setupTestGitRepo(t, p, d, repoID)
	run, err := d.InsertRun(repo.ID, "feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/42"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: headSHA, TargetKind: "gate", TargetFingerprint: "fp", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), p.WorktreeDir(repo.ID, run.ID), headSHA); err != nil {
		t.Fatal(err)
	}
	done, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStepWithStatus(done.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	active, err := d.InsertStepResult(run.ID, inFlight)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(active.ID); err != nil {
		t.Fatal(err)
	}
	return run
}

func openRestartDB(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()
	// Keep the IPC socket below the macOS Unix-domain path limit.
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return p, d
}

func TestRecoverOnStartup_ResumesCIMonitorOnOpenPR(t *testing.T) {
	p, d := openRestartDB(t)
	run := seedRestartedRun(t, p, d, "resume-ci-monitor", types.StepCI)
	ci := &ciMonitorStep{}

	runTestDaemon(t, p, d, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}, ci}
	}, 15*time.Second)

	completed := waitForRunTerminalState(t, d, run.ID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("run status = %s (error %v), want completed", completed.Status, completed.Error)
	}
	if got := ci.entered.Load(); got != 1 {
		t.Fatalf("CI monitor entered %d times after restart, want 1", got)
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.Status != types.StepStatusCompleted {
			t.Errorf("step %s = %s, want completed", step.StepName, step.Status)
		}
	}
}

func TestRecoverOnStartup_MidTestRunStillCrashes(t *testing.T) {
	p, d := openRestartDB(t)
	// A PR and a pushed head are recorded, but the step in flight is not CI.
	run := seedRestartedRun(t, p, d, "crash-mid-test", types.StepTest)
	ci := &ciMonitorStep{}
	test := &mockPassStep{name: types.StepTest}

	runTestDaemon(t, p, d, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}, test, ci}
	}, 15*time.Second)

	recovered, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed", recovered.Status)
	}
	if recovered.Error == nil || *recovered.Error != "daemon crashed during execution" {
		t.Fatalf("run error = %v, want %q", recovered.Error, "daemon crashed during execution")
	}
	if test.execCnt.Load() != 0 || ci.entered.Load() != 0 {
		t.Fatalf("a mid-test run was re-executed: test=%d ci=%d", test.execCnt.Load(), ci.entered.Load())
	}
}

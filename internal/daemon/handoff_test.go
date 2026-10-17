package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestHandleHandoffRefusesReadinessWithdrawnDuringTransition(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://example.test/o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(database.UpdateRunStatus(run.ID, types.RunRunning))
	must(database.UpdateRunPRURL(run.ID, "https://example.test/o/r/pull/1"))
	must(database.UpdateRunPRState(run.ID, "open"))
	must(database.SetRunCIReady(run.ID, true))
	step, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	must(database.UpdateStepStatus(step.ID, types.StepStatusRunning))

	m := NewRunManager(database, p, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	m.cancels[run.ID] = cancel

	withdrawStarted := make(chan struct{})
	releaseWithdraw := make(chan struct{})
	withdrawDone := make(chan error, 1)
	go func() {
		withdrawDone <- m.transitionCIReadiness(run.ID, false, false, func() error {
			close(withdrawStarted)
			<-releaseWithdraw
			return database.SetRunCIReady(run.ID, false)
		})
	}()
	<-withdrawStarted

	handoffDone := make(chan error, 1)
	go func() { handoffDone <- m.HandleHandoff(run.ID) }()
	select {
	case err := <-handoffDone:
		t.Fatalf("handoff crossed the readiness transition: %v", err)
	default:
	}
	close(releaseWithdraw)
	if err := <-withdrawDone; err != nil {
		t.Fatal(err)
	}
	if err := <-handoffDone; err == nil || !strings.Contains(err.Error(), "checks-passed") {
		t.Fatalf("handoff error = %v, want readiness refusal", err)
	}
	if ctx.Err() != nil {
		t.Fatal("refused handoff stopped the monitor")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CIReadyAt != nil {
		t.Fatal("readiness withdrawal was not persisted")
	}
}

func TestHandleHandoffRequiresGreenOpenPRMonitor(t *testing.T) {
	for _, mode := range []string{"green", "pending", "failed", "gate", "fixing", "no-pr", "closed", "completed", "no-monitor", "abort"} {
		t.Run(mode, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repo, err := database.InsertRepo(t.TempDir(), "https://example.test/o/r", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(database.UpdateRunStatus(run.ID, types.RunRunning))
			must(database.UpdateRunPRURL(run.ID, "https://example.test/o/r/pull/1"))
			must(database.UpdateRunPRState(run.ID, "open"))
			must(database.SetRunCIReady(run.ID, true))
			step, err := database.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			must(database.UpdateStepStatus(step.ID, types.StepStatusRunning))
			switch mode {
			case "pending", "failed":
				must(database.SetRunCIReady(run.ID, false))
			case "gate":
				must(database.UpdateStepStatus(step.ID, types.StepStatusAwaitingApproval))
			case "fixing":
				must(database.UpdateStepStatus(step.ID, types.StepStatusFixing))
			case "no-pr":
				must(database.UpdateRunPRURL(run.ID, ""))
			case "closed":
				must(database.UpdateRunPRState(run.ID, "closed"))
			case "completed":
				must(database.UpdateRunStatus(run.ID, types.RunCompleted))
			}
			m := NewRunManager(database, p, nil)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			if mode != "no-monitor" {
				m.cancels[run.ID] = cancel
			}
			if mode == "abort" {
				must(m.HandleCancel(run.ID))
				if context.Cause(ctx).Error() != types.RunCancelReasonAbortedByUser {
					t.Fatalf("abort cause = %v", context.Cause(ctx))
				}
				return
			}
			err = m.HandleHandoff(run.ID)
			if mode == "green" {
				must(err)
				if !errors.Is(context.Cause(ctx), pipeline.ErrHandoff) {
					t.Fatalf("cause = %v", context.Cause(ctx))
				}
				persisted := false
				must(m.transitionCIReadiness(run.ID, false, false, func() error {
					persisted = true
					return nil
				}))
				if persisted {
					t.Fatal("monitor readiness update crossed an atomic handoff claim")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "axi ") {
					t.Fatalf("want refusal and guidance, got %v", err)
				}
				if ctx.Err() != nil {
					t.Fatal("refused handoff stopped the monitor")
				}
			}
		})
	}
}

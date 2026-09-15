package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The review step's gate decides on an append-only outstanding set rather than
// on one round's output: a selected finding stays outstanding until a later
// round positively records that it re-checked the finding's file and no longer
// reports the defect, or until the operator approves, skips, or aborts the
// gate. See resolveVerifiedFindingsJSON and Executor.executeStep.

const reviewCarryTwoFindings = `{"findings":[` +
	`{"id":"review-1","severity":"error","file":"service.go","line":10,"description":"nil deref on the error path","action":"ask-user"},` +
	`{"id":"review-2","severity":"warning","file":"cache.go","line":42,"description":"unbounded cache growth","action":"ask-user"}],` +
	`"summary":"2 findings"}`

// TestExecutor_ReviewCarryForward_NoOpFixKeepsFindingParked mirrors the journey
// that made the predecessor carry design (PR #704) a work-loss hole, inverted to
// prove the hole is closed: the review reports two ask-user findings, the
// operator selects one, the fixer writes a commit that does not fix it, and the
// rereview reports nothing new without positively covering the finding's file.
// The selected finding must still be outstanding, blocking the gate, with its
// identity and action intact - the run must park again, never pass.
func TestExecutor_ReviewCarryForward_NoOpFixKeepsFindingParked(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      reviewCarryTwoFindings,
					ReviewedPaths: []string{"service.go", "cache.go"},
				}, nil
			}
			// The fixer writes a commit that does not fix the selected defect.
			if err := os.WriteFile(filepath.Join(workDir, "unrelated.txt"), []byte("tidy\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			execGit(t, workDir, "add", "unrelated.txt")
			execGit(t, workDir, "commit", "-m", "tidy unrelated code")
			// The rereview reports nothing new and offers no coverage record for
			// service.go: it did not look there, so nothing about the finding is
			// proven. Silence may never read as resolution.
			return &StepOutcome{FixSummary: "tidy unrelated code"}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}

	// The gate re-parks instead of the run completing: the rereview that
	// reported nothing new did not verify the selected finding.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("selected finding was dropped from the outstanding set before anything verified it")
	}
	parsed, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse outstanding findings: %v", err)
	}
	var selected *types.Finding
	for i := range parsed.Items {
		if parsed.Items[i].ID == "review-1" {
			selected = &parsed.Items[i]
		}
	}
	if selected == nil {
		t.Fatalf("selected finding review-1 is not outstanding after a no-op fix: %s", *steps[0].FindingsJSON)
	}
	if selected.Action != types.ActionAskUser {
		t.Errorf("selected finding action = %q, want %q", selected.Action, types.ActionAskUser)
	}
	if selected.Description != "nil deref on the error path" {
		t.Errorf("selected finding description = %q, want the original", selected.Description)
	}

	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Status == types.RunCompleted {
		t.Fatal("run reached a completed outcome with the selected finding unresolved")
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("expected the run to be parked awaiting the operator")
	}

	// The stats have to derive from the same outstanding set the gate uses:
	// nothing is fixed while the operator is still parked on it.
	stats, err := database.StepFindingStats(steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if stats.FixedFindings != 0 {
		t.Errorf("stats reported %d fixed findings while the operator is still parked", stats.FixedFindings)
	}

	// Approving is the explicit operator action that clears what silence may
	// not: the operator may still ship past the finding deliberately.
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

// TestExecutor_ReviewCarryForward_PositiveCoverageClearsFinding is the other
// half of the same contract: a selected finding does leave the outstanding set
// once the rereview positively records that it covered the finding's file and
// no longer reports the defect. Without this the carry set could only grow and
// a verified fix would park forever.
func TestExecutor_ReviewCarryForward_PositiveCoverageClearsFinding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"review-1","severity":"error","file":"service.go","line":10,"description":"nil deref","action":"ask-user"}],"summary":"1 finding"}`,
					ReviewedPaths: []string{"service.go"},
				}, nil
			}
			return &StepOutcome{ReviewedPaths: []string{"service.go"}}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("step status = %s, want %s", steps[0].Status, types.StepStatusCompleted)
	}
	if steps[0].FindingsJSON != nil {
		t.Fatalf("verified finding still stored as outstanding: %s", *steps[0].FindingsJSON)
	}
	stats, err := database.StepFindingStats(steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if stats.FixedFindings != 1 {
		t.Errorf("stats reported %d fixed findings, want the one positively verified finding", stats.FixedFindings)
	}
}

// TestExecutor_ReviewFixRoundCapBoundsTheLoop proves Part 0's bounded stop: a
// review where every round surfaces one more finding - so the loop can never be
// called stalled - still stops at the fix-round cap and parks on an explicit
// ask-user finding instead of burning rounds without bound (#269 reached
// fifteen).
func TestExecutor_ReviewFixRoundCapBoundsTheLoop(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			id := fmt.Sprintf("review-%d", round)
			file := fmt.Sprintf("file-%d.go", round)
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      fmt.Sprintf(`{"findings":[{"id":%q,"severity":"warning","file":%q,"line":1,"description":"issue %d","action":"ask-user"}],"summary":"1 finding"}`, id, file, round),
				ReviewedPaths: []string{file},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	for i := 0; i < reviewFixRoundLimit; i++ {
		selected := outstandingFindingIDs(t, database, run.ID)
		respondWhenParked(t, exec, types.StepReview, types.ActionFix, selected)
		// Wait for this round's park so the next selection sees the round it
		// belongs to and the loop cannot outrun the executor.
		waitForGateEventCount(t, events, string(types.StepStatusFixReview), i+1)
	}

	// The next round runs, reports one more finding, and then the cap fires: the
	// loop stops and parks on its own ask-user finding instead of continuing.
	waitForOutstandingContains(t, database, run.ID, "review-loop-stop")

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	fixRounds := 0
	for _, r := range rounds {
		if r.IsFixRound() {
			fixRounds++
		}
	}
	if fixRounds > reviewFixRoundLimit {
		t.Fatalf("review ran %d fix rounds, past the %d-fix-round cap", fixRounds, reviewFixRoundLimit)
	}
	if len(rounds) > reviewFixRoundLimit+1 {
		t.Fatalf("review persisted %d rounds, past the %d-fix-round cap", len(rounds), reviewFixRoundLimit)
	}

	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Status == types.RunCompleted {
		t.Fatal("capped review completed instead of parking for a decision")
	}

	// Once stopped, the gate refuses another round: a further fix request
	// re-offers the same gate rather than burning a round.
	gateCount := gateEventCount(events, string(types.StepStatusFixReview))
	respondWhenParked(t, exec, types.StepReview, types.ActionFix, outstandingFindingIDs(t, database, run.ID))
	waitForGateEventCount(t, events, string(types.StepStatusFixReview), gateCount+1)
	afterRefusal, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRefusal) != len(rounds) {
		t.Fatalf("a refused fix round still added rounds: %d -> %d", len(rounds), len(afterRefusal))
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

// TestResolveVerifiedFindingsJSON pins the verify-before-clear rule: only a
// positive coverage record that also stops reporting the defect clears a
// selected finding. Silence, a round that looked elsewhere, a re-reported
// defect, and a finding with no file all leave it outstanding.
func TestResolveVerifiedFindingsJSON(t *testing.T) {
	// An EXACT restatement is what the round's own output must contain for the
	// item to read as still reported. Identity here is still content-derived, so
	// a reworded restatement does not match: it is appended as a new item by
	// mergeOutstandingFindingsJSON (the defect stays tracked, under a new ID).
	// Stable identity is a separate design pass (Parts 2+3 of the scout report).
	reported := `{"findings":[{"id":"review-1","severity":"error","file":"service.go","line":10,"description":"nil deref on the error path","action":"ask-user"}],"summary":"1 finding"}`

	cases := []struct {
		name        string
		thisRound   string
		reviewed    []string
		pending     []string
		wantCleared bool
	}{
		{name: "no coverage record clears nothing", thisRound: "", reviewed: nil, pending: []string{"review-1"}},
		{name: "empty coverage list clears nothing", thisRound: "", reviewed: []string{}, pending: []string{"review-1"}},
		{name: "coverage of another file clears nothing", thisRound: "", reviewed: []string{"cache.go"}, pending: []string{"review-1"}},
		{name: "reported defect stays outstanding", thisRound: reported, reviewed: []string{"service.go"}, pending: []string{"review-1"}},
		{name: "finding covered and no longer reported clears", thisRound: "", reviewed: []string{"service.go"}, pending: []string{"review-1"}, wantCleared: true},
		{name: "an unwatched finding keeps its neighbour pending", thisRound: "", reviewed: []string{"cache.go"}, pending: []string{"review-1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveVerifiedFindingsJSON(reviewCarryTwoFindings, tc.pending, tc.reviewed, tc.thisRound)
			parsed, err := types.ParseFindingsJSON(got)
			if err != nil {
				t.Fatalf("parse result: %v", err)
			}
			cleared := true
			for _, item := range parsed.Items {
				if item.ID == "review-1" {
					cleared = false
				}
			}
			if cleared != tc.wantCleared {
				t.Fatalf("review-1 cleared = %v, want %v (result: %s)", cleared, tc.wantCleared, got)
			}
		})
	}
}

// TestResolveVerifiedFindingsJSON_FilelessFindingIsNeverVerifiedAway pins the
// other half of the same rule for findings the reviewer could not anchor to a
// file: no coverage record can ever match them, so only an operator action
// clears them.
func TestResolveVerifiedFindingsJSON_FilelessFindingIsNeverVerifiedAway(t *testing.T) {
	outstanding := `{"findings":[{"id":"review-loop-stop","severity":"warning","description":"review stopped looping","action":"ask-user"}],"summary":"stopped"}`
	got := resolveVerifiedFindingsJSON(outstanding, []string{"review-loop-stop"}, []string{"service.go", "cache.go"}, "")
	if !strings.Contains(got, "review-loop-stop") {
		t.Fatalf("file-less finding was verified away by an unrelated coverage record: %s", got)
	}
}

// TestReviewLoopStopReason covers Part 0's stop conditions directly.
func TestReviewLoopStopReason(t *testing.T) {
	if got := reviewLoopStopReason(0, 0); got != "" {
		t.Errorf("fresh loop stop reason = %q, want none", got)
	}
	if got := reviewLoopStopReason(reviewFixRoundLimit, 0); got == "" {
		t.Error("round cap did not stop the loop")
	}
	if got := reviewLoopStopReason(0, reviewStalledRoundLimit); got == "" {
		t.Error("stalled rounds did not stop the loop")
	}
	if got := reviewLoopStopReason(reviewFixRoundLimit-1, reviewStalledRoundLimit-1); got != "" {
		t.Errorf("loop stopped early: %q", got)
	}
}

// TestMergeOutstandingFindingsJSON_AppendsAndKeepsSelectionIdentity pins the
// append-only merge: the accumulated set is never reduced, an item keeps its
// ID (the selector `axi respond --findings <id>` uses), and a colliding new ID
// is re-minted rather than silently replacing the outstanding item.
func TestMergeOutstandingFindingsJSON_AppendsAndKeepsSelectionIdentity(t *testing.T) {
	merged := mergeOutstandingFindingsJSON(reviewCarryTwoFindings, `{"findings":[{"id":"review-1","severity":"info","file":"other.go","description":"restated as a new item","action":"ask-user"}],"summary":"1 finding"}`)
	if !strings.Contains(merged, "service.go") || !strings.Contains(merged, "cache.go") {
		t.Fatalf("append-only merge dropped an outstanding finding: %s", merged)
	}
	parsed, err := types.ParseFindingsJSON(merged)
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	seen := map[string]int{}
	for _, item := range parsed.Items {
		seen[item.ID]++
	}
	if seen["review-1"] != 1 {
		t.Fatalf("review-1 appears %d times, want exactly one: %s", seen["review-1"], merged)
	}
	var kept *types.Finding
	for i := range parsed.Items {
		if parsed.Items[i].ID == "review-1" {
			kept = &parsed.Items[i]
		}
	}
	if kept == nil || kept.File != "service.go" {
		t.Fatalf("the outstanding item's identity was replaced by the colliding new one: %s", merged)
	}
}

// --- helpers local to these tests ---

func outstandingFindingIDs(t *testing.T, database *db.DB, runID string) []string {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 || steps[0].FindingsJSON == nil {
		return nil
	}
	findings, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse outstanding findings: %v", err)
	}
	ids := make([]string, 0, len(findings.Items))
	for _, item := range findings.Items {
		if item.ID != "" {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// respondWhenParked retries until the executor is waiting at the gate. Respond
// fails while no gate is open, which makes it the readiness signal.
func respondWhenParked(t *testing.T, exec *Executor, step types.StepName, action types.ApprovalAction, ids []string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := exec.Respond(step, action, ids); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gate never accepted a %s response", action)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForOutstandingContains(t *testing.T, database *db.DB, runID, needle string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(runID)
		if err == nil && len(steps) > 0 && steps[0].FindingsJSON != nil && strings.Contains(*steps[0].FindingsJSON, needle) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("outstanding findings never contained %q", needle)
}

func gateEventCount(events *eventCollector, status string) int {
	count := 0
	for _, e := range events.all() {
		if e.Type == ipc.EventStepCompleted && e.Status != nil && *e.Status == status {
			count++
		}
	}
	return count
}

func waitForGateEventCount(t *testing.T, events *eventCollector, status string, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if gateEventCount(events, status) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("observed %d gate events with status %q, want at least %d", gateEventCount(events, status), status, want)
}

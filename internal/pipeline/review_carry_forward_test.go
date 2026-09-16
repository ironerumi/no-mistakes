package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	// lineShiftedReword is a fix that moved the defect to a different line in
	// the same file and a rereview that restated it under a different
	// description - matching neither the exact file+line key nor the content
	// fingerprint. This must never read as a clean pass: any finding reported
	// in the same file is ambiguous evidence, not positive verification.
	lineShiftedReword := `{"findings":[{"id":"review-9","severity":"info","file":"service.go","line":11,"description":"no remaining issue in this area","action":"no-op"}],"summary":"1 finding"}`

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
		{name: "line-shifted reword in the same file is ambiguous, not resolution", thisRound: lineShiftedReword, reviewed: []string{"service.go"}, pending: []string{"review-1"}},
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
	outstanding := `{"findings":[{"id":"review-1","severity":"warning","description":"finding with no file anchor","action":"ask-user"}],"summary":"1 finding"}`
	got := resolveVerifiedFindingsJSON(outstanding, []string{"review-1"}, []string{"service.go", "cache.go"}, "")
	if !strings.Contains(got, "review-1") {
		t.Fatalf("file-less finding was verified away by an unrelated coverage record: %s", got)
	}
}

func TestRemapFindingIDsJSON_UsesRemintedAutomaticSelection(t *testing.T) {
	merged := mergeOutstandingFindingsJSON(reviewCarryTwoFindings, `{"findings":[{"id":"review-1","severity":"error","file":"other.go","description":"new automatic defect","action":"auto-fix"}],"summary":"1 finding"}`, nil)
	selected := autoFixableFindingsJSON(`{"findings":[{"id":"review-1","severity":"error","file":"other.go","description":"new automatic defect","action":"auto-fix"}],"summary":"1 finding"}`)
	remapped := remapFindingIDsJSON(merged, selected)
	parsed, err := types.ParseFindingsJSON(remapped)
	if err != nil {
		t.Fatalf("parse remapped findings: %v", err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].ID != "review-3" {
		t.Fatalf("automatic selection ID = %q, want reminted review-3: %s", parsed.Items[0].ID, remapped)
	}
}

// TestMergeOutstandingFindingsJSON_AppendsAndKeepsSelectionIdentity pins the
// append-only merge: the accumulated set is never reduced, an item keeps its
// ID (the selector `axi respond --findings <id>` uses), and a colliding new ID
// is re-minted rather than silently replacing the outstanding item.
func TestMergeOutstandingFindingsJSON_UsesCurrentReviewedPathsWhenRoundIsEmpty(t *testing.T) {
	prior := `{"findings":[` +
		`{"id":"review-1","severity":"error","file":"service.go","description":"old issue","action":"ask-user"},` +
		`{"id":"review-2","severity":"warning","file":"cache.go","description":"still outstanding","action":"ask-user"}],` +
		`"reviewed_paths":["old.go"]}`

	merged := mergeOutstandingFindingsJSON(prior, "", []string{"current.go"})
	parsed, err := types.ParseFindingsJSON(merged)
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("merged findings = %d, want 2", len(parsed.Items))
	}
	if len(parsed.ReviewedPaths) != 1 || parsed.ReviewedPaths[0] != "current.go" {
		t.Fatalf("reviewed paths = %v, want [current.go]", parsed.ReviewedPaths)
	}
}

func TestMergeOutstandingFindingsJSON_AppendsAndKeepsSelectionIdentity(t *testing.T) {
	merged := mergeOutstandingFindingsJSON(reviewCarryTwoFindings, `{"findings":[{"id":"review-1","severity":"info","file":"other.go","description":"restated as a new item","action":"ask-user"}],"summary":"1 finding"}`, nil)
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

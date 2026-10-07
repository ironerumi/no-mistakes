package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAuditDocumentPointers(t *testing.T) {
	const rule = "Both halves run the same body, .github/scripts/fleet-deploy-app.sh; do not write a second copy into a workflow."
	cases := []struct {
		name, replacement, target string
		want                      int
	}{
		{"exact containment", "See [deployment](owner.md).", "Preconditions: " + rule, 0},
		{"reworded above threshold", "See [deployment](owner.md).", "Both halves run the same body, .github/scripts/fleet-deploy-app.sh; never write a second copy into a workflow.", 0},
		{"below threshold", "See [deployment](owner.md).", "Both halves run separate bodies in different places.", 1},
		{"short generic ignored", "See [deployment](owner.md).", "unrelated", 0},
		{"missing target", "See [deployment](missing.md).", "", 1},
		{"pointer in another hunk", "Replacement without a pointer.", "", 0},
		{"external link", "See [deployment](https://example.com/owner.md).", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gitCmd(t, dir, "init")
			gitCmd(t, dir, "config", "user.email", "test@example.com")
			gitCmd(t, dir, "config", "user.name", "Test")
			source := filepath.Join(dir, "guide.md")
			original := rule + "\n\n\n\n\n\n\n\n\nA short note.\n"
			if tc.name == "short generic ignored" {
				original = "A short note.\n"
			}
			if err := os.WriteFile(source, []byte(original), 0644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", ".")
			gitCmd(t, dir, "commit", "-m", "base")
			before := gitCmd(t, dir, "rev-parse", "HEAD")
			if tc.target != "" {
				if err := os.WriteFile(filepath.Join(dir, "owner.md"), []byte(tc.target+"\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			next := tc.replacement + "\n"
			if tc.name == "pointer in another hunk" {
				next += "\n\n\n\n\n\n\n\nSee [deployment](owner.md).\n"
			}
			if err := os.WriteFile(source, []byte(next), 0644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", ".")
			gitCmd(t, dir, "commit", "-m", "document")
			after := gitCmd(t, dir, "rev-parse", "HEAD")
			got, err := auditDocumentPointers(context.Background(), dir, before, after)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("findings=%+v, want %d", got, tc.want)
			}
			if len(got) > 0 && (got[0].Action != types.ActionAskUser || got[0].Category != types.FindingCategoryDocumentation || !strings.Contains(got[0].Description, "owner.md") && tc.name != "missing target") {
				t.Fatalf("wrong finding: %+v", got)
			}
		})
	}
}

func TestLocalPointerTarget_RejectsEscapesAndResolvesRootPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs", "adr"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "adr", "owner.md"), []byte("owner"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := localPointerTarget(dir, "docs/guide.md", "docs/adr/owner.md"); got != "docs/adr/owner.md" {
		t.Fatalf("root path: %q", got)
	}
	if got := localPointerTarget(dir, "docs/guide.md", "../../outside.md"); got != "" {
		t.Fatalf("traversal: %q", got)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "docs", "escape")); err == nil {
		if got := localPointerTarget(dir, "docs/guide.md", "escape/missing.md"); got != "" {
			t.Fatalf("symlink escape: %q", got)
		}
	}
}

func TestDocumentPointerIntegrity_KitPickerDeployFixture(t *testing.T) {
	// The document replacement at kit-picker 2b9ab09b points to ADR 0027.
	// That ADR states the release-set and native lockfile decisions, but not
	// either of these two operative deploy-body constraints.
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", head)
	source := filepath.Join(dir, "guide.md")
	lostBody := "Both halves run the same body, .github/scripts/fleet-deploy-app.sh; do not write a second copy into a workflow."
	lostLock := "The Mart wait-lock is the only lock released by the deploy body."
	kept := "The physical-Mart rebuild keeps ADR 0025's S3 wait-lock and Terraform's native S3 lockfile backstops concurrent applies."
	if err := os.WriteFile(source, []byte(lostBody+"\n"+lostLock+"\n"+kept+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "guide.md")
	gitCmd(t, dir, "commit", "-m", "prior docs")
	prior := gitCmd(t, dir, "rev-parse", "HEAD")
	adr := filepath.Join(dir, "docs", "adr", "0027-fleet-deploy-workflow-deterministic-artifacts-and-release-parallelism.md")
	if err := os.MkdirAll(filepath.Dir(adr), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adr, []byte("Decision 6: "+kept+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pointer := "follow ADR 0027 decisions 3/6/7 and its Settled review policies for the release set, rank order, native lockfile, baseline tags, and digest recipe. The fleet workflow owns the `v*` tag; operational mechanics belong in [ADR 0027](docs/adr/0027-fleet-deploy-workflow-deterministic-artifacts-and-release-parallelism.md)."
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"dedupe deployment docs"}`)}, os.WriteFile(source, []byte(pointer+"\n"), 0644)
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, prior, config.Commands{})
	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("document did not park: %+v", outcome)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings=%+v", findings.Items)
	}
	f := findings.Items[0]
	for _, s := range []string{"Both halves", "Mart wait-lock", "docs/adr/0027", "guide.md"} {
		if s == "guide.md" {
			if f.File != s {
				t.Fatalf("file=%q", f.File)
			}
			continue
		}
		if !strings.Contains(f.Description, s) {
			t.Fatalf("description %q lacks %q", f.Description, s)
		}
	}
	if strings.Contains(f.Description, "physical-Mart rebuild") {
		t.Fatalf("legitimate dedupe flagged: %+v", f)
	}
}

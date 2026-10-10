package steps

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxPointerFindings = 20

var (
	pointerLink = regexp.MustCompile(`\]\(([^)]+)\)|\]:\s*(\S+)`)
	pointerText = regexp.MustCompile(`(?i)(?:\bsee\b|\brefer to\b|詳しくは)[^\n]*?([\w./-]+\.[a-zA-Z][\w-]*)`)
	hunkHeader  = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+\d+(?:,\d+)? @@`)
)

type pointerHunk struct {
	line           int
	removed, added []string
}

// auditDocumentPointers inspects only the document invocation's committed diff,
// not the author's base..head diff. Per-path diffs avoid parsing quoted Git
// headers (including renamed and newline-containing paths).
func auditDocumentPointers(ctx context.Context, dir, before, after string) ([]types.Finding, error) {
	if before == after {
		return nil, nil
	}
	nameBytes, err := git.RunRaw(ctx, dir, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", before, after)
	if err != nil {
		return nil, fmt.Errorf("list document edits: %w", err)
	}
	var findings []types.Finding
	omitted := false
	for _, name := range strings.Split(strings.TrimSuffix(string(nameBytes), "\x00"), "\x00") {
		if name == "" || !documentationPath(name) {
			continue
		}
		diff, err := git.Run(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--unified=0", before, after, "--", name)
		if err != nil {
			return nil, fmt.Errorf("diff document %q: %w", name, err)
		}
		hunks, err := parsePointerHunks(diff)
		if err != nil {
			return nil, fmt.Errorf("parse document diff %q: %w", name, err)
		}
		// A deleted line retained elsewhere in the source file is not lost.
		source, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, h := range hunks {
			var targets []string
			for _, added := range h.added {
				for _, ref := range pointerReferences(added) {
					target := localPointerTarget(dir, name, ref)
					if target != "" {
						targets = append(targets, target)
					}
				}
			}
			if len(targets) == 0 {
				continue
			}
			var missing []string
			for _, deleted := range h.removed {
				tokens := meaningfulTokens(deleted)
				if len(tokens) < 8 || strings.Contains(strings.ToLower(string(source)), strings.ToLower(strings.TrimSpace(deleted))) {
					continue
				}
				preserved := false
				for _, target := range targets {
					data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(target)))
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						return nil, fmt.Errorf("read pointer target %q: %w", target, err)
					}
					if pointerPreserved(tokens, string(data)) {
						preserved = true
						break
					}
				}
				if !preserved {
					missing = append(missing, deleted)
				}
			}
			if len(missing) == 0 {
				continue
			}
			if len(findings) == maxPointerFindings {
				omitted = true
				continue
			}
			quote := strings.Join(missing, " | ")
			if utf8.RuneCountInString(quote) > 2000 {
				quote = string([]rune(quote)[:2000]) + "…"
			}
			findings = append(findings, types.Finding{ID: fmt.Sprintf("document-pointer-%d", len(findings)+1), File: name, Line: h.line, Severity: types.FindingSeverityWarning, Action: types.ActionAskUser, Category: types.FindingCategoryDocumentation, Description: fmt.Sprintf("This edit replaced operative text with a pointer %q to %s, but the referenced file does not contain the deleted rule: %q. Preserve the rule in an authoritative document or keep the pointer only if its target states it.", h.added[0], strings.Join(targets, ", "), quote)})
		}
	}
	if omitted {
		findings = append(findings, types.Finding{ID: "document-pointer-overflow", Severity: types.FindingSeverityWarning, Action: types.ActionAskUser, Category: types.FindingCategoryDocumentation, Description: "Additional document pointer replacements may require human inspection (finding limit reached)."})
	}
	return findings, nil
}

func documentationPath(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".md" || ext == ".mdx" || ext == ".txt" || ext == ".rst" || ext == ".adoc"
}

func parsePointerHunks(diff string) ([]pointerHunk, error) {
	var hunks []pointerHunk
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "@@ ") {
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("invalid hunk header %q", line)
			}
			n, _ := strconv.Atoi(m[1])
			hunks = append(hunks, pointerHunk{line: n})
			continue
		}
		if len(hunks) == 0 {
			continue
		}
		h := &hunks[len(hunks)-1]
		if strings.HasPrefix(line, "-") {
			h.removed = append(h.removed, line[1:])
		}
		if strings.HasPrefix(line, "+") {
			h.added = append(h.added, line[1:])
		}
	}
	return hunks, nil
}

func pointerReferences(line string) []string {
	var refs []string
	for _, m := range pointerLink.FindAllStringSubmatch(line, -1) {
		if m[1] != "" {
			refs = append(refs, strings.Fields(m[1])[0])
		} else {
			refs = append(refs, m[2])
		}
	}
	for _, m := range pointerText.FindAllStringSubmatch(line, -1) {
		refs = append(refs, m[1])
	}
	return refs
}

func localPointerTarget(dir, source, ref string) string {
	ref = strings.Trim(ref, "`<>.,;: ")
	ref = strings.SplitN(ref, "#", 2)[0]
	if ref == "" || strings.Contains(ref, ":") || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "\\") || strings.Contains(ref, "?") {
		return ""
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	missing := ""
	for _, candidate := range []string{path.Join(path.Dir(source), ref), path.Clean(ref)} {
		if candidate == ".." || strings.HasPrefix(candidate, "../") {
			continue
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(dir, filepath.FromSlash(candidate)))
		if os.IsNotExist(err) {
			// A dangling symlink is not a missing local document.
			if _, statErr := os.Lstat(filepath.Join(dir, filepath.FromSlash(candidate))); !os.IsNotExist(statErr) || !localMissingParent(dir, root, candidate) {
				return ""
			}
			missing = candidate
			continue
		}
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() {
			return filepath.ToSlash(rel)
		}
	}
	return missing
}

// A missing leaf may still sit under a symlink escaping the repository.
func localMissingParent(dir, root, candidate string) bool {
	for parent := path.Dir(candidate); ; parent = path.Dir(parent) {
		resolved, err := filepath.EvalSymlinks(filepath.Join(dir, filepath.FromSlash(parent)))
		if err == nil {
			rel, err := filepath.Rel(root, resolved)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		}
		if !os.IsNotExist(err) || parent == "." {
			return false
		}
	}
}

func meaningfulTokens(s string) []string {
	s = strings.ToLower(norm.NFKC.String(s))
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

func pointerPreserved(deleted []string, target string) bool {
	// Lines and short adjacent windows preserve wrapped sentences without
	// allowing unrelated claims from distant sections to combine into a match.
	lines := strings.Split(target, "\n")
	for i := range lines {
		for end := i + 1; end <= len(lines) && end <= i+3; end++ {
			window := meaningfulTokens(strings.Join(lines[i:end], " "))
			if len(window) > 120 {
				break
			}
			if tokenMatch(deleted, window) {
				return true
			}
		}
	}
	return false
}

func tokenMatch(needle, hay []string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		same := true
		for j := range needle {
			if needle[j] != hay[i+j] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	set := make(map[string]bool)
	for _, t := range needle {
		set[t] = true
	}
	matched := 0
	for _, t := range hay {
		if set[t] {
			matched++
			delete(set, t)
		}
	}
	unique := len(mapTokens(needle))
	return matched >= 8 && float64(matched)/float64(unique) >= 0.80
}

func mapTokens(tokens []string) map[string]bool {
	m := make(map[string]bool)
	for _, t := range tokens {
		m[t] = true
	}
	return m
}

package pipeline

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// findingIDsJSON extracts the finding IDs from a findings JSON payload and
// returns them as a JSON array string. Empty result means there were no
// findings or parsing failed.
func findingIDsJSON(raw string) string {
	if raw == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return ""
	}
	ids := make([]string, 0, len(findings.Items))
	for _, item := range findings.Items {
		if item.ID == "" {
			continue
		}
		ids = append(ids, item.ID)
	}
	return marshalFindingIDs(ids)
}

// findingIDList extracts the finding IDs from a findings JSON payload as a
// plain slice (no JSON encoding), for selection bookkeeping like the review
// loop's pending-verification set.
func findingIDList(raw string) []string {
	if raw == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(findings.Items))
	for _, item := range findings.Items {
		if item.ID != "" {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func retainFindingIDs(raw string, ids []string) []string {
	if raw == "" || len(ids) == 0 {
		return nil
	}
	present := make(map[string]bool)
	for _, id := range findingIDList(raw) {
		present[id] = true
	}
	retained := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" && present[id] && !seen[id] {
			retained = append(retained, id)
			seen[id] = true
		}
	}
	return retained
}

func appendFindingIDs(existing, additional []string) []string {
	result := append([]string(nil), existing...)
	seen := make(map[string]bool, len(existing)+len(additional))
	for _, id := range existing {
		if id != "" {
			seen[id] = true
		}
	}
	for _, id := range additional {
		if id != "" && !seen[id] {
			result = append(result, id)
			seen[id] = true
		}
	}
	return result
}

func findingIDsInMergedJSON(mergedRaw, selectedRaw string) []string {
	if selectedRaw == "" {
		return nil
	}
	merged, mergedErr := types.ParseFindingsJSON(mergedRaw)
	selected, selectedErr := types.ParseFindingsJSON(selectedRaw)
	if mergedErr != nil || selectedErr != nil {
		return findingIDList(selectedRaw)
	}
	mergedCounts := countFindingFingerprints(merged.Items)
	selectedCounts := countFindingFingerprints(selected.Items)
	matched := make(map[int]bool, len(merged.Items))
	ids := make([]string, 0, len(selected.Items))
	for _, selectedItem := range selected.Items {
		if selectedItem.ID == "" {
			continue
		}
		exact := map[types.Finding]bool{findingKey(selectedItem): true}
		for index, mergedItem := range merged.Items {
			if matched[index] || mergedItem.ID == "" || !hasFindingMatch(mergedItem, exact, mergedCounts, selectedCounts) {
				continue
			}
			matched[index] = true
			ids = append(ids, mergedItem.ID)
			break
		}
	}
	return ids
}

// marshalFindingIDs encodes a list of finding IDs as a JSON array. Empty
// input returns an empty string so the caller can leave the DB column NULL.
func marshalFindingIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func findingKey(item types.Finding) types.Finding {
	item.ID = ""
	item.Action = ""
	item.Source = ""
	item.UserInstructions = ""
	return item
}

func findingFingerprint(item types.Finding) types.Finding {
	item = findingKey(item)
	item.Line = 0
	return item
}

func countFindingFingerprints(items []types.Finding) map[types.Finding]int {
	counts := make(map[types.Finding]int, len(items))
	for _, item := range items {
		counts[findingFingerprint(item)]++
	}
	return counts
}

func hasFindingMatch(item types.Finding, exact map[types.Finding]bool, itemCounts, candidateCounts map[types.Finding]int) bool {
	if exact[findingKey(item)] {
		return true
	}
	fingerprint := findingFingerprint(item)
	return itemCounts[fingerprint] == 1 && candidateCounts[fingerprint] == 1
}

func normalizeFindingsJSON(raw string, prefix string) string {
	if raw == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	normalized := types.NormalizeFindings(findings, prefix)
	normalizedRaw, err := types.MarshalFindingsJSON(normalized)
	if err != nil {
		return raw
	}
	return normalizedRaw
}

func excludeFindingsJSON(raw string, ids []string) string {
	if raw == "" || len(ids) == 0 {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return ""
	}
	excluded := types.ExcludeFindings(findings, ids)
	if len(excluded.Items) == 0 {
		return ""
	}
	excludedRaw, err := types.MarshalFindingsJSON(excluded)
	if err != nil {
		return ""
	}
	return excludedRaw
}

func mergeFindingsJSON(existingRaw, additionalRaw string) string {
	if existingRaw == "" {
		return additionalRaw
	}
	if additionalRaw == "" {
		return existingRaw
	}
	existing, err := types.ParseFindingsJSON(existingRaw)
	if err != nil {
		return additionalRaw
	}
	additional, err := types.ParseFindingsJSON(additionalRaw)
	if err != nil {
		return existingRaw
	}
	seen := make(map[types.Finding]bool, len(existing.Items)+len(additional.Items))
	existingCounts := countFindingFingerprints(existing.Items)
	additionalCounts := countFindingFingerprints(additional.Items)
	merged := types.FindingsMetadata(existing)
	for _, item := range existing.Items {
		merged.Items = append(merged.Items, item)
		seen[findingKey(item)] = true
	}
	for _, item := range additional.Items {
		if hasFindingMatch(item, seen, additionalCounts, existingCounts) {
			continue
		}
		key := findingKey(item)
		if seen[key] {
			continue
		}
		merged.Items = append(merged.Items, item)
		seen[key] = true
	}
	if len(merged.Items) == 0 {
		return ""
	}
	mergedRaw, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		return existingRaw
	}
	return mergedRaw
}

func removeMatchingFindingsJSON(existingRaw, removeRaw string) string {
	if existingRaw == "" || removeRaw == "" {
		return existingRaw
	}
	existing, err := types.ParseFindingsJSON(existingRaw)
	if err != nil {
		return existingRaw
	}
	remove, err := types.ParseFindingsJSON(removeRaw)
	if err != nil {
		return existingRaw
	}
	toRemove := make(map[types.Finding]bool, len(remove.Items))
	existingCounts := countFindingFingerprints(existing.Items)
	removeCounts := countFindingFingerprints(remove.Items)
	for _, item := range remove.Items {
		toRemove[findingKey(item)] = true
	}
	filtered := types.FindingsMetadata(existing)
	for _, item := range existing.Items {
		if hasFindingMatch(item, toRemove, existingCounts, removeCounts) {
			continue
		}
		filtered.Items = append(filtered.Items, item)
	}
	if len(filtered.Items) == 0 {
		return ""
	}
	filteredRaw, err := types.MarshalFindingsJSON(filtered)
	if err != nil {
		return existingRaw
	}
	return filteredRaw
}

func retainMatchingFindingsJSON(existingRaw, keepRaw string) string {
	if existingRaw == "" || keepRaw == "" {
		return ""
	}
	existing, err := types.ParseFindingsJSON(existingRaw)
	if err != nil {
		return ""
	}
	keep, err := types.ParseFindingsJSON(keepRaw)
	if err != nil {
		return ""
	}
	allowed := make(map[types.Finding]bool, len(keep.Items))
	existingCounts := countFindingFingerprints(existing.Items)
	keepCounts := countFindingFingerprints(keep.Items)
	for _, item := range keep.Items {
		allowed[findingKey(item)] = true
	}
	filtered := types.FindingsMetadata(existing)
	for _, item := range existing.Items {
		if !hasFindingMatch(item, allowed, existingCounts, keepCounts) {
			continue
		}
		filtered.Items = append(filtered.Items, item)
	}
	if len(filtered.Items) == 0 {
		return ""
	}
	filteredRaw, err := types.MarshalFindingsJSON(filtered)
	if err != nil {
		return ""
	}
	return filteredRaw
}

func autoFixableFindingsJSON(raw string) string {
	if raw == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	fixable := types.AutoFixableFindings(findings)
	if len(fixable.Items) == 0 {
		return ""
	}
	fixableRaw, err := types.MarshalFindingsJSON(fixable)
	if err != nil {
		return raw
	}
	return fixableRaw
}

func hasAskUserFindingsJSON(raw string) bool {
	if raw == "" {
		return false
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	return types.HasAskUserFindings(findings)
}

func hasActionableFindingsJSON(raw string) bool {
	if raw == "" {
		return false
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	return types.HasActionableFindings(findings)
}

// reviewFixRoundLimit bounds the review step's fix-round loop. It is the loop
// budget the executor enforces: once this many fix rounds have run, the step
// stops looping and parks on an explicit ask-user finding instead of starting
// another round, so a review that never converges is a bounded, reportable
// stop rather than an open-ended burn (issue #269 reached fifteen rounds).
//
// The other two stop conditions are deliberately not extra counters: a round
// that adds no new findings and resolves none is detected as a stalled round
// (reviewStalledRoundLimit), and the per-invocation agent budget
// (review_agent_timeout) plus the automatic-round budget (auto_fix.review)
// already bound each individual round. This mirrors open-code-review's
// MAX_REVIEW_ROUNDS shape (1/2/3 by effort) rather than its machinery.
const reviewFixRoundLimit = 3

// reviewStalledRoundLimit is how many consecutive review fix rounds may leave
// the outstanding set byte-identical (no new findings, nothing positively
// resolved) before the loop stops and parks. One stalled round is often a
// no-op fixer turn worth retrying with different guidance; two prove repeating
// the same invitation is not converging.
const reviewStalledRoundLimit = 2

// reviewLoopStopReason reports why the review fix-round loop must not start
// another round. An empty string means the loop may continue.
func reviewLoopStopReason(fixRounds, stalledRounds int) string {
	switch {
	case fixRounds >= reviewFixRoundLimit:
		return fmt.Sprintf("review reached its %d-fix-round cap", reviewFixRoundLimit)
	case stalledRounds >= reviewStalledRoundLimit:
		return fmt.Sprintf("%d consecutive review fix rounds added no new findings and resolved none", stalledRounds)
	default:
		return ""
	}
}

// reviewLoopStopFindingsJSON renders the bounded stop as an explicit ask-user
// finding, so a capped or stalled review parks for a human decision on the
// still-outstanding findings instead of silently completing or burning another
// round. The finding has no file and is therefore never verified away; only
// approve, skip, or abort clears it.
const reviewLoopStopFindingID = "review-loop-stop"

func reviewLoopStopFindingsJSON(reason string) string {
	if reason == "" {
		return ""
	}
	encoded, err := types.MarshalFindingsJSON(types.Findings{
		Items: []types.Finding{{
			ID:          reviewLoopStopFindingID,
			Severity:    types.FindingSeverityWarning,
			Description: "Review stopped looping: " + reason + ". The outstanding findings above are still unresolved. Decide: approve to ship as-is, skip the step, or abort.",
			Action:      types.ActionAskUser,
		}},
		Summary:       "review fix-round loop stopped: " + reason,
		RiskLevel:     "high",
		RiskRationale: reason,
		RiskScope:     types.FindingsRiskScopeSourceOrExternal,
	})
	if err != nil {
		return ""
	}
	return encoded
}

func isReviewLoopStopFinding(item types.Finding) bool {
	return strings.HasPrefix(item.ID, reviewLoopStopFindingID) && item.Action == types.ActionAskUser && item.File == "" && strings.HasPrefix(item.Description, "Review stopped looping: ")
}

func reviewLoopStopFindingPresent(raw string) bool {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		if isReviewLoopStopFinding(item) {
			return true
		}
	}
	return false
}

// reviewedPathsJSON extracts a review round's coverage record (the files the
// turn actually examined) from its raw findings payload.
func reviewedPathsJSON(raw string) []string {
	if raw == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	return findings.ReviewedPaths
}

// normalizeCoveredPath canonicalizes a reviewed or finding path for coverage
// comparison. A mismatch (including a finding with no file at all) fails the
// verification closed: the item simply stays outstanding.
func normalizeCoveredPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func findingReportedForVerification(item types.Finding, reportedItems []types.Finding, reported map[types.Finding]bool, outstandingCounts, reportedCounts map[types.Finding]int) bool {
	if hasFindingMatch(item, reported, outstandingCounts, reportedCounts) {
		return true
	}
	for _, reportedItem := range reportedItems {
		if sameFindingLocation(item, reportedItem) {
			return true
		}
	}
	return false
}

func sameFindingLocation(left, right types.Finding) bool {
	leftFile := normalizeCoveredPath(left.File)
	rightFile := normalizeCoveredPath(right.File)
	if leftFile == "" || leftFile != rightFile {
		return false
	}
	return left.Line == right.Line
}

// resolveVerifiedFindingsJSON returns outstandingRaw minus every finding whose
// ID is in pendingIDs and for which this round is a POSITIVE verification
// record: the round listed the finding's file in its ReviewedPaths coverage,
// and the round's own output (thisRoundRaw) no longer reports the defect.
//
// This is the only way a selected-and-fixed finding leaves the outstanding set
// besides an explicit operator action (approve/skip/abort). A file the round
// did not list, a missing coverage record, a finding with no file, or a round
// that re-reports the defect all leave the item in place: silence, or a round
// that did not look, is never resolution. That is the P1 this closes - the
// predecessor dropped a selected finding the moment its fix was requested, so
// a no-op fix could let the run complete with the defect unresolved.
func resolveVerifiedFindingsJSON(outstandingRaw string, pendingIDs []string, reviewedPaths []string, thisRoundRaw string) string {
	if outstandingRaw == "" || len(pendingIDs) == 0 || len(reviewedPaths) == 0 {
		return outstandingRaw
	}
	outstanding, err := types.ParseFindingsJSON(outstandingRaw)
	if err != nil {
		return outstandingRaw
	}
	pending := make(map[string]bool, len(pendingIDs))
	for _, id := range pendingIDs {
		if id != "" {
			pending[id] = true
		}
	}
	if len(pending) == 0 {
		return outstandingRaw
	}
	covered := make(map[string]bool, len(reviewedPaths))
	for _, reviewed := range reviewedPaths {
		if normalized := normalizeCoveredPath(reviewed); normalized != "" {
			covered[normalized] = true
		}
	}
	if len(covered) == 0 {
		return outstandingRaw
	}
	thisRound, _ := types.ParseFindingsJSON(thisRoundRaw)
	reported := make(map[types.Finding]bool, len(thisRound.Items))
	for _, item := range thisRound.Items {
		reported[findingKey(item)] = true
	}
	outstandingCounts := countFindingFingerprints(outstanding.Items)
	thisRoundCounts := countFindingFingerprints(thisRound.Items)
	result := types.FindingsMetadata(outstanding)
	for _, item := range outstanding.Items {
		if pending[item.ID] && covered[normalizeCoveredPath(item.File)] && !findingReportedForVerification(item, thisRound.Items, reported, outstandingCounts, thisRoundCounts) {
			continue
		}
		result.Items = append(result.Items, item)
	}
	if len(result.Items) == len(outstanding.Items) {
		return outstandingRaw
	}
	if len(result.Items) == 0 {
		return ""
	}
	encoded, err := types.MarshalFindingsJSON(result)
	if err != nil {
		return outstandingRaw
	}
	return encoded
}

// mergeOutstandingFindingsJSON merges one review round's output into the
// append-only outstanding set.
//
// Two things make it more than mergeFindingsJSON: the merged set keeps only
// one item per ID (this round's positional normalization can re-mint an ID an
// outstanding item already holds, and the outstanding item's ID is what
// `axi respond --findings <id>` selects, so the colliding NEW item is
// re-minted instead), and the merged payload carries this round's coverage
// record rather than the outstanding set's stale copy.
//
// Identity is still content-derived: a reworded restatement of an existing
// finding does not match its original and is appended as a second item. That
// is accepted rather than fixed here - it over-blocks instead of dropping
// anything, and stable finding identity is a separate design pass (Parts 2+3
// of the scout report).
func mergeOutstandingFindingsJSON(existingRaw, additionalRaw string) string {
	if additionalRaw == "" {
		return existingRaw
	}
	mergedRaw := mergeFindingsJSON(existingRaw, additionalRaw)
	if mergedRaw == "" {
		return ""
	}
	merged, err := types.ParseFindingsJSON(mergedRaw)
	if err != nil {
		return mergedRaw
	}
	merged.ReviewedPaths = reviewedPathsJSON(additionalRaw)
	seen := make(map[string]bool, len(merged.Items))
	changed := false
	for i := range merged.Items {
		id := merged.Items[i].ID
		if id != "" && !seen[id] {
			seen[id] = true
			continue
		}
		if isReviewLoopStopFinding(merged.Items[i]) {
			merged.Items[i].ID = nextFreeReviewLoopStopFindingID(seen)
		} else {
			merged.Items[i].ID = nextFreeReviewFindingID(seen)
		}
		seen[merged.Items[i].ID] = true
		changed = true
	}
	if !changed && len(merged.ReviewedPaths) == 0 {
		return mergedRaw
	}
	encoded, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		return mergedRaw
	}
	return encoded
}

func nextFreeReviewLoopStopFindingID(seen map[string]bool) string {
	if !seen[reviewLoopStopFindingID] {
		return reviewLoopStopFindingID
	}
	for i := 1; ; i++ {
		id := reviewLoopStopFindingID + "-" + strconv.Itoa(i)
		if !seen[id] {
			return id
		}
	}
}

func nextFreeReviewFindingID(seen map[string]bool) string {
	for i := 1; ; i++ {
		id := "review-" + strconv.Itoa(i)
		if !seen[id] {
			return id
		}
	}
}

// combineSelectedFindingIDs returns the ordered list of finding IDs that
// were dispatched to the fix agent: the user's selected agent-produced
// IDs plus any user-authored finding IDs (which only appear in the merged
// list).
func combineSelectedFindingIDs(selected []string, mergedFindings string) []string {
	if mergedFindings == "" {
		return selected
	}
	merged, err := types.ParseFindingsJSON(mergedFindings)
	if err != nil {
		return selected
	}
	seen := make(map[string]bool, len(selected))
	for _, id := range selected {
		if id != "" {
			seen[id] = true
		}
	}
	result := append([]string(nil), selected...)
	for _, item := range merged.Items {
		if item.ID == "" || seen[item.ID] {
			continue
		}
		result = append(result, item.ID)
		seen[item.ID] = true
	}
	return result
}

// mergeUserOverridesJSON takes a findings JSON payload and applies
// per-finding user instructions and user-authored findings. When no
// overrides are present the input is returned unchanged.
func mergeUserOverridesJSON(raw string, instructions map[string]string, added []types.Finding) string {
	if len(instructions) == 0 && len(added) == 0 {
		return raw
	}
	base, err := types.ParseFindingsJSON(raw)
	if err != nil {
		base = types.Findings{}
	}
	merged := types.MergeUserOverrides(base, instructions, added)
	encoded, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		return raw
	}
	return encoded
}

func filterFindingsJSON(raw string, ids []string) string {
	if raw == "" {
		return raw
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	filtered := types.FilterFindings(findings, ids)
	if len(ids) == 0 {
		filtered = types.FindingsMetadata(findings)
		filtered.Summary = "0 selected findings"
	}
	filteredRaw, err := types.MarshalFindingsJSON(filtered)
	if err != nil {
		return raw
	}
	return filteredRaw
}

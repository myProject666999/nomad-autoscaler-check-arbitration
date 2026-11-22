// Copyright IBM Corp. 2020, 2026
// SPDX-License-Identifier: MPL-2.0

package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/nomad-autoscaler/sdk"
)

const (
	// arbitrationMetaKeyParticipants is the ScalingAction Meta key under which
	// check arbitration stores compact summaries of the non-winner checks that
	// took part in the evaluation. Summaries are rendered in policy.Checks
	// declaration order and truncated to arbitrationMaxParticipantSummaries
	// entries.
	//
	// This key is intentionally not part of the sdk strategyActionMeta* series
	// ("nomad_autoscaler.dry_run", "nomad_autoscaler.dry_run.count",
	// "nomad_autoscaler.count.capped", "nomad_autoscaler.count.original",
	// "nomad_autoscaler.reason_history"): those keys describe a single action,
	// while this one describes the arbitration across the checks of a policy.
	arbitrationMetaKeyParticipants = "nomad_autoscaler.arbitration.participants"

	// arbitrationMetaKeyParticipantsTotal is the ScalingAction Meta key under
	// which check arbitration stores the total number of non-winner
	// participants, regardless of any truncation applied to the summaries
	// list stored under arbitrationMetaKeyParticipants.
	arbitrationMetaKeyParticipantsTotal = "nomad_autoscaler.arbitration.participants_total"
)

// arbitrationMaxParticipantSummaries bounds how many participant summaries
// are stored under arbitrationMetaKeyParticipants. The truncation is stable:
// the first entries in policy.Checks declaration order are kept, and the full
// count is always available under arbitrationMetaKeyParticipantsTotal.
const arbitrationMaxParticipantSummaries = 10

// Participant statuses used in arbitration summaries.
const (
	// arbitrationStatusPreempted marks a participant that competed and lost.
	arbitrationStatusPreempted = "preempted"

	// arbitrationStatusNoneIgnored marks a `none` action that was excluded
	// from comparison by the group none-handling rules.
	arbitrationStatusNoneIgnored = "none_ignored"
)

// Meta keys written by sdk.ScalingAction.CapCount. They are duplicated here
// because the sdk constants are unexported; keep in sync with sdk/strategy.go.
const (
	sdkMetaKeyCountCapped   = "nomad_autoscaler.count.capped"
	sdkMetaKeyCountOriginal = "nomad_autoscaler.count.original"
)

// arbitrationOptions holds optional knobs that alter check arbitration.
//
// It currently carries no policy-configurable settings. A future policy
// option to prefer the lowest count when scaling down (instead of the
// historical "highest count wins" rule) would be threaded through here. If
// that option is confirmed, policy/file/parse.go and policy/nomad must parse
// and validate it consistently, and its zero value must keep the historical
// behavior described by compareCheckResults.
type arbitrationOptions struct{}

// arbitrationParticipant records how a single check result fared in
// arbitration.
type arbitrationParticipant struct {
	result checkResult
	status string
	note   string
}

// arbitrateCheckResults picks the winning scaling action out of the results
// of all checks of a policy and reports how every other check fared.
//
// Each result's order field must hold its index in policy.Checks; the slice
// itself may arrive in any order. The arbitration is fully deterministic and
// does not depend on map iteration or input slice ordering:
//
//  1. Results are grouped by their check group. In a named (non-empty)
//     group, `none` actions are ignored unless _every_ check in the group
//     voted `none`, in which case the first declared check represents the
//     group. In the default (empty) group, `none` actions compete normally.
//     This asymmetry is historical and intentional: grouped checks have
//     correlated metrics, so a lone `none` must not silence real votes, and
//     an all-none group must not look like a scale-down vote against other
//     groups.
//  2. Each group produces a single representative using the precedence rules
//     of sdk.PreemptScalingAction (see compareCheckResults).
//  3. The group representatives compete under the same rules; the winner is
//     the action of the policy.
//
// The returned participants are every non-winner check in policy.Checks
// declaration order, each annotated with how it was processed.
func arbitrateCheckResults(results []checkResult, _ arbitrationOptions) (checkResult, []arbitrationParticipant) {
	var winner checkResult

	// Filter out nil actions and handlers defensively; runCheckAndCapCount
	// never produces them, but a result without an action cannot compete and
	// a result without a handler cannot be summarized, so neither is a
	// participant.
	competing := make([]checkResult, 0, len(results))
	for _, r := range results {
		if r.action != nil && r.handler != nil {
			competing = append(competing, r)
		}
	}
	if len(competing) == 0 {
		return winner, nil
	}

	// Work in policy.Checks declaration order regardless of the order the
	// results arrived in.
	sort.SliceStable(competing, func(i, j int) bool {
		return competing[i].order < competing[j].order
	})

	participants := make([]arbitrationParticipant, len(competing))
	for i, r := range competing {
		participants[i] = arbitrationParticipant{result: r, status: arbitrationStatusPreempted}
	}

	// Group results, preserving declaration order within each group. The
	// group list is driven by first-seen declaration order and the map is
	// only ever accessed by key, so map iteration order plays no role.
	groupOrder := []string{}
	groupIdx := make(map[string][]int)
	for i, r := range competing {
		if _, ok := groupIdx[r.group]; !ok {
			groupOrder = append(groupOrder, r.group)
		}
		groupIdx[r.group] = append(groupIdx[r.group], i)
	}

	winnerIdx := -1
	for _, group := range groupOrder {
		idxs := groupIdx[group]

		if group != "" {
			nonNone := make([]int, 0, len(idxs))
			for _, i := range idxs {
				if competing[i].action.Direction == sdk.ScaleDirectionNone {
					participants[i].status = arbitrationStatusNoneIgnored
					participants[i].note = "none vote ignored: group has non-none check results"
				} else {
					nonNone = append(nonNone, i)
				}
			}

			if len(nonNone) > 0 {
				idxs = nonNone
			} else {
				// Every check in the group voted none. Let the first declared
				// check represent the group so the group is not treated as a
				// scale-down vote when compared with other groups.
				for _, i := range idxs[1:] {
					participants[i].status = arbitrationStatusNoneIgnored
					participants[i].note = "none vote ignored: all group checks voted none, first declared check represents the group"
				}
				idxs = idxs[:1]
				participants[idxs[0]].status = arbitrationStatusPreempted
				participants[idxs[0]].note = ""
			}
		}

		groupWinner := idxs[0]
		for _, i := range idxs[1:] {
			if compareCheckResults(competing[i], competing[groupWinner]) < 0 {
				groupWinner = i
			}
		}

		if winnerIdx == -1 || compareCheckResults(competing[groupWinner], competing[winnerIdx]) < 0 {
			winnerIdx = groupWinner
		}
	}

	winner = competing[winnerIdx]

	// Everyone but the winner is a reported participant, in declaration order.
	out := make([]arbitrationParticipant, 0, len(participants)-1)
	for i, p := range participants {
		if i != winnerIdx {
			out = append(out, p)
		}
	}

	return winner, out
}

// compareCheckResults orders two competing check results, returning a
// negative number if a wins and a positive number if b wins.
//
// The precedence rules are exactly those of sdk.PreemptScalingAction:
//   - the stronger direction wins, using the sdk.ScaleDirection declaration
//     order (down < none < up < recommendation); recommendation is 127, above
//     up, which is the existing Enterprise Dynamic Application Sizing
//     convention and is preserved;
//   - with equal up/down directions, the higher count wins, so for down the
//     action that removes fewer instances is preferred;
//   - none and recommendation actions never compare counts.
//
// The only addition over the historical behavior is the final tie-break: when
// direction and count are equal, the check declared earlier in policy.Checks
// wins. This can only change the winner when the inputs are tied.
func compareCheckResults(a, b checkResult) int {
	aWins := sdk.PreemptScalingAction(a.action, b.action) == a.action
	bWins := sdk.PreemptScalingAction(b.action, a.action) == b.action

	switch {
	case aWins && !bWins:
		return -1
	case bWins && !aWins:
		return 1
	}

	// Full tie: policy.Checks declaration order decides.
	switch {
	case a.order < b.order:
		return -1
	case a.order > b.order:
		return 1
	}
	return 0
}

// attachArbitrationMeta records the arbitration outcome in the winning
// action's Meta so operators can see why the other checks did not take
// effect. The winner's Reason and its existing Meta entries (including the
// CapCount explanation) are left untouched, and nothing is written when the
// winner was the only participant, so Reason consumers and single-check
// policies are unaffected.
func attachArbitrationMeta(winner *sdk.ScalingAction, participants []arbitrationParticipant) {
	if len(participants) == 0 {
		return
	}

	n := len(participants)
	if n > arbitrationMaxParticipantSummaries {
		n = arbitrationMaxParticipantSummaries
	}

	summaries := make([]string, 0, n)
	for _, p := range participants[:n] {
		summaries = append(summaries, formatArbitrationParticipant(p))
	}

	winner.Canonicalize()
	winner.Meta[arbitrationMetaKeyParticipants] = summaries
	winner.Meta[arbitrationMetaKeyParticipantsTotal] = len(participants)
}

// formatArbitrationParticipant renders a participant as a compact single
// line of space-separated key=value pairs in a fixed field order, for
// example:
//
//	check=mem group=app direction=up count=10 capped=true original=12 status=preempted
//	check=q group=queue direction=none count=0 capped=false status=none_ignored note="none vote ignored: ..."
func formatArbitrationParticipant(p arbitrationParticipant) string {
	a := p.result.action

	group := p.result.group
	if group == "" {
		group = "(default)"
	}

	capped, original := cappedCountInfo(a)

	var sb strings.Builder
	fmt.Fprintf(&sb, "check=%s group=%s direction=%s count=%d capped=%t",
		p.result.handler.name(), group, a.Direction, a.Count, capped)
	if capped {
		fmt.Fprintf(&sb, " original=%d", original)
	}
	fmt.Fprintf(&sb, " status=%s", p.status)
	if p.note != "" {
		fmt.Fprintf(&sb, " note=%q", p.note)
	}
	return sb.String()
}

// cappedCountInfo reports whether sdk.ScalingAction.CapCount adjusted the
// action's count and, if so, the original value.
func cappedCountInfo(a *sdk.ScalingAction) (bool, int64) {
	if a.Meta == nil {
		return false, 0
	}

	capped, _ := a.Meta[sdkMetaKeyCountCapped].(bool)
	if !capped {
		return false, 0
	}

	original, _ := a.Meta[sdkMetaKeyCountOriginal].(int64)
	return true, original
}

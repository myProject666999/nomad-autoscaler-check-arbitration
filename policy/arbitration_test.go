// Copyright IBM Corp. 2020, 2026
// SPDX-License-Identifier: MPL-2.0

package policy

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad-autoscaler/sdk"
	"github.com/shoenig/test/must"
)

// newArbitrationTestHandler builds a Handler whose check runners are the
// given stubs, in declaration order. No metrics or strategy plugins are
// involved.
func newArbitrationTestHandler(stubs ...*stubChecker) *Handler {
	runners := make([]checker, len(stubs))
	for i, s := range stubs {
		runners[i] = s
	}
	return &Handler{
		log: hclog.NewNullLogger(),
		policy: &sdk.ScalingPolicy{
			ID:     "test-policy",
			Target: &sdk.ScalingPolicyTarget{Name: "mock-target", Config: map[string]string{}},
		},
		checkRunners: runners,
	}
}

// arbitrationParticipants renders the participant summaries stored in the
// winning action's Meta.
func arbitrationParticipants(t *testing.T, action sdk.ScalingAction) []string {
	t.Helper()
	summaries, ok := action.Meta[arbitrationMetaKeyParticipants].([]string)
	must.True(t, ok, must.Sprintf("missing %s in meta %v", arbitrationMetaKeyParticipants, action.Meta))
	return summaries
}

// arbitrationTotal returns the participants total stored in the winning
// action's Meta.
func arbitrationTotal(t *testing.T, action sdk.ScalingAction) int {
	t.Helper()
	total, ok := action.Meta[arbitrationMetaKeyParticipantsTotal].(int)
	must.True(t, ok, must.Sprintf("missing %s in meta %v", arbitrationMetaKeyParticipantsTotal, action.Meta))
	return total
}

func TestHandler_calculateNewCount_SingleCheckNoArbitrationMeta(t *testing.T) {
	h := newArbitrationTestHandler(&stubChecker{
		checkName: "only",
		action:    sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 5, Reason: "up to 5"},
	})

	action, err := h.calculateNewCount(context.Background(), 2)
	must.NoError(t, err)
	must.Eq(t, sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 5, Reason: "up to 5"}, action)
}

func TestHandler_calculateNewCount_SameGroupNoneIgnored(t *testing.T) {
	h := newArbitrationTestHandler(
		&stubChecker{
			checkName: "idle", groupName: "app",
			action: sdk.ScalingAction{Direction: sdk.ScaleDirectionNone, Count: 6},
		},
		&stubChecker{
			checkName: "cpu", groupName: "app",
			action: sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 9, Reason: "cpu up"},
		},
	)

	action, err := h.calculateNewCount(context.Background(), 6)
	must.NoError(t, err)
	must.Eq(t, "cpu up", action.Reason)
	must.Eq(t, []string{
		`check=idle group=app direction=none count=6 capped=false status=none_ignored note="none vote ignored: group has non-none check results"`,
	}, arbitrationParticipants(t, action))
	must.Eq(t, 1, arbitrationTotal(t, action))
}

func TestHandler_calculateNewCount_CrossGroup(t *testing.T) {
	h := newArbitrationTestHandler(
		&stubChecker{
			checkName: "web", groupName: "g1",
			action: sdk.ScalingAction{Direction: sdk.ScaleDirectionDown, Count: 3, Reason: "web down"},
		},
		&stubChecker{
			checkName: "batch", groupName: "g2",
			action: sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 7, Reason: "batch up"},
		},
	)

	action, err := h.calculateNewCount(context.Background(), 6)
	must.NoError(t, err)
	must.Eq(t, "batch up", action.Reason)
	must.Eq(t, []string{
		"check=web group=g1 direction=down count=3 capped=false status=preempted",
	}, arbitrationParticipants(t, action))
}

func TestHandler_calculateNewCount_AllNoneGroups(t *testing.T) {
	h := newArbitrationTestHandler(
		&stubChecker{
			checkName: "a", groupName: "g1",
			action: sdk.ScalingAction{Direction: sdk.ScaleDirectionNone, Count: 6, Reason: "a none"},
		},
		&stubChecker{
			checkName: "b", groupName: "g1",
			action: sdk.ScalingAction{Direction: sdk.ScaleDirectionNone, Count: 6},
		},
		&stubChecker{
			checkName: "c", groupName: "g2",
			action: sdk.ScalingAction{Direction: sdk.ScaleDirectionNone, Count: 6},
		},
	)

	action, err := h.calculateNewCount(context.Background(), 6)
	must.NoError(t, err)

	// All checks voted none: the first declared check represents its group
	// and wins the all-none tie by declaration order.
	must.Eq(t, sdk.ScaleDirectionNone, action.Direction)
	must.Eq(t, "a none", action.Reason)
	must.Eq(t, []string{
		`check=b group=g1 direction=none count=6 capped=false status=none_ignored note="none vote ignored: all group checks voted none, first declared check represents the group"`,
		"check=c group=g2 direction=none count=6 capped=false status=preempted",
	}, arbitrationParticipants(t, action))
	must.Eq(t, 2, arbitrationTotal(t, action))
}

// TestHandler_calculateNewCount_DefaultGroupNoneCompetes pins the intentional
// asymmetry between the default (empty) group and named groups: in the
// default group a none action competes normally (and beats a down action),
// while in a named group it is ignored when other checks vote to scale.
func TestHandler_calculateNewCount_DefaultGroupNoneCompetes(t *testing.T) {
	t.Run("default_group_none_beats_down", func(t *testing.T) {
		h := newArbitrationTestHandler(
			&stubChecker{
				checkName: "idle",
				action:    sdk.ScalingAction{Direction: sdk.ScaleDirectionNone, Count: 6, Reason: "idle none"},
			},
			&stubChecker{
				checkName: "latency",
				action:    sdk.ScalingAction{Direction: sdk.ScaleDirectionDown, Count: 4},
			},
		)

		action, err := h.calculateNewCount(context.Background(), 6)
		must.NoError(t, err)
		must.Eq(t, sdk.ScaleDirectionNone, action.Direction)
		must.Eq(t, "idle none", action.Reason)
		must.Eq(t, []string{
			"check=latency group=(default) direction=down count=4 capped=false status=preempted",
		}, arbitrationParticipants(t, action))
	})

	t.Run("named_group_none_ignored_down_wins", func(t *testing.T) {
		h := newArbitrationTestHandler(
			&stubChecker{
				checkName: "idle", groupName: "g",
				action: sdk.ScalingAction{Direction: sdk.ScaleDirectionNone, Count: 6},
			},
			&stubChecker{
				checkName: "latency", groupName: "g",
				action: sdk.ScalingAction{Direction: sdk.ScaleDirectionDown, Count: 4, Reason: "latency down"},
			},
		)

		action, err := h.calculateNewCount(context.Background(), 6)
		must.NoError(t, err)
		must.Eq(t, sdk.ScaleDirectionDown, action.Direction)
		must.Eq(t, "latency down", action.Reason)
		must.Eq(t, []string{
			`check=idle group=g direction=none count=6 capped=false status=none_ignored note="none vote ignored: group has non-none check results"`,
		}, arbitrationParticipants(t, action))
	})
}

// TestHandler_calculateNewCount_CappedTiePrefersFirstDeclared simulates two
// checks whose counts were both capped to the policy Max (requests of 12 and
// 20 with Max=10). The tie is broken by declaration order, the winner keeps
// its own Reason and cap explanation, and the loser is summarized with its
// capping information.
func TestHandler_calculateNewCount_CappedTiePrefersFirstDeclared(t *testing.T) {
	cappedAction := func(reason string, original int64) sdk.ScalingAction {
		return sdk.ScalingAction{
			Direction: sdk.ScaleDirectionUp,
			Count:     10,
			Reason:    reason,
			Meta: map[string]any{
				"nomad_autoscaler.count.capped":   true,
				"nomad_autoscaler.count.original": original,
			},
		}
	}

	h := newArbitrationTestHandler(
		&stubChecker{
			checkName: "first",
			action:    cappedAction("capped count from 12 to 10 to stay within limits", 12),
		},
		&stubChecker{
			checkName: "second",
			action:    cappedAction("capped count from 20 to 10 to stay within limits", 20),
		},
	)

	action, err := h.calculateNewCount(context.Background(), 6)
	must.NoError(t, err)
	must.Eq(t, int64(10), action.Count)
	must.Eq(t, "capped count from 12 to 10 to stay within limits", action.Reason)
	must.Eq(t, []string{
		"check=second group=(default) direction=up count=10 capped=true original=20 status=preempted",
	}, arbitrationParticipants(t, action))
	must.Eq(t, 1, arbitrationTotal(t, action))
}

// TestHandler_calculateNewCount_RecommendationBeatsUp pins the existing
// Enterprise Dynamic Application Sizing convention:
// ScaleDirectionRecommendation (127) is stronger than ScaleDirectionUp.
func TestHandler_calculateNewCount_RecommendationBeatsUp(t *testing.T) {
	must.Eq(t, 127, int(sdk.ScaleDirectionRecommendation))
	must.True(t, sdk.ScaleDirectionRecommendation > sdk.ScaleDirectionUp)

	h := newArbitrationTestHandler(
		&stubChecker{
			checkName: "cpu",
			action:    sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 10, Reason: "cpu up"},
		},
		&stubChecker{
			checkName: "das",
			action:    sdk.ScalingAction{Direction: sdk.ScaleDirectionRecommendation, Count: 8, Reason: "das recommends 8"},
		},
	)

	action, err := h.calculateNewCount(context.Background(), 6)
	must.NoError(t, err)
	must.Eq(t, sdk.ScaleDirectionRecommendation, action.Direction)
	must.Eq(t, "das recommends 8", action.Reason)
	must.Eq(t, []string{
		"check=cpu group=(default) direction=up count=10 capped=false status=preempted",
	}, arbitrationParticipants(t, action))
}

// TestHandler_calculateNewCount_OnErrorIgnoredExcluded verifies that a check
// whose failure is ignored by on_error configuration neither competes nor
// appears in the arbitration summary.
func TestHandler_calculateNewCount_OnErrorIgnoredExcluded(t *testing.T) {
	h := newArbitrationTestHandler(
		&stubChecker{checkName: "failing", err: errCheckIgnoredByOnError},
		&stubChecker{
			checkName: "strong",
			action:    sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 8, Reason: "strong up"},
		},
		&stubChecker{
			checkName: "weak",
			action:    sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 5},
		},
	)

	action, err := h.calculateNewCount(context.Background(), 6)
	must.NoError(t, err)
	must.Eq(t, "strong up", action.Reason)

	summaries := arbitrationParticipants(t, action)
	must.Eq(t, []string{
		"check=weak group=(default) direction=up count=5 capped=false status=preempted",
	}, summaries)
	must.Eq(t, 1, arbitrationTotal(t, action))
	for _, s := range summaries {
		must.StrNotContains(t, s, "failing")
	}
}

// TestHandler_calculateNewCount_WorkedExample is the end-to-end example from
// the design notes: Min=2, Max=10, current count 6. The app group's cpu
// requests up to 9 and mem requests up to 12 (capped to 10), the queue group
// votes none, and the default group's latency check requests down to 4. mem
// wins because up is stronger than none and down, and 10 beats 9 within up.
func TestHandler_calculateNewCount_WorkedExample(t *testing.T) {
	h := newArbitrationTestHandler(
		&stubChecker{
			checkName: "cpu", groupName: "app",
			action: sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 9, Reason: "cpu: scale up to 9"},
		},
		&stubChecker{
			checkName: "mem", groupName: "app",
			action: sdk.ScalingAction{
				Direction: sdk.ScaleDirectionUp,
				Count:     10,
				Reason:    "capped count from 12 to 10 to stay within limits",
				Meta: map[string]any{
					"nomad_autoscaler.count.capped":   true,
					"nomad_autoscaler.count.original": int64(12),
					"nomad_autoscaler.reason_history": []string{"mem: scale up to 12"},
				},
			},
		},
		&stubChecker{
			checkName: "queue", groupName: "queue",
			action: sdk.ScalingAction{Direction: sdk.ScaleDirectionNone, Count: 6},
		},
		&stubChecker{
			checkName: "latency",
			action:    sdk.ScalingAction{Direction: sdk.ScaleDirectionDown, Count: 4, Reason: "latency: scale down to 4"},
		},
	)

	action, err := h.calculateNewCount(context.Background(), 6)
	must.NoError(t, err)

	// The winner keeps its own Reason (including the cap explanation) and its
	// pre-existing Meta entries.
	must.Eq(t, sdk.ScaleDirectionUp, action.Direction)
	must.Eq(t, int64(10), action.Count)
	must.Eq(t, "capped count from 12 to 10 to stay within limits", action.Reason)
	must.Eq(t, int64(12), action.Meta["nomad_autoscaler.count.original"].(int64))
	must.Eq(t, []string{"mem: scale up to 12"}, action.Meta["nomad_autoscaler.reason_history"].([]string))

	// The remaining participants are summarized in declaration order.
	must.Eq(t, []string{
		"check=cpu group=app direction=up count=9 capped=false status=preempted",
		"check=queue group=queue direction=none count=6 capped=false status=preempted",
		"check=latency group=(default) direction=down count=4 capped=false status=preempted",
	}, arbitrationParticipants(t, action))
	must.Eq(t, 3, arbitrationTotal(t, action))
}

// TestHandler_calculateNewCount_TieBreakFollowsDeclarationOrder documents
// that changing the declaration order of tied checks changes the winner by
// rule; this is distinct from the stability guaranteed for a fixed
// declaration order.
func TestHandler_calculateNewCount_TieBreakFollowsDeclarationOrder(t *testing.T) {
	mk := func(name, reason string) *stubChecker {
		return &stubChecker{
			checkName: name,
			action:    sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 10, Reason: reason},
		}
	}

	action, err := newArbitrationTestHandler(mk("a", "a wins"), mk("b", "b wins")).calculateNewCount(context.Background(), 6)
	must.NoError(t, err)
	must.Eq(t, "a wins", action.Reason)

	action, err = newArbitrationTestHandler(mk("b", "b wins"), mk("a", "a wins")).calculateNewCount(context.Background(), 6)
	must.NoError(t, err)
	must.Eq(t, "b wins", action.Reason)
}

// TestHandler_calculateNewCount_ArbitrationSummaryTruncation builds thirty
// tied checks and verifies the summary is truncated to the first entries in
// declaration order while the total stays exact.
func TestHandler_calculateNewCount_ArbitrationSummaryTruncation(t *testing.T) {
	stubs := make([]*stubChecker, 30)
	for i := range stubs {
		stubs[i] = &stubChecker{
			checkName: fmt.Sprintf("check-%02d", i),
			action: sdk.ScalingAction{
				Direction: sdk.ScaleDirectionUp,
				Count:     10,
				Reason:    fmt.Sprintf("reason-%02d", i),
			},
		}
	}

	action, err := newArbitrationTestHandler(stubs...).calculateNewCount(context.Background(), 6)
	must.NoError(t, err)

	// All checks tied: the first declared check wins.
	must.Eq(t, "reason-00", action.Reason)

	must.Eq(t, 29, arbitrationTotal(t, action))

	summaries := arbitrationParticipants(t, action)
	must.Eq(t, arbitrationMaxParticipantSummaries, len(summaries))
	for i, s := range summaries {
		must.Eq(t, fmt.Sprintf(
			"check=check-%02d group=(default) direction=up count=10 capped=false status=preempted", i+1), s)
	}
}

// Test_arbitrateCheckResults_EmptyAndNil covers the defensive paths: no
// results and nil actions or handlers produce no winner and no participants.
func Test_arbitrateCheckResults_EmptyAndNil(t *testing.T) {
	winner, participants := arbitrateCheckResults(nil, arbitrationOptions{})
	must.Nil(t, winner.action)
	must.Nil(t, winner.handler)
	must.Eq(t, 0, len(participants))

	winner, participants = arbitrateCheckResults([]checkResult{
		{action: nil, handler: &stubChecker{checkName: "nil-action"}, order: 0},
		{action: &sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 5}, handler: nil, order: 1},
	}, arbitrationOptions{})
	must.Nil(t, winner.action)
	must.Nil(t, winner.handler)
	must.Eq(t, 0, len(participants))
}

// Test_arbitrateCheckResults_StableUnderInputShuffling fixes the declaration
// order (the order fields) and shuffles only the order in which the check
// results arrive and groups are inserted, over hundreds of rounds. The
// winner, its Reason and its Meta must be byte-identical every round.
func Test_arbitrateCheckResults_StableUnderInputShuffling(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	// build creates a fresh set of results each round so Meta mutation by
	// attachArbitrationMeta cannot leak between rounds. The declaration order
	// is fixed by the order fields; the slice itself is shuffled below.
	build := func() []checkResult {
		mk := func(name, group string, order int, a sdk.ScalingAction) checkResult {
			a.Canonicalize()
			return checkResult{action: &a, handler: &stubChecker{checkName: name}, group: group, order: order}
		}
		return []checkResult{
			mk("a", "g1", 0, sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 10, Reason: "a"}),
			mk("b", "g2", 1, sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 10, Reason: "b"}),
			mk("c", "g2", 2, sdk.ScalingAction{Direction: sdk.ScaleDirectionNone, Count: 6}),
			mk("d", "", 3, sdk.ScalingAction{Direction: sdk.ScaleDirectionUp, Count: 10, Reason: "d"}),
			mk("e", "", 4, sdk.ScalingAction{Direction: sdk.ScaleDirectionDown, Count: 4}),
		}
	}

	render := func(winner checkResult, participants []arbitrationParticipant) (sdk.ScalingAction, []string) {
		attachArbitrationMeta(winner.action, participants)
		summaries := make([]string, 0, len(participants))
		for _, p := range participants {
			summaries = append(summaries, formatArbitrationParticipant(p))
		}
		return *winner.action, summaries
	}

	first := build()
	wantAction, wantSummaries := render(arbitrateCheckResults(first, arbitrationOptions{}))

	// The all-up-10 tie must resolve to the first declared check.
	must.Eq(t, "a", wantAction.Reason)

	for round := 0; round < 300; round++ {
		results := build()
		rng.Shuffle(len(results), func(i, j int) {
			results[i], results[j] = results[j], results[i]
		})

		gotAction, gotSummaries := render(arbitrateCheckResults(results, arbitrationOptions{}))
		must.Eq(t, wantAction, gotAction, must.Sprintf("round %d: winner action differs", round))
		must.Eq(t, wantSummaries, gotSummaries, must.Sprintf("round %d: participants differ", round))
	}
}

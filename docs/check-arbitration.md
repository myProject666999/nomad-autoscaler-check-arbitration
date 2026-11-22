# Check Arbitration and Participant Summaries

When a policy declares multiple checks, the autoscaler evaluates every check
(`runCheckAndCapCount`, which caps each result to the policy `min`/`max`) and
then arbitrates between the results to pick a single scaling action. This
document describes the arbitration rules, what changed to make them
deterministic, and the Meta the winning action now carries so operators can
see why the other checks did not take effect.

## Arbitration rules

The precedence rules are exactly those of `sdk.PreemptScalingAction` and are
unchanged:

1. The stronger direction wins, using the `sdk.ScaleDirection` declaration
   order: `down` < `none` < `up` < `recommendation`.
   `ScaleDirectionRecommendation` is `127`, greater than `up`; this is the
   existing Enterprise Dynamic Application Sizing (DAS) convention and is
   preserved.
2. With equal `up`/`down` directions, the higher count wins. For `down` this
   means the action that removes fewer instances is preferred.
3. `none` and `recommendation` actions never compare counts.

Grouping rules (also unchanged, and intentionally asymmetric):

- In a named (non-empty) group, `none` actions are ignored unless _every_
  check in the group voted `none`; in that case the first declared check
  represents the group. Grouped checks have correlated metrics, so a lone
  `none` must not silence real votes, and an all-`none` group must not look
  like a scale-down vote against other groups.
- In the default (empty) group, `none` actions compete normally, so a `none`
  beats a `down` there. These two behaviors differ on purpose and are not
  unified.

### Tie-breaking by declaration order (new)

The only addition is a total order for ties: when direction and count are
equal, the check declared earlier in `policy.Checks` wins. Previously the
groups were held in a map and the first result iterated won a tie, so the
winner's `Reason` could vary between evaluations. Because the tie-break only
applies when the inputs are tied, **only the winner of tied inputs can
change** compared to the historical behavior; every non-tied outcome is
identical. If the declaration order itself changes, the tied winner may
change by rule — that is expected and must not be confused with instability.

Capping happens before arbitration, so ties are broken on capped counts. For
example with `Max=10`, two checks requesting 12 and 20 are both capped to 10
and tie; the check declared earlier wins. Comparing pre-cap counts instead
was considered and is **not** adopted; if it ever is, it must be called out
as a rule change.

## Participant summary Meta

The winner's `Reason` (including the existing `CapCount` explanation) and its
pre-existing Meta entries are untouched. When at least one other check
participated, the winning action gains two Meta keys:

- `nomad_autoscaler.arbitration.participants`: `[]string` of compact
  summaries of the non-winner participants, in `policy.Checks` declaration
  order, truncated to the first **10** entries.
- `nomad_autoscaler.arbitration.participants_total`: total number of
  non-winner participants, regardless of truncation.

These keys are deliberately distinct from the `strategyActionMeta*` series in
`sdk/strategy.go` (`nomad_autoscaler.dry_run`, `nomad_autoscaler.dry_run.count`,
`nomad_autoscaler.count.capped`, `nomad_autoscaler.count.original`,
`nomad_autoscaler.reason_history`): those describe a single action, while the
arbitration keys describe the comparison across checks. In particular
`reason_history` remains the rewrite history of one action's `Reason` and is
not used to store other checks. (The exact key names are marked as open to
confirmation in the design notes; they are unexported constants in
`policy/arbitration.go`.)

Each summary is a single line of space-separated `key=value` pairs in a fixed
field order:

```
check=<name> group=<group|(default)> direction=<down|none|up|recommendation> count=<n> capped=<bool> [original=<n>] status=<preempted|none_ignored> [note="..."]
```

- `capped`/`original` reflect the `CapCount` Meta of that check's action.
- `status=preempted`: the check competed and lost.
- `status=none_ignored`: a `none` vote excluded by the grouping rules; the
  `note` explains why (mixed group, or an all-`none` group represented by the
  first declared check).

Truncation is stable: the first 10 entries in declaration order are kept and
`participants_total` always reports the full count, so no participant is
randomly dropped by map iteration or concurrency.

### Worked example

`Min=2`, `Max=10`, current count `6`; checks declared in this order:

- `cpu` (group `app`): up to 9
- `mem` (group `app`): up to 12, capped to 10
- `queue` (group `queue`): none
- `latency` (default group): down to 4

`mem` wins: within group `app`, 10 beats 9; across groups, `up` is stronger
than `none` and `down`. The action keeps mem's `Reason`
(`capped count from 12 to 10 to stay within limits`) and gains:

```
nomad_autoscaler.arbitration.participants:
  - check=cpu group=app direction=up count=9 capped=false status=preempted
  - check=queue group=queue direction=none count=6 capped=false status=preempted
  - check=latency group=(default) direction=down count=4 capped=false status=preempted
nomad_autoscaler.arbitration.participants_total: 3
```

## Consumers and compatibility boundaries

- `Reason` consumers (logs, Nomad events, UIs) see exactly the same string as
  before; the new information lives only in the two new Meta keys.
- Single-check policies gain no new Meta keys at all.
- Checks whose failure is ignored by `on_error`/`on_check_error` no longer
  participate in arbitration and are not written into the summary (they
  previously competed as an empty `none` action). The existing warning log
  for the failure is kept, and evaluation of the remaining checks continues.
- The `calculating scaling target` info log keeps its format (`policy_id`,
  `from`, `to`, `reason`, `meta`). Its `meta` field now includes the
  arbitration keys whenever two or more checks participated, so the line gets
  longer in that case; it is unchanged for single-check policies.

## Future extension: prefer lowest count on scale-down

A policy option to prefer the _lowest_ count when scaling down (instead of
the historical highest-count rule) has been considered but is **not** added.
The arbitration entry point already takes an `arbitrationOptions` parameter
so such a knob can be threaded through without reshaping the code. If the
option is confirmed, `policy/file/parse.go` and `policy/nomad` must parse and
validate it consistently, and the default must keep the current rules.

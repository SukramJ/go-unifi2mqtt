# Phase 9 adversarial review — the twelve findings, fixed

Reviewed at `3afd6ec` (PRs #21–#28). The review found **no live
defect**. One finding put an operator in a position to delete their own
entities by following a log line; nine mutations survived the suite, two
of them under the sentences that justify the phase's central decision.

Nothing on the wire moved. No golden and no digest was regenerated; the
surface invariants rebuild from the real builders and were green
throughout.

## Finding 2 — the report that advised deleting live entities

`Discovery.OrphanConfigs` appended to `unclaimed` before consulting
readiness, and the line the operator reads was unconditional: *"If no
second UniFi console shares this broker and MQTT root, clear them by
publishing an empty retained payload to each"*, with `addon/DOCS.md`
supplying the `mosquitto_pub -r -n` one-liner.

Trigger: any enabled source that fails to complete a first cycle within
`defaultReconcileTimeout` (3 min) — a console unreachable at start-up,
rotated credentials, a controller upgrade. `healthAnnounced` stays
false, so none of the ~11 site-health configs is in `Published`, and
every one was printed with the delete advice. A three-minute console
outage reported **every retained config of this daemon** as safely
deletable. `coordinator.reconcile_partial` was logged too, but the
actionable string was the one without the caveat.

`OrphanConfigs` now returns `(orphans, unclaimed, unready)`:

- `unclaimed` — not published by this process, **class reported**. The
  clearing advice, now qualified with "and the source that would have is
  up to date".
- `unready` — not published by this process, **class silent**. Logged at
  Warn as `coordinator.reconcile_unclaimed_unready`, naming the silent
  classes, saying explicitly that these are **not** safe to clear.

README and `addon/DOCS.md` say the same thing, with the two lines
distinguished.

## The mutation record

Every fix is pinned by a mutation applied to a copy of the tree. Each
was run six times; the report's own mitigation and the finding-2 pair
were run twice over, twelve of twelve.

| # | mutation | killed by | runs |
|---|----------|-----------|------|
| 2 | `unclaimed` appended before the readiness gate | `TestOrphanConfigs`; `TestASilentSourcesConfigsAreNotReportedAsSafeToClear` | 6/6; 12/12 |
| 2 | drop the `reconcile_unclaimed_unready` block | `TestASilentSourcesConfigsAreNotReportedAsSafeToClear` | 12/12 |
| 2 | drop the `silent_classes` attribute | `TestASilentSourcesConfigsAreNotReportedAsSafeToClear` | 12/12 |
| 1 | delete the whole `if len(unclaimed) > 0` block | `TestUnclaimedConfigsAreReportedAtInfoWithTheirTopics` | 12/12 |
| 1 | `log.Info` → `log.Debug` | same | 12/12 |
| 1 | drop `slog.Any("topics", …)` | same | 12/12 |
| 3 | drop `LegacyEntityTopics` from `RuntimeConfig` | `TestTheRuntimeConfigStatesTheLegacyTopicForm` | 6/6 |
| 4 | drop `device_tracker` from `publishedPlatforms` | `TestTheSweepPredicateOwnsEveryConfigThisDaemonPublishes` | 6/6 |
| 4 | drop `button` | same | 6/6 |
| 4 | add `climate` the catalogue never emits | same | 6/6 |
| 5 | widen the bypass to `if c.direct != nil` | `TestOnlyTheAvailabilityMarkerBypassesTheBreaker` | 6/6 |
| 6 | `healthAnnounced.Store(false)` in `rediscoverOnReconnect` | `TestAReconnectReopensDiscoveryWithoutUnreadyingTheSweep` | 6/6 |
| 7 | `awaitReady` timeout returns "everything ready" | `TestAwaitReadyReportsOnlyWhatReportedWhenItTimesOut` | 12/12 |
| 12 | claim the topic before the send | `TestAConfigPublishTheBrokerRefusedIsNotClaimed` | 6/6 |

Two mechanics were load-bearing and are recorded so the next reader does
not repeat them:

- `logCapture.Enabled` answers true for every level, so a
  capture-based assertion cannot see a demotion to Debug. The handler
  now records `slog.Record.Level`, and the finding-1 test checks it
  explicitly. Without that check the demotion mutant survives.
- The first draft of the `silent_classes` assertion was a substring
  check for `"site"` over the whole rendered line — which the topic
  `unifi_site_default/wan_status` satisfies on its own. It survived 0/6.
  The assertion is now on the attribute, `silent_classes=[site]`.

## Finding 12 — attempted vs. accepted

`publishConfig` recorded `p.published[topic]` **before** `p.out.Publish`
and never removed it on failure, while the documented meaning is *"did
this process ever put that topic on the broker"* and go-hamqtt states
the opposite discipline outright (*"Record what the broker accepted,
never what was merely attempted"*).

**Adopted the stronger discipline.** The reviewer is right that no
failing case exists today — a wrongly claimed topic must also leave
`Announced`, which only a *successful* publish does, and that success
has already cleared whatever was retained. But `published` is the
sweep's entire licence to delete a retained config, and the argument
that keeps a wrong entry harmless runs through three other invariants,
any one of which a later step could move. Recording on acceptance costs
one lock section and makes the claim list mean what its own comment
says. The move is strictly narrowing, so it cannot cause a deletion that
the old code would not also have caused.

Note the asymmetry that stays: `configs` is still recorded **before**
the send, deliberately, because change detection suppresses the send
while the entity remains claimed — recording it on acceptance would drop
a topic out of the announced set the moment its payload stopped
changing, and the sweep would then read it back as an orphan.

## Findings 8–11 — comments corrected, no behaviour touched

- `readyClasses`' comment described pre-#24 behaviour: with
  `CLIENTS.ENABLE` off the sweep cannot clear those configs at all,
  because none is in `Published`. Readiness now decides which of the two
  reports a config lands in, not whether a disabled source's leftovers
  get swept — nothing sweeps them.
- `plane.go` claimed all three QoS constants are read off a recorded
  transport call. `CommandQoS` is a **subscribe** QoS and no test on
  this bridge records a subscribe's QoS anywhere; the comment now says
  so and states its provenance as a decision rather than a measurement.
- `subscribeCommands`' `CheckDisjoint` does not run over "every
  published state topic at boot" — it runs before the device, stats,
  clients and health loops start, so `client/<key>/blocked`, the nearest
  neighbour of `client/+/blocked/set`, is typically absent. The static
  guarantee is the command suffix no state topic carries; the comment
  now names both.
- `watchHomeAssistant`'s `<prefix>/status` and the sweep window
  `<prefix>/#` **overlap** for the few seconds the window is open, so
  "all eight subscriptions are disjoint" stopped holding at step 5.
  Benign (`c.rediscover` is buffered-1 with drop-on-full, worst case one
  extra re-announce) and now documented at both ends.

## Deliberately not changed

- The sweep's behaviour. `#24`'s claim list, the readiness gating of
  retraction, and the five-segment form are untouched; the review
  confirmed all of it sound and re-killed five of #27's nine closed
  survivors.
- `changelog.md`. The 1.2.0 section was cut at #28 and this repo opens a
  section at release time rather than carrying an "Unreleased" heading.

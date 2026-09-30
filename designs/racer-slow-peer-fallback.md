# Known-body slow candidate fallback

The old idle-only policy allowed continuous trickle progress to consume the entire
acquisition budget. That left no time for the alternate route/candidate intended
by `/home/azureuser/design.md:29-33`. This change does not extend signed authority,
change placement, reweight nodes, or exempt any client from coverage.

Only an attempt with another opportunity and remaining attempt/eight-link failure
route credits installs a local body reserve. Reserve one original candidate share,
capped at half the time remaining after that initial share. This retains an
extension interval even for the two-opportunity subscription/fixed-page path.
The reserve is an opportunity, not a guarantee: cancellation must still wait for
accepted I/O completion, and subsequent candidates can fail too.

For a known-length HTTP peer body, observe from the first received bytes. After
one share of actual body observation, compare remaining bytes at the average
post-first-byte rate against time before the fallback reserve. If completion
would cross the reserve, return a local deadline failure. An incomplete body also
stops at the reserve boundary. No absolute bytes/second or RTT threshold is used.
Predictions are conservative heuristics, not a guarantee of future bandwidth.

Checkout, admission, headers and other pre-body waiting do not enter the rate
estimate. First/same-time samples do not invent a rate. Unknown-length operations
and attempts without an affordable alternate retain idle/hard policy. Completion
clears active body tracking before verification; it does not renew any authority.
Native transfers and opaque relay streaming do not gain a new estimator here;
the final HTTP body receiver owns this policy. Existing signed deadlines, pinned
versions, link/attempt debits and completion fences remain unchanged.

The existing trickle-to-hard-ceiling test remains valid without an installed
alternative reserve. New tests explicitly install the reserve and assert the
changed contract: healthy progress beyond the initial share, early slow-body
failure, and fenced ranked fallback without budget refunds or deadline renewal.
No existing assertion is relaxed to turn a failing test green.

## Focused validation

On September 30, 2026, bounded Rust test filters passed: `candidate` (27 tests),
`body_progress` (3 tests), and `runtime::deadline::tests` (5 tests, overlapping
three policy tests from the candidate filter). The new HTTP reserve regression
failed on `reserved_slow` with the transport temporarily restored to idle-only
progress, then passed after restoring known-body progress. No tests were removed.
Scoped rustfmt and git diff whitespace checks passed. Required `make fmt` was
attempted; the installed Go 1.26-built linter panicked on Go 1.27 source, leaving
no Go changes. No production deployment or throughput claim accompanies this fix.

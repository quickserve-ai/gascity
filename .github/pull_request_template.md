freeze: a|b|c|d — <why>

<!-- Fork freeze (ga-gsr09s, armed 2026-09-25). Keep exactly one letter on the
line above and say why. A PR that cannot name one is deferred, not merged.
  a  outage, data-loss or security fix (an upstream cherry-pick of a FIX counts; say so)
  b  takes REVIEWS out of the Gas City ecosystem (review v2 and its feeder)
  c  stands up SEATS outside Gas City
  d  operational carry a frozen version needs to keep serving (existing pin/pack/config plumbing)
No new platform features and no upstream pulls. -->

<!-- Link an issue when there is one (`Closes #123`); it is recommended for
user-visible bugs and changes worth discussing, and optional for small,
self-explanatory fixes. Either way, say why the change is needed below. See
CONTRIBUTING.md. -->

Closes #

## What changed

-

## Evidence it works

<!-- End-to-end evidence: commands run and their output, a scenario you
exercised, screenshots. "Unit tests pass" alone is not enough. -->

## Checklist

<!-- CI gates on Bazel; these make targets run the same `bazel test`
commands (TESTING.md "Building and testing"). Plain `go test` is not a
substitute. -->

- [ ] `make check` (`bazel test //...` plus shell guards)
- [ ] `make bazel-sync` and committed the result if imports, packages, or files changed
- [ ] `make check-docs` (`bazel test //test/docsync:docsync_test`) if docs, navigation, or links changed
  > **Note:** `docs/` is authored for [docs.gascityhall.com](https://docs.gascityhall.com) (Mintlify), not for direct GitHub viewing. Use extensionless page links (e.g. `/tutorials/01-beads`, not `/tutorials/01-beads.md`). If something looks broken on GitHub but works on the live site, that's intentional.
- [ ] `make test-acceptance` (`bazel test --config=acceptance //test/acceptance:acceptance_test`) if `gc` command behavior changed
- [ ] `make test-integration` (`bazel test --config=integration ...`) if runtime, controller, or workflow behavior changed
- [ ] Added or updated tests for behavior changes
- [ ] Updated docs for user-facing changes
- [ ] Updated the owning `AGENTS.md` if an invariant or boundary changed
- [ ] Called out breaking changes or migration notes

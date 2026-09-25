freeze: a|b|c|d — <why>

<!-- Fork freeze (ga-gsr09s, armed 2026-09-25). Keep exactly one letter on the
line above and say why. A PR that cannot name one is deferred, not merged.
  a  outage, data-loss or security fix (an upstream cherry-pick of a FIX counts; say so)
  b  takes REVIEWS out of the Gas City ecosystem (review v2 and its feeder)
  c  stands up SEATS outside Gas City
  d  operational carry a frozen version needs to keep serving (existing pin/pack/config plumbing)
No new platform features and no upstream pulls. -->

## Summary

- Explain the change and why it is needed.

## Testing

- [ ] `make check`
- [ ] `make check-docs` if docs, navigation, or links changed
  > **Note:** `docs/` is authored for [docs.gascityhall.com](https://docs.gascityhall.com) (Mintlify), not for direct GitHub viewing. Use extensionless page links (e.g. `/tutorials/01-beads`, not `/tutorials/01-beads.md`). If something looks broken on GitHub but works on the live site, that's intentional.
- [ ] `make test-integration` if runtime, controller, or workflow behavior changed

## Checklist

- [ ] Linked an issue, or explained why one is not needed
- [ ] Added or updated tests for behavior changes
- [ ] Updated docs for user-facing changes
- [ ] Called out breaking changes or migration notes

## Summary
<!-- One-line description of what this PR does -->

## Related Issue
<!-- Fixes #ISI-XXXX / Epic X / Story X.X -->
<!-- If no ticket, explain why -->

## Motivation / Why
<!-- Why is this change needed? What problem does it solve? -->

## What Changed
<!-- Detailed breakdown of changes -->
### Added
-
### Changed
-
### Deprecated
-
### Removed
-
### Fixed
-
### Security Impact
<!-- Any security implications? New attack surface? Auth/RBAC changes? -->

## Breaking Changes
- [ ] No breaking changes
- [ ] Breaking changes (describe below)
<!-- If breaking: migration guide + version bump -->

## How Was This Tested?
- [ ] Unit tests added/updated
- [ ] Integration tests pass
- [ ] Manual testing (describe below)
- [ ] Chaos/concurrency tests (if coordination/sandbox change)
<!-- Test environment: KSquad version, K8s version, runtime -->

### Test Results
<!-- Paste relevant test output, benchmarks, or screenshots -->

## Reviewer Focus
<!-- What should reviewers pay special attention to? (concurrency, security, performance, edge cases) -->

## Checklist
### Code Quality
- [ ] Code follows project conventions (golangci-lint passes)
- [ ] Self-reviewed my own code
- [ ] Commented complex/hard-to-understand sections
- [ ] No debug logging left in

### Security & Invariants
- [ ] No secrets/credentials in code (Secret refs only)
- [ ] No P2P coordination paths introduced (§6 no-P2P invariant)
- [ ] New endpoints go through RBAC deny-by-default middleware (§12.3)
- [ ] Agent identity propagation intact (initiatedByUserId, §12.4)

### CRD Changes (if applicable)
- [ ] DeepCopy methods regenerated
- [ ] CEL/webhook validation added
- [ ] ksquad.io/created-by annotation preserved

### Testing
- [ ] Unit tests cover happy path + edge cases
- [ ] Tests pass locally (`make test`)
- [ ] No flaky tests introduced

### Documentation
- [ ] Inline docs (GoDoc) updated
- [ ] README updated (if feature/change is user-visible)
- [ ] Docs site updated (if applicable)
- [ ] Changelog updated

### Commit Hygiene
- [ ] DCO signed (all commits)
- [ ] Conventional commit messages
- [ ] Commits are focused (no unrelated changes)
- [ ] No merge commits (rebase)

## Screenshots / Recordings (if console/UI change)
<!-- Before/after screenshots or screen recordings -->

## Post-Merge
<!-- Any follow-up needed? (deploy steps, config migration, announcement) -->

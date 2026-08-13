# Developer Certificate of Origin

KSquad requires every contribution to be signed off under the
**Developer Certificate of Origin (DCO)**, version 1.1. The DCO is a
lightweight way for contributors to certify that they wrote, or otherwise
have the right to submit, the code they are contributing. It is the same
mechanism used by the Linux kernel and by CNCF projects.

## How to sign off

Add a `Signed-off-by` trailer to every commit message, matching the name and
email in your Git author identity:

    Signed-off-by: Jane Developer <jane@example.com>

Git adds this automatically when you commit with the `-s` flag:

    git commit -s -m "feat: add squad reconciler"

To sign off a commit you already made:

    git commit --amend -s --no-edit

To sign off a range of existing commits before opening a PR:

    git rebase --signoff <base>

By adding the sign-off, you certify the statement below.

## Enforcement

The DCO check runs on every pull request (`.github/workflows/dco.yml`) and is a
**required status check** for merge. A PR whose commits are not all signed off
will fail the check; fix it by amending/rebasing with `--signoff` and
force-pushing your branch.

---

## Developer Certificate of Origin 1.1

    Developer Certificate of Origin
    Version 1.1

    Copyright (C) 2004, 2006 The Linux Foundation and its contributors.
    1 Letterman Drive
    Suite D4700
    San Francisco, CA, 94129

    Everyone is permitted to copy and distribute verbatim copies of this
    license document, but changing it is not allowed.


    Developer's Certificate of Origin 1.1

    By making a contribution to this project, I certify that:

    (a) The contribution was created in whole or in part by me and I
        have the right to submit it under the open source license
        indicated in the file; or

    (b) The contribution is based upon previous work that, to the best
        of my knowledge, is covered under an appropriate open source
        license and I have the right under that license to submit that
        work with modifications, whether created in whole or in part
        by me, under the same open source license (unless I am
        permitted to submit under a different license), as indicated
        in the file; or

    (c) The contribution was provided directly to me by some other
        person who certified (a), (b) or (c) and I have not modified
        it.

    (d) I understand and agree that this project and the contribution
        are public and that a record of the contribution (including all
        personal information I submit with it, including my sign-off) is
        maintained indefinitely and may be redistributed consistent with
        this project or the open source license(s) involved.

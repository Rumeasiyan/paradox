# Security policy

PARADOX wakes systems and runs their jobs with signed calls, so a flaw in it can reach every system it serves. Please report security problems privately.

## Reporting a vulnerability

- Use GitHub's private reporting: the **Security** tab of this repository, then **Report a vulnerability**.
- Do not open a public issue, pull request or discussion for a security problem.
- Include what you found, how to reproduce it, the version or commit, and what an attacker could do with it.

You will get an acknowledgement within 7 days and an assessment within 30 days. Fixes are released as a new version, and the advisory is published once a fix is available. You will be credited unless you ask not to be.

## Supported versions

Only the latest release receives security fixes.

## In scope

- Signature checks on calls into PARADOX and on calls PARADOX makes to systems (forged, replayed or redirected calls).
- A system queueing, reading or cancelling another system's jobs.
- Job data or keys leaking into logs, run records or error messages.
- Anything that lets a caller reach PARADOX beyond the queue path.

## Out of scope

- Weaknesses in a particular deployment's server, proxy, database or key storage. Report those to whoever runs that deployment.
- Problems in third-party software PARADOX depends on (River, PostgreSQL, Sablier). Report those upstream; tell us too if PARADOX needs a change.

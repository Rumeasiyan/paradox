# Contributing to PARADOX

Thank you for helping. PARADOX keeps time for other systems, so changes are judged first on whether they keep jobs on time, safe and recorded.

## Before you start

- **Bugs:** open an issue with what you expected, what happened, and how to reproduce it.
- **Security problems:** do not open an issue. Follow [SECURITY.md](SECURITY.md).
- **New features or behaviour changes:** open an issue first so we can agree on the change before you write it. The product requirements are in [docs/prd.md](docs/prd.md); a change that adds or alters a requirement updates that file in the same pull request.
- **Contract changes:** the schedule file format, the queue request and the signing scheme are a published, versioned contract. A change that breaks them needs a new major version (PX-CON-02), so raise it in an issue before any code.

## Sign your commits (DCO)

Every commit must carry a sign-off line certifying the [Developer Certificate of Origin](https://developercertificate.org/): that you wrote the change, or otherwise have the right to submit it under this project's licence.

```
Signed-off-by: Your Name <you@example.com>
```

`git commit -s` adds it for you. Use your real name and an email you can be reached at. Pull requests with unsigned commits are not merged; to fix older commits, run `git rebase --signoff main` and force-push your branch.

By contributing, you agree that your contribution is licensed under the [Apache License 2.0](LICENSE), as section 5 of that licence states.

## Pull requests

- Keep each pull request to one change.
- Run `gofmt -l .`, `go vet ./... ./sign/...` and `go test ./... ./sign/...` before you push; all must pass.
- Add or update tests for any behaviour you change.
- Write in plain British English in docs and messages.
- No site-specific values (server paths, hostnames, keys) in code, tests or docs. Use settings and examples instead.

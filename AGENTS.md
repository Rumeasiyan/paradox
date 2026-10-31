# AGENTS.md

Guide for AI agents and new contributors. Read it before changing anything. `CLAUDE.md` imports this file; Codex reads it directly.

## What this project is

PARADOX is a scheduler and job queue for systems that sleep when idle (scaled to zero by Sablier). Systems declare timed jobs in a schedule file or queue one-off jobs; PARADOX wakes the system, calls its job address with a signed request, retries failures, reports what still fails to a push monitor, and records every run. It is a public, generic Apache-2.0 project: one deployment runs it for several systems on a shared Docker server, and its real settings live in that deployment's own private repository, never here.

Status: design is frozen (PRD v1.2); the build follows `docs/backlog.md`.

## Where to look

| Path | When you need it |
|---|---|
| `docs/prd.md` | What PARADOX must do. Every behaviour has an ID (`PX-<AREA>-<NN>`); cite IDs in issues, commits and code comments where a rule comes from. |
| `docs/contract.md` | The published contract: systems folder, schedule file, keys, signing, calls to systems, queue API, settings. A test keeps its settings table equal to the code's defaults. |
| `docs/testsystem.md` | Settings and answers of `paradox testsystem`, the stand-in system shipped in the image. |
| `docs/how-it-works.md` | The two main flows (scheduled job, queued job) step by step, the draft schedule file, who holds which key. |
| `docs/diagrams/paradox-architecture.dot` | Reference architecture (PRD Annex B). Link numbers must match the Annex B table. |
| `docs/diagrams/build_erd.py` | The data model (PRD Annex A): entities and links in one script that draws `paradox-erd.svg` and writes the PRD data dictionary. Edit the script, never the generated files. |
| `docs/backlog.md` | Build order: numbered stories with Run mode (Solo, Wave, With you), citations and Needs. Build top to bottom; tick Done in the same commit. |
| `docs/decisions.md` | Why things are the way they are. Read before reversing anything. |
| `CHANGELOG.md`, `VERSION` | Release history and the one version number. |
| `CONTRIBUTING.md`, `SECURITY.md` | Contribution rules (DCO) and private vulnerability reporting. |
| `sign/` | The signing helper, a separate Go module with its own version, imported by systems. Standard library only. Its signature base is pinned by `TestSignatureBaseIsStable`: if that test must change, the contract has a breaking change. |
| `dev/` | Local stack for development and tests only; its passwords are throwaway. |
| `brand/` | Logo files and the prompt that generated them (`brand/prompt.txt`). |

## Constraints

| Rule | Why | Where it is stated |
|---|---|---|
| **This repository is public. Never commit, or write in an issue, PR or commit message: hostnames, domains, server paths, internal URLs, keys, env files, job payloads, personal data, or links to private repositories.** Use settings and neutral examples (`citadel`, `example.com`). | Everything here is readable by anyone, and git history and issue edits cannot be fully taken back. | PX-NFR-07, `CONTRIBUTING.md` |
| Every deployment-specific value is a setting (env var or the systems folder), with a generic default. | PARADOX must run unchanged for any operator; site values belong to the deployment repository. | PX-NFR-07, decisions 2026-10-31 |
| Private keys never enter the repo or logs. `.gitignore` blocks `.env`, `.env.*`, `*.key`, `*.pem`; test keys are generated in the test, not stored. | A leaked PARADOX private key lets anyone trigger jobs on every system. | PX-NFR-05, `.gitignore` |
| Job data (payloads) is never logged and is deleted when the job ends. | Payloads may carry personal data. | PX-NFR-06, PX-REC-02 |
| The schedule file format, the queue API and the signing scheme are a published contract other repositories build against. A breaking change needs a new major contract version and an issue first. | Systems and deployments break silently if the contract shifts under them. | PX-CON-01, PX-CON-02 |
| Signatures cover method, path, time, direction and body; checks refuse wrong direction, wrong system, or older than the window. Do not weaken a check to make a test pass. | These checks are the only thing stopping forged or replayed job calls. | PX-SIG-01 to PX-SIG-05 |
| Times are stored in UTC; the schedule timezone is a setting. | Schedules are written in local time but must run correctly across restarts and daylight-saving changes. | PX-SCH-03 |
| PARADOX reads only its one read-only systems folder. | The deployment mounts only that folder; reading elsewhere fails in production. | PX-SCH-01 |
| Fits in 250 MB of memory and 1 CPU, read-only root file system, non-root user. | That is the box it is deployed in. | PX-NFR-01, PX-NFR-05 |

## Commands

| Task | Command |
|---|---|
| Re-render the architecture diagram after editing the `.dot` | `dot -Tsvg docs/diagrams/paradox-architecture.dot -o docs/diagrams/paradox-architecture.svg` |
| Redraw the data model and rewrite the PRD dictionary | `python3 docs/diagrams/build_erd.py` |
| Read the version | `cat VERSION` |
| Go checks (Go 1.27; `go.work` joins the service and the `sign` module) | `gofmt -l .` (must print nothing), `go vet ./... ./sign/...`, `go test ./... ./sign/...` |
| Run the command | `go run ./cmd/paradox -version` |
| Local database for tests (PostgreSQL + PgBouncer, session pool of 2, non-superuser role) | `docker compose -f dev/compose.yaml up -d`, then `export PARADOX_TEST_DATABASE_URL=postgres://paradox:paradox@localhost:6432/paradox`. Database tests skip without it; run them with `go test -p 1 ./...` (they share one database) before marking a database story done. |

## Conventions

- **Commits:** `Area: summary` (for example `Docs: ...`, `Legal: ...`), body explains why. Signed off with `git commit -s` (DCO). Reference the issue (`Refs #N` or `Closes #N`). Commits carry only the human author: no AI co-author trailers or "generated with" lines.
- **Branching:** work lands directly on `main` and is pushed. Outside contributors use pull requests (`CONTRIBUTING.md`).
- **Writing:** plain British English, short sentences, they/them for people. No em dashes anywhere; use a comma, colon, full stop or parentheses.
- **Docs move with behaviour:** a change to behaviour updates the PRD row and, if a flow changes, `docs/how-it-works.md` in the same commit. A changed link in the architecture updates the `.dot`, the `.svg` and the Annex B table together. A data model change goes through `build_erd.py`.
- **Diagrams:** Graphviz `.dot` for architecture, Mermaid in Markdown for flows (GitHub renders it). In Mermaid labels avoid double quotes, apostrophes and semicolons; they break the parser.

## Versioning

- **Source of truth:** `VERSION`. Nothing else holds a hand-written version: `version.go` embeds the file (`paradox.Version`), and the image tag must read it too.
- **When:** bump with every completed change that alters behaviour, in the same commit, and add a `CHANGELOG.md` entry.

| Change | Bump | Example |
|---|---|---|
| Breaking (contract or behaviour users rely on) | MAJOR, reset others | `1.4.2` to `2.0.0` |
| Backward-compatible feature | MINOR, reset PATCH | `1.4.2` to `1.5.0` |
| Bug fix, hotfix, non-breaking security fix | PATCH | `1.4.2` to `1.4.3` |
| Docs, tests, refactor, formatting, CI with no behaviour change | none | |

Several changes in one commit take the highest bump. Never add a fourth part (`1.4.2.1`). There is no separate build number: images are tagged `v<VERSION>`. Do not push a tag, publish an image or release unless the owner asked for it.

The contract and signing helper are imported by other repositories and will carry their own version, set up in #2.

## Working agreements

**Anything raised only in conversation is lost.** An open question, a deferred fix, a found bug, a risky assumption, or a TODO you are about to write into code becomes a GitHub issue the moment it is found, not in a closing summary.

**Issues are the task tracker.** Use the issue form. Each issue must stand alone for a reader who saw none of the conversation: what it is, why it matters (what breaks if ignored), where it surfaced (paths, PRD IDs), and for decisions the realistic options with a recommendation and its reasoning. Never "as discussed". Assign to `Rumeasiyan`. Remember the public rule above: describe server matters generically.

**Too small for an issue:** typo and wording fixes, formatting, re-rendering a diagram with no content change.

**Decisions** that someone could later ask "why is it like this?" about go into `docs/decisions.md` (newest first) in the same commit as the change.

## Workflow

1. Take the next unticked story in `docs/backlog.md` (or the task you were given). Find its issue, or open one with the form; assign it to `Rumeasiyan` and label it.
2. Read the PRD rows it touches and any related entry in `docs/decisions.md`.
3. If the work needs a choice that belongs to the owner, ask and record the answer in the issue before building.
4. Make the change with tests. Update the PRD, `how-it-works.md` and diagrams if behaviour or architecture changed.
5. Decide the version bump from the table; update `VERSION` and `CHANGELOG.md` if it is not "none".
6. Add a decision log entry if the change settles a "why".
7. Re-read the diff for anything that breaks the public rule.
8. Commit (`git commit -s`, `Area: summary`, `Closes #N`) and push to `main`.
9. Comment on the issue: what was built, what was verified and how, the resulting version, and anything deferred with a link to its new issue. Close it.

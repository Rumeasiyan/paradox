# Decision log

Newest first. Append only: a reversed decision gets a new entry that points to the old one; old entries are not edited.

**What belongs here:** anything a competent person would later ask "why is it like this?" about: architecture, dependencies, contract shape, resolved open questions, reversals. Not routine implementation choices. The **Why** should be longer than the **Decision**: the what is visible in the code, the why is not. This file is public: describe deployments generically.

Format: date, title, **Decision**, **Why** (including what was rejected and the trade-off accepted), **Consequences**, **Refs**.

---

## 2026-10-31: Data model: six tables, keys in files, River tables outside the model

**Decision:** PARADOX's own data is six tables: `system`, `job`, `queue_request`, `run`, `attempt`, `run_action` (PRD Annex A). A `job` is a named job of a system; a `run` is one execution of it, and the ID the queue API returns is `run.id`. Public keys are not stored in the database; only the fingerprint of the key that signed each queue request is. River's own tables are working state and are not part of the model; each River job carries the `run_id` it executes. A system that leaves the systems folder keeps its row with `removed_at`.

**Why:** Storing public keys as well as reading them from `system.pub` would create two sources that drift; the fingerprint is enough for audit. River deletes finished jobs on its own schedule, so it cannot be the 90-day record or answer a status query; `run` can. The PRD used "job ID" for a queued item and "run" for its record, which would confuse the API and the tables; one execution is now always a `run`, and the contract (#2) names the API field. Deleting a removed system's row would orphan up to 90 days of its runs. Rejected: one table per River job (couples the record to River's internals).

**Consequences:** Retention: `run`, `attempt`, `run_action` 90 days (default); `queue_request` 24 hours. A data model change edits `docs/diagrams/build_erd.py` and re-runs it, which redraws the diagram and rewrites the dictionary in the PRD.

**Refs:** `docs/prd.md` Annex A, `docs/diagrams/build_erd.py`, #2, #4

## 2026-10-31: Version starts at 0.1.0 and bumps with every completed change

**Decision:** One `VERSION` file is the only version source, starting at `0.1.0`. Every completed change that alters behaviour bumps it by semantic versioning in the same commit, with a `CHANGELOG.md` entry. No separate build number; images are tagged `v<VERSION>`.

**Why:** The owner wants `main` to always say exactly what it contains. Rejected: bumping only when preparing a release (fewer bumps, but `main` would show an old number while carrying new features). Rejected: starting at `0.0.0` (more honest while nothing runs, but the owner preferred to start at the planned first version). Trade-off accepted: many small versions, and the first published image will carry whatever version `main` has reached, not necessarily `0.1.0`.

**Consequences:** Each behaviour change carries a version decision. The Go build and image workflow must read `VERSION`, not hold their own number. The contract and signing helper get their own version stream because other repositories import them.

**Refs:** `VERSION`, `AGENTS.md` (Versioning)

## 2026-10-31: GitHub issues are the task tracker

**Decision:** Every unit of work, open question, found bug and deferred fix is a self-contained GitHub issue, assigned to the owner, referenced in commits and closed with a comment on what was built and verified.

**Why:** Decisions and open questions were otherwise living in chat transcripts that no later reader sees. Rejected: issues for bugs and questions only (lighter, but planned work would have no trail). Trade-off: more issues to keep tidy; typo-level changes are exempt.

**Consequences:** Issues are public, so they follow the public-repo rule in `AGENTS.md`.

**Refs:** `AGENTS.md` (Working agreements), `.github/ISSUE_TEMPLATE/`

## 2026-10-31: Per-system folder for schedule files and public keys

**Decision:** PARADOX reads one read-only folder (a setting) with one subfolder per system, holding `schedules.yaml` and `system.pub`. `schedules.yaml` also sets the system's wake mode (`sablier` or `none`) and an optional address. `system.pub` may hold two keys during a key change.

**Why:** Agreed with the reference deployment's maintainers, who copy each system's files at deploy. Keeping a system's timetable and its key side by side means adding or removing a system is one folder. Rejected: public keys in environment variables (an env var per system, easy to get out of step with the schedule files). Rejected: a separate `wake` file (one more file to keep in step; wake mode is a property of the system, so it sits with its schedule). The key file is named `system.pub`, not after PARADOX, because it holds the system's key.

**Consequences:** PRD v1.1 changed PX-SCH-01, PX-SYS-01, PX-SYS-03, PX-SYS-04 and Annex B links 1 to 3. PARADOX listens on 8080.

**Refs:** `docs/prd.md`, `docs/how-it-works.md`

## 2026-10-31: Apache-2.0, owned by the author, DCO for contributions

**Decision:** Licence Apache-2.0, copyright held personally by the author (see `NOTICE`). Contributors sign off commits under the Developer Certificate of Origin.

**Why:** Apache-2.0 is the norm for Go infrastructure, includes a patent grant, and lets any system import the signing helper without licence friction. Rejected: MIT (no patent grant). Rejected: AGPL-3.0 (would have forced the helper into a separate licence and deters adopters). DCO over a CLA: far lower friction for contributors; the accepted trade-off is that relicensing contributed code later needs each contributor's consent.

**Consequences:** `LICENSE` is the unmodified official text; the copyright line lives in `NOTICE`. Security reports go through GitHub private vulnerability reporting.

**Refs:** `LICENSE`, `NOTICE`, `CONTRIBUTING.md`, `SECURITY.md`

## 2026-10-31: Public, generic project; deployment details live elsewhere

**Decision:** The repository is public. Every deployment-specific value (systems folder path, timezone, waker URL, monitor URLs, addresses, keys) is a setting with a generic default. The real values, the server wiring and the original internal spec live in the deployment's private repository.

**Why:** PARADOX is useful beyond one server: it is a self-hosted equivalent of a cloud scheduler calling scale-to-zero services. Publishing it is only safe if nothing in it describes a real server. Rejected: a private, site-specific service (simpler, never shareable). The owner chose to go public before any code, accepting that design history is visible from the start.

**Consequences:** The public-repo rule in `AGENTS.md`. The original spec issue was moved out of this repository before it went public.

**Refs:** `docs/prd.md` (Overview, PX-NFR-07)

## 2026-10-31: Signing with Ed25519 key pairs, in both directions

**Decision:** Every call from PARADOX to a system, and every queue request from a system to PARADOX, is signed with the sender's Ed25519 private key over method, path, time, direction and body, and checked with the sender's public key. Signatures older than 5 minutes (default) or for the wrong direction are refused; a request ID stops repeats; a system may queue only its own jobs. Format based on HTTP Message Signatures (RFC 9421).

**Why:** The first spec used a per-system shared secret (HMAC) for outgoing calls only. Two facts changed that: queue requests into PARADOX also need authenticating, and some systems may run on other hosting. With shared secrets, PARADOX would hold every system's secret (one leak exposes all systems, both ways), and each outside team would need a secret handed over safely. With key pairs, private keys never leave their owner, only public keys are exchanged, and a key change is one-sided. Rejected: per-system bearer tokens (a leaked token works until changed), network trust only (any compromised container could queue any job), mTLS (needs a certificate authority and renewals for every system). Accepted: signing is slightly slower than HMAC, still far above PARADOX's load.

**Consequences:** The deployment stores PARADOX's private key and systems' public keys, no shared secrets. Calls across the internet use HTTPS, and only the queue path is exposed publicly. Systems not on the server are never woken.

**Refs:** PX-SIG-01 to PX-SIG-06, PX-SYS-02 to PX-SYS-04, `docs/how-it-works.md`

## 2026-10-31: No screen; operator works from the command line

**Decision:** PARADOX has no web interface. The operator lists runs and retries or cancels jobs with a command-line tool inside the container.

**Why:** A screen would need its own login and attack surface on a service whose job is background timing. Rejected: an htmx dashboard (nice to have, but more code and a login to protect). Rejected: no tooling at all (the operator would have to query the database by hand).

**Consequences:** PX-REC-03 and PX-REC-04 cover the command line.

**Refs:** `docs/prd.md` 3.7

## 2026-10-30: Go service on River, waking through Sablier, alerts through a push monitor

**Decision:** PARADOX is one always-on Go service using River (PostgreSQL-backed job queue) for the timetable and queue, asking Sablier to wake stopped containers before calling them, and reporting failures and a heartbeat to a push monitor. It never messages people directly.

**Why:** Systems on the target server sleep to save memory, and a sleeping system cannot keep time. Keeping each system awake for its own timers would defeat that. River keeps all state in PostgreSQL, which the server already runs and backs up, so no extra queue server is needed. Alerts go through the monitor so there is one alert path, which keeps working when the notification systems are themselves asleep or broken.

**Consequences:** River relies on LISTEN/NOTIFY and session state, which PgBouncer's transaction mode breaks. Plan: a session-mode pool of 2 connections; still to be confirmed against River's documentation, with polling through the transaction pool as the fallback.

**Refs:** `docs/prd.md` 5 (Technology stack), Annex B

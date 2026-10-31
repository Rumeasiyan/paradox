# PARADOX backlog

Source: [PRD](prd.md) v1.1 (requirement IDs `PX-...`, Annex A data model, Annex B links `AB n`) and [how it works](how-it-works.md) (flow steps `HW A1` to `HW A15`, `HW B1` to `HW B5`). Stories cite these; read them for the detail.

## Definition of done (every story)

- Tests cover every rule the story touches, including the refusal and failure paths, and `gofmt -l .`, `go vet ./...` and `go test ./...` pass.
- The cited flow steps were walked locally (see How to build) and behave as the PRD and the guide say.
- No site values, keys or job data in code, tests, logs or messages; test keys are generated in the test.
- Every new setting has a generic default and is listed in the contract docs.
- If behaviour changed: PRD row, `how-it-works.md` and diagrams updated (data model through `docs/diagrams/build_erd.py`).
- `VERSION` and `CHANGELOG.md` updated per `AGENTS.md`; decision log entry if a "why" was settled.
- Committed with `git commit -s` referencing the issue; the issue gets a comment on what was built and verified.

## How to build

- **Solo:** the main session builds it alone. All of R1 is Solo because it sets the patterns everything copies (module layout, settings, signing, tests).
- **Wave n:** the stories sharing a wave need nothing from each other. Build them at the same time, one subagent per story in its own git worktree, at most five at once; subagents get the public-repo rule and the story's citations. The main session reviews, merges, runs all tests and ticks Done before the next row.
- **With you:** needs the owner (publishing a tag or image, server access, keys). The agent prepares everything, then stops and asks.
- **Verify before Done:** PARADOX has no screen, so the walk-through is a local run: start the local stack, drive the cited flow steps with the test system (story 15) and signed requests, and check the answers, the run records and the logs against the PRD and the guide. Tick Done only when tests pass and the walk matches; otherwise fix and walk again.

Rows are in build order: going down the list is always valid.

## Releases

| Release | Goal |
|---|---|
| R1 Contract | The published contract and the signing helper, tagged, so deployments and systems can build against them. |
| R2 Scheduled job | A scheduled job wakes a stand-in system, runs, retries, reports and is recorded, all on this machine. |
| R3 Queued job | A system queues its own job with a signed request; refusals, repeats, status and cancel work. |
| R4 Operation | Missed jobs, limits, shutdown, retention, the operator command line, and the capacity and security checks. |
| R5 Go live | Real Sablier and monitor adapters, the image, and the checks on the server. |

## R1 Contract

Tracked in #2.

| # | Done | Run | ID | Story | Cites | Pri | Needs |
|---|---|---|---|---|---|---|---|
| 1 | [x] | Solo | E-ENB-01 | Go module skeleton, `paradox -version` read from `VERSION` | PX-NFR-07 | Must | |
| 2 | [x] | Solo | E-SYS-01 | Settings from the environment with generic defaults | PX-NFR-07, PX-SYS-04 | Must | 1 |
| 3 | [x] | Solo | E-SCH-01 | Schedule file parser with file, line and fault in errors | PX-SCH-02, PX-SCH-04, PX-SYS-03, PX-SYS-04, PX-CON-01 | Must | 1 |
| 4 | [x] | Solo | E-SIG-01 | Signing helper module: sign and verify in both directions | PX-SIG-01, PX-SIG-02, PX-SIG-03, PX-SIG-04, PX-SIG-06, PX-CON-03 | Must | 1 |
| 5 | [x] | Solo | E-SYS-02 | Systems folder reader: schedule file and public keys per system | PX-SCH-01, PX-SYS-01, PX-SYS-02, PX-SYS-05, PX-SIG-06 | Must | 2, 3, 4 |
| 6 | [x] | Solo | E-CON-01 | Key generator command | PX-CON-04 | Must | 4 |
| 7 | [x] | Solo | E-CON-02 | Contract docs: schedule format, queue API, signing scheme, settings | PX-CON-01, PX-CON-02 | Must | 2, 3, 4 |
| 8 | [x] | With you | E-CON-03 | Tag the first contract version | PX-CON-01, PX-CON-02 | Must | 5, 6, 7 |

Notes:
- **E-ENB-01:** no CI yet (CI is a third-party service; it arrives in story 35).
- **E-SIG-01:** its own Go module in its own folder, standard library only, with its own version tags, because systems import it (decision log, Version entry). Covers the queue request format of PX-JOB-01 only as far as signing needs.
- **E-CON-02:** settle the API field name for the run ID (decision log, Data model entry) and the queue answers listed in #2.
- **E-CON-03:** pushing the tag publishes it. After the owner's OK, tell the deployment's maintainers the tag.

## R2 Scheduled job

Tracked in #4; story 9 answers #3.

| # | Done | Run | ID | Story | Cites | Pri | Needs |
|---|---|---|---|---|---|---|---|
| 9 | [ ] | Solo | E-ENB-02 | Local database stack with PgBouncer in session mode; River connected | AB 5, AB 6, PX-NFR-02 | Must | 1 |
| 10 | [ ] | Solo | E-REC-01 | Database schema and migrations from Annex A | PX-REC-01 | Must | 9 |
| 11 | [ ] | Wave 1 | E-SCH-02 | Load the timetable on start, times in UTC | PX-SCH-03, PX-SYS-05; HW A1 to A3 | Must | 5, 10 |
| 12 | [ ] | Wave 1 | E-RUN-01 | Waker interface with a local stand-in | PX-RUN-01, PX-RUN-02; AB 7 | Must | 1 |
| 13 | [ ] | Wave 1 | E-REP-01 | Reporter interface with a log stand-in; heartbeat | PX-REP-02, PX-REP-03; AB 10 | Must | 1 |
| 14 | [ ] | Wave 1 | E-REP-02 | Health check tied to the database | PX-REP-04 | Must | 10 |
| 15 | [ ] | Wave 1 | E-ENB-03 | Test system command `paradox testsystem` that answers `/jobs/<name>` | HW A8, HW A9 | Must | 4 |
| 16 | [ ] | Solo | E-RUN-02 | Runner: due job, run, wake, signed call, time limit, record | PX-RUN-01, PX-RUN-02, PX-RUN-03, PX-REC-01; HW A4 to A12 | Must | 11, 12, 15 |
| 17 | [ ] | Solo | E-REP-03 | Retries with backoff, then one failure report | PX-REP-01, PX-REP-02; HW A13 to A15 | Must | 13, 16 |

Notes:
- **E-ENB-02:** if River's documentation rules out session mode, switch to polling, record it in the decision log and tell the deployment's maintainers.
- **E-RUN-01:** the stand-in can be told to answer ready, answer late, or time out, so PX-RUN-01's limit is testable.
- **E-ENB-03:** a subcommand of the same binary, so a deployment can run the PARADOX image as its sleeping test target. It checks PARADOX's signature with the helper (answers 401 if wrong) and can be set per job to succeed, fail or answer too slowly.
- **E-RUN-02:** each River job carries only the `run_id` (Annex A); job data stays on the run.

## R3 Queued job

Tracked in #4.

| # | Done | Run | ID | Story | Cites | Pri | Needs |
|---|---|---|---|---|---|---|---|
| 18 | [ ] | Solo | E-JOB-01 | Queue API: accept a system's own job, data and start-time limits | PX-JOB-01, PX-JOB-02; HW B1, HW B4, HW B5 | Must | 7, 16 |
| 19 | [ ] | Solo | E-SIG-02 | Signature and ownership checks on every queue request | PX-SIG-02, PX-SIG-03, PX-SIG-04, PX-SIG-05, PX-NFR-06; HW B2, HW B3 | Must | 18 |
| 20 | [ ] | Wave 2 | E-JOB-02 | Repeated request ID returns the first run | PX-JOB-03 | Must | 19 |
| 21 | [ ] | Wave 2 | E-JOB-03 | Job status for the owning system | PX-JOB-04 | Must | 19 |
| 22 | [ ] | Wave 2 | E-JOB-04 | Cancel a queued job | PX-JOB-05 | Must | 19 |
| 23 | [ ] | Wave 2 | E-JOB-05 | Queued jobs survive a restart | PX-JOB-06 | Must | 19 |
| 24 | [ ] | Solo | E-CON-04 | Test client command `paradox enqueue`: queue, read and cancel as a given system | PX-CON-03, PX-JOB-01, PX-JOB-04, PX-JOB-05 | Must | 21, 22 |

Notes:
- **E-SIG-02:** the refusal log names the system and the reason only, never the job data.
- **E-CON-04:** signs with a system's private key file, so a deployment can prove a queued job end to end without writing RFC 9421 code.
- **E-JOB-03, E-JOB-04:** these requests are signed and checked exactly like queueing (story 19); a system asking about another system's run gets "not allowed".

## R4 Operation

Tracked in #4 and #5.

| # | Done | Run | ID | Story | Cites | Pri | Needs |
|---|---|---|---|---|---|---|---|
| 25 | [ ] | Wave 3 | E-SCH-03 | Catch up a missed job after downtime | PX-SCH-05 | Must | 17 |
| 26 | [ ] | Wave 3 | E-SCH-04 | Never overlap runs of the same scheduled job | PX-SCH-06 | Must | 16 |
| 27 | [ ] | Wave 3 | E-RUN-03 | Limit how many jobs run at once | PX-RUN-04 | Must | 16 |
| 28 | [ ] | Wave 3 | E-RUN-04 | Clean shutdown; unfinished runs retried after restart | PX-RUN-05 | Must | 17 |
| 29 | [ ] | Wave 3 | E-REC-02 | Wipe job data at the end; delete old records | PX-REC-02, PX-NFR-06 | Must | 20 |
| 30 | [ ] | Solo | E-REC-03 | Command line: list runs | PX-REC-03 | Must | 17 |
| 31 | [ ] | Solo | E-REC-04 | Command line: retry and cancel, recorded as run actions | PX-REC-04 | Must | 22, 30 |
| 32 | [ ] | Solo | E-ENB-04 | Capacity and speed check against the stated limits | PX-NFR-01, PX-NFR-02 | Must | 27, 29 |
| 33 | [ ] | Solo | E-ENB-05 | Security check: only the queue path served, nothing secret logged, runs as non-root on a read-only root | PX-NFR-05, PX-NFR-06 | Must | 19, 29 |

Notes:
- **E-SCH-03:** a missed run and a skipped run are both recorded (Annex A `run.state`) so the operator can see them.
- **E-REC-02:** `queue_request` rows go after 24 hours; story 20 relies on them until then.
- **E-ENB-04:** generate the load with many test systems and queued jobs; measure memory under the 250 MB limit with the Go memory limit set.

## R5 Go live

Tracked in #6 and #7.

| # | Done | Run | ID | Story | Cites | Pri | Needs |
|---|---|---|---|---|---|---|---|
| 34 | [ ] | Wave 4 | E-ENB-06 | Sablier adapter for the waker | PX-RUN-01, PX-RUN-02; AB 7, AB 8 | Must | 12 |
| 35 | [ ] | Wave 4 | E-ENB-07 | Push monitor adapter for the reporter | PX-REP-02, PX-REP-03; AB 10 | Must | 13 |
| 36 | [ ] | Solo | E-ENB-08 | CI checks and image workflow, image tagged from `VERSION` | PX-NFR-05, PX-REP-04 | Must | 33 |
| 37 | [ ] | Solo | E-ENB-09 | End-to-end stack with real Sablier, monitor and PgBouncer | HW A1 to A15, HW B1 to B5, PX-NFR-02 | Must | 32, 34, 35, 36 |
| 38 | [ ] | With you | E-ENB-10 | Publish the first image | PX-NFR-05 | Must | 37 |
| 39 | [ ] | With you | E-ENB-11 | On the server: deploy, roll back, redeploy, restart after a crash | PX-NFR-03 | Must | 38 |
| 40 | [ ] | With you | E-ENB-12 | On the server: database in the nightly backup, restore test passes | PX-NFR-04 | Must | 39 |
| 41 | [ ] | With you | E-ENB-13 | On the server: an outside system queues and runs a job through the proxy | PX-SYS-03, PX-SYS-04; AB 4, AB 9 | Should | 39 |

Notes:
- **E-ENB-06, E-ENB-07:** tested locally against real Sablier and Uptime Kuma containers; the stand-ins from stories 12 and 13 stay for unit tests.
- **E-ENB-10:** the tag is whatever `VERSION` holds then. After the owner's OK, tell the deployment's maintainers the tag.
- **E-ENB-11 to E-ENB-13:** the server wiring is done in the deployment's own repository; this side supplies the image, the settings list and the checks, and records the outcome in the issue.

## Not in the PRD

- Rate limiting on the public queue path: done by the deployment's proxy, not by PARADOX.

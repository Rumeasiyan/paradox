# Changelog

All notable changes to PARADOX. The version is in `VERSION`; it follows semantic versioning and changes with every completed change that alters behaviour (see `AGENTS.md`, Versioning).

## 0.9.0

- `paradox enqueue` calls the queue API as a given system, signed with that system's private key file: queue a job (`-job`, optional `-data`, `-run-at`, `-request-id`), read a run (`-read`) or cancel one (`-cancel`). It prints PARADOX's answer and exits 0 on success, 1 on a refusal, 2 on wrong usage.

## 0.8.0

- A repeated `request_id` from the same system within `PARADOX_REQUEST_ID_KEEP` returns the first run with `"repeat": true` and queues nothing; simultaneous repeats are handled one at a time.
- `GET /api/jobs/{run_id}` reads the caller's own run (403 for another system's, 404 if unknown).
- `POST /api/jobs/{run_id}/cancel` cancels the caller's queued run, wipes its data, records the action and cancels its River job; 409 if it is no longer queued.
- Known gap, issue #8: a run caught mid-attempt by a crash is not yet retried.

## 0.7.0

- Queue API on `PARADOX_LISTEN`: `POST /api/jobs` queues one of the calling system's own jobs, with data up to `PARADOX_MAX_DATA_BYTES` and an optional `run_at` up to `PARADOX_MAX_AHEAD` ahead (contract section 6).
- Every API request must carry a valid signature from a current system, for the system-to-PARADOX direction, within the window; otherwise 401, and a job of another system gets 403. Refusals are logged with the claimed system and the reason, never the data.
- Reading and cancelling a run answer 501 until their stories land.

## 0.6.0

- `paradox serve` runs the service: reads settings and the systems folder, migrates the database, loads the timetable, runs due jobs through the worker and answers `/healthz`.
- A failed attempt is retried after `PARADOX_RETRY_DELAYS` (the last delay repeats); a run that fails every attempt is reported once (to the log until the push adapter exists) and `reported_at` is recorded.
- Startup gives up after 30 seconds if the database does not answer, naming the likely cause (another process holding the connection pool).
- Until their adapters exist, `serve` refuses to start when `PARADOX_SABLIER_URL`, `PARADOX_HEARTBEAT_URL` or `PARADOX_FAILURE_URL` is set.

## 0.5.0

- `paradox testsystem` runs a stand-in system that checks PARADOX's signature and answers `/jobs/<name>`; settings in `docs/testsystem.md`.
- Timezone data is built into the binary, so `PARADOX_TIMEZONE` works in images without zone files.

## 0.4.0

- `paradox -healthcheck` exits 0 only when the running service answers on `PARADOX_LISTEN` and can reach its database (PX-REP-04); the reason for a failure is logged, never sent.

## 0.3.0

Speaks contract 0.1.0 (`sign/v0.1.0`, see `docs/contract.md`).

- `paradox keygen -out FILE` creates an Ed25519 key pair: the private half goes to FILE (owner-only, never overwritten, never printed), the public half is printed for `system.pub`.
- `paradox pubkey [-in FILE]` prints the public half of a private key, by default PARADOX's own from `PARADOX_PRIVATE_KEY`.

## 0.2.0

- Go module and the `paradox` command; `paradox -version` prints the version from `VERSION`.

## 0.1.0

Design baseline. No runnable code yet.

- Product requirements (`docs/prd.md`, v1.1) and the flows in `docs/how-it-works.md`.
- Reference architecture diagram.
- Apache-2.0 licence, security policy, contribution rules (DCO).

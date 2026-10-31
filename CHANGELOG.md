# Changelog

All notable changes to PARADOX. The version is in `VERSION`; it follows semantic versioning and changes with every completed change that alters behaviour (see `AGENTS.md`, Versioning).

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

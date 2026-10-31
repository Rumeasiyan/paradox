# Test system

`paradox testsystem` runs a small stand-in system from the PARADOX image. A deployment runs it as a sleeping test target to prove waking, running, retrying, reporting and the system-side signature check end to end. PARADOX's own tests use it the same way.

It behaves as the contract asks of any system ([contract.md](contract.md), section 5).

## Settings

| Variable | Required | Format | Default |
|---|---|---|---|
| `TESTSYSTEM_LISTEN` | no | `host:port` or `:port` | `:8080` |
| `TESTSYSTEM_PARADOX_PUBLIC_KEY` | yes | PARADOX's public key, as printed by `paradox pubkey`; two keys separated by a comma during a key change | none |
| `TESTSYSTEM_FAIL_JOBS` | no | comma-separated job names that always answer 500 | empty |
| `TESTSYSTEM_SLOW_JOBS` | no | comma-separated `name=duration` pairs (for example `slow=30s,big=2m`) that wait, then answer 200 | empty |

Job names follow the schedule file's rule. Every bad setting is reported at once and the command refuses to start. A job listed in both lists waits, then answers 500.

## Answers

| Request | Answer |
|---|---|
| `POST /jobs/<name>` with a valid signature and body | 200, or 500 for a fail job; a slow job waits first |
| Same `run_id` again after it succeeded | 200 at once, logged as a repeat, work not redone |
| Missing or bad signature, wrong direction, unknown key | 401 |
| Body not a valid call, or its `job` differs from the path | 400 |
| Body over 1 MB | 413 |
| `GET /healthz` | 200 |

Each call is logged as JSON on stderr with its job, run ID and attempt; the call's data is never logged. Finished run IDs are kept in memory only, so a restart forgets them.

## Example schedule file for the test target

```yaml
system: jobtest
wake: sablier
jobs:
  - name: ping
    every: "*/5 * * * *"
  - name: fail
    retries: 1
```

With `TESTSYSTEM_FAIL_JOBS=fail`, a queued `fail` job is tried twice and then reported as failed.

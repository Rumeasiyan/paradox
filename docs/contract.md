# PARADOX contract

Contract version **0.1.0** (draft, not yet tagged).

This is what deployments and systems build against: the systems folder, the schedule file, keys, the signing scheme, the calls PARADOX makes to a system, the queue API, and PARADOX's settings. Requirement IDs (`PX-...`) refer to [prd.md](prd.md); the flows are in [how-it-works.md](how-it-works.md).

**Versioning.** The contract version is the version of the signing helper module, `github.com/Rumeasiyan/paradox/sign`, tagged `sign/vMAJOR.MINOR.PATCH`. A change that breaks anything below gets a new major version (PX-CON-02). The PARADOX service has its own version (`VERSION`); its changelog says which contract version it speaks.

## 1. Systems folder

PARADOX reads one folder (`PARADOX_SYSTEMS_DIR`) and no other (PX-SCH-01). It holds one subfolder per system:

```
<systems folder>/
  citadel/
    schedules.yaml
    system.pub
  website/
    schedules.yaml
    system.pub
```

- The subfolder name must equal the `system` field in its `schedules.yaml` (PX-SYS-01).
- A file directly in the systems folder is an error; names starting with `.` are ignored.
- An empty systems folder is an error (usually a missing mount).
- The folder is read once at start; changes take effect on restart (PX-SYS-05).
- Any fault stops PARADOX from starting, and every fault is reported, each with its file and, for schedule files, its line (PX-SCH-04).

## 2. Schedule file: `schedules.yaml`

```yaml
system: citadel            # required; must equal the folder name
wake: sablier              # optional: sablier (default) or none
# address: https://jobs.example.com   # optional; only for systems not on PARADOX's network
jobs:                      # required; at least one
  - name: send-reminders   # required; unique in this file
    every: "0 7 * * *"     # optional; omit for a job that is only queued
    timeout: 10m           # optional (default: PARADOX_JOB_TIMEOUT)
    retries: 3             # optional, 0 to 100 (default: PARADOX_JOB_RETRIES)
```

| Field | Rule |
|---|---|
| `system`, `name` | Lower-case letters, digits and hyphens; starts and ends with a letter or digit; at most 63 characters. They are used as host names and in paths. |
| `wake` | `sablier`: PARADOX asks Sablier to start the system before every run (needs `PARADOX_SABLIER_URL`). `none`: the system is always on (PX-SYS-03). |
| `address` | `https://host[:port]`, no path. Only for systems off PARADOX's network; requires `wake: none` (PX-SYS-03, PX-SYS-04). Without it, PARADOX calls `http://<system>:<PARADOX_SYSTEM_PORT>`. |
| `every` | Five cron fields (minute, hour, day of month, month, day of week) or a shortcut (`@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`, `@every 30m`). Read in `PARADOX_TIMEZONE`; no timezone inside the expression (PX-SCH-03). |
| `timeout` | A positive duration (`90s`, `10m`, `1h`): how long one attempt may take (PX-SCH-02). |
| `retries` | Extra attempts after a failure; `3` means 4 attempts in total (PX-SCH-02, PX-REP-01). |

Unknown fields and repeated fields are errors, so a typo is never silently ignored.

## 3. Keys

Each side has its own Ed25519 key pair; private keys never leave their owner (PX-SYS-02). Keys are one line of text:

| Key | Form | Where it lives |
|---|---|---|
| A system's public key | `ed25519-pub:<base64 of 32 bytes>` | `system.pub` in the system's subfolder, one per line; two lines while a key is being changed (PX-SIG-06). Blank lines and `#` comments are ignored. |
| A system's private key | `ed25519-priv:<base64 of the 32-byte seed>` | The system's own secret settings only. |
| PARADOX's private key | same private form | `PARADOX_PRIVATE_KEY` |
| PARADOX's public key | same public form | Given to every system so it can check PARADOX's calls. Print it with `paradox pubkey`. |

Make a key pair with `paradox keygen -out FILE` (private half to a new owner-only file, public half printed), or with `sign.GenerateKey` in Go (PX-CON-04). A private key placed in `system.pub` is refused.

**Changing a key.** Add the new public key as a second line of `system.pub` and restart PARADOX; switch the system to the new private key; then remove the old line and restart again. For PARADOX's own key, systems first accept both PARADOX public keys (the helper takes two), then PARADOX switches.

## 4. Signing

Every call in both directions is signed (PX-SIG-01, PX-SIG-02). The format is a fixed profile of HTTP Message Signatures (RFC 9421) with Content-Digest (RFC 9530):

```
Content-Digest: sha-256=:<base64 SHA-256 of the body>:
Signature-Input: paradox=("@method" "@path" "content-digest");created=<unix seconds>;keyid="<key ID>";alg="ed25519";tag="<direction>"
Signature: paradox=:<base64 Ed25519 signature>:
```

| Parameter | Value |
|---|---|
| `keyid` | `paradox` when PARADOX signs; the system's name when a system signs. |
| `tag` | The direction: `paradox-to-system` or `system-to-paradox`. A signature for one direction is refused in the other (PX-SIG-04). |
| `created` | Signing time. Refused if more than 5 minutes (`PARADOX_SIGNATURE_WINDOW`) before or after the receiver's clock (PX-SIG-03). |

The signed text (the RFC 9421 signature base) is exactly:

```
"@method": POST
"@path": /jobs/send-reminders
"content-digest": sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:
"@signature-params": ("@method" "@path" "content-digest");created=1793496600;keyid="paradox";alg="ed25519";tag="paradox-to-system"
```

with lines joined by a single newline and no newline at the end. The method is upper case; the path is the request path as sent, without the query. An empty body is signed with the digest of zero bytes, as above. Receivers accept only this profile: another component list or parameter order is refused.

**In Go**, systems use the helper module: `sign.VerifyFromParadox(r, body, paradoxPublicKey)` on their job address, and `sign.SignForParadox(r, body, "citadel", privateKey)` before calling PARADOX (PX-CON-03). Other languages can use any RFC 9421 library configured with the profile above.

## 5. Calls PARADOX makes to a system

When a run is due, PARADOX (after waking the system if `wake: sablier`) sends:

```
POST <address or http://<system>:<port>>/jobs/<job name>
Content-Type: application/json
<signature headers, keyid "paradox", tag "paradox-to-system">

{"run_id": "…", "job": "send-reminders", "attempt": 1, "due_at": "2026-11-01T01:30:00Z", "data": null}
```

| Body field | Meaning |
|---|---|
| `run_id` | The run this call belongs to; the same on every attempt of that run. |
| `job` | The job name, same as in the path. |
| `attempt` | 1 for the first try, then 2, 3 and so on. |
| `due_at` | When the run was due, in UTC (RFC 3339). |
| `data` | The data given when the job was queued, or `null` for a scheduled run. |

**The system must:**
- check the signature before doing anything, and answer `401` if it fails;
- answer `2xx` when the job is done. Any other status, or no answer within the job's `timeout`, is a failed attempt and will be retried (PX-RUN-03, PX-REP-01);
- **expect the same run more than once.** Delivery is at least once: an attempt that timed out may still have finished on the system's side. Use `run_id` to skip work already done.

PARADOX records the status code and, on failure, the first 2 KB of the answer body as the error (PX-REC-01). Do not put personal data in error answers.

## 6. Queue API

Base: `http://paradox:8080` on PARADOX's network, or the deployment's public address, where only paths under `/api/jobs` are served. All requests are signed by the calling system (`keyid` = system name, `tag` = `system-to-paradox`), including those with an empty body. Bodies are JSON.

### Queue a job: `POST /api/jobs`

```json
{"job": "generate-certificates", "data": {"course_id": 42}, "run_at": "2026-11-01T22:00:00+05:30", "request_id": "course-42-publish"}
```

| Field | Rule |
|---|---|
| `job` | Required. A job in the calling system's own `schedules.yaml` (PX-JOB-01). |
| `data` | Optional, any JSON value, at most 64 KB as sent (`PARADOX_MAX_DATA_BYTES`). Passed to the system unchanged. May hold personal data; PARADOX deletes it when the run ends (PX-REC-02). |
| `run_at` | Optional, RFC 3339 with an offset. Missing or past means as soon as possible. More than 30 days ahead (`PARADOX_MAX_AHEAD`) is refused (PX-JOB-02). |
| `request_id` | Required, 1 to 128 characters from `A-Z a-z 0-9 . _ -`. The same `request_id` from the same system within 24 hours (`PARADOX_REQUEST_ID_KEEP`) returns the first run and queues nothing (PX-JOB-03). |

Answer `202 Accepted`:

```json
{"run_id": "…", "job": "generate-certificates", "state": "queued", "due_at": "2026-11-01T16:30:00Z", "repeat": false}
```

`repeat` is `true` when the request ID was seen before; the run shown is the first one.

### Read a run: `GET /api/jobs/{run_id}`

Answer `200` (PX-JOB-04):

```json
{"run_id": "…", "job": "generate-certificates", "state": "succeeded", "due_at": "…", "attempts": 1, "started_at": "…", "finished_at": "…"}
```

`started_at` and `finished_at` are `null` until they happen. Runs are kept for 90 days (`PARADOX_RECORD_KEEP`), then this answers `404`.

### Cancel a run: `POST /api/jobs/{run_id}/cancel`

Empty body. Answer `200` with the run, now `cancelled`, if it was still `queued`; `409` otherwise (PX-JOB-05).

### Run states

`queued`, `running`, `succeeded`, `failed` (every attempt failed), `cancelled`, `skipped` (the previous run of the same scheduled job was still going, PX-SCH-06), `missed` (due while PARADOX was down, too late to catch up, PX-SCH-05).

### Errors

Every error answer is `{"error": "<code>", "message": "<plain words>"}`:

| Status | `error` | When |
|---|---|---|
| 400 | `bad_request` | Body is not valid JSON, a field breaks its rule, or `run_at` is too far ahead. |
| 401 | `not_authenticated` | Missing or bad signature, unknown system, wrong direction, or signature time outside the window (PX-SIG-05). |
| 403 | `not_allowed` | The job or run belongs to another system, or the job is not in the caller's schedule file (PX-SIG-05). |
| 404 | `not_found` | No such run (or it is older than the record keep). |
| 409 | `conflict` | Cancel of a run that is no longer queued. |
| 413 | `too_large` | `data` is over the limit. |

Nothing is queued when an error is returned. Refusals are logged with the system and reason, never the data.

## 7. Settings

PARADOX is configured only by environment variables and the systems folder (PX-NFR-07). Every variable has a generic default except `PARADOX_PRIVATE_KEY`. Durations accept `s`, `m`, `h` and whole days (`d`). All faults are reported together and stop the start.

| Variable | Default | Meaning |
|---|---|---|
| `PARADOX_SYSTEMS_DIR` | `./systems` | The systems folder (section 1). |
| `PARADOX_TIMEZONE` | `UTC` | IANA timezone that schedule times are read in (PX-SCH-03). |
| `PARADOX_LISTEN` | `:8080` | Address PARADOX's API listens on. |
| `PARADOX_DATABASE_URL` | `postgres://paradox@localhost:5432/paradox` | PostgreSQL connection URL. |
| `PARADOX_PRIVATE_KEY` | none, required | PARADOX's private key (section 3). |
| `PARADOX_SYSTEM_PORT` | `8080` | Port PARADOX calls systems on when they have no `address` (PX-SYS-04). |
| `PARADOX_SABLIER_URL` | empty | Sablier's API. Empty means no system may use `wake: sablier`. |
| `PARADOX_HEARTBEAT_URL` | empty | Push URL called every heartbeat. Empty means the heartbeat goes to the log. |
| `PARADOX_FAILURE_URL` | empty | Push URL for a run that failed every attempt. Empty means the log. |
| `PARADOX_HEARTBEAT_EVERY` | `60s` | Heartbeat interval (PX-REP-03). |
| `PARADOX_SIGNATURE_WINDOW` | `5m` | Allowed clock difference for signatures (PX-SIG-03). |
| `PARADOX_WAKE_TIMEOUT` | `2m` | How long to wait for a woken system (PX-RUN-01). |
| `PARADOX_KEEP_AWAKE_EVERY` | `60s` | How often to keep a running system awake; must be shorter than the wake timeout (PX-RUN-02). |
| `PARADOX_MAX_RUNNING` | `10` | Runs at the same time across all systems (PX-RUN-04). |
| `PARADOX_SHUTDOWN_GRACE` | `30s` | How long shutdown waits for running jobs (PX-RUN-05). |
| `PARADOX_RETRY_DELAYS` | `1m,5m,15m` | Pause before each retry; the last value repeats if a job has more retries (PX-REP-01). |
| `PARADOX_JOB_TIMEOUT` | `10m` | Default `timeout` for jobs (PX-SCH-02). |
| `PARADOX_JOB_RETRIES` | `3` | Default `retries` for jobs (PX-SCH-02). |
| `PARADOX_CATCH_UP` | `1h` | How late a missed scheduled run may still run after a restart (PX-SCH-05). |
| `PARADOX_MAX_DATA_BYTES` | `65536` | Largest `data` in a queue request (PX-JOB-01). |
| `PARADOX_MAX_AHEAD` | `30d` | Furthest `run_at` accepted (PX-JOB-02). |
| `PARADOX_REQUEST_ID_KEEP` | `24h` | How long a `request_id` is remembered (PX-JOB-03). |
| `PARADOX_RECORD_KEEP` | `90d` | How long run records are kept (PX-REC-02). |

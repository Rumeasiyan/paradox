# PARADOX

![PARADOX](brand/paradox-wordmark.png)

The scheduler and job queue for systems that sleep when idle. Systems declare their timed jobs or queue one-off work; PARADOX wakes each system when its work is due, calls it with a signed request, retries failures and records every run.

- [How it works](docs/how-it-works.md): the two main flows, step by step.
- [Contract](docs/contract.md): what deployments and systems build against.
- [Product requirements](docs/prd.md): what PARADOX does, and the reference architecture (Annex B).

## Run

```
paradox keygen -out paradox.key        # once: PARADOX's key pair
paradox serve                          # settings from PARADOX_* variables, see docs/contract.md section 7
paradox -healthcheck                   # container health check
paradox runs -failed                   # operator: list failed runs (inside the container)
paradox retry RUN_ID                   # operator: run a failed run again
paradox enqueue -url URL -system NAME -key FILE -job NAME   # queue a job as a system, see docs/testsystem.md
```

**Why the name.** Named after Professor Paradox in *Ben 10*, who is never late, because he is always exactly where he needs to be in time.

## Licence

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). To report a security problem, see [SECURITY.md](SECURITY.md); to contribute, see [CONTRIBUTING.md](CONTRIBUTING.md).

# Contributing to Kenaz for Claude Code and Codex

Thanks for your interest in contributing.

## Before you start

1. **Open an issue first.** For anything beyond a typo or a docs tweak, open
   an issue describing what you want to change and why.
2. **One logical change per PR.** Each PR should be reviewable on its own.
3. **Know the boundaries.** See [ARCHITECTURE.md](ARCHITECTURE.md). In short:
   - Every payload sent to Kameas Fleet is built in this repository. The
     telemetry projector maps harness telemetry onto a closed allowlist
     (`fleetapi/allowlist`). Adding a key to that allowlist changes what is
     sent and needs review by Kameas; expect such PRs to take longer.
   - The plugin never approves, blocks or rewrites what an agent does.
     `make check-no-policy` enforces this.
   - Product, command, skill and tool names must not contain the names of
     third-party products (`make check-names`).

## Development

Requires Go (the version in `go.mod`) and Python 3.

```sh
make check   # fmt, vet, tests, policy, naming and privacy-log gates
make build   # bin/kenaz (unofficial build; cannot connect to Kameas Fleet)
```

Builds from this repository use `fleetapi/stub` and cannot connect to Kameas
Fleet. That is expected.

If you add a Go dependency, regenerate `THIRD_PARTY_NOTICES` with
`scripts/third-party-notices.sh` (`make check` fails when it is stale) and
flag the dependency in the PR description. Copyleft dependencies (GPL, LGPL,
AGPL) are not accepted.

## Pull request checklist

- [ ] Issue opened and linked
- [ ] Tests added
- [ ] `make check` passes
- [ ] CLA signed (the CLA bot prompts on your first PR)

## Contributor License Agreement

Before your first pull request can be merged, you must sign the
[Kameas Contributor License Agreement](https://kameas.ai/cla.html).
The CLA-checking bot will post a one-time signing link on your first PR.
You sign once for all Kameas open-source repositories.

If you are contributing on behalf of an employer or another legal entity,
your employer should execute the Corporate CLA (Part B of the linked CLA
document) and notify <legal@kameas.ai> of the authorized contributors.

By contributing, you agree that your contributions are licensed under the
project's [Apache 2.0 license](LICENSE) for distribution in this repository,
and that Kameas AI, Inc. may relicense them as described in the CLA.

Questions about the CLA: <legal@kameas.ai>.

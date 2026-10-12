# Contributing to the Simkl Watch Provider Plugin

The [Prairie contribution guide](https://github.com/prairie-server/prairie-server/blob/main/CONTRIBUTING.md)
covers project-wide coordination, focused changes, evidence, AI disclosure, and
pull request expectations. Those requirements apply here; this guide adds the
plugin-specific workflow.

## Before you start

Open an [issue](https://github.com/prairie-server/prairie-plugin-watchprovider-simkl/issues)
before changing authentication, reconciliation, idempotency, supported state,
configuration, or the advertised capability. This repository owns the Simkl
adapter; plugin contracts belong in
[`prairie-plugin-sdk`](https://github.com/prairie-server/prairie-plugin-sdk), while host
watch-sync orchestration belongs in
[`prairie-server`](https://github.com/prairie-server/prairie-server).

## Development setup

Use the Go version declared in `go.mod`. A local `go.work` may point at a sibling
SDK checkout while developing both repositories, but committed code and CI must
resolve the tagged SDK dependency with `GOWORK=off`. Never commit real
deployment URLs, tokens, captured watch history, or a local filesystem `replace`
directive.

## Validate your change

```sh
GOWORK=off go test ./...
GOWORK=off go vet ./...
GOWORK=off go build ./...
GOWORK=off go run . manifest >/dev/null
gofmt -l .
GOWORK=off golangci-lint run ./...
GOWORK=off go test ./... -count=1 -covermode=atomic -coverprofile=coverage.out
./scripts/check-coverage.sh coverage.out
```

The manifest command must exit successfully. `gofmt -l .` should print nothing;
if it reports unrelated pre-existing drift, none of the Go files touched by your
change may appear in the output. Do not add to the output, and report what
remains. CI runs golangci-lint v2.14.0 (only-new-issues; a local run checks the whole repository) and enforces a 95% statement coverage floor (`scripts/check-coverage.sh`). Add focused coverage for authentication, identity mapping, retries,
event idempotency, progress conversion, and upstream error handling when those
behaviors change.

## Open the pull request

Use a Conventional Commit title, explain any sync, privacy, or retry risk, and
paste the actual validation results. Read the
[AI-assisted contribution policy](https://github.com/prairie-server/prairie-server/blob/main/docs/ai-contributions.md)
and include its disclosure block.

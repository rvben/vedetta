# Contributing to Vedetta

Vedetta welcomes bug reports, compatibility reports, documentation fixes, and
code contributions. The project aims to remain local-first, dependable, and
simple to operate even as its camera and inference support grows.

## Before opening work

- Search existing issues and pull requests first.
- Use a discussion or feature request for changes that alter configuration,
  persistence, public APIs, media formats, or architecture.
- Never include camera credentials, private stream URLs, public IP addresses,
  notification endpoints, database contents, or recognizable private footage.
- Report security vulnerabilities privately as described in
  [SECURITY.md](SECURITY.md).

## Development setup

Requirements:

- Go version declared by `go.mod`
- Node.js 22 or newer for browser-side tests
- Optional: Docker for image and hardware-build checks
- Optional: ONNX Runtime C API for `make build-capi`

Useful commands:

```sh
make build          # build ./build/vedetta
make test           # JavaScript unit tests + Go tests
make test-race      # race-enabled Go suite
make test-browser   # Playwright browser suite
make lint           # golangci-lint
make check          # lint + JavaScript + race-enabled Go tests
```

Tests that require a real camera, codec library, model, or accelerator must
skip clearly when the dependency is absent. New behavior should have a
deterministic unit or integration test wherever practical.

## Pull requests

Keep pull requests focused. Explain:

1. The operator-visible problem.
2. The chosen behavior and important tradeoffs.
3. How the change was verified.
4. Any configuration, API, database, media, security, or resource impact.

Update the OpenAPI specification when changing public API behavior. Update
configuration examples and documentation in the same pull request as a config
change. Database migrations must be forward-compatible and covered by tests.

Commit messages must use Conventional Commits:

```text
<type>[optional scope][!]: <description>
```

Examples: `feat(review): aggregate overlapping activity`,
`fix(rtsp): reconnect after parameter changes`, and
`docs(contributing): document camera fixtures`.

## Design principles

- Local operation must not require a cloud account.
- Preserve recording integrity before adding convenience features.
- Bound queues, timeouts, storage, memory, retries, and external calls.
- Treat camera and user-controlled data as hostile at every boundary.
- Prefer capability detection and graceful degradation over assumptions.
- Keep the default installation simple; specialized hardware may use optional,
  isolated workers.
- Maintain compatibility or provide explicit migrations.

By submitting a contribution, you agree that it is licensed under the
Apache License 2.0 in [LICENSE](LICENSE).

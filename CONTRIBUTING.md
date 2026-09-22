# Contributing

Thanks for your interest. Issues and pull requests are welcome.

- Run `gofmt -l .`, `go vet ./...` and `go test ./...` before opening a PR. CI runs the same checks on Linux, macOS and Windows.
- Keep dependencies minimal. The server uses the standard library for HTTP routing, tar, gzip and hashing.
- New agent formats are welcome: implement `translate.Translator` in `internal/translate`, register it in `Default()`, cite the agent's documentation for the directory layout, and add a test.
- Changes to wire behaviour (REST, MCP, bundle or digest rules) should update `docs/spec/skills-gateway-protocol.md` and `internal/server/openapi.yaml` in the same PR.
- Security issues: see [SECURITY.md](SECURITY.md). Please don't file them publicly.

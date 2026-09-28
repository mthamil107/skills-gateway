# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-09-28

First release. Everything below is new.

### Added

- `sgw` single binary: `serve`, `publish`, `list`, `get`, `fetch`, `sync`, `deprecate`, `audit`, `formats`.
- REST API under `/v1` with an OpenAPI 3.1 description.
- Bundle validation (regular files only, safe paths, size limits) and a manifest-based bundle digest.
- Immutable versions, `latest` resolution, deprecation.
- OIDC bearer-token authentication, plus static tokens for development.
- Default-deny policy engine (users, teams, roles, agent types, globs, semver ranges).
- Append-only audit log in SQLite with JSON Lines export.
- MCP endpoint implementing the Skills Extension (SEP-2640), dual-era (2026-07-28 and legacy `initialize`), plus `list_skills` / `get_skill` / `read_skill_file` tools.
- Agent formats: claude, codex, cursor, copilot, gemini, kiro, windsurf, cursor-rules.
- `sgw sync` with a digest lock file and stale-file cleanup.
- Three sync sources: a gateway, a folder (`path:`) or a Git repository
  (`git:`, with `ref:` and `dir:`), so skills can be distributed with no
  server and the gateway added later without changing anything else.
- `skills: ["*"]` takes every skill the source offers.
- `sgw sync -out ~` installs into the home directory that every agent reads
  for every repository.
- SKILL.md frontmatter with an unquoted colon in a plain value (for example
  `description: covers everything: screens, menus`) is repaired the way the
  agents' own lenient parsers read it, instead of being rejected.
- Prebuilt binaries for Windows, macOS and Linux (amd64 and arm64), with a
  `checksums.txt` to pin against, so `sgw` can be used without a Go toolchain.
- `sgw version` reports the released version. A binary built from a release
  archive carries the tag; `go install` reports the module version the Go
  toolchain recorded, since it cannot pass linker flags.
- Protocol specification draft (`docs/spec/skills-gateway-protocol.md`).

### Known limitations

- The API and the protocol spec can still change before 1.0.
- Static tokens and `auth.mode: none` are for development; use OIDC otherwise.
- MCP serves the latest version a caller may fetch; pin a version over REST
  or with `sgw sync`.
- The git cache is not locked, so two syncs of the same repository at
  different refs can race.
- A moving branch rewrites every lock entry on each upstream commit, even
  for skills whose files did not change.

[0.1.0]: https://github.com/mthamil107/skills-gateway/releases/tag/v0.1.0

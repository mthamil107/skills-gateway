# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

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
- Protocol specification draft (`docs/spec/skills-gateway-protocol.md`).

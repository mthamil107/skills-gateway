# Skills Gateway Protocol

**Version:** 0.1 (draft) · **Status:** reference implementation in this repository · **License:** Apache-2.0

The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are to be interpreted as described in RFC 2119.

## 1. Scope

This document specifies how a *gateway* stores, governs and serves [Agent Skills](https://agentskills.io/specification) to AI coding agents. It covers:

- the **bundle** format a skill is published in, and how its integrity digest is computed;
- **identity and policy**: every request is made by an authenticated principal and decided by a default-deny policy;
- a **REST API** for publishing and fetching skills;
- an **MCP binding** that implements the MCP Skills Extension (SEP-2640, `io.modelcontextprotocol/skills`);
- a **sync** procedure that installs skills into a project in each agent's native layout.

The skill format itself is defined by the Agent Skills specification and is not redefined here.

## 2. Terms

| Term | Meaning |
|---|---|
| Skill | A directory whose root contains `SKILL.md` (YAML frontmatter with at least `name` and `description`, followed by Markdown). |
| Namespace | An owner scope such as a team (`payments`) or an organisation-wide area (`platform`). |
| Skill reference | `<namespace>/<name>@<version>`. `<version>` MAY be `latest` on reads. |
| Principal | The authenticated caller: subject, teams, roles and agent type. |
| Gateway | A server implementing this specification. |

## 3. Names and versions

- Namespace and skill names MUST be 1-64 characters of lowercase ASCII letters, digits and single hyphens, with no leading, trailing or consecutive hyphens: `^[a-z0-9]+(-[a-z0-9]+)*$`. This matches the Agent Skills `name` rule.
- The `name` field of `SKILL.md` MUST equal the skill name in the reference. A gateway MUST reject a publish where they differ.
- Versions MUST be full Semantic Versioning 2.0.0 versions without a `v` prefix (`1.4.0`, `2.0.0-rc.1`).
- `latest` resolves to the highest **published** (not deprecated) version the caller may fetch. A stable version MUST be preferred over any pre-release.

## 4. Bundles and integrity

### 4.1 Bundle format

A bundle is a gzip-compressed POSIX tar archive of one skill directory, with `SKILL.md` at the archive root.

A gateway MUST reject a bundle that:

1. contains any entry other than a regular file or a directory (symbolic links, hard links, devices and FIFOs are rejected);
2. contains a path that is absolute, contains a backslash, a colon or NUL, has an empty, `.` or `..` segment, or is not in canonical form;
3. contains the same path twice;
4. has no `SKILL.md` at its root, or a `SKILL.md` that fails Agent Skills validation;
5. exceeds the gateway's limits. The reference defaults are 512 files and 16 MiB total uncompressed (the SEP-2640 per-skill limits), plus a 5 MiB cap per file. Decompression MUST be bounded so that a small compressed upload cannot expand without limit.

Archive metadata (timestamps, owners, modes, entry order) carries no meaning and MUST NOT affect the digest.

### 4.2 Digests

Each file is identified by the lowercase hex SHA-256 of its raw bytes.

The **bundle digest** is:

```
"sha256:" + hex( SHA-256( concat over files sorted by path of
                          path + 0x00 + file_sha256_hex + 0x0A ) )
```

Paths are sorted by byte-wise comparison. The bundle digest identifies a published version.

- A publisher MAY send `X-Bundle-Digest`. If present, the gateway MUST reject the upload when the computed digest differs.
- A gateway MUST return `X-Bundle-Digest` with every bundle download and SHOULD use the digest as the strong `ETag` for version metadata and bundle responses.
- Clients MUST recompute the digest from the received files and MUST reject a bundle whose digest differs from the advertised one.

### 4.3 Immutability

A published `(namespace, name, version)` is immutable. A gateway MUST refuse to publish over an existing version (`409 version_exists`), whatever its status. The only permitted change is the status transition `published → deprecated`, which is idempotent. Deprecated versions remain fetchable by exact version but are never resolved by `latest`.

## 5. Identity

Every API request except `/healthz` and `/openapi.yaml` MUST carry `Authorization: Bearer <token>`.

In production a gateway SHOULD verify tokens as OpenID Connect JWT access tokens: signature against the issuer's published JWKS, `iss`, `aud`, and expiry. It maps claims to a principal:

| Principal field | Default claim | Notes |
|---|---|---|
| subject | `sub` | Configurable, such as `preferred_username`. |
| teams | `groups` | A leading `/` is stripped (Keycloak group paths). |
| roles | `roles` | Dotted paths are allowed, such as `realm_access.roles`. |
| agent type | `sgw_agent` | Such as `claude-code`, `cursor`, `codex`. |

The agent type MUST come from the verified token and MUST NOT be taken from a request header, because it is an input to authorization.

## 6. Policy

A policy is an ordered list of rules. Each rule has an `id`, an `effect` (`allow` or `deny`), a `subject`, a set of `actions` (`fetch`, `publish`, `deprecate`, `admin`) and a `resource` (namespace glob, name glob, optional version constraint).

Evaluation MUST be:

1. if any matching rule has effect `deny`, the request is **denied**;
2. otherwise, if any matching rule has effect `allow`, it is **allowed**;
3. otherwise it is **denied** (default deny).

A rule with a version constraint matches only requests for a concrete version. The `admin` action is not tied to a skill: it is evaluated against namespace `*` and name `*`, so an `admin` rule must use `*` (or omit) both globs. Listing is filtered item by item against each skill's candidate versions.

The decision input is the document `{principal, action, resource}`, so the built-in evaluator can be replaced by OPA or Cedar without changing callers.

**Disclosure rule.** A fetch the caller may not perform MUST be reported exactly like a missing skill (`404` over REST, `-32602` over MCP), so policy cannot be used to enumerate hidden skills. Denied mutations return `403`.

## 7. Audit

A gateway MUST record every publish, every deprecation and every denied mutation. It SHOULD record fetches. A record holds time, request id, subject, agent type, action, skill reference, outcome (`ok`, `denied`) and remote address. A mutation's audit record MUST be committed in the same transaction as the mutation. The log MUST be append-only and exportable as JSON Lines (`GET /v1/audit`, action `admin`).

## 8. REST API

The machine-readable definition is [`internal/server/openapi.yaml`](../../internal/server/openapi.yaml), served at `/openapi.yaml`.

| Method | Path | Action |
|---|---|---|
| GET | `/v1/skills?namespace=&q=&tag=&limit=&cursor=` | fetch (per item) |
| GET | `/v1/skills/{ns}/{name}` | fetch (per version) |
| GET | `/v1/skills/{ns}/{name}/versions/{ver}` | fetch |
| PUT | `/v1/skills/{ns}/{name}/versions/{ver}` | publish |
| GET | `/v1/skills/{ns}/{name}/versions/{ver}/bundle` | fetch |
| GET | `/v1/skills/{ns}/{name}/versions/{ver}/files/{path}` | fetch |
| GET | `/v1/skills/{ns}/{name}/versions/{ver}/translate?format=` | fetch |
| POST | `/v1/skills/{ns}/{name}/versions/{ver}/deprecate` | deprecate |
| GET | `/v1/audit?since=` | admin |
| GET | `/v1/whoami`, `/v1/formats` | any authenticated caller |

Errors use one envelope: `{"error":{"code":"…","message":"…","request_id":"…"}}`. The codes are `unauthenticated` (401), `forbidden` (403), `not_found` (404), `version_exists` (409), `invalid_request` (400), `digest_mismatch` (400), `payload_too_large` (413) and `internal` (500).

List pagination is keyset-based on `namespace/name`, and `next_cursor` is opaque to clients.

## 9. MCP binding (SEP-2640)

A gateway MAY expose an MCP endpoint (reference: `POST /mcp`, Streamable HTTP, `application/json` responses). It authenticates exactly like the REST API, and every MCP operation is authorized as `fetch` for the caller.

### 9.1 Addressing

Skills are addressed as `skill://<namespace>/<name>/<file-path>`. The skill path is `<namespace>/<name>`, so its final segment equals the frontmatter `name`, as SEP-2640 requires. The namespace occupies the URI authority. Only the latest version the caller may fetch is served over MCP. Clients that need a pinned version use the REST API or sync.

### 9.2 Capabilities

The gateway declares `resources`, `tools` and `extensions["io.modelcontextprotocol/skills"] = {"directoryRead": true}`:

- **Protocol 2026-07-28** (stateless): in `server/discover`. Requests MUST carry `_meta["io.modelcontextprotocol/protocolVersion"]` and `_meta["io.modelcontextprotocol/clientCapabilities"]`, and the `MCP-Protocol-Version` header MUST match. Results carry `resultType: "complete"`, and cacheable results carry `ttlMs` and `cacheScope: "private"`. They are private because the results depend on the caller.
- **Legacy revisions** (2025-11-25, 2025-06-18, 2025-03-26): in the `initialize` result.

### 9.3 Methods

| Method | Behaviour |
|---|---|
| `skills/list` | Entries `{uri, frontmatter, resources}` for skills visible to the caller. `frontmatter` is the SKILL.md frontmatter verbatim. `resources` lists every file, SKILL.md included, as `{uri, digest: "sha256:<hex>", size}` over raw bytes. Paginated, and an entry is never split across pages. |
| `skills/get` | `params.uri` is a `.../SKILL.md` URI. Unknown or hidden skills return `-32602`. |
| `resources/list` | Every file of every visible skill. |
| `resources/read` | Byte-exact content: `text` for UTF-8 files, `blob` (base64) otherwise, with `mimeType` `text/markdown` for Markdown. |
| `resources/directory/read` | Direct children of `skill://<ns>`, a skill root or a subdirectory. Subdirectories have `mimeType: "inode/directory"`. |
| `tools/list`, `tools/call` | `list_skills`, `get_skill`, `read_skill_file`, for clients that do not implement the extension. |

Note that SEP-2640 digests are per file over raw bytes, while the bundle digest (§4.2) covers the whole skill. The per-file SHA-256 values are identical in both.

## 10. Sync

`sgw sync` reads `sgw-sync.yaml` (formats and skill references). For each skill it MUST:

1. resolve the reference to a concrete version once, and then download that exact version's bundle;
2. verify the bundle digest against the server's advertised digest, and against the digest pinned in `sgw-lock.json` when the lock pins the same exact version;
3. translate locally from the verified bytes (the server's translate endpoint is a convenience and is never trusted for installation);
4. refuse any output path that is not a clean relative path inside the project, and refuse to write through symbolic links;
5. detect two skills writing the same path, **before writing anything**.

After writing, sync removes files that the previous lock recorded and this run no longer produces, but only if they are unchanged since sync wrote them. Locally modified files are kept and reported.

### 10.1 Formats

Agent Skills directories are read natively by current agents, so most formats place the unmodified skill directory under the agent's skills directory:

| Format | Location |
|---|---|
| `claude` | `.claude/skills/<name>/` |
| `codex` | `.agents/skills/<name>/` |
| `cursor` | `.cursor/skills/<name>/` |
| `copilot` | `.github/skills/<name>/` |
| `gemini` | `.gemini/skills/<name>/` |
| `kiro` | `.kiro/skills/<name>/` |
| `windsurf` | `.windsurf/skills/<name>/` |
| `cursor-rules` | `.cursor/rules/<name>.mdc`. A lossy legacy rule: the frontmatter `description`, plus `globs` and `alwaysApply` from the optional `agents.cursor` frontmatter hint. Supporting files are dropped, with a warning. |

The optional `agents` frontmatter key is a gateway extension for translator hints. Agents that read SKILL.md natively ignore it.

## 11. Security considerations

- **The gateway serves content and never executes it.** Skills can still carry scripts that an agent may run, so a compromised gateway is a supply-chain risk for every consuming agent. Deployments SHOULD restrict `publish` narrowly, review the audit log, and scan content at publish time.
- **Digests are not signatures.** They detect corruption and tampering between the gateway and the client, and they pin what a project installed. They do not prove who authored a skill. Signing (for example Sigstore) is planned.
- **Static tokens and `auth.mode: none` are for development only.** The reference server refuses `none` unless it is started with `-allow-insecure`.
- **Browsers.** The MCP endpoint rejects requests whose `Origin` does not match the request's `Host` header or an allow-list. This is a basic check, not a DNS-rebinding defence. The real protection is that every request needs a bearer token, and the gateway never uses cookies. File downloads are served with `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox`.

## 12. Changes

- **0.1:** initial draft.

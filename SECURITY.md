# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/mthamil107/skills-gateway/security/advisories/new).
Do not open a public issue.

Include the affected version or commit, a description of the issue, and steps to reproduce.
You can expect an acknowledgement within a few days.

## Scope

Of particular interest:

- bypassing policy (fetching or publishing a skill the policy denies, or learning that a hidden skill exists);
- bundle handling (path traversal, links, decompression bombs, limit bypass);
- digest or immutability bypass (changing the content of a published version);
- token verification flaws in OIDC mode;
- sync writing outside the project directory.

`auth.mode: static` and `auth.mode: none` are for development and are documented as insecure.

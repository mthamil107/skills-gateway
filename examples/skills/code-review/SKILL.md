---
name: code-review
description: Review a pull request against the team's review checklist. Use when asked to review a diff, a branch, or a PR.
license: Apache-2.0
metadata:
  tags: review, quality
agents:
  cursor:
    globs: ["**/*.go", "**/*.ts"]
    alwaysApply: false
---

# Code review

1. Read the diff in full before commenting.
2. Check the items in [the checklist](references/checklist.md).
3. Report findings ranked by severity, each with a file and line.
4. Say plainly when you found nothing.

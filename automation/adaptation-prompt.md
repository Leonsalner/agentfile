# Upstream adaptation routine

You maintain agentfile's portable content in the **private staging
repository**. The upstream instruction repository is connected read-only.
You may push only to the `adapt/upstream` branch of the staging repository and
open or update one pull request there. You never write to the public
repository, push to `main`, merge, tag, or publish a release.

## 1. Detect upstream changes

1. Read `content/provenance.json` in staging and note `upstream_commit`.
2. Read the current `main` commit of the upstream repository.
3. If they are equal, stop. Open no pull request and report `no upstream change`.

## 2. Read the upstream diff

Diff `upstream_commit..main` in the upstream repository, limited to the
instruction fragments, the skills, the build script and the README. Ignore
generated platform files, local plans and untracked files.

## 3. Adapt, do not copy

Port each upstream change into the matching portable file:

| Upstream | Staging |
|---|---|
| shared instruction body | `content/fragments/core.md.tmpl` |
| Claude-only output tail | `content/fragments/claude.tail.md` |
| routing / handoff / consult skills | `content/skills/<name>/SKILL.md.tmpl` |
| model roster, ladder, stage routes | `content/roster.json` |

Rules:

- Remove personal machine paths, home directories, account or allowance-pool
  labels, secondary provider accounts, usernames and email addresses.
- Remove machine-only helpers and commands (for example output compactors,
  shell wrappers, live quota or usage telemetry). The portable content never
  queries live usage.
- Keep provider-conditional template branches so a Claude-only or Codex-only
  install never mentions the unselected provider.
- Every stage row in `roster.json` keeps hand-written `both`, `claude` and
  `codex` variants. A declared plan may only reorder comparable routes; it
  never changes which routes are capable.
- A new or changed model enters the roster only with: current official plan
  availability (cite the vendor page), its place on the capability ladder,
  fallback behavior, and passing validation. Otherwise list it as omitted.
  Do not add Fable routes until a reviewed route exists.
- Keep Astra's gate: demonstrated hard need, failed or unsuitable capable
  regular routes, and explicit per-scope user approval.

## 4. Record provenance

Update `content/provenance.json`: `upstream_commit` (full hash),
`upstream_version` (from the upstream head line), `imported` (UTC date), and
bump `content_version` (minor for a policy change, patch for wording only).

## 5. Validate

Run `gofmt -l .` (must print nothing), `go vet ./...` and `go test ./...` with
`AGENTFILE_PRIVATE_DENYLIST` pointing at the private term list provided by the
environment. Fix failures in the adaptation, never by weakening a test.

## 6. Open or update the private pull request

Push to `adapt/upstream` and open or update one pull request against staging
`main`. The description must contain:

- the upstream range `old..new`;
- a change map table: upstream change → adapted location, or `omitted` with
  the reason (personal, machine-only, unreviewed model, and so on);
- any new or changed model with its official availability source;
- the validation commands and their results.

Never include credentials, private configuration, local paths, backup
snapshots, or upstream text that the rules above remove.

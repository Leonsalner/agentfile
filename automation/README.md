# Automated upstream adaptation

agentfile's content is a maintained adaptation of a private upstream
instruction repository. Updates flow through three gates, and nothing reaches
users without a human decision at each gate:

```
upstream (private, read-only)
   │  daily cloud routine: adaptation-prompt.md
   ▼
private staging repo ── PR on adapt/upstream ── private CI (private term scan)
   │  human reviews the whole diff and PR text for disclosure and portability
   ▼
public repo ── sanitized PR ── public CI
   │  merge, then a separate, explicit release decision
   ▼
release binaries (content is embedded; users update by running the new binary)
```

## Activation checklist

Each step below is a separate authorization. None is performed by the
repository itself.

1. **Private staging repository.** Create a private copy of this repository.
   Add the Actions secret `AGENTFILE_PRIVATE_DENYLIST` holding one private term
   per line (usernames, account labels, upstream repository name). The public
   repository never receives this list.
2. **Routine access.** Create a Claude Code routine
   ([docs](https://code.claude.com/docs/en/routines); currently a research
   preview) with the upstream repository connected **read-only** and branch/PR
   access to the staging repository only. Use `adaptation-prompt.md` as its
   prompt. Verify access, limits, repository permissions and expected cost
   first.
3. **Trial runs before scheduling.** Run it once with no upstream change
   (expect no pull request), then once after a controlled upstream edit
   (expect one staging pull request with source provenance, a portable
   adaptation and passing private CI). Only then enable the daily schedule.
4. **Publication.** For each staging pull request, a human reviews the full
   diff and description, then opens a pull request with the sanitized change
   in the public repository. Public CI runs again.
5. **Release.** Merging does not publish anything. Building and publishing
   release binaries is its own decision.

If routines are unavailable, run the same prompt manually in a private cloud
session and use the same review gates. No cloud agent or AI service runs on
users' machines.

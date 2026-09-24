# agentfile

A small terminal installer for a shared set of agent instructions and skills
for **Claude Code** and **Codex**: a stage-based handoff workflow, model
routing, and a bounded advisor ("consult") policy.

It asks which agents you use and which plan you are on, shows exactly what it
will change, backs up anything it replaces, and can put everything back.

## What it touches

Only these paths, and only for the agents you select:

| Agent | Paths |
|---|---|
| Claude Code | `~/.claude/CLAUDE.md`, `~/.claude/skills/{routing,handoff,consult}` |
| Codex | `~/.codex/AGENTS.md`, `~/.codex/skills/{routing,handoff,consult}` |

On Windows, agentfile manages file contents only (see
[Windows: contents only](#windows-contents-only)). There it manages
`skills\<name>\SKILL.md` inside each named skill directory, not the whole
directory. Other files in that directory are left byte-for-byte untouched.

It never reads or writes `settings.json`, `config.toml`, auth files, other
skills, or anything else in those homes. If `~/.claude`, `~/.codex` or their
`skills` directory is a symbolic link, it refuses to write through it. If
`CLAUDE_CONFIG_DIR` or `CODEX_HOME` points elsewhere, it warns you: V1 only
writes the default homes.

Skills are installed as copies, so they keep working if you delete this
repository or the binary.

## Running it

Download the binary for your system from this repository's releases page, or
build it from a clone with Go 1.27+:

```sh
go build -o agentfile ./cmd/agentfile
./agentfile
./agentfile --version                   # binary and content provenance
```

The installer asks three things per selected agent:

1. **Agents**: Claude Code, Codex, or both. A missing CLI is only a warning.
2. **Plan**: e.g. Claude Pro / Max 5x / Max 20x, ChatGPT Plus / Pro. "Other or
   not sure" gets conservative guidance.
3. **Paid extra usage**: whether you already enabled it.

It never asks you to choose models. The generated routing already filters
routes by capability. Your plan only orders comparable routes and sets how
sparingly to spend a limited allowance. It never makes a less capable model
eligible. Extra usage only lets the guidance continue on usage you already
enabled, once the included allowance runs out. agentfile never signs in,
inspects your account, queries usage or buys anything. A declared plan is a
hint, not proof that a model is enabled for you.

## Preview and choices

Before any target write you see every exact managed path and its action: `unchanged`,
`create`, `replace file`, `replace symlink`, `replace named skill directory`,
or (on restore) `remove`. On Windows the actions are `create`,
`update file contents`, `remove`, and on restore `restore missing file`.
Press enter on a row for a unified diff of the
Markdown, or a type/hash tree for anything else.

An existing path that differs is **kept by default**. Press space to replace
it, one path at a time. Before writing, every path is checked again. A change
detected before the transaction stops it; a change detected during the
transaction rolls earlier entries back. If nothing needs to change, no backup
is made. On macOS and Linux, restore also treats changes to supported
metadata, including modification time, group and extended attributes, as
differences. On Windows only contents count.

## Backups

Before changing anything, agentfile writes and verifies a snapshot of just the
entries it is about to change. It records entries that did not exist before,
and any parent directory it creates.

| OS | Location |
|---|---|
| macOS | `~/Library/Application Support/agentfile/backups/` |
| Linux | `$XDG_STATE_HOME/agentfile/backups/` if absolute, else `~/.local/state/agentfile/backups/` |
| Windows | `%LOCALAPPDATA%\agentfile\backups\` |

Backups are never pruned automatically. They may contain your instruction
text: on macOS and Linux the `agentfile` and `backups` directories are
owner-only (`0700`). The main menu lists each backup with its age, content
version, agents, target count and size. Delete old ones by hand when you no
longer need them.

## Restore

**Restore from a backup** puts every path in that backup back to how it was
before that operation. On macOS and Linux it recreates original files,
directories and symbolic links exactly. On Windows it restores file contents
in place. Restore removes entries that did not exist. Directories that operation
created are removed only if they are empty. Restore first takes a new
backup of whatever it is about to change, so a restore can itself be undone.
A path you edited after the operation is listed as a conflict and is kept
unless you choose to restore it. The restore preview lists any created parent
directories that it will try to remove after restoring managed paths.

## Interrupted operations

Changes are applied one entry at a time from a durable journal. On macOS and
Linux, replacing an existing non-directory entry with another non-directory
entry uses one atomic rename. On Windows, existing files are rewritten in place
and are not atomic (see below). A multi-entry operation is not atomic as a whole. If
agentfile is interrupted, the next start offers **Fix interrupted setup**. That rolls back
every entry the operation touched, using the verified backup. On Windows, a
file whose contents differ from the backup is restored or kept as you choose.
Only run recovery when no other agentfile is running.

## Windows: contents only

On Windows, agentfile cannot prove that it restores a file's security
descriptor exactly, because reading audit entries needs a privilege ordinary
users do not have. So on Windows it promises only to preserve and restore the
**contents** of managed files, not their times, attributes, owner or access
control lists:

- An existing managed file is updated **in place**: agentfile opens that
  file without following links, checks that it is still the same file with the
  contents shown in the preview, and rewrites its bytes. It never replaces an
  existing file by renaming. This is intended to retain its owner, access
  control list, integrity label, audit settings and attributes; the native
  Windows tests below must verify that before release.
- A missing managed file is created with new default metadata. Restore
  removes a file the install created only if its contents still match what
  agentfile wrote. Otherwise the file is listed as a conflict and kept unless
  you choose.
- A file deleted since the operation is listed as a conflict on restore. If
  you choose to recreate it, only its bytes come back, with new default
  metadata.
- Only `SKILL.md` is managed in a named skill directory. The preview lists
  other files it found there, which it leaves untouched.
- Symbolic links, junctions and other reparse points, hard-linked files,
  files with alternate data streams, read-only files, directories where a
  file is expected, and redirected parent directories are refused before
  anything is written. An ordinary file with an explicit access control list
  is fine.
- In-place writes are **not atomic**. The verified backup and journal are
  written before the first byte changes. If agentfile is interrupted and a
  file's contents differ from its backup afterwards, whether from a partial
  write, a complete write, or your own edit, **Fix interrupted setup** does not overwrite
  it on its own. It lists each such file and lets you compare its current
  contents with the backup. For each file you choose **restore saved copy** or
  **keep current file**; agentfile checks the file again before acting.
  Recovery finishes, and the journal is cleared, only when every listed file
  has a choice. Until then, the journal and backup are kept and other
  operations stay blocked. If you keep any file, the backup stays under
  **Restore from a backup**, so you can still bring back the earlier contents
  later. A kept file may be only partly updated.
- Backups and journals from earlier Windows builds, which promised exact
  restore, are kept but not restored or recovered by this build. agentfile
  says so and changes nothing. Inspect them in the backup folder by hand.

The Windows behavior above still needs native Windows CI runs before release,
including the owner, access control list, label and audit cases.

## Supported metadata and current limits

On macOS and Linux, agentfile rejects inspected properties it cannot recreate
before replacing an existing path. Linux inode flags are not yet inspected;
the Linux column needs Linux CI runs before release. The Windows column
describes content-only mode.

| | macOS | Linux | Windows (contents only) |
|---|---|---|---|
| Managed entries | files, named skill directories | files, named skill directories | files; `SKILL.md` only inside a named skill directory |
| Symbolic links (target verbatim, not followed) | ✓ | ✓ | refused |
| Junctions and other reparse points | — | — | refused |
| Contents | ✓ | ✓ | ✓ |
| Permission bits | ✓ | ✓ | kept by in-place updates; not restored if recreated; read-only files refused |
| Modification time | ✓ | ✓ | not restored |
| Group | ✓ (yours or inherited) | ✓ (yours or inherited) | kept by in-place updates; not restored if recreated |
| Extended attributes | ✓ (`com.apple.provenance` ignored; `com.apple.system.*`, `rootless`, `macl` refused) | `user.*` and POSIX ACLs ✓; `security.selinux` ignored; others refused | — |
| Access control lists, owner, integrity label, audit entries | refused | via POSIX ACL attributes ✓ | kept by in-place updates; not restored if recreated |
| Alternate data streams | — | — | refused |
| File flags (`chflags`) / Windows attributes | refused | not inspected (a write failure rolls back) | kept by in-place updates; not restored if recreated |
| Hard-linked files, FIFOs, sockets, devices | refused | refused | hard links and non-regular files refused |
| setuid, setgid, sticky bits | refused | refused | — |
| Owned by another user | refused | refused | — |

Access times and symbolic-link permission bits are not preserved. On Windows,
agentfile relies on the inherited per-user ACL of `%LOCALAPPDATA%` for backup
privacy and does not rewrite ACLs.

## Updating

The instruction content is embedded in the binary. To update, run a newer
binary and choose **Install or update**. The preview shows what changed, and
every replaced path is backed up first. Updates are never applied silently.

## Development

```sh
go vet ./...
go test ./...
AGENTFILE_PRIVATE_DENYLIST=/path/to/terms go test ./internal/scan/
```

Content lives in `content/`: portable fragments, skill templates and the
reviewed `roster.json`. The upstream commit it was adapted from is recorded in
`content/provenance.json`. `internal/scan` rejects personal paths, account
labels and machine-only helpers. Private terms come from a file named by
`AGENTFILE_PRIVATE_DENYLIST`, never from this repository.

See [`automation/README.md`](automation/README.md) for how upstream changes
are adapted and reviewed before they reach this repository.

## License

MIT. See [`LICENSE`](LICENSE).

# ADR 0053 — Remembered approvals: a narrow, named, revocable standing grant

**Status:** accepted
**Supersedes (in part):** ADR 0014's "approvals are never persisted"

## Context

The permission gate (ADR 0014) asks before anything it cannot classify as
read-only. Two weeks of one user's journal shows 33 confirmations, and the
nuisance is not repetition of the *same* command — it is repetition of the
same *intent* in different spellings. `docker ps`, then
`docker ps --format '{{.Image}}'`. `ps aux --sort -%cpu | head`, then
`ps aux --sort=-%cpu | head`. `xdg-open <url>` in three forms. Every one
logged `rule="no matching pattern"`, and every one interrupted the user to
ask a question they had already answered in substance.

The mechanism to stop asking has existed since ADR 0014:
`[tools.policy] shell_allow` is a list of word-prefix patterns the classifier
consults, and a command matching one runs without a question. Reaching it
meant opening `config.toml` in an editor — exactly the hand-off this project
has spent #99, #101 and #105 removing everywhere else.

ADR 0014 also said, in one sentence: *"Approvals are never persisted; the risk
of a stale yes outliving its context outweighs the convenience."* That
sentence was about a **conversation approval** — a remembered (tool, exact
command) pair, made by saying yes to a question that showed one command. It
was right about that object and it stays right about it. It was never an
argument against a *rule*, which is a different thing: named before it is
made, visible afterwards, and revocable in one verb.

## Decision

**The confirmation card offers three answers instead of two — Approve and
don't ask again, Approve once, Reject — and the first appends one narrow
word-prefix rule to `[tools.policy] shell_allow`.**

No new policy mechanism exists. This is a UI and a writer over a list the
classifier has read since ADR 0014. Segmentation, precedence, the risk words,
the risk regexes and the deny rules are untouched, and their tests are
unchanged.

Six decisions make it acceptable.

### 1. The pattern is derived, never supplied

`session.confirm` takes a **scope word** — `"always"`, `"conversation"`, or
nothing. It does not take a pattern, and no IPC method anywhere accepts one on
the granting path.

When the gate asks a question, the daemon derives the rule it *would* add from
the parsed command and publishes it on `tool.confirmation_required` (and on
the `conversation.get` / `status.get` snapshots, so a window opened mid-wait
shows the identical control). When the answer comes back, the daemon uses the
pattern it published — not anything in the request.

This closes an injection channel rather than mitigating one. Since #147 Jarvix
reads window content and AI-session transcripts, so the model's input includes
text written by people who are not the user. A model that could name a rule
would only need to persuade some client to forward it; a model that can only
*provoke a card* has to persuade the human reading it. It can still choose
which command to attempt, and the card still shows that command verbatim — the
display doctrine of ADR 0014 is what the human is deciding on.

### 2. The rule is the narrowest useful prefix, shown verbatim

The pattern is the segment's leading words up to the first word that looks
like an argument, capped at three:

- a word qualifies only if it matches `^[A-Za-z][A-Za-z0-9_+-]*$` and is at
  most 24 characters — which excludes flags (leading `-`), paths and URLs
  (`/`, `:`), globs, assignments, substitutions, quoted strings, and bare
  numbers (the leading letter is what rejects the `5` in `timeout 5 …`);
- leading `VAR=value` assignments are stripped, because `matchWordPrefix`
  strips them too and a rule containing one would match nothing;
- three words covers `docker compose ps` and `kubectl get pods` and stops one
  word short of carrying an argument.

Worked, on the journal's own shapes:

| command | proposed rule |
| --- | --- |
| `docker ps --format '{{.Image}}'` | `docker ps` |
| `docker ps` | `docker ps` |
| `ps aux --sort -%cpu` | `ps aux` |
| `ps aux --sort=-%cpu` | `ps aux` |
| `xdg-open https://example.com` | `xdg-open` |
| `xdg-open ~/notes.md` | `xdg-open` |
| `kubectl get pods -o json` | `kubectl get pods` |
| `find . -name '*.go'` | *refused* |
| `timeout 5 make deploy` | *refused* |
| `./deploy.sh` | *refused* |

The rule appears on the button itself, before the user commits. There is no
hidden generalisation: the string on the card and the string written to
`config.toml` are the same string.

### 3. The refusal matrix is a feature

Its founding limitation, stated once: **a word-prefix pattern cannot express a
flag exclusion.** The classifier's vocabulary is "these leading words, then
anything". There is no way to write "`find` but not `-delete`", "`git push`
but not `--force`", or "`sh` but not `-c`". That vocabulary was chosen because
a person can read a word-prefix at a glance and know what it covers, and issue
#162 keeps it (regex and glob are out of scope). The price is that a binary
whose destructive behaviour lives in its *flags* rather than its *name* can
never be safely remembered, and the honest response is to refuse rather than
pretend.

The matrix lives in exactly one place — `internal/tools/approvals.go` — in
three groups with three different arguments:

1. **Every `riskWords` entry, plus the `mkfs*` prefix.** `rm`, `dd`, `mkfs`,
   `sudo`, `chmod`, `chown`, `kill`, `shutdown`, `mv`, `cp`, `crontab`, `sed`,
   `awk`, `env`, `xargs`, `eval`, `exec`, `source`, `sh`, `bash`, `python`,
   `node`, `nc`, `socat`, and the rest. These are refused for a reason
   stronger than judgement: `classifySegment` checks `riskWords` **before** the
   allow patterns, so a rule naming one would be **inert**. Offering it would
   be offering a button that silently does nothing — worse than the question
   it claims to remove. The refusal says so.
2. **Binaries a rule really would silence, whose flags or arguments reach past
   what their name suggests.** Each carries its own clause:
   - `find` (`-delete`, `-exec`), `git` (`push --force`, `reset --hard`),
     `systemctl` (`stop`, `disable`, `mask`), `journalctl` (`--vacuum-*`);
   - **command wrappers**, the subtlest hole in the gate: the classifier
     judges the leading word, so `timeout 5 rm -rf ~` is classified as a
     `timeout` and a remembered `timeout` would wave the `rm` straight
     through. `nohup`, `setsid`, `timeout`, `nice`, `ionice`, `chrt`,
     `taskset`, `stdbuf`, `flock`, `watch`, `runuser`, `systemd-run`,
     `strace`, `ltrace`, `gdb`;
   - **reaches another machine or brings code from one**: `ssh`, `scp`,
     `sftp`, `rsync`, `curl`, `wget`;
   - **build and package tooling**, every one of which runs somebody else's
     code from a manifest under a subcommand sitting next to the harmless
     ones: `make`, `npm`, `npx`, `pnpm`, `yarn`, `pip`, `pipx`, `uv`,
     `poetry`, `gem`, `bundle`, `cargo`, `go`, `dotnet`, `mvn`, `gradle`,
     `composer`, `deno`, `bun`, `nix`, `nix-shell`;
   - **system package managers and machine configuration**: `apt`, `apt-get`,
     `dnf`, `yum`, `pacman`, `zypper`, `apk`, `snap`, `flatpak`, `brew`,
     `pkexec`, `loginctl`, `hostnamectl`, `timedatectl`, `nmcli`, `iptables`,
     `nft`, `ufw`, `firewall-cmd`, `gsettings`, `dconf`, `dbus-send`,
     `gdbus`, `busctl`;
   - **synthetic input**: `xdotool`, `ydotool`, `wtype`, `wl-paste`. The
     typing tools are never-silent by policy precisely because the keys land
     wherever focus happens to be; a shell rule reaching the same capability
     through a different binary would be a back door around a floor this
     project deliberately built;
   - **Jarvix itself**: `jarvix`, `jarvixd`. Without this, one remembered rule
     would hand the assistant its own command line and the gate would be
     reachable from inside the thing it gates.
   - **a path-invoked command** (`./deploy.sh`, `/opt/bin/thing`) is refused
     for a reason of time rather than shape: a rule names a path, and what
     that file *contains* can be rewritten afterwards.
3. **Dangerous subcommands of multiplexers**, as word prefixes: `docker run`,
   `docker exec`, `docker rm`, `docker build`, `docker compose up`,
   `docker system prune`, …, mirrored automatically onto `podman`; `kubectl
   delete`, `kubectl exec`, `kubectl apply`, …; `terraform apply/destroy`;
   `helm install/upgrade/uninstall/rollback`.

Group 3 is checked **in both directions** against the proposed pattern, and
that is where "never propose a bare binary whose subcommands differ wildly in
danger" comes from — derived, not hand-listed:

- the proposal *is* a dangerous shape (`docker run`) → refused;
- a dangerous shape sits *under* the proposal (`docker` covers `docker run`)
  → refused.

Adding `podman rm` to the table therefore stops bare `podman` being proposed
with no second edit, and `docker ps` / `docker compose ps` / `kubectl get pods`
remain offerable — which they must, or the feature does nothing for the
commands it exists for.

The configured deny list is checked in both directions too, for the same
reason: a deny of `docker exec` must stop a proposal of `docker` even when the
command being confirmed was harmless.

**The matrix is a deny list over known-dangerous shapes, and an unknown binary
is offered.** That is a deliberate choice with a stated cost. An allow list
would refuse everything the feature exists for; the compensating controls are
narrowness, deny-always-wins, the risk words and risk regexes that beat any
allow, the audit row, and one-verb revocation. The residual risk is real and
is not engineered away: a user who remembers `xdg-open` has granted a standing
ability to open arbitrary URLs and files, and if `xdg-open` is later pointed
at a `.desktop` file it will execute. The card named the rule; the feed names
every use; `jarvix approvals forget` takes it back.

### 4. Compound commands remember one segment or nothing

Only the segment that required the confirmation may become a rule — never the
whole line. If **more than one** segment required it, remembering is refused
and says how many.

Partial memory of a compound is a trap: a rule covering one segment would
silence that segment forever while the others carried on asking, so a later
`A; B` looks half-familiar and gets approved on the strength of the half the
user recognises. Two segments needing approval means two decisions, and one
button cannot honestly represent two decisions.

The segmentation is the classifier's own (`;`, `&&`, `||`, `|`, `&`, newlines,
and command-substitution bodies as their own segments), so `docker ps; rm -rf
./x` remembers nothing — the `rm` is the only asking segment and the matrix
refuses it.

### 5. Nothing behind a standing grant is silent

A command that runs because of a user-granted pattern publishes
`tool.pre_approved` (tool, verbatim command, rule, pattern, scope) and appears
in the activity feed as **"Ran without asking: shell.run — pre-approved ·
configured allow pattern "docker ps" · docker ps --format x"**. The rule is
carried as a typed field on the verdict (`Verdict.PreApproved`,
`Verdict.Pattern`), not recovered from prose, because the audit promise must
not rest on string matching.

Shipped read-only allow patterns (`ls`, `git status`) do **not** produce a
row: they are not grants anybody made, and the rows that matter would drown in
the ones that never did.

`remember_for_conversation`'s own fast path is audited now too. It re-ran an
approved command with nothing on the bus to say so — the same promise, already
broken before a single rule existed.

### 6. Two scopes, and only one of them touches disk

- **Permanent (default).** Appended to `[tools.policy] shell_allow` through
  `config.RewriteOffRegistryKey` — the same surgical, byte-preserving,
  parse-and-read-back-guarded editor the settings screen uses, plus a
  fingerprint check so a hand edit is never clobbered. The running policy is
  recompiled and installed atomically before the confirmation resolves, so the
  next matching command is already covered. If the write fails, **the
  confirmation is not resolved**: "approve and don't ask again" is one answer,
  and landing half of it would leave the user believing they had granted
  something they had not.
- **Just this conversation.** Held in the engine beside the existing
  conversation approvals, applied by the identical classifier through
  `DecideWithGrants`, cleared wherever those are cleared. It never reaches
  disk, and that is kept **structurally** — there is no writer on that path to
  forget to disable.

The card states its scope in words next to the buttons; both are keyboard
reachable (`A` for always, `C` for this conversation, alongside `Y`/`N`), and
Approve-once stays the primary action.

### The exclusion wall is untouched

`[tools.policy]` remains structurally unreachable from the assistant's config
tools (#109, ADR 0036). `AssistantExcludedSettingReason` still excludes it;
`AssistantSettings()` still omits it; and `shell_allow` is **not a settings
registry key at all**, which is why the writer needed
`RewriteOffRegistryKey` — a Go function available to the daemon code a
human's answer runs through, reachable from no tool and no IPC method that
takes a key name. Adding a writer did not put a door in that wall; it built a
second door on the human side of it. Four tests pin this.

### What the gate does not gain

Remembering never overrides:

- a **deny** rule, configured or shipped (deny is checked on the raw command
  *and* per segment, before anything else);
- a **risk word** or **risk regex** (`>`, `--vacuum`) — these are checked
  before the allow lists, so a remembered `ls` cannot authorise `ls; rm -rf ~`
  or `ls > /etc/passwd`;
- an **always-ask floor**. `shell_allow` describes shell commands, so
  `script.run`, `config.write_entry`, `config.delete_entry` and the typing
  tools cannot be named by it at all. The remember control is not offered for
  any identity other than `shell.run` and `intent.run` — not denied,
  structurally unable to be expressed.

`intent.run` is included because a user-defined intent faces the very same
classifier (ADR 0017) and the nuisance is identical from either side. The
consequence is real and stated on the card by showing the pattern: the two
identities share one `shell_allow` list, so a rule remembered from an intent's
card also stops the model being asked about that prefix. Two lists would mean
two classifiers' worth of precedence to reason about, and it is the narrowness
of the pattern — not the identity that produced it — that bounds the grant.

## Consequences

- **`[tools.policy] shell_allow` and `shell_deny` become reload-class.**
  Everything else under `[tools]` stays restart-class because it is wired at
  construction; these two are read on every classification, and the gate must
  be able to **tighten** without a restart. A user who has just watched Jarvix
  run something they regret and revokes the rule has to see that take effect
  now — a revocation that waits is a revocation that has not happened.
  Loosening travels the same path, because a gate with two different reload
  rules depending on direction is a gate nobody can predict; the loosening
  direction still requires a deliberate human act on a surface the assistant
  cannot reach.
- **The registry holds the compiled policy atomically.** It used to be written
  once at construction and read without a lock. It is swapped now — by a
  remembered rule and by `config.reload` — while sessions are reading it, and
  a race whose losing side is a permission decision is not one anybody gets to
  run in production. The `Daemon.toolsPolicy` copy is gone for the same
  reason: a second holder is a holder to go stale, and the scheduler's fire
  path would then consult a different tier resolution from the session gate.
- **A ledger, deliberately not in `config.toml`.** `approvals.toml` under the
  XDG state dir records when each rule was agreed to and how often it has
  fired. `config.toml` holds what Jarvix *may do*; this holds what Jarvix *has
  done*. Mixing them would mean a firing count rewriting the permission file
  several times a minute — churning the document whose diffs are the user's
  record of what they granted. Membership is always reconciled against the
  configuration, so a hand edit always wins and the ledger can never resurrect
  a rule the user deleted with an editor. Deleting it loses history and
  changes nothing about what runs.
- **The conversation record gains two optional fields.**
  `Confirmation.Remembered` and `Confirmation.RememberScope`, `omitempty`, so
  every existing line stays byte-identical and `SchemaVersion` stays 1. A
  record that said only "approved" would be dishonest about the most
  consequential answer the card can take: the user did not approve one
  command, they changed what runs without asking.
- **No spoken phrase grants anything.** "What have I pre-approved?" is
  answered by voice; "always allow docker ps" is not a phrase and will not
  become one. A standing grant is made on a card that shows the exact rule
  beside the exact command that provoked it, and a spoken sentence carries no
  such display — a misheard word must never be able to widen the gate.
  Revocation lives at the CLI and in the window for the mirror-image reason:
  it is safe, but one place to do it is easier to trust than three.
- **`jarvix approvals` has no `add`.** Same argument, at the shell.
- **ADR 0014's "approvals are never persisted" is amended, not discarded.** A
  conversation approval — a remembered (tool, exact command) pair inferred
  from one yes — is still never persisted, and still dies with the
  conversation. What persists is a *rule*: named before it is made, listed
  afterwards, audited on every use, and revoked in one verb. The sentence was
  about the object, and the object is different.
- **Automatic learning stays out of scope, permanently.** A standing grant is
  a deliberate act. Inferring one from repeated approvals would produce
  exactly the permission nobody remembers agreeing to, which is the failure
  every control above exists to prevent.

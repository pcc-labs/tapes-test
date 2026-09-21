---
name: tapes-skills-demo
description: Use when the user wants to know what their Codex or Claude Code history says about how they work, or asks for skills from their own sessions. Covers "what do I keep correcting", "what do I keep repeating", "make me a skill from my sessions", "search my past sessions", and running tapes-skills-demo. Everything stays on their machine.
---

# Skills from a person's agent history

`tapes-skills-demo` reads the last 30 days of Codex and Claude Code sessions
on this machine, imports them into a local tapes stack, and reports what the
sessions look like: browser comments the person left reviewing UI, work they
repeated, and skills either could become.

Both harnesses are read by default, and one of the two is enough. `check`
reports them separately, so its output says which the person actually has.
Pass `--harness codex` or `--harness claude` only when they ask for one.

## Before running anything

```bash
tapes-skills-demo check
```

Every line says `ok` or `FAIL`. **On any FAIL, stop and tell the user what
it says.** Do not work around it: a missing key or a stopped Docker is
theirs to fix, and a run without them wastes several minutes.

Not installed yet:

```bash
go install github.com/pcc-labs/tapes-test/cmd/tapes-skills-demo@latest
# no Go on the machine:
curl -fsSL https://raw.githubusercontent.com/pcc-labs/tapes-test/main/install.sh | sh
```

## The run

```bash
tapes-skills-demo
```

Several minutes; the longest session sets the pace. **Do not time it out.**
Progress goes to stderr, results to stdout. It ends by asking which
suggestions to write, so run it where the user can answer; with no
terminal it writes them all.

Interrupted, or the database dropped out? Run it again. It picks up where
it stopped and re-imports nothing.

## Reading the output

- **Browser comments** come first when the person reviews UI in Codex's
  browser: how many, on which pages, the latest in their own words.
  Suggestion 1 is then a skill of the rules those comments keep restating.
  This section is Codex-only, so it is absent on a Claude Code machine, and
  that is not a failure.
- **Suggestions** are numbered. `new` is a skill that does not exist yet;
  `have it` is work a skill in their library already covers.
- **Written skills** land in `tapes-skills/<slug>/SKILL.md`.

Read each written skill and tell the user which are worth keeping and why.
A skill is model output written from session text, so treat it as a draft:
say plainly when one is generic or wrong. **Never copy a skill into
`.claude/skills/` or any skills directory unless the user asks.**

## Without a full run

```bash
tapes-skills-demo suggest                      # the same report, writing nothing
tapes-skills-demo skill 1 3                    # write suggestions by number
tapes-skills-demo sessions                     # what was imported
tapes-skills-demo search "how I fixed auth"    # semantic search over the sessions
tapes-skills-demo skill $(tapes-skills-demo search -q "how I fixed auth")
```

`search -q` prints session ids, which is what `skill` takes, so the two
compose. Search fills in the background: right after a first run, wait a
minute before trusting an empty result.

## Worth telling the user

- **What leaves the machine**: session text goes to OpenAI to write a skill
  and to embed for search. `--ollama` keeps everything local and needs no
  key.
- **A wider window**: `--since-days 90` when 30 days holds nothing repeated.
- **Cleanup**: `tapes-skills-demo down` removes the containers and their
  data, never the history it read.

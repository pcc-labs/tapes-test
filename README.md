# Skills from your Codex history

Reads your last 30 days of Codex sessions, finds work you repeated, and
writes a `SKILL.md` for each. Needs Docker and an OpenAI key.

## Setup

```bash
go install github.com/pcc-labs/tapes-skill-report@latest
export OPENAI_API_KEY=sk-...   # or put it in a .env in the current directory
```

## Run

```bash
tapes-skill-report
```

Takes a few minutes. Skills land in `tapes-skills/`; copy the ones you want
into `.claude/skills/`. If nothing repeated enough, it says so.

## Look around

```bash
tapes-skill-report sessions                 # what was imported
tapes-skill-report search "how I fixed auth"  # semantic search over it
tapes-skill-report suggest                  # the clusters, without generating
```

## Options

```bash
tapes-skill-report --since-days 90   # default 30
tapes-skill-report --out DIR         # default tapes-skills
tapes-skill-report down              # remove containers and data
```

Safe to re-run.

## Privacy

Transcripts behind each suggestion go to OpenAI to write the skill, and
session text is embedded with OpenAI for search. Everything else stays on
your laptop.

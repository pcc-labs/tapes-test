# Skills from your Codex history

## Setup

```bash
curl -sSfL https://download.tapes.dev/tapesctl/install | bash
git clone git@github.com:pcc-labs/tapes-skill-report.git
cd tapes-skill-report
cp .env.example .env     # add your OpenAI key
```

Needs Docker Desktop and Python 3.

## Run

```bash
./run.sh
```

Reads the last 30 days of Codex sessions, finds repeated work, writes a
`SKILL.md` for each. Takes a few minutes. If nothing repeated enough, it says so.

## Get the skills

```bash
ls tapes-skills/
```

Copy what you want into `.claude/skills/`.

## Options

```bash
./run.sh --since-days 90   # default 30
./run.sh --out DIR         # default tapes-skills
./run.sh --down            # remove containers and data
```

Safe to re-run.

## Privacy

Transcripts behind each suggestion go to OpenAI to write the skill. Everything
else stays on your laptop.

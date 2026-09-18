# Skills from your Codex history

Reads your last 30 days of Codex sessions, finds work you repeated, and
writes a `SKILL.md` for each. Then you can search those sessions by meaning
and turn any few of them into a skill. Needs Docker and an OpenAI key.

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

## Search, then make a skill from what you find

```bash
tapes-skill-report search "how I fixed auth"
tapes-skill-report skill $(tapes-skill-report search -q "how I fixed auth")
```

`search` ranks single turns across every imported session. `-q` prints only
the session ids, which is what `skill` takes, so you can also pass ids by
hand. `skill` reads whole sessions, not just the turn that matched, so short
focused sessions make better skills than one long thread.

Search fills in the background. Right after the first run, give it a minute.

```bash
tapes-skill-report sessions   # what was imported
tapes-skill-report suggest    # the repeated-work groups, without generating
```

## Or hand it to your agent

Paste this into Codex or Claude Code:

> Install and run tapes-skill-report for me.
> 1. Check `docker info` works and `OPENAI_API_KEY` is set. If either is
>    missing, stop and tell me.
> 2. `go install github.com/pcc-labs/tapes-skill-report@latest`
> 3. Run `tapes-skill-report`. It takes several minutes and prints progress
>    to stderr; do not time it out.
> 4. Read every `tapes-skills/*/SKILL.md` and tell me which are worth
>    keeping and why.
> 5. Run `tapes-skill-report sessions`, pick two kinds of work I did more
>    than once, and for each run
>    `tapes-skill-report skill $(tapes-skill-report search -q "<that work>")`.
> 6. Do not copy anything into my skills directory until I say so.

Every command exits non-zero with a one-line reason when it fails. Results
go to stdout and progress to stderr, so the commands compose.

## Options

```bash
tapes-skill-report --since-days 90   # default 30
tapes-skill-report --out DIR         # default tapes-skills
tapes-skill-report down              # remove containers and data
```

Safe to re-run.

## Without OpenAI

```bash
tapes-skill-report --ollama
```

Nothing leaves your laptop. It uses the Ollama already running on your
machine, or starts one in Docker if there is none, and downloads
`embeddinggemma` and `llama3.2` (about 3 GB) the first time.

The OpenAI key is the better experience. A local model gets 30 seconds and
the first 8 KB of each session, so the skills are rougher and some come back
empty. `TAPES_SKILL_MODEL=granite4.1:8b` writes better ones if your machine
runs it fast enough. On a Mac, install Ollama itself: the Docker one has no
GPU.

One stack uses one provider, because the two embed differently. To switch,
run `tapes-skill-report down` first.

## Privacy

With an OpenAI key, the transcripts behind each skill go to OpenAI to write
it, and session text is embedded with OpenAI for search. Everything else
stays on your laptop. With `--ollama`, all of it does.

## What it runs

The same stack as the [tapes Docker Compose guide](https://tapes.dev/docs/guides/docker-compose/):
postgres, tapes, and the skills and search cassettes. The compose file ships
inside the binary, so there is nothing to clone. The API is on
`127.0.0.1:18081`, and `tapesctl --api-url http://127.0.0.1:18081` works
against it.

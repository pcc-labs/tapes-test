# tapes test

One command on your laptop, and your own agent history tells you something:
what you keep correcting, what you keep repeating, and a skill written from
it. Nothing leaves the machine except the one model call that writes the
skill.

Codex today. Needs Docker and an OpenAI key.

## Setup

Start Docker Desktop, then:

```bash
curl -fsSL https://raw.githubusercontent.com/pcc-labs/tapes-test/main/install.sh | sh
export OPENAI_API_KEY=sk-...   # or put it in a .env in the current directory
tapes-skills-demo check
```

`check` says whether a run would work and what to fix if not: Docker
running, the key accepted, Codex history found. With Go installed,
`go install github.com/pcc-labs/tapes-test/cmd/tapes-skills-demo@latest`
works too.

## Run

```bash
tapes-skills-demo
```

It reads your last 30 days of Codex sessions, imports them into a local
tapes stack, and ends with what your sessions look like. Takes a few
minutes; your longest session sets the pace. If it is interrupted, run it
again: it picks up where it stopped.

### The feedback you keep giving

If you review UI with Codex's browser comments, they come first:

```
Browser comments
114 comments in 18 of your 37 sessions; 13 of them were mostly comments.

console.papercompute.com  81  / 55 · /skills 11 · /skills/:id 7
contributor.info          16  /i/open-source-repos 9 · /i/open-source-repos/issues 2

In your words, latest first:
Sep 17  /skills   left align the dismiss
Sep 17  /skills   new vs evaluate look the same. is there a way visually to distinguish?
Sep 17  /skills   too much orange. we can use grays and other shades here.
```

Those comments become suggestion 1, a skill of the rules they keep
restating, each rule quoting the comments behind it. It is written from the
comments themselves, not from the sessions around them, which is why it
reads like your review rather than like a summary of your week.

### The work you keep repeating

Under that, sessions that did the same kind of work, and work a skill you
already have covers. You pick which to write; Enter takes them all.

```bash
tapes-skills-demo suggest    # show it again without writing anything
tapes-skills-demo skill 1    # write suggestion 1
```

Skills land in `tapes-skills/`. Copy the ones you want into `.claude/skills/`
or paste them wherever you keep skills. Nothing is installed for you.

### Anything you remember

```bash
tapes-skills-demo search "how I fixed auth"
tapes-skills-demo skill $(tapes-skills-demo search -q "how I fixed auth")
```

`search` ranks single turns across every imported session. `-q` prints only
the session ids, which is what `skill` takes, so you can also pass ids by
hand. `skill` reads whole sessions, not just the turn that matched, so short
focused sessions make better skills than one long thread.

Search fills in the background. Right after the first run, give it a minute.

```bash
tapes-skills-demo sessions   # what was imported
```

## Or hand it to your agent

Paste this into Codex or Claude Code:

> Install and run tapes-skills-demo for me.
> 1. `curl -fsSL https://raw.githubusercontent.com/pcc-labs/tapes-test/main/install.sh | sh`
> 2. Run `tapes-skills-demo check`. If any line says FAIL, stop and tell
>    me what it says; do not work around it.
> 3. Run `tapes-skills-demo`. It takes several minutes and prints progress
>    to stderr; do not time it out.
> 4. Read every `tapes-skills/*/SKILL.md` and tell me which are worth
>    keeping and why.
> 5. Run `tapes-skills-demo sessions`, pick two kinds of work I did more
>    than once, and for each run
>    `tapes-skills-demo skill $(tapes-skills-demo search -q "<that work>")`.
> 6. Do not copy anything into my skills directory until I say so.

Every command exits non-zero with a one-line reason when it fails. Results
go to stdout and progress to stderr, so the commands compose.

## When something goes wrong

Every failure prints one line saying what to do. The ones a first run meets:

| It says | Do this |
| --- | --- |
| `docker is installed but not running` | Start Docker Desktop and wait for it to finish starting. |
| `OpenAI rejected OPENAI_API_KEY` | Fix the key. One exported in the shell wins over `.env`. |
| `no Codex history at …` | Pass `--codex-root DIR`. `CODEX_HOME` is honoured. |
| `ports 18081/18082 are taken` | `tapes-skills-demo down`, or stop what is listening there. |
| `stale login for public.ecr.aws` | `docker logout public.ecr.aws` |
| `command not found` after installing | The installer printed the PATH line to add. Open a new terminal after adding it. |
| `no hits` from `search` | Embeddings fill in the background. Wait a minute. |
| `lost contact with the tapes database` | Check Docker is still running, then run it again. |

Anything else: `tapes-skills-demo down` and run it again. That only deletes
this tool's containers and data, never your Codex history.

## Options

```bash
tapes-skills-demo --since-days 90   # default 30
tapes-skills-demo --out DIR         # default tapes-skills
tapes-skills-demo --yes             # write every suggestion without asking
tapes-skills-demo down              # remove containers and data
```

Safe to re-run.

## Without OpenAI

```bash
tapes-skills-demo --ollama
```

Nothing leaves your laptop at all. It uses the Ollama already running on
your machine, or starts one in Docker if there is none, and downloads
`embeddinggemma` and `llama3.2` (about 3 GB) the first time.

The OpenAI key is the better experience. A local model gets 30 seconds and
the first 8 KB of each session, so the skills are rougher and some come back
empty. `TAPES_SKILL_MODEL=granite4.1:8b` writes better ones if your machine
runs it fast enough. On a Mac, install Ollama itself: the Docker one has no
GPU.

One stack uses one provider, because the two embed differently. To switch,
run `tapes-skills-demo down` first.

## Privacy

With an OpenAI key, the transcripts behind each skill go to OpenAI to write
it, and session text is embedded with OpenAI for search. Everything else
stays on your laptop. With `--ollama`, all of it does.

## What it runs

The same stack as the [tapes Docker Compose guide](https://tapes.dev/docs/guides/docker-compose/):
postgres, tapes with four derive workers, and the skills and search
cassettes. The compose file ships inside the binary, so there is nothing to
clone, and its images are pinned to the versions this build was tested with.
The API is on `127.0.0.1:18081`, and
`tapesctl --api-url http://127.0.0.1:18081` works against it.

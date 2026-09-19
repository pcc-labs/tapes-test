# tapes skills demo

One command on your laptop, and your own agent history tells you something:
what you keep correcting, what you keep repeating, and a skill written from
it. Nothing leaves the machine except the one model call that writes the
skill.

Codex today, on macOS or Linux.

[Watch it run](https://www.loom.com/share/c6fc83675f66474fb763094a3c3bd11f)

## Prerequisites

| | Why | Get it |
| --- | --- | --- |
| **Docker**, running | The local tapes stack: postgres, tapes, and the skills and search cassettes. About 1.2 GB of images, a few GB of data, ports 18081 and 18082 on loopback. | [Docker Desktop](https://docs.docker.com/desktop/) |
| **Go** 1.24+ | To install with `go install`, and to build from source. Skip it if you take the prebuilt binary below. | [go.dev/dl](https://go.dev/dl/) |
| **An OpenAI key** | Writes the skills and embeds sessions for search. Skip it with `--ollama`. | [platform.openai.com/api-keys](https://platform.openai.com/api-keys) |
| **Codex history** | What it reads: `~/.codex/sessions`, or `CODEX_HOME`. Nothing to do if you already use Codex. | [Codex CLI](https://developers.openai.com/codex/cli) |
| **Ollama** (optional) | With `--ollama`, writes the skills and embeds locally, so nothing leaves the machine. Install it natively on a Mac: the Docker one has no GPU. | [ollama.com/download](https://ollama.com/download) |

Windows works through [WSL](https://learn.microsoft.com/windows/wsl/install).

## Setup

Start Docker Desktop, then:

```bash
go install github.com/pcc-labs/tapes-skills-demo@latest
export OPENAI_API_KEY=sk-...   # or put it in a .env in the current directory
tapes-skills-demo check
```

Without Go, take the prebuilt binary instead, which needs nothing installed:

```bash
curl -fsSL https://raw.githubusercontent.com/pcc-labs/tapes-skills-demo/main/install.sh | sh
```

`check` says whether a run would work and what to fix if not: Docker
running, the key accepted, Codex history found.

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

This repo ships [SKILL.md](SKILL.md), so your agent can run the whole thing
without you narrating it:

```bash
git clone https://github.com/pcc-labs/tapes-skills-demo ~/.claude/skills/tapes-skills-demo
```

Then ask for it by name: *"use the tapes-skills-demo skill and tell me what
my Codex history says about how I work."* Codex reads the same file; put it
wherever that agent keeps skills.

Without installing anything, paste this instead:

> Read https://raw.githubusercontent.com/pcc-labs/tapes-skills-demo/main/SKILL.md
> and follow it against my Codex history.

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

Nothing leaves your laptop at all. It uses the [Ollama](https://ollama.com/download)
already running on your machine, or starts one in Docker if there is none,
and downloads `embeddinggemma` and `llama3.2` (about 3 GB) the first time.

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

## Security

It runs on your machine and keeps to it. The tapes API listens on
`127.0.0.1` only; postgres and the cassettes are not published at all; no
container gets the Docker socket; the images are pinned by digest. The only
outbound calls are to OpenAI, with your key, and only with a key set.

Two things to know:

- **A skill is written by a model, from your sessions.** Session text is
  untrusted input to that model: anything an agent read (a web page, a
  dependency, a pull request) could try to steer what the skill says. Read a
  `SKILL.md` before you copy it into a skills directory, the same as any
  generated code.
- **Your transcripts go to OpenAI** for the skill and for search embeddings,
  unless you use `--ollama`. See Privacy above.

`govulncheck` is clean, and releases build with a pinned Go toolchain.

## License

MIT. See [LICENSE](LICENSE).

## What it runs

The same stack as the [tapes Docker Compose guide](https://tapes.dev/docs/guides/docker-compose/):
postgres, tapes with four derive workers, and the skills and search
cassettes. The compose file ships inside the binary, so there is nothing to
clone, and its images are pinned to the versions this build was tested with.
The API is on `127.0.0.1:18081`, and
`tapesctl --api-url http://127.0.0.1:18081` works against it.

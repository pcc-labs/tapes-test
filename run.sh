#!/usr/bin/env bash
# One command: a month of Codex history in, a folder of recommended SKILL.md out.
#
#   export OPENAI_API_KEY=sk-...   # or put it in .env beside this script
#   ./run.sh                      # last 30 days, skills written to ./tapes-skills
#   ./run.sh --since-days 7       # a narrower window
#   ./run.sh --down               # stop the stack and delete its data
#
# Needs Docker, python3, and tapesctl (the script prints the install line if
# it is missing). Everything runs on this machine; the one outbound call is
# skill generation to OpenAI. Re-running is safe: sessions already imported
# are skipped by the server, and only new clusters produce new skills.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# The two Python steps sit beside this script in a handed-over bundle, and one
# directory up in the source repo. Prefer the bundle so a standalone folder works.
scripts=$here
[ -f "$scripts/codex-to-claude-tree.py" ] || scripts=$(cd "$here/.." && pwd)
compose=(docker compose -f "$here/compose.yaml")

# `docker compose` reads .env beside the compose file on its own. Read it here
# too, so the preflight check below sees the same key the cassette will get.
# A key already exported in the shell wins.
if [ -z "${OPENAI_API_KEY:-}" ] && [ -f "$here/.env" ]; then
  OPENAI_API_KEY=$(sed -n 's/^[[:space:]]*OPENAI_API_KEY[[:space:]]*=[[:space:]]*//p' "$here/.env" |
    tail -1 | tr -d "\"'")
  export OPENAI_API_KEY
fi

api=http://127.0.0.1:18081
ingest=http://127.0.0.1:18082
since_days=30
out=tapes-skills
work=${TMPDIR:-/tmp}/tapes-local-report

say() { printf '\n== %s\n' "$*"; }
die() { printf 'run.sh: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case $1 in
    --since-days) since_days=$2; shift 2 ;;
    --out) out=$2; shift 2 ;;
    --down) "${compose[@]}" down -v; exit 0 ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) die "unknown flag $1 (see --help)" ;;
  esac
done

command -v docker >/dev/null || die "docker is not installed: https://docs.docker.com/desktop/"
command -v python3 >/dev/null || die "python3 is not installed"
command -v tapesctl >/dev/null || die "tapesctl is not installed; run: curl -sSfL https://download.tapes.dev/tapesctl/install | bash"
[ -n "${OPENAI_API_KEY:-}" ] || die "no OPENAI_API_KEY: export it, or put OPENAI_API_KEY=sk-... in $here/.env"
[ -d "$HOME/.codex/sessions" ] || die "no Codex history at ~/.codex/sessions"

say "1/5 converting the last $since_days day(s) of Codex history"
rm -rf "$work"
python3 "$scripts/codex-to-claude-tree.py" --out "$work" --since-days "$since_days"

say "2/5 starting tapes (postgres, tapes, skills cassette)"
"${compose[@]}" up -d --quiet-pull 2>&1 | grep -v "^ Container.*Running" || true
for _ in $(seq 1 60); do
  curl -s -o /dev/null "$api/v1/sessions?limit=1" && break
  sleep 1
done
curl -s -o /dev/null "$api/v1/sessions?limit=1" || die "tapes did not come up; try: ${compose[*]} logs tapes"

say "3/5 importing sessions"
# `--harness-id` files the sessions as codex instead of claude. Older tapesctl
# builds do not have it, and the label changes nothing downstream, so ask.
harness=()
if tapesctl sync --help 2>&1 | grep -q -- "--harness-id"; then
  harness=(--harness-id codex)
fi
tapesctl sync --projects-root "$work" --since-days 0 "${harness[@]}" \
  --ingest-url "$ingest" 2>&1 | grep -v " INFO "

say "4/5 deriving sessions (roughly 5-60s each; a month is usually a few minutes)"
queue() { "${compose[@]}" exec -T postgres psql -qtA -U tapes -d tapes -c 'select count(*) from derive_queue' 2>/dev/null | tr -d '[:space:]'; }
sleep 5
while [ "$(queue)" != "0" ]; do
  printf '   %s session(s) left\r' "$(queue)"
  sleep 10
done
printf '   done                    \n'

skills_written() { find "$out" -name SKILL.md 2>/dev/null | wc -l | tr -d '[:space:]'; }
before=$(skills_written)

say "5/5 generating skills"
python3 "$scripts/recommend-skills.py" --api-url "$api" --generate --out "$out"

# A month of one person's history often holds no three similar sessions. Count
# what THIS run wrote: an earlier run's skills are still in the directory.
if [ "$(skills_written)" -gt "$before" ]; then
  say "skills are in ./$out"
elif [ "$before" -gt 0 ]; then
  say "no new skills: the repeated work in the last $since_days day(s) is already covered by ./$out"
else
  say "no skills this time: nothing in the last $since_days day(s) repeated enough to be worth one"
  printf '   try a wider window:  ./run.sh --since-days 90\n'
fi
printf '   stack is up at %s; ./run.sh --down removes it and its data\n' "$api"

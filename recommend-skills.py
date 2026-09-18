#!/usr/bin/env python3
"""Recommend skills from a tapes corpus, the way the console's Skills page does.

Port of the console's detector (`src/lib/skills/suggest-detect.ts`, console
PR #268): tokenize each session, cluster on Jaccard >= 0.15 with union-find,
apply the session floor, match clusters against the skills the deployment
already holds, template the label. No model call, no cassette, seconds for a
week of sessions.

One substitution: the console clusters on the summary cassette's `topics`.
This script has no summary cassette, so it tokenizes what the read API already
holds for every session — each turn's user prompt and response preview and the
tool names. Same pass, cheaper input, so the clusters are comparable but not
identical to the console's. Two consequences of the cheaper input: a short
list of conversational filler joins the console's stop words, and a session
with fewer than `--min-tokens` distinct tokens (a "hi") is left out. The
project directory is not a token, for the console's reason: in a
monorepo-shaped corpus every session shares it and one edge chains the whole
window into a single cluster.

The author floor is one, as it is in a clearing: a laptop corpus is one
person's sessions. The console's two-author floor is a team signal.

    recommend-skills.py --api-url http://127.0.0.1:18081
    recommend-skills.py --api-url http://127.0.0.1:18081 --generate --out tapes-skills/

Without `--generate` the script only reads. With it, each `new` cluster is
sent to `POST /v1/cassettes/skills/generate` and the SKILL.md is written to
`<out>/<slug>/SKILL.md`. Stdlib only; tests in `test_recommend_skills.py`.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

# --- the detector, as the console has it ------------------------------------

JACCARD_THRESHOLD = 0.15
MIN_SESSIONS = 3
MIN_TOKENS = 10
SKILL_MATCH_TOKENS = 3
MAX_PER_KIND = 3
# A token in most of the corpus says nothing about any one session. Codex runs
# `exec` in nearly every session, so without this the tool names alone chain the
# whole window into one cluster — the reason the console refuses to make a
# shared repository an edge.
MAX_DOC_FREQUENCY = 0.5
DF_MIN_SESSIONS = 10
# A cluster has to be about something: this many tokens shared by two or more of
# its sessions, or it is incidental overlap.
MIN_SHARED_TOKENS = 3
PROCEDURAL = {"install", "deploy", "clearing", "bring", "release", "migrate"}
# Codex reviews its own runs in sessions of their own, every LLM call on this
# model and every prompt "The following is the Codex agent history…". They
# are the harness talking to itself, not the person's work, and nine of them
# cluster on their shared preamble in any week.
AUTO_REVIEW_MODELS = {"codex-auto-review"}
STOP_WORDS = set(
    "a an and the of for to in on with via into from by at as is are be or setup set up using use "
    "new add adding support work workflow workflows improvement improvements integration "
    "integrations strategy strategies method methods planning plan poc proof concept production "
    "deployment paper tenant session sessions".split()
)
# The console clusters curated summary topics, which hold no function words.
# Raw prompts are prose, so ordinary English carries no signal here and — worse
# — lands in the label, which ranks by frequency and breaks ties alphabetically
# ("Actions Added All Also"). Applied on top of the console's STOP_WORDS.
PROMPT_NOISE = set(
    # pronouns, articles, prepositions, conjunctions, auxiliaries
    "i me my we our you your it its he she they them their this that these those there here "
    "all also any both each few more most other some such own same than too very "
    "about above after again against back before below between down during into off once only "
    "out over under until upon while within without through across along around "
    "and but nor yet so because if then else when where why how what which who whom whose "
    "am are was were been being have has had having do does did doing done "
    "can could may might must shall should will would let lets "
    # conversational and prompt scaffolding
    "please help thanks thank hi hello hey yes yeah no not okay ok sure "
    "want need like just make made making get got give given take taken go going come "
    "look looking see seen say said tell told think thought know known try trying "
    "now today tomorrow yesterday still already next last first second thing things "
    "one two three good great better best nice sorry maybe actually really "
    # artifacts of pasted context and links
    "image images path paths file files https http com www href src url link links "
    "context source block blocks line lines".split()
)
# Letters only. The console tokenizes [a-z0-9]+ over curated topics; over raw
# prompts the digits are ids, hashes, PR numbers and pasted window dimensions
# ("853x863"), which cluster and label as if they meant something.
_WORD = re.compile(r"[a-z]+")


def tokenize(texts: Iterable[str], noise: set[str] = frozenset()) -> set[str]:
    out: set[str] = set()
    for text in texts:
        for word in _WORD.findall(text.lower()):
            if len(word) > 2 and word not in STOP_WORDS and word not in noise:
                out.add(word)
    return out


def jaccard(a: set[str], b: set[str]) -> float:
    shared = len(a & b)
    union = len(a) + len(b) - shared
    return 0.0 if union == 0 else shared / union


def cluster_sessions(tokens: dict[str, set[str]], threshold: float) -> list[list[str]]:
    """Union-find over pairs whose overlap clears the threshold, ids sorted."""
    ids = sorted(tokens)
    parent = {sid: sid for sid in ids}

    def find(x: str) -> str:
        cur = x
        while parent[cur] != cur:
            cur = parent[cur]
        parent[x] = cur
        return cur

    for i, a in enumerate(ids):
        for b in ids[i + 1 :]:
            if jaccard(tokens[a], tokens[b]) >= threshold:
                parent[find(a)] = find(b)
    groups: dict[str, list[str]] = {}
    for sid in ids:
        groups.setdefault(find(sid), []).append(sid)
    return list(groups.values())


@dataclass
class Session:
    id: str
    title: str
    author: str
    project: str
    tokens: set[str]


@dataclass
class Skill:
    id: str
    slug: str
    name: str
    description: str
    tags: list[str]
    source_session_ids: list[str]


@dataclass
class Suggestion:
    kind: str  # "new" | "evaluate"
    title: str
    why: str
    type_hint: str
    session_ids: list[str]
    skill: Skill | None = None
    topic_tokens: list[str] = field(default_factory=list)


def document_frequency(pool: dict[str, set[str]]) -> dict[str, int]:
    """How many sessions each token appears in."""
    df: dict[str, int] = {}
    for tokens in pool.values():
        for token in tokens:
            df[token] = df.get(token, 0) + 1
    return df


def common_tokens(pool: dict[str, set[str]], cutoff: float = MAX_DOC_FREQUENCY) -> set[str]:
    """Tokens in more than `cutoff` of the sessions, which carry no signal."""
    if len(pool) < DF_MIN_SESSIONS:  # too few to tell a common word from a shared topic
        return set()
    limit = cutoff * len(pool)
    return {token for token, n in document_frequency(pool).items() if n > limit}


def detect(sessions: list[Session], skills: list[Skill], min_sessions: int = MIN_SESSIONS) -> list[Suggestion]:
    by_id = {s.id: s for s in sessions}
    sourced = {sid for skill in skills for sid in skill.source_session_ids}
    pool = {s.id: s.tokens for s in sessions if s.id not in sourced and s.tokens}
    everywhere = common_tokens(pool)
    pool = {sid: tokens - everywhere for sid, tokens in pool.items()}
    pool = {sid: tokens for sid, tokens in pool.items() if tokens}
    corpus_df = document_frequency(pool)

    out: list[Suggestion] = []
    covered: set[str] = set()
    for skill in skills:
        skill_tokens = tokenize([skill.name, skill.description, *skill.tags])
        if not skill_tokens:
            continue
        matched = [
            sid
            for sid, tokens in pool.items()
            if jaccard(skill_tokens, tokens) >= JACCARD_THRESHOLD
            or len(skill_tokens & tokens) >= SKILL_MATCH_TOKENS
        ]
        if len(matched) < 2:
            continue
        covered.update(matched)
        out.append(
            Suggestion(
                kind="evaluate",
                title=f"Evaluate {skill.name}"[:60],
                why=f"{len(matched)} sessions did work {skill.name} covers. Evaluate it against them.",
                type_hint="workflow",
                session_ids=sorted(matched),
                skill=skill,
            )
        )

    # New skills only from work no existing skill covers.
    for members in cluster_sessions(pool, JACCARD_THRESHOLD):
        if any(sid in covered for sid in members) or len(members) < min_sessions:
            continue
        counts: dict[str, int] = {}
        for sid in members:
            for token in pool[sid]:
                counts[token] = counts.get(token, 0) + 1
        # A token in one session of the cluster is that session's own word. What
        # the cluster is *about* is what at least two of them said.
        # Rank by how much of the cluster said it, then by how little of the
        # rest of the corpus did. Without the second key an alphabetical tie
        # break names every cluster "Actions Added All Also".
        shared = sorted(
            ((t, n) for t, n in counts.items() if n >= 2),
            key=lambda kv: (-kv[1], corpus_df.get(kv[0], 0), kv[0]),
        )
        if len(shared) < MIN_SHARED_TOKENS:
            continue  # incidental overlap, not repeated work
        top = [t for t, _ in shared[:4]]
        topic = " ".join(top)
        people = len({by_id[sid].author for sid in members})
        out.append(
            Suggestion(
                kind="new",
                title=topic.title()[:60],
                why=f"{len(members)} sessions by {people} {'person' if people == 1 else 'people'} worked on {topic} with no skill to hand off.",
                type_hint="procedure" if any(t in PROCEDURAL for t in top) else "workflow",
                session_ids=members,
                topic_tokens=top,
            )
        )

    out.sort(key=lambda s: -len(s.session_ids))
    per_kind: dict[str, int] = {}
    kept: list[Suggestion] = []
    for s in out:
        if per_kind.get(s.kind, 0) >= MAX_PER_KIND:
            continue
        per_kind[s.kind] = per_kind.get(s.kind, 0) + 1
        kept.append(s)
    return kept


# --- reading the corpus from the tapes read API -------------------------------


class Api:
    def __init__(self, base: str) -> None:
        self.base = base.rstrip("/")

    def get(self, path: str, **query: Any) -> Any:
        url = self.base + path
        if query:
            url += "?" + urllib.parse.urlencode({k: v for k, v in query.items() if v is not None})
        with urllib.request.urlopen(url, timeout=60) as resp:
            return json.load(resp)

    def post(self, path: str, body: Any, timeout: int = 120) -> Any:
        data = json.dumps(body).encode()
        req = urllib.request.Request(
            self.base + path, data=data, headers={"Content-Type": "application/json"}, method="POST"
        )
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.load(resp)

    def text(self, path: str) -> str:
        with urllib.request.urlopen(self.base + path, timeout=60) as resp:
            return resp.read().decode("utf-8")


def session_tokens(traces: dict[str, Any]) -> set[str]:
    """Prompts, previews, and tool names stand in for summary topics.

    Empty when the session is only the harness reviewing itself.
    """
    texts: list[str] = []
    models: set[str] = set()
    for entry in traces.get("traces") or []:
        trace = entry.get("trace") or {}
        texts.append(trace.get("user_prompt") or "")
        texts.append(trace.get("response_preview") or "")
        for span in entry.get("spans") or []:
            if span.get("kind") == "tool":
                texts.append(span.get("name") or "")
            elif span.get("kind") == "llm" and span.get("model"):
                models.add(span["model"])
    if models and models <= AUTO_REVIEW_MODELS:
        return set()
    return tokenize(texts, PROMPT_NOISE)


def load_sessions(api: Api, limit: int | None, min_tokens: int, log) -> list[Session]:
    out: list[Session] = []
    seen: set[str] = set()  # one entry per harness session, whatever it was filed under
    cursor: str | None = None
    while True:
        page = api.get("/v1/sessions", limit=200, cursor=cursor)
        for row in page.get("items") or []:
            key = row.get("harness_session_id") or row["id"]
            if key in seen or not (row.get("rollup") or {}).get("turn_count"):
                continue
            seen.add(key)
            tokens = session_tokens(api.get(f"/v1/sessions/{row['id']}/traces"))
            if len(tokens) < min_tokens:
                continue
            out.append(
                Session(
                    id=row["id"],
                    title=row.get("display_title") or row["id"],
                    author=row.get("auth_subject") or "",
                    project=(row.get("cwd") or "").rstrip("/").rsplit("/", 1)[-1],
                    tokens=tokens,
                )
            )
            if limit and len(out) >= limit:
                return out
        cursor = page.get("next_cursor")
        log(f"read {len(out)} sessions with turns")
        if not cursor:
            return out


def load_skills(api: Api) -> list[Skill]:
    try:
        page = api.get("/v1/cassettes/skills")
    except urllib.error.HTTPError as err:
        if err.code == 404:
            return []  # no skills cassette on this deployment
        raise
    return [
        Skill(
            id=row["id"],
            slug=row.get("slug") or "",
            name=row.get("name") or "",
            description=row.get("description") or "",
            tags=list(row.get("tags") or []),
            source_session_ids=list(row.get("originatingSessionIds") or []),
        )
        for row in page.get("items") or []
    ]


# --- output ---------------------------------------------------------------------


def render(suggestions: list[Suggestion], sessions: dict[str, Session], api_url: str) -> str:
    if not suggestions:
        return "no clusters cleared the floor; try --min-sessions 2 or a wider corpus\n"
    lines: list[str] = []
    for n, s in enumerate(suggestions, 1):
        lines.append(f"{n}. [{s.kind}] {s.title}")
        lines.append(f"   {s.why}")
        for sid in s.session_ids[:6]:
            sess = sessions[sid]
            lines.append(f"   - {sid}  {sess.project}: {sess.title[:60]}")
        if len(s.session_ids) > 6:
            lines.append(f"   - … {len(s.session_ids) - 6} more")
        if s.kind == "new":
            body = json.dumps({"sessionIds": s.session_ids[:3], "hint": {"type": "workflow"}})
            lines.append(f"   tapesctl cassettes skills generate-skill --api-url {api_url} --body '{body}'")
        else:
            lines.append(f"   skill: {s.skill.slug} ({s.skill.id})")
        lines.append("")
    return "\n".join(lines)


def generate(api: Api, suggestions: list[Suggestion], out: Path, log) -> int:
    written = 0
    for s in suggestions:
        if s.kind != "new":
            continue
        try:
            skill = api.post("/v1/cassettes/skills/generate", {"sessionIds": s.session_ids[:3], "hint": {"type": "workflow"}})
        except urllib.error.HTTPError as err:
            log(f"generate failed for {s.title!r}: {err.code} {err.read().decode(errors='replace')[:200]}")
            continue
        markdown = api.text(f"/v1/cassettes/skills/{skill['id']}/skill.md")
        dest = out / skill["slug"] / "SKILL.md"
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text(markdown, encoding="utf-8")
        log(f"wrote {dest}")
        written += 1
    return written


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--api-url", default="http://127.0.0.1:18081", help="tapes read API (default: the sandbox)")
    parser.add_argument("--min-sessions", type=int, default=MIN_SESSIONS, help=f"cluster floor (default {MIN_SESSIONS})")
    parser.add_argument("--min-tokens", type=int, default=MIN_TOKENS, help=f"skip sessions with fewer distinct tokens (default {MIN_TOKENS})")
    parser.add_argument("--limit", type=int, default=None, help="stop after this many sessions (newest first)")
    parser.add_argument("--generate", action="store_true", help="generate a skill for each new cluster")
    parser.add_argument("--out", type=Path, default=Path("tapes-skills"), help="where --generate writes <slug>/SKILL.md")
    parser.add_argument("--json", action="store_true", help="print suggestions as JSON instead of text")
    args = parser.parse_args(argv)

    log = lambda msg: print(f"recommend-skills: {msg}", file=sys.stderr)  # noqa: E731
    api = Api(args.api_url)
    try:
        sessions = load_sessions(api, args.limit, args.min_tokens, log)
        skills = load_skills(api)
    except (urllib.error.URLError, OSError) as err:
        log(f"cannot read {args.api_url}: {err}")
        return 1
    log(f"{len(sessions)} sessions, {len(skills)} existing skills")

    suggestions = detect(sessions, skills, args.min_sessions)
    by_id = {s.id: s for s in sessions}
    if args.json:
        print(json.dumps([{**s.__dict__, "skill": s.skill.__dict__ if s.skill else None} for s in suggestions], indent=2))
        return 0
    if args.generate:
        # Generating is the product. The cluster is how sessions get grouped,
        # and its label is four frequent words — worth nothing next to the
        # skill the model writes from the sessions themselves, so it stays out
        # of the way. Drop --generate to inspect the clusters instead.
        written = generate(api, suggestions, args.out, log)
        if not written:
            covered = [s for s in suggestions if s.kind == "evaluate"]
            if covered:
                print(f"nothing new: {len(covered)} group(s) match a skill you already have")
            else:
                print("no group of 3 or more sessions repeated the same work")
        return 0
    print(render(suggestions, by_id, args.api_url), end="")
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Rewrite Codex rollouts into a Claude-shaped projects tree for `tapesctl sync`.

Quick path for PCC-1415. `tapesctl sync` sweeps `<root>/<project>/<session>.jsonl`
and the tapes server derives Claude Code records. Codex keeps its history at
`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` with the content one level down
under `payload`, so the sweep finds nothing there. This script bridges the two
on disk; nothing in tapes changes:

    codex-to-claude-tree.py --out /tmp/codex-as-claude --since-days 30
    tapesctl sync --projects-root /tmp/codex-as-claude --since-days 0 \\
        --harness-id codex --ingest-url http://127.0.0.1:18082

Carried per session: session id, cwd, Codex CLI version, the model per turn
(from `turn_context`), user prompts, assistant text, reasoning summaries as
thinking blocks, tool calls with parsed arguments, tool results, and per-call
input / cached / output / reasoning token counts (from `token_count`).

Dropped: developer and system messages, encrypted reasoning content, and the
reasoning-effort setting, which has no field in the Claude record shape.

A resumed Codex thread writes a second rollout under the same session id; the
later rollout (by path order) wins and the run reports it as `resumed`.

Exit status is 1 when no session was written, so an empty or wrong `--codex-root`
does not read as a clean run. Stdlib only; tests in `test_codex_to_claude_tree.py`.
"""

from __future__ import annotations

import argparse
import json
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

Record = dict[str, Any]


def encode_cwd(cwd: str) -> str:
    """Project directory name for a cwd.

    Byte-for-byte the Rust `tapes_harnesses::attribution::claude::fork_parent::
    encode_cwd`: only `/` and `.` become `-`. The sweep never decodes it, so the
    encoding only has to be stable.
    """
    return cwd.replace("/", "-").replace(".", "-")


@dataclass
class Session:
    """One converted rollout."""

    session_id: str
    cwd: str | None
    version: str | None
    model: str | None
    records: list[Record] = field(default_factory=list)


@dataclass
class Summary:
    """Counts for one run, in the order the report prints them."""

    written: int = 0
    resumed: int = 0
    skipped_no_meta: int = 0
    skipped_empty: int = 0
    bad_lines: int = 0

    def render(self) -> str:
        parts = [f"wrote {self.written} session(s)"]
        if self.resumed:
            parts.append(f"{self.resumed} resumed (later rollout kept)")
        if self.skipped_no_meta:
            parts.append(f"{self.skipped_no_meta} skipped: no session_meta")
        if self.skipped_empty:
            parts.append(f"{self.skipped_empty} skipped: no records")
        if self.bad_lines:
            parts.append(f"{self.bad_lines} unparseable line(s) ignored")
        return ", ".join(parts)


def text_blocks(content: Any) -> list[Record]:
    """Codex message content parts to Claude text blocks; non-text parts drop."""
    if isinstance(content, str):
        return [{"type": "text", "text": content}] if content else []
    out: list[Record] = []
    for part in content or []:
        if not isinstance(part, dict):
            continue
        if part.get("type") in ("input_text", "output_text", "text"):
            out.append({"type": "text", "text": part.get("text", "")})
    return out


def tool_arguments(payload: Record) -> Record:
    raw = payload.get("arguments")
    if raw is None:
        raw = payload.get("input")
    if isinstance(raw, dict):
        return raw
    if isinstance(raw, str):
        try:
            parsed = json.loads(raw)
        except ValueError:
            return {"raw": raw}
        return parsed if isinstance(parsed, dict) else {"raw": parsed}
    return {}


class _Builder:
    """Accumulates Claude-shaped records for one rollout.

    Every record gets a fresh uuid chained to the previous one through
    parentUuid, which is what the server's transcript projection walks. The
    top-level cwd / version fields are what `tapesctl sync` reads to fill the
    upload envelope; Claude Code stamps them on every line, so we do too.
    """

    def __init__(self, session_id: str, cwd: str | None, version: str | None) -> None:
        self.session_id = session_id
        self.cwd = cwd
        self.version = version
        self.model: str | None = None
        self.records: list[Record] = []
        self._seq = 0
        self._parent: str | None = None
        self._last_assistant: int | None = None

    def _base(self, timestamp: str, kind: str) -> Record:
        self._seq += 1
        uuid = f"{self.session_id}-{self._seq:05d}"
        rec: Record = {
            "type": kind,
            "uuid": uuid,
            "parentUuid": self._parent,
            "timestamp": timestamp,
            "sessionId": self.session_id,
            "cwd": self.cwd,
            "version": self.version,
            "isSidechain": False,
        }
        self._parent = uuid
        return rec

    def user(self, timestamp: str, content: list[Record]) -> None:
        rec = self._base(timestamp, "user")
        rec["message"] = {"role": "user", "content": content}
        self.records.append(rec)

    def assistant(self, timestamp: str, content: list[Record], stop_reason: str) -> None:
        rec = self._base(timestamp, "assistant")
        rec["message"] = {
            "id": f"msg-{self._seq}",
            "role": "assistant",
            "model": self.model or "codex",
            "content": content,
            "stop_reason": stop_reason,
            "usage": {},
        }
        self.records.append(rec)
        self._last_assistant = len(self.records) - 1

    def usage(self, last: Record) -> None:
        """Attach a `token_count` to the assistant record it follows.

        Codex reports usage after the response items it covers, one report per
        API call, so the most recent assistant record is the one it belongs to.
        A report with no assistant record yet (a prompt-only rollout) is dropped.
        """
        if self._last_assistant is None:
            return
        self.records[self._last_assistant]["message"]["usage"] = {
            "input_tokens": last.get("input_tokens", 0),
            "cache_read_input_tokens": last.get("cached_input_tokens", 0),
            "output_tokens": last.get("output_tokens", 0),
            "reasoning_output_tokens": last.get("reasoning_output_tokens", 0),
        }


def _apply(builder: _Builder, record: Record) -> None:
    kind = record.get("type")
    payload = record.get("payload") or {}
    timestamp = record.get("timestamp", "")

    if kind == "turn_context":
        builder.model = payload.get("model") or builder.model
        return

    if kind == "event_msg":
        if payload.get("type") == "token_count":
            last = (payload.get("info") or {}).get("last_token_usage") or {}
            if last:
                builder.usage(last)
        return

    if kind != "response_item":
        return

    item = payload.get("type")
    if item == "message":
        blocks = text_blocks(payload.get("content"))
        if not blocks:
            return
        role = payload.get("role")
        if role == "user":
            builder.user(timestamp, blocks)
        elif role == "assistant":
            builder.assistant(timestamp, blocks, "end_turn")
        # developer / system messages: the projection ignores Claude's system
        # records too, so there is nothing to carry.
    elif item == "reasoning":
        summary = " ".join(
            part.get("text", "")
            for part in payload.get("summary") or []
            if isinstance(part, dict)
        ).strip()
        if summary:
            builder.assistant(timestamp, [{"type": "thinking", "thinking": summary}], "tool_use")
    elif item in ("function_call", "custom_tool_call"):
        builder.assistant(
            timestamp,
            [
                {
                    "type": "tool_use",
                    "id": payload.get("call_id"),
                    "name": payload.get("name"),
                    "input": tool_arguments(payload),
                }
            ],
            "tool_use",
        )
    elif item in ("function_call_output", "custom_tool_call_output"):
        output = payload.get("output")
        if not isinstance(output, str):
            output = json.dumps(output)
        builder.user(
            timestamp,
            [{"type": "tool_result", "tool_use_id": payload.get("call_id"), "content": output}],
        )


def convert(lines: Iterable[str], summary: Summary) -> Session | None:
    """Convert one rollout's JSONL lines. Returns None when there is nothing to write."""
    builder: _Builder | None = None
    for line in lines:
        line = line.strip()
        if not line:
            continue
        try:
            record = json.loads(line)
        except ValueError:
            summary.bad_lines += 1
            continue
        if not isinstance(record, dict):
            summary.bad_lines += 1
            continue
        if record.get("type") == "session_meta":
            payload = record.get("payload") or {}
            session_id = payload.get("session_id") or payload.get("id")
            if session_id:
                builder = _Builder(session_id, payload.get("cwd"), payload.get("cli_version"))
            continue
        if builder is not None:
            _apply(builder, record)

    if builder is None:
        summary.skipped_no_meta += 1
        return None
    if not builder.records:
        summary.skipped_empty += 1
        return None
    return Session(
        session_id=builder.session_id,
        cwd=builder.cwd,
        version=builder.version,
        model=builder.model,
        records=builder.records,
    )


def rollouts(codex_root: Path, since_days: int) -> list[Path]:
    """Rollouts under `codex_root`, oldest path first, filtered by mtime."""
    cutoff = time.time() - since_days * 86400 if since_days else 0
    return [
        path
        for path in sorted(codex_root.rglob("rollout-*.jsonl"))
        if not cutoff or path.stat().st_mtime >= cutoff
    ]


def write_session(out: Path, session: Session) -> Path:
    project = out / encode_cwd(session.cwd or "unknown")
    project.mkdir(parents=True, exist_ok=True)
    dest = project / f"{session.session_id}.jsonl"
    with dest.open("w", encoding="utf-8") as handle:
        for record in session.records:
            handle.write(json.dumps(record) + "\n")
    return dest


def run(codex_root: Path, out: Path, since_days: int, verbose: bool) -> Summary:
    summary = Summary()
    seen: set[str] = set()
    for rollout in rollouts(codex_root, since_days):
        with rollout.open(encoding="utf-8") as handle:
            session = convert(handle, summary)
        if session is None:
            continue
        if session.session_id in seen:
            summary.resumed += 1
        seen.add(session.session_id)
        dest = write_session(out, session)
        summary.written += 1
        if verbose:
            print(
                f"{dest}  records={len(session.records)} model={session.model} cwd={session.cwd}",
                file=sys.stderr,
            )
    return summary


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument(
        "--codex-root",
        type=Path,
        default=Path.home() / ".codex" / "sessions",
        help="Codex sessions tree (default: ~/.codex/sessions)",
    )
    parser.add_argument("--out", type=Path, required=True, help="Claude-shaped tree to write")
    parser.add_argument(
        "--since-days",
        type=int,
        default=30,
        help="only rollouts modified within this many days; 0 = everything (default: 30)",
    )
    parser.add_argument("-v", "--verbose", action="store_true", help="print one line per session")
    args = parser.parse_args(argv)

    if not args.codex_root.is_dir():
        print(f"codex-to-claude-tree: no such directory: {args.codex_root}", file=sys.stderr)
        return 1

    summary = run(args.codex_root, args.out, args.since_days, args.verbose)
    print(f"codex-to-claude-tree: {summary.render()}", file=sys.stderr)
    if summary.written == 0:
        print(
            f"codex-to-claude-tree: nothing written from {args.codex_root} "
            f"(since-days={args.since_days}); widen --since-days or check the path",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

"""Build the PARADOX entity relationship diagram and data dictionary.

The entity list and links below are the single source for both outputs:
- docs/diagrams/paradox-erd.svg (and paradox-erd.dot, kept for diffs);
- the data dictionary in docs/prd.md, between the markers
  <!-- dictionary:start --> and <!-- dictionary:end -->.

Run from the repository root:  python3 docs/diagrams/build_erd.py
Needs Graphviz (the `dot` command) on PATH. No other dependencies.
"""

import pathlib
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[2]
OUT_DIR = ROOT / "docs" / "diagrams"
PRD = ROOT / "docs" / "prd.md"

# Subject areas: (key, title, colour)
AREAS = [
    ("systems", "Systems", "#1E3A8A"),
    ("intake", "Intake", "#B45309"),
    ("runs", "Runs", "#0E7490"),
    ("operation", "Operation", "#64748B"),
]

# Entities: name -> area, keys (shown in the box), fields (dictionary only).
# Keys are (marker, column, note); fields are (column, description).
ENTITIES = {
    "system": {
        "area": "systems",
        "keys": [("PK", "id", "")],
        "fields": [
            ("name", "text, unique; the system's folder name"),
            ("wake_mode", "sablier or none (default: sablier)"),
            ("address", "text, optional; only for systems off the internal network"),
            ("first_seen_at", "timestamp (UTC)"),
            ("removed_at", "timestamp (UTC), optional; set when the folder disappears, so old runs still resolve"),
        ],
    },
    "job": {
        "area": "systems",
        "keys": [("PK", "id", ""), ("FK", "system_id", "")],
        "fields": [
            ("name", "text, unique per system"),
            ("repeat", "repeating time to the minute, optional; none means the job is only queued"),
            ("timeout_s", "seconds (default: 600)"),
            ("retries", "number (default: 3, so 4 attempts)"),
            ("next_due_at", "timestamp (UTC), optional"),
            ("last_due_at", "timestamp (UTC), optional; used to catch up a missed job"),
        ],
    },
    "queue_request": {
        "area": "intake",
        "keys": [("PK FK", "system_id", ""), ("PK", "request_id", ""), ("FK", "run_id", "")],
        "fields": [
            ("received_at", "timestamp (UTC)"),
            ("key_fingerprint", "text; which of the system's public keys signed it"),
        ],
        "note": "Deleted after 24 hours (default). Public keys themselves are not stored; the system.pub files are the source.",
    },
    "run": {
        "area": "runs",
        "keys": [("PK", "id", ""), ("FK", "job_id", "")],
        "fields": [
            ("source", "schedule or queue"),
            ("data", "job data, max 64 KB (default); wiped when the run ends"),
            ("due_at", "timestamp (UTC)"),
            ("state", "queued, running, succeeded, failed, cancelled, skipped or missed"),
            ("max_attempts", "number (retries + 1)"),
            ("attempts", "number (0 to max_attempts)"),
            ("started_at", "timestamp (UTC), optional"),
            ("finished_at", "timestamp (UTC), optional"),
            ("last_error", "text, max 2 KB, optional"),
            ("reported_at", "timestamp (UTC), optional; when the failure report was sent"),
        ],
        "note": "Deleted after 90 days (default). The ID the queue API returns is run.id.",
    },
    "attempt": {
        "area": "runs",
        "keys": [("PK", "id", ""), ("FK", "run_id", "")],
        "fields": [
            ("number", "number (1 to max_attempts)"),
            ("started_at", "timestamp (UTC)"),
            ("ended_at", "timestamp (UTC), optional"),
            ("outcome", "success, error, timeout or wake_timeout"),
            ("status_code", "number, optional; the system's answer"),
            ("error", "text, max 2 KB, optional"),
        ],
        "note": "Deleted with its run.",
    },
    "run_action": {
        "area": "operation",
        "keys": [("PK", "id", ""), ("FK", "run_id", ""), ("FK", "by_system_id", "optional")],
        "fields": [
            ("action", "retry or cancel"),
            ("acted_at", "timestamp (UTC)"),
        ],
        "note": "by_system_id is empty when the operator acted from the command line. Deleted with its run.",
    },
}

# Links from child to parent: (child, parent, kind, label)
# kind: "many" = many to one; "optional" = many to zero or one (dashed);
#       "one" = one to zero or one (bar on both ends, labelled).
LINKS = [
    ("job", "system", "many", ""),
    ("queue_request", "system", "many", ""),
    ("queue_request", "run", "one", "0..1"),
    ("run", "job", "many", ""),
    ("attempt", "run", "many", ""),
    ("run_action", "run", "many", ""),
    ("run_action", "system", "optional", ""),
]


def esc(text):
    return text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def box(name, spec, colour):
    rows = [f'<tr><td bgcolor="{colour}" colspan="2"><font color="#FFFFFF"><b>{name}</b></font></td></tr>']
    for marker, column, note in spec["keys"]:
        extra = f' <font point-size="8">({note})</font>' if note else ""
        rows.append(
            f'<tr><td align="left"><font point-size="9">{marker}</font></td>'
            f'<td align="left">{esc(column)}{extra}</td></tr>'
        )
    return f'<<table border="0" cellborder="1" cellspacing="0" cellpadding="4" color="{colour}">{"".join(rows)}</table>>'


def build_dot():
    colours = {key: colour for key, _, colour in AREAS}
    lines = [
        "// Generated by build_erd.py. Edit the script, not this file.",
        "digraph paradox_erd {",
        "  rankdir=BT; splines=ortho; newrank=true; nodesep=0.6; ranksep=0.8;",
        '  fontname="Helvetica"; node [shape=plain, fontname="Helvetica", fontsize=11];',
        '  edge [fontname="Helvetica", fontsize=10, color="#334155", penwidth=1.2, dir=both, arrowtail=crow, arrowhead=tee];',
    ]
    for key, title, colour in AREAS:
        lines.append(f"  subgraph cluster_{key} {{")
        lines.append(f'    label="{title}"; style="dashed"; color="{colour}"; fontcolor="{colour}";')
        for name, spec in ENTITIES.items():
            if spec["area"] == key:
                lines.append(f"    {name} [label={box(name, spec, colour)}];")
        lines.append("  }")
    for child, parent, kind, label in LINKS:
        attrs = []
        if kind == "optional":
            attrs.append("style=dashed")
        if kind == "one":
            attrs.append("arrowtail=tee")
        if label:
            attrs.append(f'xlabel="{label}"')
        lines.append(f"  {child} -> {parent} [{', '.join(attrs)}];")
    lines.append("}")
    return "\n".join(lines) + "\n"


def build_dictionary():
    out = []
    for key, title, _ in AREAS:
        out.append(f"#### {title}\n")
        out.append("| Entity | Fields |")
        out.append("|---|---|")
        for name, spec in ENTITIES.items():
            if spec["area"] != key:
                continue
            if spec["fields"]:
                fields = "<br>".join(f"`{col}`: {desc}" for col, desc in spec["fields"])
            else:
                fields = "keys only"
            if spec.get("note"):
                fields += f"<br>*{spec['note']}*"
            out.append(f"| `{name}` | {fields} |")
        out.append("")
    return "\n".join(out)


def check():
    names = set(ENTITIES)
    for child, parent, _, _ in LINKS:
        assert child in names and parent in names, f"unknown entity in link {child} -> {parent}"
    for name, spec in ENTITIES.items():
        fks = [c for m, c, _ in spec["keys"] if "FK" in m]
        linked = [p for c, p, _, _ in LINKS if c == name]
        assert len(fks) == len(linked), f"{name}: {len(fks)} foreign keys but {len(linked)} drawn links"
    return len(ENTITIES), len(LINKS)


def main():
    entities, links = check()
    dot_text = build_dot()
    (OUT_DIR / "paradox-erd.dot").write_text(dot_text)
    subprocess.run(
        ["dot", "-Tsvg", str(OUT_DIR / "paradox-erd.dot"), "-o", str(OUT_DIR / "paradox-erd.svg")],
        check=True,
    )
    prd = PRD.read_text()
    start, end = "<!-- dictionary:start -->", "<!-- dictionary:end -->"
    head, rest = prd.split(start, 1)
    _, tail = rest.split(end, 1)
    PRD.write_text(f"{head}{start}\n\n{build_dictionary()}\n{end}{tail}")
    print(f"{entities} entities, {links} links; wrote paradox-erd.svg and the PRD dictionary")


if __name__ == "__main__":
    main()

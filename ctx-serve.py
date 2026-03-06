#!/usr/bin/env python3
# ~/.context/ctx-serve.py
# ctx UI — Compile, Learnings, Synthesize, Candidates, Graph, Namespace
# FastAPI + Pico.css + Mermaid.js + marked.js. No npm.
# http://localhost:7337

import json, sqlite3, sys, os, re, subprocess
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path.home() / ".context"))

try:
    from fastapi import FastAPI, Request, UploadFile, File, Form
    from fastapi.responses import HTMLResponse, JSONResponse, PlainTextResponse
    import uvicorn
except ImportError:
    print("ctx-serve: fastapi and uvicorn required.", file=sys.stderr)
    print("  Install with: pip3 install fastapi uvicorn python-multipart", file=sys.stderr)
    sys.exit(1)

try:
    import toml
except ImportError:
    import tomllib as toml

CONTEXT_DIR = Path.home() / ".context"
DB_PATH     = CONTEXT_DIR / "index.db"

app = FastAPI()

def _config():
    return toml.load(CONTEXT_DIR / "ctx.toml")

def get_db():
    return sqlite3.connect(DB_PATH)


# ── API: Compile ───────────────────────────────────────────────────────────────

@app.post("/api/compile")
async def api_compile(request: Request):
    body = await request.json()
    task = body.get("task", "")
    tag = body.get("tag", "")
    if not task and not tag:
        return JSONResponse({"error": "task or tag required"}, status_code=400)
    try:
        cmd = [sys.executable, str(CONTEXT_DIR / "ctx-compile.py")]
        if tag:
            cmd.extend(["--tag", tag])
        if task:
            cmd.append(task)
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=180)
        slug = re.sub(r'[^a-z0-9\s-]', '', (task or f"all knowledge for {tag}").lower().strip())
        slug = re.sub(r'[\s]+', '-', slug)
        slug = re.sub(r'-+', '-', slug)[:60].rstrip('-')
        json_path = CONTEXT_DIR / "compiled" / f"{slug}.json"
        md_path = CONTEXT_DIR / "compiled" / f"{slug}.md"
        if json_path.exists():
            return {"structured": json.loads(json_path.read_text()), "bundle": md_path.read_text() if md_path.exists() else ""}
        if md_path.exists():
            return {"bundle": md_path.read_text(), "structured": None}
        return {"bundle": result.stdout, "error": result.stderr if result.returncode != 0 else None}
    except Exception as e:
        return JSONResponse({"error": str(e)}, status_code=500)


# ── API: Synthesize ────────────────────────────────────────────────────────────

@app.post("/api/synthesize")
async def api_synthesize(request: Request):
    body = await request.json()
    sources = body.get("sources", [])
    output = body.get("output", "")
    synth_type = body.get("type", "skill")
    context = body.get("context", "")
    refine = body.get("refine", "")
    feedback = body.get("feedback", "")
    if not sources and not refine:
        return JSONResponse({"error": "sources required"}, status_code=400)
    if not output and not refine:
        return JSONResponse({"error": "output path required"}, status_code=400)
    cmd = [sys.executable, str(CONTEXT_DIR / "ctx-synthesize.py")]
    for s in sources:
        cmd.extend(["--source", s])
    if output:
        cmd.extend(["--output", output])
    if synth_type:
        cmd.extend(["--type", synth_type])
    if context:
        cmd.extend(["--context", context])
    if refine:
        cmd.extend(["--refine", refine])
    if feedback:
        cmd.extend(["--feedback", feedback])
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=180)
        return {"stdout": result.stdout, "stderr": result.stderr, "ok": result.returncode == 0}
    except Exception as e:
        return JSONResponse({"error": str(e)}, status_code=500)


# ── API: Tags ──────────────────────────────────────────────────────────────────

@app.get("/api/tags")
def list_tags():
    conn = get_db()
    rows = conn.execute("SELECT DISTINCT project_tag FROM summaries WHERE project_tag != '' ORDER BY project_tag").fetchall()
    conn.close()
    return [r[0] for r in rows]


# ── API: Stats ─────────────────────────────────────────────────────────────────

@app.get("/api/stats")
def get_stats():
    conn = get_db()
    stats = {}
    for s in ("pending", "approved", "dismissed"):
        stats[s] = conn.execute("SELECT COUNT(*) FROM candidates WHERE status = ?", (s,)).fetchone()[0]
    stats["traces"] = conn.execute("SELECT COUNT(*) FROM traces").fetchone()[0]
    stats["summaries"] = conn.execute("SELECT COUNT(*) FROM summaries").fetchone()[0]
    stats["embeddings"] = conn.execute("SELECT COUNT(*) FROM embeddings").fetchone()[0]
    try:
        stats["chunks"] = conn.execute("SELECT COUNT(*) FROM chunks").fetchone()[0]
    except Exception:
        stats["chunks"] = 0
    try:
        stats["knowledge"] = conn.execute("SELECT COUNT(*) FROM knowledge").fetchone()[0]
    except Exception:
        stats["knowledge"] = 0
    conn.close()
    return stats


# ── API: Traces / Learnings ────────────────────────────────────────────────────

@app.get("/api/traces")
def list_traces():
    conn = get_db()
    rows = conn.execute(
        "SELECT t.id, t.ts, t.source, t.status, t.project, t.prompt, t.files_modified, t.loop_count, "
        "s.problem, s.pattern, s.skills, s.pitfalls, s.embedding_text, "
        "s.project_tag, s.component, s.domain_knowledge "
        "FROM traces t LEFT JOIN summaries s ON t.id = s.trace_id "
        "ORDER BY t.ts DESC"
    ).fetchall()
    conn.close()
    cols = ["id", "ts", "source", "status", "project", "prompt", "files_modified", "loop_count",
            "problem", "pattern", "skills", "pitfalls", "embedding_text",
            "project_tag", "component", "domain_knowledge"]
    return [dict(zip(cols, r)) for r in rows]


@app.get("/api/traces/by-skill/{slug}")
def traces_by_skill(slug: str):
    conn = get_db()
    rows = conn.execute(
        "SELECT t.id, t.ts, t.source, t.project, t.prompt, "
        "s.problem, s.pattern, s.skills, s.embedding_text "
        "FROM traces t JOIN summaries s ON t.id = s.trace_id "
        "WHERE s.skills LIKE ? ORDER BY t.ts DESC", (f'%"{slug}"%',)
    ).fetchall()
    conn.close()
    cols = ["id", "ts", "source", "project", "prompt", "problem", "pattern", "skills", "embedding_text"]
    return [dict(zip(cols, r)) for r in rows]


# ── API: Delete ────────────────────────────────────────────────────────────────

@app.post("/api/delete")
async def delete_item(request: Request):
    """Delete a trace, file, or knowledge item. Removes from disk + all DB tables."""
    body = await request.json()
    trace_id = body.get("trace_id")
    file_path = body.get("path")
    conn = get_db()
    deleted = []
    if trace_id:
        conn.execute("DELETE FROM embeddings WHERE trace_id = ?", (trace_id,))
        conn.execute("DELETE FROM summaries WHERE trace_id = ?", (trace_id,))
        conn.execute("DELETE FROM traces WHERE id = ?", (trace_id,))
        conn.execute("DELETE FROM chunks WHERE source_id = ?", (trace_id,))
        deleted.append(f"db:{trace_id}")
        learn_dir = CONTEXT_DIR / "learnings"
        if learn_dir.exists():
            for md in learn_dir.glob("*.md"):
                if f"id: {trace_id}" in md.read_text()[:500]:
                    md.unlink()
                    deleted.append(f"file:{md.name}")
                    break
    if file_path:
        full = CONTEXT_DIR / file_path
        if full.exists() and str(full.resolve()).startswith(str(CONTEXT_DIR.resolve())):
            full.unlink()
            deleted.append(f"file:{file_path}")
        source_id_candidates = [
            f"ns-{file_path.replace('/', '-').replace('.md', '')}",
            f"synth-skill_external-{Path(file_path).parent.name}",
            f"synth-skill_internal-{Path(file_path).parent.name}",
            f"synth-pattern-{Path(file_path).parent.name}",
        ]
        for sid in source_id_candidates:
            conn.execute("DELETE FROM chunks WHERE source_id = ?", (sid,))
            conn.execute("DELETE FROM knowledge WHERE id = ?", (sid,))
    conn.commit()
    conn.close()
    return {"deleted": deleted}


# ── API: Candidates ────────────────────────────────────────────────────────────

@app.get("/api/candidates")
def list_candidates(status: str = None):
    conn = get_db()
    if status:
        rows = conn.execute(
            "SELECT id, type, slug, definition, status, frequency, "
            "source_traces, detected_at, resolved_at FROM candidates WHERE status = ? "
            "ORDER BY detected_at DESC", (status,)
        ).fetchall()
    else:
        rows = conn.execute(
            "SELECT id, type, slug, definition, status, frequency, "
            "source_traces, detected_at, resolved_at FROM candidates "
            "ORDER BY CASE status WHEN 'pending' THEN 0 WHEN 'approved' THEN 1 ELSE 2 END, detected_at DESC"
        ).fetchall()
    conn.close()
    cols = ["id", "type", "slug", "definition", "status", "frequency", "source_traces", "detected_at", "resolved_at"]
    return [dict(zip(cols, r)) for r in rows]


@app.post("/api/candidates/{cand_id}/approve")
async def approve_candidate(cand_id: str):
    conn = get_db()
    row = conn.execute("SELECT id, type, slug, definition, frequency, source_traces FROM candidates WHERE id = ?", (cand_id,)).fetchone()
    if not row:
        conn.close()
        return JSONResponse({"error": "not found"}, status_code=404)
    _, cand_type, slug, definition, frequency, src_json = row
    now = datetime.now(timezone.utc).isoformat()
    ns_map = {"skill": ("skills/internal", "skill_internal"), "pattern": ("patterns", "pattern"),
              "debugging": ("debugging", "debugging"), "primitive": ("primitives", "primitive")}
    dir_name, ns_key = ns_map.get(cand_type, ("patterns", "pattern"))
    target_dir = CONTEXT_DIR / dir_name
    if cand_type == "skill":
        target_dir = target_dir / slug
    target_dir.mkdir(parents=True, exist_ok=True)
    filename = "skill.md" if cand_type == "skill" else f"{slug}.md"
    target_path = target_dir / filename
    try:
        source_traces = json.loads(src_json) if src_json else []
    except (json.JSONDecodeError, TypeError):
        source_traces = []
    fm = f"---\nid: {ns_key}-{slug}\nsource: internal\npromoted_from: [{', '.join(source_traces)}]\npromoted_at: {now[:10]}\nfrequency: {frequency}\ntags: [{slug}]\n---\n"
    target_path.write_text(f"{fm}\n# {slug.replace('-',' ').title()}\n\n{definition}\n")
    conn.execute("UPDATE candidates SET status='approved', resolved_at=? WHERE id=?", (now, cand_id))
    kid = f"{ns_key}-{slug}"
    conn.execute(
        "INSERT INTO knowledge (id,namespace,slug,file_path,summary,tags,source,frequency,promoted_at) "
        "VALUES (?,?,?,?,?,?,'internal',?,?) ON CONFLICT(id) DO UPDATE SET summary=excluded.summary,frequency=excluded.frequency,promoted_at=excluded.promoted_at",
        (kid, ns_key, slug, str(target_path), definition, json.dumps([slug]), frequency, now))
    conn.commit()
    conn.close()
    return {"status": "approved", "file": str(target_path)}


@app.post("/api/candidates/{cand_id}/dismiss")
async def dismiss_candidate(cand_id: str, request: Request):
    body = await request.json() if request.headers.get("content-type") == "application/json" else {}
    reason = body.get("reason", "")
    conn = get_db()
    now = datetime.now(timezone.utc).isoformat()
    conn.execute("UPDATE candidates SET status='dismissed', resolved_at=?, definition = CASE WHEN ?!='' THEN definition||'\n\n[DISMISSED: '||?||']' ELSE definition END WHERE id=?", (now, reason, reason, cand_id))
    conn.commit()
    conn.close()
    return {"status": "dismissed"}


# ── API: Namespace ─────────────────────────────────────────────────────────────

@app.get("/api/namespace")
def list_namespaces():
    dirs = ["learnings", "candidates", "patterns", "debugging", "skills/internal", "skills/external",
            "primitives", "docs", "plans"]
    result = []
    for d in dirs:
        full = CONTEXT_DIR / d
        files = sorted(full.rglob("*.md")) if full.exists() else []
        items = [{"name": f.name, "path": str(f.relative_to(CONTEXT_DIR)),
                  "size": f.stat().st_size,
                  "mtime": datetime.fromtimestamp(f.stat().st_mtime, tz=timezone.utc).isoformat()[:16]}
                 for f in files]
        result.append({"namespace": d, "count": len(items), "files": items})
    return result


@app.get("/api/file")
def get_file(path: str = ""):
    if not path:
        return JSONResponse({"error": "path required"}, status_code=400)
    full = CONTEXT_DIR / path
    if not full.exists() or not str(full.resolve()).startswith(str(CONTEXT_DIR.resolve())):
        return JSONResponse({"error": "not found"}, status_code=404)
    return PlainTextResponse(full.read_text())


@app.post("/api/file/upload")
async def upload_file(file: UploadFile = File(...), namespace: str = Form(...)):
    valid = ["patterns", "debugging", "skills/internal", "skills/external", "primitives", "docs", "plans"]
    if namespace not in valid:
        return JSONResponse({"error": f"invalid namespace"}, status_code=400)
    target_dir = CONTEXT_DIR / namespace
    target_dir.mkdir(parents=True, exist_ok=True)
    name = file.filename or "untitled.md"
    if not name.endswith(".md"):
        name += ".md"
    target_path = target_dir / name
    target_path.write_bytes(await file.read())
    return {"status": "ok", "path": str(target_path.relative_to(CONTEXT_DIR))}


@app.post("/api/file/move")
async def move_file(request: Request):
    body = await request.json()
    src, dest = body.get("src", ""), body.get("dest", "")
    if not src or not dest:
        return JSONResponse({"error": "src and dest required"}, status_code=400)
    src_full = CONTEXT_DIR / src
    dest_full = CONTEXT_DIR / dest
    if not src_full.exists():
        return JSONResponse({"error": "source not found"}, status_code=404)
    dest_full.parent.mkdir(parents=True, exist_ok=True)
    src_full.rename(dest_full)
    return {"status": "ok"}


# ── API: Tags edit ─────────────────────────────────────────────────────────────

@app.get("/api/file/tags")
def get_file_tags(path: str = ""):
    if not path:
        return JSONResponse({"error": "path required"}, status_code=400)
    full = CONTEXT_DIR / path
    if not full.exists():
        return JSONResponse({"error": "not found"}, status_code=404)
    m = re.search(r'^tags:\s*\[([^\]]*)\]', full.read_text(), re.MULTILINE)
    if m:
        tags = [t.strip().strip('"').strip("'") for t in m.group(1).split(",") if t.strip()]
        return {"tags": tags}
    return {"tags": []}


@app.post("/api/tags/edit")
async def edit_tags(request: Request):
    body = await request.json()
    path, tags = body.get("path", ""), body.get("tags", [])
    if not path:
        return JSONResponse({"error": "path required"}, status_code=400)
    full = CONTEXT_DIR / path
    if not full.exists():
        return JSONResponse({"error": "not found"}, status_code=404)
    content = full.read_text()
    tags_str = "[" + ", ".join(tags) + "]"
    if re.search(r'^tags:\s*\[.*\]', content, re.MULTILINE):
        content = re.sub(r'^tags:\s*\[.*\]', f'tags: {tags_str}', content, count=1, flags=re.MULTILINE)
    elif "---" in content:
        parts = content.split("---", 2)
        if len(parts) >= 3:
            content = f"---{parts[1]}tags: {tags_str}\n---{parts[2]}"
    full.write_text(content)
    return {"status": "ok", "tags": tags}


# ── API: Knowledge graph ──────────────────────────────────────────────────────

@app.get("/api/graph")
def get_graph():
    conn = get_db()
    knowledge = conn.execute("SELECT id, namespace, slug, frequency FROM knowledge").fetchall()
    summaries = conn.execute("SELECT trace_id, skills FROM summaries").fetchall()
    conn.close()
    lines = ["graph LR"]
    lines.append("    classDef skill fill:#F5F5F0,stroke:#E63946,stroke-width:2px,color:#1A1A1A")
    lines.append("    classDef pattern fill:#F5F5F0,stroke:#1A1A1A,stroke-width:2px,color:#1A1A1A")
    lines.append("    classDef debugging fill:#F5F5F0,stroke:#8A8A82,stroke-width:2px,color:#1A1A1A")
    lines.append("    classDef primitive fill:#F5F5F0,stroke:#D4D4CC,stroke-width:2px,color:#1A1A1A")
    node_ids = set()
    ns_class = {"skill_internal":"skill","skill_external":"skill","pattern":"pattern","debugging":"debugging","primitive":"primitive"}
    for kid, ns, slug, freq in knowledge:
        nid = slug.replace("-","_")
        if nid not in node_ids:
            lines.append(f'    {nid}["{slug}"]:::{ns_class.get(ns,"pattern")}')
            node_ids.add(nid)
    cooccur = {}
    for _, skills_json in summaries:
        try:
            skills = json.loads(skills_json) if skills_json else []
        except (json.JSONDecodeError, TypeError):
            continue
        skills = [s.strip().lower() for s in skills if s.strip()]
        for i, a in enumerate(skills):
            for b in skills[i+1:]:
                pair = tuple(sorted([a, b]))
                cooccur[pair] = cooccur.get(pair, 0) + 1
    for (a, b), count in cooccur.items():
        if count < 2:
            continue
        for x in (a, b):
            nid = x.replace("-","_")
            if nid not in node_ids:
                lines.append(f'    {nid}["{x}"]:::skill')
                node_ids.add(nid)
        lines.append(f"    {a.replace('-','_')} --- |{count}x| {b.replace('-','_')}")
    if not node_ids:
        lines.append('    empty["No knowledge yet"]')
    return PlainTextResponse("\n".join(lines))


# ── API: Knowledge items (for synthesize tab) ─────────────────────────────────

@app.get("/api/knowledge")
def list_knowledge():
    conn = get_db()
    rows = conn.execute("SELECT id, namespace, slug, file_path, summary, tags, frequency, promoted_at FROM knowledge ORDER BY promoted_at DESC").fetchall()
    conn.close()
    cols = ["id", "namespace", "slug", "file_path", "summary", "tags", "frequency", "promoted_at"]
    return [dict(zip(cols, r)) for r in rows]


# ── HTML ───────────────────────────────────────────────────────────────────────

HTML = r"""<!DOCTYPE html>
<html lang="en" data-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ctx</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@picocss/pico@2/css/pico.min.css">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;600&family=IBM+Plex+Sans:wght@400;500;600&display=swap" rel="stylesheet">
<script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/marked/marked.min.js"></script>
<style>
:root{--pico-font-family:'IBM Plex Sans',sans-serif;--pico-font-family-monospace:'IBM Plex Mono',monospace;
--pico-background-color:#F5F5F0;--pico-color:#1A1A1A;--pico-primary:#E63946;--pico-primary-hover:#C5303C;
--pico-muted-color:#8A8A82;--pico-muted-border-color:#D4D4CC;--pico-card-background-color:#FFF;--pico-border-radius:2px;--pico-spacing:1rem}
body{background:#F5F5F0;color:#1A1A1A;max-width:1200px;margin:0 auto;padding:2rem 1rem}
h1{font-family:'IBM Plex Mono',monospace;font-size:1.5rem;font-weight:600;margin-bottom:0;letter-spacing:-0.02em}
.header{display:flex;justify-content:space-between;align-items:baseline;border-bottom:2px solid #1A1A1A;padding-bottom:.5rem;margin-bottom:1.5rem}
.hstats{font-family:'IBM Plex Mono',monospace;font-size:.8rem;color:#8A8A82}
.hstats span{margin-left:1.5rem}.hstats .hl{color:#E63946}
.tabs{display:flex;gap:0;border-bottom:1px solid #D4D4CC;margin-bottom:1.5rem}
.tab{font-family:'IBM Plex Mono',monospace;font-size:.85rem;padding:.5rem 1rem;cursor:pointer;border:none;
background:none;color:#8A8A82;border-bottom:2px solid transparent;margin-bottom:-1px}
.tab:hover{color:#1A1A1A}.tab.active{color:#1A1A1A;border-bottom-color:#E63946}
.tc{display:none}.tc.active{display:block}
table{font-size:.85rem;border-collapse:collapse;width:100%}
th{font-family:'IBM Plex Mono',monospace;font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;
color:#8A8A82;border-bottom:2px solid #1A1A1A;padding:.5rem .75rem;text-align:left;font-weight:400}
td{padding:.6rem .75rem;border-bottom:1px solid #D4D4CC;vertical-align:top}
tr:last-child td{border-bottom:none}
.mono{font-family:'IBM Plex Mono',monospace;font-size:.8rem}
.muted{color:#8A8A82}.small{font-size:.75rem}
.act{font-family:'IBM Plex Mono',monospace;font-size:.8rem;cursor:pointer;background:none;border:none;padding:0}
.act-r{color:#E63946}.act-r:hover{text-decoration:underline}
.act-m{color:#8A8A82}.act-m:hover{text-decoration:underline}
.sl{font-family:'IBM Plex Mono',monospace;font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;color:#8A8A82;margin:2rem 0 .75rem}
.empty{color:#8A8A82;font-style:italic;padding:1.5rem 0}
/* Compile */
.c-area textarea{font-family:'IBM Plex Sans',sans-serif;width:100%;border:1px solid #D4D4CC;padding:.75rem;
background:#FFF;font-size:.85rem;resize:vertical;min-height:120px}
.c-ctrl{display:flex;gap:.5rem;align-items:center;margin:.75rem 0}
.c-ctrl select{font-family:'IBM Plex Mono',monospace;font-size:.8rem;border:1px solid #D4D4CC;padding:.4rem .6rem;background:#FFF}
.c-ctrl button{font-family:'IBM Plex Mono',monospace;background:#1A1A1A;color:#F5F5F0;border:none;padding:.5rem 1rem;cursor:pointer;font-size:.85rem}
.c-ctrl button:hover{background:#E63946}
.c-result{display:none}
.c-approach{background:#FFF;border:1px solid #D4D4CC;padding:1rem;margin-bottom:1rem;font-size:.9rem;line-height:1.6}
.c-section{margin-bottom:.5rem}
.c-item{font-family:'IBM Plex Mono',monospace;font-size:.8rem;padding:.3rem 0;cursor:pointer;color:#1A1A1A}
.c-item:hover{color:#E63946}
.c-score{color:#8A8A82;font-size:.75rem}
.c-heading{color:#E63946;font-size:.75rem;font-style:italic}
.c-desc{font-family:'IBM Plex Sans',sans-serif;font-size:.8rem;color:#8A8A82;padding-left:1rem;margin-bottom:.3rem}
.c-questions{background:#FFF;border:1px solid #D4D4CC;padding:1rem;margin-top:1rem}
.c-questions li{font-size:.85rem;margin-bottom:.4rem}
.c-sources{font-family:'IBM Plex Mono',monospace;font-size:.75rem;color:#8A8A82;margin-top:1rem}
.c-sources div{cursor:pointer;padding:.15rem 0}.c-sources div:hover{color:#E63946}
/* Trace */
.trace-detail{display:none}.trace-detail.show{display:table-row}
.trace-detail td{background:#FAFAF5;padding:.75rem;font-size:.8rem;line-height:1.6}
.trace-toggle{cursor:pointer}.trace-toggle:hover{color:#E63946}
/* Synthesize */
.syn-sources{margin:1rem 0}.syn-src{display:flex;gap:.5rem;align-items:center;margin-bottom:.4rem;font-family:'IBM Plex Mono',monospace;font-size:.8rem}
.syn-src input{flex:1;border:1px solid #D4D4CC;padding:.3rem .5rem;font-family:'IBM Plex Mono',monospace;font-size:.8rem;background:#FFF}
.syn-rm{cursor:pointer;color:#8A8A82;font-size:.9rem}.syn-rm:hover{color:#E63946}
.syn-add{display:flex;gap:.5rem;margin-bottom:.75rem}
.syn-add button{font-family:'IBM Plex Mono',monospace;font-size:.75rem;background:none;border:1px solid #D4D4CC;padding:.25rem .5rem;cursor:pointer;color:#8A8A82}
.syn-add button:hover{border-color:#E63946;color:#E63946}
.syn-out{display:flex;gap:.5rem;margin-bottom:.75rem;align-items:center}
.syn-out select,.syn-out input{font-family:'IBM Plex Mono',monospace;font-size:.8rem;border:1px solid #D4D4CC;padding:.4rem .5rem;background:#FFF}
.syn-out input{flex:1}
.syn-ctx textarea{font-family:'IBM Plex Sans',sans-serif;width:100%;border:1px solid #D4D4CC;padding:.5rem;background:#FFF;font-size:.85rem;resize:vertical;min-height:60px;margin-bottom:.75rem}
.syn-result{font-family:'IBM Plex Mono',monospace;font-size:.8rem;white-space:pre-wrap;background:#FFF;border:1px solid #D4D4CC;padding:1rem;margin-top:1rem;display:none}
.syn-skills{margin-top:2rem}
/* File viewer */
.fv{display:none;position:fixed;top:0;right:0;bottom:0;width:55%;background:#FFF;border-left:2px solid #1A1A1A;padding:1.5rem;overflow-y:auto;z-index:100}
.fv.open{display:block}
.fv-hdr{display:flex;justify-content:space-between;align-items:center;margin-bottom:1rem;border-bottom:1px solid #D4D4CC;padding-bottom:.5rem}
.fv-path{font-family:'IBM Plex Mono',monospace;font-size:.8rem;color:#8A8A82}
.fv-acts{display:flex;gap:.75rem;align-items:center}
.fv-close{font-family:'IBM Plex Mono',monospace;font-size:1rem;cursor:pointer;background:none;border:none;color:#8A8A82}
.fv-close:hover{color:#E63946}
.fv-toggle{font-family:'IBM Plex Mono',monospace;font-size:.75rem;cursor:pointer;background:none;border:1px solid #D4D4CC;padding:.2rem .5rem;color:#8A8A82}
.fv-toggle.active{border-color:#E63946;color:#E63946}
.fv-body{font-family:'IBM Plex Mono',monospace;font-size:.8rem;white-space:pre-wrap;line-height:1.6}
.fv-body.rendered{font-family:'IBM Plex Sans',sans-serif;white-space:normal}
.fv-body.rendered h1{font-size:1.2rem}.fv-body.rendered h2{font-size:1.05rem}
.fv-body.rendered code{font-family:'IBM Plex Mono',monospace;font-size:.8rem;background:#F5F5F0;padding:.1rem .3rem}
.fv-body.rendered blockquote{border-left:3px solid #E63946;padding-left:.75rem;color:#8A8A82;margin-left:0}
.fv-del{margin-top:1rem;display:flex;justify-content:flex-end}
.fv-del button{font-family:'IBM Plex Mono',monospace;font-size:.75rem;background:none;border:1px solid #D4D4CC;padding:.25rem .75rem;cursor:pointer;color:#8A8A82}
.fv-del button:hover{border-color:#E63946;color:#E63946}
/* Tags */
.tag-ed{margin-top:1rem;padding-top:.75rem;border-top:1px solid #D4D4CC}
.tag-lbl{font-family:'IBM Plex Mono',monospace;font-size:.7rem;text-transform:uppercase;color:#8A8A82;letter-spacing:.05em;margin-bottom:.4rem}
.tag-list{display:flex;flex-wrap:wrap;gap:.3rem;margin-bottom:.4rem}
.tag-chip{font-family:'IBM Plex Mono',monospace;font-size:.75rem;background:#F5F5F0;border:1px solid #D4D4CC;padding:.15rem .5rem;display:inline-flex;align-items:center;gap:.3rem}
.tag-rm{cursor:pointer;color:#8A8A82;font-size:.85rem}.tag-rm:hover{color:#E63946}
.tag-in{font-family:'IBM Plex Mono',monospace;font-size:.75rem;border:1px solid #D4D4CC;padding:.2rem .4rem;background:#FFF;width:140px}
/* Namespace */
.ns-grid{display:grid;grid-template-columns:200px 1fr;gap:1rem;min-height:400px}
.ns-list{border-right:1px solid #D4D4CC}
.ns-item{font-family:'IBM Plex Mono',monospace;font-size:.8rem;padding:.4rem .75rem;cursor:pointer;display:flex;justify-content:space-between}
.ns-item:hover{background:#EEEEE8}.ns-item.active{background:#EEEEE8;border-right:2px solid #E63946}
.ns-count{color:#8A8A82}
.ns-files{padding-left:.5rem}
.ns-file{font-family:'IBM Plex Mono',monospace;font-size:.8rem;padding:.3rem 0;cursor:pointer;color:#1A1A1A;border-bottom:1px solid #EEEEE8;display:flex;justify-content:space-between;align-items:center}
.ns-file:hover{color:#E63946}
.ns-file-meta{font-size:.7rem;color:#8A8A82}
.ns-drop{border:2px dashed #D4D4CC;padding:1rem;text-align:center;font-family:'IBM Plex Mono',monospace;font-size:.8rem;color:#8A8A82;margin-top:1rem}
.ns-drop.drag-over{border-color:#E63946;background:#FFF5F5;color:#E63946}
/* Graph */
#graph-container{background:#FFF;border:1px solid #D4D4CC;padding:1rem;min-height:300px}
.graph-layout{display:grid;grid-template-columns:1fr 360px;gap:1rem}
.side-panel{background:#FFF;border:1px solid #D4D4CC;padding:1rem;font-family:'IBM Plex Mono',monospace;font-size:.8rem;white-space:pre-wrap;max-height:600px;overflow-y:auto}
.sp-title{font-family:'IBM Plex Mono',monospace;font-size:.75rem;text-transform:uppercase;color:#8A8A82;margin-bottom:.5rem;letter-spacing:.05em}
/* Dismiss */
.dr{display:none}.dr.show{display:table-row}
.dr td{padding:.25rem .75rem .75rem}
.dr input{font-family:'IBM Plex Mono',monospace;font-size:.8rem;border:1px solid #D4D4CC;padding:.3rem .5rem;background:#F5F5F0;width:300px}
</style>
</head>
<body>
<div class="header"><h1>ctx</h1><div class="hstats" id="hstats"></div></div>
<div class="tabs" id="tab-bar">
  <button class="tab active" onclick="sw('compile')">Compile</button>
  <button class="tab" onclick="sw('learnings')">Learnings</button>
  <button class="tab" onclick="sw('synthesize')">Synthesize</button>
  <button class="tab" onclick="sw('candidates')">Candidates</button>
  <button class="tab" onclick="sw('namespace')">Namespace</button>
  <button class="tab" onclick="sw('graph')">Graph</button>
</div>

<!-- COMPILE -->
<div class="tc active" id="tab-compile">
  <div class="c-area"><textarea id="c-in" placeholder="Paste task description, problem statement, spec, error, or anything relevant..."></textarea></div>
  <div class="c-ctrl">
    <select id="c-tag"><option value="">all projects</option></select>
    <button onclick="runCompile()">compile</button>
  </div>
  <div class="c-result" id="c-result">
    <div class="c-approach" id="c-approach"></div>
    <div id="c-sections"></div>
    <div class="c-questions" id="c-questions"></div>
    <div class="c-sources" id="c-sources"></div>
  </div>
</div>

<!-- LEARNINGS -->
<div class="tc" id="tab-learnings"><div id="learn-table"></div></div>

<!-- SYNTHESIZE -->
<div class="tc" id="tab-synthesize">
  <div class="sl">Output</div>
  <div class="syn-out">
    <select id="syn-ns"><option value="skills/external">skills/external</option><option value="skills/internal">skills/internal</option><option value="patterns">patterns</option></select>
    <input id="syn-slug" placeholder="slug (e.g. eval-design)">
    <select id="syn-type"><option value="skill">skill</option><option value="pattern">pattern</option></select>
  </div>
  <div class="sl">Sources</div>
  <div class="syn-sources" id="syn-sources"></div>
  <div class="syn-add">
    <button onclick="addSynSrc('url')">+ URL</button>
    <button onclick="addSynSrc('file')">+ File</button>
  </div>
  <div class="sl">Context (optional)</div>
  <div class="syn-ctx"><textarea id="syn-ctx" placeholder="Calibration context about your work..."></textarea></div>
  <div class="c-ctrl"><button onclick="runSynthesize()">synthesize</button></div>
  <div class="syn-result" id="syn-result"></div>
  <div class="syn-skills">
    <div class="sl">Existing Skills</div>
    <div id="syn-existing"></div>
  </div>
</div>

<!-- CANDIDATES -->
<div class="tc" id="tab-candidates">
  <div class="sl">Pending</div><div id="pending"></div>
  <div class="sl">Promoted</div><div id="approved"></div>
  <div class="sl">Dismissed</div><div id="dismissed"></div>
</div>

<!-- NAMESPACE -->
<div class="tc" id="tab-namespace">
  <div class="ns-grid">
    <div class="ns-list" id="ns-list"></div>
    <div id="ns-right">
      <div class="ns-files" id="ns-files"></div>
      <div class="ns-drop" id="ns-drop" ondragover="dOver(event)" ondragleave="dLeave(event)" ondrop="dDrop(event)">Drop files here</div>
    </div>
  </div>
</div>

<!-- GRAPH -->
<div class="tc" id="tab-graph">
  <div class="graph-layout">
    <div id="graph-container"><pre class="mermaid" id="mermaid-graph"></pre></div>
    <div class="side-panel" id="graph-panel"><div class="sp-title">Select a node</div><div id="gp-content" class="muted">Click a skill node to see traces.</div></div>
  </div>
</div>

<!-- FILE VIEWER -->
<div class="fv" id="fv">
  <div class="fv-hdr">
    <span class="fv-path" id="fv-path"></span>
    <div class="fv-acts">
      <button class="fv-toggle" id="fv-md" onclick="toggleMd()">rendered</button>
      <button class="fv-close" onclick="closeFv()">x</button>
    </div>
  </div>
  <div class="fv-body" id="fv-body"></div>
  <div class="tag-ed" id="fv-tags">
    <div class="tag-lbl">Tags</div>
    <div class="tag-list" id="fv-tag-list"></div>
    <input class="tag-in" id="fv-tag-in" placeholder="add tag..." onkeydown="if(event.key==='Enter'){addTag();event.preventDefault();}">
  </div>
  <div class="fv-del"><button onclick="deleteFvFile()">delete</button></div>
</div>

<script>
mermaid.initialize({startOnLoad:false,theme:'base',themeVariables:{fontFamily:'IBM Plex Mono',fontSize:'12px',primaryColor:'#F5F5F0',primaryBorderColor:'#1A1A1A',lineColor:'#8A8A82'}});
let _nsData=null,_curNs=null,_fvRaw='',_fvPath='',_fvMd=false,_fvTags=[],_synSrcs=[];

function sw(n){document.querySelectorAll('.tab').forEach(t=>t.classList.remove('active'));
document.querySelectorAll('.tc').forEach(t=>t.classList.remove('active'));
event.target.classList.add('active');document.getElementById('tab-'+n).classList.add('active');
if(n==='graph')loadGraph();if(n==='learnings')loadLearnings();if(n==='namespace')loadNs();
if(n==='compile')loadTags();if(n==='synthesize')loadExistingSkills();if(n==='candidates')loadCands();}

function esc(s){const d=document.createElement('div');d.textContent=s;return d.innerHTML}

// ── Stats ──
async function loadStats(){
  const s=await fetch('/api/stats').then(r=>r.json());
  document.getElementById('hstats').innerHTML=
    `<span class="hl">Pending ${s.pending}</span><span>Knowledge ${s.knowledge}</span><span>Traces ${s.traces}</span><span>Chunks ${s.chunks}</span>`;
}

// ── Compile ──
async function loadTags(){
  const sel=document.getElementById('c-tag');
  if(sel.options.length>1)return;
  const tags=await fetch('/api/tags').then(r=>r.json());
  for(const t of tags){const o=document.createElement('option');o.value=t;o.textContent=t;sel.appendChild(o);}
}
async function runCompile(){
  const task=document.getElementById('c-in').value.trim();
  const tag=document.getElementById('c-tag').value;
  if(!task&&!tag)return;
  const res=document.getElementById('c-result');
  res.style.display='block';
  document.getElementById('c-approach').textContent='Compiling...';
  document.getElementById('c-sections').innerHTML='';
  document.getElementById('c-questions').innerHTML='';
  document.getElementById('c-sources').innerHTML='';
  try{
    const resp=await fetch('/api/compile',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({task,tag})});
    const data=await resp.json();
    if(data.error){document.getElementById('c-approach').textContent='Error: '+data.error;return;}
    const s=data.structured;
    if(!s){document.getElementById('c-approach').textContent=data.bundle||'No output.';return;}
    document.getElementById('c-approach').textContent=s.approach||'';
    let h='';
    const secs=[['Patterns',s.sections.patterns],['Skills',s.sections.skills],['Docs',s.sections.docs],
                ['Learnings',s.sections.learnings],['Primitives',s.sections.primitives]];
    for(const [title,items] of secs){
      if(!items||!items.length)continue;
      h+=`<div class="sl">${title}</div>`;
      for(const it of items){
        const heading=it.heading&&it.heading!=='(full)'&&it.heading!=='(preamble)'?` <span class="c-heading">## ${esc(it.heading)}</span>`:'';
        const tag=it.project_tag?` [${it.project_tag}]`:'';
        h+=`<div class="c-item" onclick="openFv('${esc(it.path)}')">`+
          `-> ${esc(it.path)} <span class="c-score">[${it.score}]</span>${heading}${tag}</div>`+
          `<div class="c-desc">${esc(it.description)}</div>`;
      }
    }
    document.getElementById('c-sections').innerHTML=h;
    if(s.open_questions&&s.open_questions.length){
      let q='<div class="sl">Open Questions</div><ul>';
      for(const oq of s.open_questions)q+=`<li>${esc(oq)}</li>`;
      document.getElementById('c-questions').innerHTML=q+'</ul>';
    }
    if(s.sources&&s.sources.length){
      let src='<div class="sl">Sources</div>';
      for(let i=0;i<s.sources.length;i++){
        const ss=s.sources[i];
        src+=`<div onclick="openFv('${esc(ss.path)}')">${i+1}. ${esc(ss.path)} [${ss.score}]</div>`;
      }
      document.getElementById('c-sources').innerHTML=src;
    }
  }catch(e){document.getElementById('c-approach').textContent='Error: '+e.message;}
}

// ── Learnings ──
async function loadLearnings(){
  const traces=await fetch('/api/traces').then(r=>r.json());
  let h='<table><thead><tr><th></th><th>ID</th><th>Date</th><th>Tag</th><th>Skills</th><th>Problem</th><th></th></tr></thead><tbody>';
  for(const t of traces){
    const date=(t.ts||'').slice(0,10);
    let skills='';try{skills=JSON.parse(t.skills||'[]').join(', ');}catch(e){}
    h+=`<tr class="trace-toggle" onclick="toggleTr('${t.id}')">`;
    h+=`<td class="mono muted">+</td><td class="mono">${t.id}</td><td class="mono">${date}</td>`;
    h+=`<td class="mono">${t.project_tag||''}</td><td class="mono small">${skills}</td>`;
    h+=`<td>${esc(t.problem||'(unsummarized)')}</td>`;
    h+=`<td><button class="act act-m" onclick="event.stopPropagation();delTrace('${t.id}')">x</button></td></tr>`;
    h+=`<tr class="trace-detail" id="td-${t.id}"><td colspan="7">`;
    if(t.prompt)h+=`<strong>Prompt:</strong> ${esc(t.prompt.slice(0,500))}<br><br>`;
    if(t.pattern)h+=`<strong>Pattern:</strong> ${esc(t.pattern)}<br>`;
    if(t.pitfalls)h+=`<strong>Pitfalls:</strong> ${esc(t.pitfalls)}<br>`;
    if(t.embedding_text)h+=`<br><strong>Summary:</strong> ${esc(t.embedding_text)}`;
    if(t.domain_knowledge)h+=`<br><br><strong>Domain:</strong> ${esc(t.domain_knowledge)}`;
    h+=`</td></tr>`;
  }
  document.getElementById('learn-table').innerHTML=h+'</tbody></table>';
}
function toggleTr(id){document.getElementById('td-'+id).classList.toggle('show')}
async function delTrace(id){
  if(!confirm('Delete trace '+id+'? Removes file and all embeddings.'))return;
  await fetch('/api/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({trace_id:id})});
  loadLearnings();loadStats();
}

// ── Synthesize ──
function addSynSrc(type){
  const id='syn-'+Date.now();
  _synSrcs.push(id);
  const ph=type==='url'?'https://...':'path/to/file.md';
  const el=document.getElementById('syn-sources');
  el.innerHTML+=`<div class="syn-src" id="${id}"><input placeholder="${ph}"><span class="syn-rm" onclick="rmSynSrc('${id}')">x</span></div>`;
}
function rmSynSrc(id){document.getElementById(id).remove();_synSrcs=_synSrcs.filter(s=>s!==id);}
async function runSynthesize(){
  const ns=document.getElementById('syn-ns').value;
  const slug=document.getElementById('syn-slug').value.trim();
  const type=document.getElementById('syn-type').value;
  const ctx=document.getElementById('syn-ctx').value.trim();
  if(!slug){alert('Slug required');return;}
  const sources=[];
  document.querySelectorAll('.syn-src input').forEach(inp=>{if(inp.value.trim())sources.push(inp.value.trim());});
  if(!sources.length){alert('Add at least one source');return;}
  const output=ns+'/'+slug;
  const res=document.getElementById('syn-result');
  res.style.display='block';res.textContent='Synthesizing...';
  try{
    const resp=await fetch('/api/synthesize',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({sources,output,type,context:ctx})});
    const data=await resp.json();
    res.textContent=data.ok?data.stdout:'Error:\n'+data.stderr;
    if(data.ok)loadExistingSkills();
  }catch(e){res.textContent='Error: '+e.message;}
}
async function loadExistingSkills(){
  const items=await fetch('/api/knowledge').then(r=>r.json());
  const skills=items.filter(i=>i.namespace.includes('skill'));
  if(!skills.length){document.getElementById('syn-existing').innerHTML='<div class="empty">No skills yet.</div>';return;}
  let h='<table><thead><tr><th>Slug</th><th>Namespace</th><th>Summary</th><th></th></tr></thead><tbody>';
  for(const s of skills){
    h+=`<tr><td class="mono">${s.slug}</td><td class="mono muted">${s.namespace}</td>`;
    h+=`<td>${esc((s.summary||'').slice(0,120))}</td>`;
    h+=`<td><button class="act act-r" onclick="refineSkill('${esc(s.file_path)}')">refine</button></td></tr>`;
  }
  document.getElementById('syn-existing').innerHTML=h+'</tbody></table>';
}
function refineSkill(path){
  sw('synthesize');
  alert('Refine mode: add sources and feedback, then click synthesize.\nRefining: '+path);
}

// ── Candidates ──
async function loadCands(){
  const cands=await fetch('/api/candidates').then(r=>r.json());
  renderCands('pending',cands.filter(c=>c.status==='pending'),false);
  renderCands('approved',cands.filter(c=>c.status==='approved'),true);
  renderCands('dismissed',cands.filter(c=>c.status==='dismissed'),true);
}
function renderCands(id,items,ro){
  const el=document.getElementById(id);
  if(!items.length){el.innerHTML='<div class="empty">'+(id==='pending'?'No pending. Run detection.':'None.')+'</div>';return;}
  let h='<table><thead><tr><th>Slug</th><th>Type</th><th>Freq</th><th>Definition</th>';
  if(!ro)h+='<th></th>';
  h+='</tr></thead><tbody>';
  for(const c of items){
    let def=(c.definition||'').replace(/\n\n\[DISMISSED:.*\]$/,'');
    h+=`<tr><td class="mono">${c.slug}</td><td class="mono muted">${c.type}</td><td class="mono">${c.frequency}</td><td>${esc(def)}</td>`;
    if(!ro){
      h+=`<td style="white-space:nowrap"><button class="act act-r" onclick="approveCand('${c.id}')">approve</button> `;
      h+=`<button class="act act-m" onclick="toggleDr('${c.id}')">dismiss</button></td>`;
    }
    h+='</tr>';
    if(!ro){
      h+=`<tr class="dr" id="dr-${c.id}"><td colspan="5"><input id="reason-${c.id}" placeholder="Reason (optional)" onkeydown="if(event.key==='Enter')dismissCand('${c.id}')"> `;
      h+=`<button class="act act-m" onclick="dismissCand('${c.id}')">confirm</button></td></tr>`;
    }
  }
  el.innerHTML=h+'</tbody></table>';
}
function toggleDr(id){document.getElementById('dr-'+id).classList.toggle('show')}
async function approveCand(id){await fetch('/api/candidates/'+id+'/approve',{method:'POST'});loadCands();loadStats();}
async function dismissCand(id){
  const reason=document.getElementById('reason-'+id).value;
  await fetch('/api/candidates/'+id+'/dismiss',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({reason})});
  loadCands();loadStats();
}

// ── Namespace ──
async function loadNs(){
  _nsData=await fetch('/api/namespace').then(r=>r.json());
  let h='';
  for(const ns of _nsData)h+=`<div class="ns-item" data-ns="${ns.namespace}" onclick="selNs('${ns.namespace}')"><span>${ns.namespace}</span><span class="ns-count">${ns.count}</span></div>`;
  document.getElementById('ns-list').innerHTML=h;
  if(_nsData.length)selNs(_curNs||_nsData[0].namespace);
}
function selNs(name){
  _curNs=name;
  document.querySelectorAll('.ns-item').forEach(el=>el.classList.toggle('active',el.dataset.ns===name));
  const ns=_nsData.find(n=>n.namespace===name);
  if(!ns||!ns.files.length){document.getElementById('ns-files').innerHTML='<div class="empty">Empty.</div>';return;}
  let h='';
  for(const f of ns.files){
    h+=`<div class="ns-file" draggable="true" ondragstart="fDrag(event,'${f.path}')" onclick="openFv('${f.path}')">`;
    h+=`<span>${f.name}</span><span class="ns-file-meta">${f.mtime}</span></div>`;
  }
  document.getElementById('ns-files').innerHTML=h;
  const writable=['patterns','debugging','skills/internal','skills/external','primitives','docs','plans'];
  document.getElementById('ns-drop').style.display=writable.includes(name)?'block':'none';
}
function fDrag(ev,path){ev.dataTransfer.setData('text/plain',path);ev.dataTransfer.effectAllowed='move';}
function dOver(ev){ev.preventDefault();ev.currentTarget.classList.add('drag-over');}
function dLeave(ev){ev.currentTarget.classList.remove('drag-over');}
async function dDrop(ev){
  ev.preventDefault();ev.currentTarget.classList.remove('drag-over');
  if(!_curNs)return;
  if(ev.dataTransfer.files&&ev.dataTransfer.files.length>0){
    for(const file of ev.dataTransfer.files){const fd=new FormData();fd.append('file',file);fd.append('namespace',_curNs);await fetch('/api/file/upload',{method:'POST',body:fd});}
    _nsData=null;loadNs();return;
  }
  const src=ev.dataTransfer.getData('text/plain');
  if(src&&_curNs){const name=src.split('/').pop();const dest=_curNs+'/'+name;if(src===dest)return;if(!confirm('Move '+src+' to '+dest+'?'))return;
  await fetch('/api/file/move',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({src,dest})});_nsData=null;loadNs();}
}

// ── File viewer ──
async function openFv(path){
  const resp=await fetch('/api/file?path='+encodeURIComponent(path));
  if(!resp.ok)return;
  _fvRaw=await resp.text();_fvPath=path;_fvMd=false;
  document.getElementById('fv-path').textContent=path;
  document.getElementById('fv-md').classList.remove('active');
  renderFv();document.getElementById('fv').classList.add('open');
  loadFvTags(path);
}
function renderFv(){
  const b=document.getElementById('fv-body');
  if(_fvMd){b.innerHTML=marked.parse(_fvRaw);b.classList.add('rendered');}
  else{b.textContent=_fvRaw;b.classList.remove('rendered');}
}
function toggleMd(){_fvMd=!_fvMd;document.getElementById('fv-md').classList.toggle('active',_fvMd);renderFv();}
function closeFv(){document.getElementById('fv').classList.remove('open');}
document.addEventListener('keydown',e=>{if(e.key==='Escape')closeFv();});
async function deleteFvFile(){
  if(!confirm('Delete '+_fvPath+'?'))return;
  await fetch('/api/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:_fvPath})});
  closeFv();loadNs();loadStats();
}

// ── Tags ──
async function loadFvTags(path){
  const resp=await fetch('/api/file/tags?path='+encodeURIComponent(path));
  const data=await resp.json();_fvTags=data.tags||[];renderTags();
}
function renderTags(){
  document.getElementById('fv-tag-list').innerHTML=_fvTags.map(t=>`<span class="tag-chip">${esc(t)}<span class="tag-rm" onclick="rmTag('${esc(t)}')">x</span></span>`).join('');
}
async function addTag(){
  const inp=document.getElementById('fv-tag-in');
  const tag=inp.value.trim().toLowerCase().replace(/[^a-z0-9-]/g,'-');
  if(!tag||_fvTags.includes(tag)){inp.value='';return;}
  _fvTags.push(tag);inp.value='';renderTags();await saveTags();
}
async function rmTag(tag){_fvTags=_fvTags.filter(t=>t!==tag);renderTags();await saveTags();}
async function saveTags(){
  await fetch('/api/tags/edit',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:_fvPath,tags:_fvTags})});
  const resp=await fetch('/api/file?path='+encodeURIComponent(_fvPath));
  if(resp.ok){_fvRaw=await resp.text();renderFv();}
}

// ── Graph ──
async function loadGraph(){
  const resp=await fetch('/api/graph');const diagram=await resp.text();
  const c=document.getElementById('mermaid-graph');c.removeAttribute('data-processed');c.textContent=diagram;
  await mermaid.run({nodes:[c]});
  setTimeout(()=>{c.querySelectorAll('.node').forEach(node=>{node.style.cursor='pointer';
    node.addEventListener('click',()=>{const l=node.querySelector('.nodeLabel')?.textContent||'';if(l)loadSkillTr(l);});
  });},100);
}
async function loadSkillTr(slug){
  const p=document.getElementById('gp-content');const t=document.querySelector('#graph-panel .sp-title');
  t.textContent=slug;p.textContent='Loading...';p.classList.remove('muted');
  try{const traces=await fetch('/api/traces/by-skill/'+encodeURIComponent(slug)).then(r=>r.json());
    if(!traces.length){p.textContent='No traces found.';return;}
    let text=`${traces.length} trace(s):\n\n`;
    for(const t of traces){text+=`${t.id}  ${(t.ts||'').slice(0,10)}  ${t.project||''}\n`;
      if(t.problem)text+=`  ${t.problem}\n`;text+='\n';}
    p.textContent=text;
  }catch(e){p.textContent='Error: '+e.message;}
}

// ── Init ──
loadStats();loadTags();
</script>
</body></html>"""


@app.get("/", response_class=HTMLResponse)
def index():
    return HTML


def main():
    cfg = _config()
    port = cfg["serve"]["port"]
    for i, arg in enumerate(sys.argv[1:]):
        if arg == "--port" and i + 2 <= len(sys.argv):
            port = int(sys.argv[i + 2])
    print(f"ctx-serve: starting at http://localhost:{port}")
    uvicorn.run(app, host="0.0.0.0", port=port, log_level="warning")


if __name__ == "__main__":
    main()

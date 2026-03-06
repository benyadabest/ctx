# ~/.context/model_router.py
# Single module that all scripts import for model calls.
# Swapping a model means changing one line in ctx.toml — nothing else.
# Tracks usage (tokens + cost) in index.db usage table.

import json, os, sqlite3, urllib.request, urllib.error
from datetime import datetime, timezone
from pathlib import Path
try:
    import toml
except ImportError:
    import tomllib as toml

CONTEXT_DIR = Path.home() / ".context"
DB_PATH     = CONTEXT_DIR / "index.db"

# Pricing per million tokens (input, output)
PRICING = {
    "claude-sonnet-4-6":  (3.0, 15.0),
    "claude-haiku-4-5-20251001": (0.25, 1.25),
    "claude-opus-4-6":    (15.0, 75.0),
}


def _config() -> dict:
    return toml.load(CONTEXT_DIR / "ctx.toml")


def get_model(task: str) -> str:
    """task: 'summarize' | 'detect' | 'compile' | 'synthesize'"""
    return _config()["models"][task]


def _ensure_usage_table():
    if not DB_PATH.exists():
        return
    conn = sqlite3.connect(DB_PATH)
    conn.execute("""
        CREATE TABLE IF NOT EXISTS usage (
            id           INTEGER PRIMARY KEY AUTOINCREMENT,
            ts           TEXT NOT NULL,
            task         TEXT NOT NULL,
            model        TEXT NOT NULL,
            input_tokens  INTEGER NOT NULL DEFAULT 0,
            output_tokens INTEGER NOT NULL DEFAULT 0,
            cost_usd     REAL NOT NULL DEFAULT 0.0,
            trace_id     TEXT
        )
    """)
    conn.commit()
    conn.close()


def _log_usage(task: str, model: str, input_tokens: int, output_tokens: int, trace_id: str = None):
    if not DB_PATH.exists():
        return
    _ensure_usage_table()
    pricing = PRICING.get(model, (3.0, 15.0))
    cost = (input_tokens / 1_000_000) * pricing[0] + (output_tokens / 1_000_000) * pricing[1]
    conn = sqlite3.connect(DB_PATH)
    conn.execute(
        "INSERT INTO usage (ts, task, model, input_tokens, output_tokens, cost_usd, trace_id) "
        "VALUES (?, ?, ?, ?, ?, ?, ?)",
        (datetime.now(timezone.utc).isoformat(), task, model, input_tokens, output_tokens, cost, trace_id),
    )
    conn.commit()
    conn.close()


def call(task: str, prompt: str, system: str = "", max_tokens: int = 1000,
         temperature: float = None, trace_id: str = None) -> str:
    """
    Route a call to the correct model for the given task.
    Handles both Ollama and Anthropic transparently.
    Logs usage to SQLite.
    """
    model = get_model(task)

    if model.startswith("ollama/"):
        return _call_ollama(
            model=model.removeprefix("ollama/"),
            prompt=prompt,
            system=system,
            temperature=temperature,
        )
    else:
        return _call_anthropic(
            model=model,
            prompt=prompt,
            system=system,
            max_tokens=max_tokens,
            temperature=temperature,
            task=task,
            trace_id=trace_id,
        )


def _call_ollama(model: str, prompt: str, system: str, temperature: float = None) -> str:
    cfg     = _config()
    base    = cfg["api"].get("ollama_base_url", "http://localhost:11434")
    body    = {
        "model":  model,
        "prompt": prompt,
        "system": system,
        "stream": False,
        "format": "json",   # critical: forces structured output, prevents markdown wrapping
    }
    if temperature is not None:
        body["options"] = {"temperature": temperature}
    payload = json.dumps(body).encode()

    req = urllib.request.Request(
        f"{base}/api/generate",
        data=payload,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            return json.loads(r.read())["response"]
    except Exception as e:
        raise RuntimeError(f"Ollama call failed ({model}): {e}. "
                           f"Is Ollama running? Try: ollama serve") from e


def _call_anthropic(model: str, prompt: str, system: str, max_tokens: int,
                    temperature: float = None, task: str = "", trace_id: str = None) -> str:
    cfg     = _config()
    api_key = cfg["api"].get("anthropic_api_key") or os.environ.get("ANTHROPIC_API_KEY", "")
    if not api_key:
        raise RuntimeError("No Anthropic API key. Set in ctx.toml [api] or ANTHROPIC_API_KEY env var.")

    body = {
        "model":      model,
        "max_tokens": max_tokens,
        "messages":   [{"role": "user", "content": prompt}],
    }
    if system:
        body["system"] = system
    if temperature is not None:
        body["temperature"] = temperature
    payload = json.dumps(body).encode()

    req = urllib.request.Request(
        "https://api.anthropic.com/v1/messages",
        data=payload,
        headers={
            "Content-Type":    "application/json",
            "x-api-key":       api_key,
            "anthropic-version": "2023-06-01",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            data = json.loads(r.read())
            # Extract token counts from response
            usage = data.get("usage", {})
            input_tokens = usage.get("input_tokens", 0)
            output_tokens = usage.get("output_tokens", 0)
            _log_usage(task, model, input_tokens, output_tokens, trace_id)
            return data["content"][0]["text"]
    except urllib.error.HTTPError as e:
        err_body = e.read().decode() if e.fp else ""
        raise RuntimeError(
            f"Anthropic API error {e.code}: {err_body[:500]}"
        ) from e


if __name__ == "__main__":
    import sys
    if len(sys.argv) < 3:
        print("Usage: python3 model_router.py <task> <prompt> [system]")
        sys.exit(1)
    task   = sys.argv[1]
    prompt = sys.argv[2]
    system = sys.argv[3] if len(sys.argv) > 3 else ""
    print(call(task, prompt, system))

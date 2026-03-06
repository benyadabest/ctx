# ~/.context/model_router.py
# Single module that all scripts import for model calls.
# Swapping a model means changing one line in ctx.toml — nothing else.

import json, os, urllib.request, urllib.error
from pathlib import Path
try:
    import toml
except ImportError:
    import tomllib as toml

CONTEXT_DIR = Path.home() / ".context"


def _config() -> dict:
    return toml.load(CONTEXT_DIR / "ctx.toml")


def get_model(task: str) -> str:
    """task: 'summarize' | 'detect' | 'compile'"""
    return _config()["models"][task]


def call(task: str, prompt: str, system: str = "", max_tokens: int = 1000,
         temperature: float = None) -> str:
    """
    Route a call to the correct model for the given task.
    Handles both Ollama and Anthropic transparently.
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
                    temperature: float = None) -> str:
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

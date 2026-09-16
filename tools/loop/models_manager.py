#!/usr/bin/env python3
"""
models_manager.py
Manages hot-swapping and inference requests between Qwen 2.5 Coder and Gemma 4
via LM Studio CLI (`lms`) and its OpenAI-compatible local API.
"""

import json
import subprocess
import time
import urllib.request
import urllib.error
from typing import Dict, Any, List, Optional

LMS_BIN = "/Users/giuliosemino/.lmstudio/bin/lms"
BASE_URL = "http://localhost:1234/v1"

MODELS_CONFIG = {
    "qwen": {
        "key": "qwen2.5-coder-14b-instruct-mlx",
        "identifier": "qwen2.5-coder",
        "role": "The Coder (Implementation & TDD greening)"
    },
    "gemma": {
        "key": "google/gemma-4-12b-qat",
        "identifier": "gemma-4",
        "role": "The Auditor (Security, edge cases & code review)"
    }
}

def get_loaded_model_identifier() -> Optional[str]:
    """Check which model is currently loaded in memory via LM Studio API."""
    try:
        req = urllib.request.Request(f"{BASE_URL}/models")
        with urllib.request.urlopen(req, timeout=3) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            models = data.get("data", [])
            # Check via lms ps for precise loaded status
            ps_proc = subprocess.run(
                [LMS_BIN, "ps"],
                capture_output=True,
                text=True,
                check=False
            )
            for target_name, cfg in MODELS_CONFIG.items():
                if cfg["identifier"] in ps_proc.stdout or cfg["key"] in ps_proc.stdout:
                    return cfg["identifier"]
            return None
    except Exception as e:
        return None

def ensure_model(model_name: str) -> str:
    """
    Ensure the requested model ('qwen' or 'gemma') is loaded in LM Studio.
    If another model is loaded, unloads it and loads the target.
    Returns the identifier to use for inference.
    """
    if model_name not in MODELS_CONFIG:
        raise ValueError(f"Unknown model name: {model_name}. Allowed: {list(MODELS_CONFIG.keys())}")
    
    cfg = MODELS_CONFIG[model_name]
    target_id = cfg["identifier"]
    target_key = cfg["key"]

    current_id = get_loaded_model_identifier()
    if current_id == target_id:
        print(f"[ModelManager] Model '{target_id}' is already loaded in memory. Zero swap overhead.")
        return target_id

    print(f"[ModelManager] Swapping active model: Unloading and loading '{target_key}'...")
    start_time = time.time()

    # 1. Unload all models
    subprocess.run([LMS_BIN, "unload", "-a"], capture_output=True, text=True, check=False)

    # 2. Load target model with identifier
    load_proc = subprocess.run(
        [LMS_BIN, "load", target_key, "-y", "--identifier", target_id],
        capture_output=True,
        text=True,
        check=False
    )
    
    elapsed = time.time() - start_time
    if load_proc.returncode != 0:
        raise RuntimeError(f"Failed to load {target_key}: {load_proc.stderr}\n{load_proc.stdout}")
    
    print(f"[ModelManager] Successfully loaded '{target_id}' in {elapsed:.2f}s.")
    return target_id

def chat_completion(
    model_name: str,
    messages: List[Dict[str, str]],
    temperature: float = 0.2,
    max_tokens: int = 2048,
    system_prompt: Optional[str] = None
) -> str:
    """
    Ensures the model is loaded and sends a chat completion request to LM Studio.
    Returns the assistant text response.
    """
    model_id = ensure_model(model_name)

    full_messages = []
    if system_prompt:
        full_messages.append({"role": "system", "content": system_prompt})
    full_messages.extend(messages)

    payload = {
        "model": model_id,
        "messages": full_messages,
        "temperature": temperature,
        "max_tokens": max_tokens,
        "stream": True
    }

    req = urllib.request.Request(
        f"{BASE_URL}/chat/completions",
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST"
    )

    try:
        collected_content: List[str] = []
        with urllib.request.urlopen(req, timeout=60) as resp:
            for line_bytes in resp:
                line = line_bytes.decode("utf-8").strip()
                if not line or not line.startswith("data: "):
                    continue
                data_str = line[6:]
                if data_str == "[DONE]":
                    break
                try:
                    chunk = json.loads(data_str)
                    choice = chunk.get("choices", [{}])[0]
                    delta = choice.get("delta", {})
                    content = delta.get("content")
                    if content:
                        collected_content.append(content)
                    elif delta.get("reasoning_content"):
                        # Only collect reasoning if no content has arrived yet
                        if not collected_content:
                            collected_content.append(delta["reasoning_content"])
                except json.JSONDecodeError:
                    continue
        return "".join(collected_content).strip()
    except urllib.error.HTTPError as e:
        err_body = e.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"LM Studio HTTP error {e.code}: {err_body}")

if __name__ == "__main__":
    import sys
    if len(sys.argv) > 1 and sys.argv[1] == "test":
        target = sys.argv[2] if len(sys.argv) > 2 else "qwen"
        print(f"Testing chat completion with '{target}'...")
        reply = chat_completion(
            model_name=target,
            messages=[{"role": "user", "content": "Respond only with: READY_FOR_ENGINEERING"}],
            max_tokens=20
        )
        print(f"Response: {reply}")

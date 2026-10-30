#!/usr/bin/env python3
"""
models_manager.py
Manages hot-swapping and inference requests between Qwen 2.5 Coder, DeepSeek R1 Distill,
and Gemma 4 via LM Studio CLI (`lms`) and its OpenAI-compatible local API.
"""

import json
import re
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
        "role": "The Coder (Implementation & TDD greening)",
        "default_temp": 0.1,
        "default_max_tokens": 3000,
        "is_reasoning": False
    },
    "deepseek": {
        "key": "deepseek-r1-distill-qwen-14b",
        "identifier": "deepseek-r1-distill-qwen-14b",
        "role": "The Auditor & Security Reasoner (Deep CoT verification, concurrency & SAT compliance)",
        "default_temp": 0.6,
        "default_max_tokens": 4096,
        "is_reasoning": True
    },
    "gemma": {
        "key": "google/gemma-4-12b-qat",
        "identifier": "gemma-4",
        "role": "Secondary Auditor (Fast review fallback)",
        "default_temp": 0.2,
        "default_max_tokens": 1500,
        "is_reasoning": False
    }
}

def strip_thinking_blocks(text: str) -> str:
    """Remove <think>...</think> blocks emitted by DeepSeek R1 models."""
    cleaned = re.sub(r"<think>.*?</think>", "", text, flags=re.DOTALL)
    # In case an unclosed <think> tag was truncated, strip up to a markdown header if present
    if "<think>" in cleaned and "</think>" not in cleaned:
        parts = cleaned.split("<think>", 1)
        cleaned = parts[0]
    return cleaned.strip()

def get_loaded_model_identifier() -> Optional[str]:
    """Check which model is currently loaded in memory via LM Studio CLI (`lms ps`)."""
    try:
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
    except Exception:
        return None

def ensure_model(model_name: str) -> str:
    """
    Ensure the requested model ('qwen', 'deepseek', or 'gemma') is loaded in LM Studio.
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
    temperature: Optional[float] = None,
    max_tokens: Optional[int] = None,
    system_prompt: Optional[str] = None,
    timeout: int = 180
) -> str:
    """
    Ensures the model is loaded and sends a chat completion request to LM Studio.
    Handles Chain-of-Thought (reasoning_content / <think> tags) cleanly.
    Returns the cleaned assistant text response.
    """
    model_id = ensure_model(model_name)
    cfg = MODELS_CONFIG[model_name]

    effective_temp = temperature if temperature is not None else cfg.get("default_temp", 0.2)
    effective_max_tokens = max_tokens if max_tokens is not None else cfg.get("default_max_tokens", 2048)

    full_messages = []
    if system_prompt:
        full_messages.append({"role": "system", "content": system_prompt})
    full_messages.extend(messages)

    payload = {
        "model": model_id,
        "messages": full_messages,
        "temperature": effective_temp,
        "max_tokens": effective_max_tokens,
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
        collected_reasoning: List[str] = []
        with urllib.request.urlopen(req, timeout=timeout) as resp:
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
                    reasoning = delta.get("reasoning_content")
                    if reasoning:
                        collected_reasoning.append(reasoning)
                    if content:
                        collected_content.append(content)
                except json.JSONDecodeError:
                    continue

        raw_output = "".join(collected_content).strip()
        if not raw_output and collected_reasoning:
            # Fallback if the server only populated reasoning_content
            raw_output = "".join(collected_reasoning).strip()

        if cfg.get("is_reasoning"):
            if collected_reasoning:
                reasoning_chars = len("".join(collected_reasoning))
                print(f"[ModelManager] DeepSeek R1 completed reasoning chain ({reasoning_chars} chars).")
            return strip_thinking_blocks(raw_output)

        return raw_output
    except urllib.error.HTTPError as e:
        err_body = e.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"LM Studio HTTP error {e.code}: {err_body}")

if __name__ == "__main__":
    import sys
    if len(sys.argv) > 1 and sys.argv[1] == "test":
        target = sys.argv[2] if len(sys.argv) > 2 else "deepseek"
        print(f"Testing chat completion with '{target}'...")
        reply = chat_completion(
            model_name=target,
            messages=[{"role": "user", "content": "Respond only with: READY_FOR_ENGINEERING"}],
            max_tokens=512
        )
        print(f"Response: {reply}")

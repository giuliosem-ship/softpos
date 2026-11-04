#!/usr/bin/env python3
"""
tools/loop/lms_consult.py
CLI tool to consult local LM Studio models (Qwen 2.5 Coder 14B, DeepSeek-R1 14B, Gemma 4)
for SoftPOS architecture, code implementation, PCI security audit, and Banxico rules.
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

# Ensure repo root is on sys.path
REPO_ROOT = Path(__file__).resolve().parent.parent.parent
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from tools.loop.models_manager import (
    LMStudioManager,
    QWEN_CODER_MODEL,
    DEEPSEEK_R1_MODEL,
    GEMMA_FAST_MODEL,
    SOFTPOS_QWEN_SYSTEM_PROMPT,
)


def main():
    parser = argparse.ArgumentParser(
        description="Consult LM Studio models (Qwen 2.5 Coder / DeepSeek R1 / Gemma 4) for SoftPOS Platform."
    )
    parser.add_argument(
        "prompt",
        nargs="*",
        help="Prompt or question to send to the model. If omitted, prompts interactively.",
    )
    parser.add_argument(
        "--model",
        "-m",
        choices=["qwen", "deepseek", "gemma"],
        default="qwen",
        help="Model to consult (default: qwen).",
    )
    parser.add_argument(
        "--tokens",
        "-t",
        type=int,
        default=4096,
        help="Maximum predicted tokens (default: 4096).",
    )
    parser.add_argument(
        "--temperature",
        type=float,
        default=None,
        help="Sampling temperature (defaults to preset value).",
    )
    parser.add_argument(
        "--json-schema",
        action="store_true",
        help="Request structured JSON output with quant schema preset.",
    )

    args = parser.parse_args()

    prompt_text = " ".join(args.prompt).strip()
    if not prompt_text:
        print("Escribe tu consulta para el modelo (Ctrl+D para enviar):")
        prompt_text = sys.stdin.read().strip()
        if not prompt_text:
            print("Error: No se proporcionó ningún texto de consulta.")
            sys.exit(1)

    mgr = LMStudioManager()

    if args.model == "qwen":
        target_model = QWEN_CODER_MODEL
        preset = "softpos_quant_json" if args.json_schema else "softpos_coder_dev"
        sys_prompt = SOFTPOS_QWEN_SYSTEM_PROMPT
    elif args.model == "deepseek":
        target_model = DEEPSEEK_R1_MODEL
        preset = "softpos_reasoning_r1"
        sys_prompt = "You are a Principal Security Auditor and Financial Systems Architect for Mexico's SoftPOS & SAT compliance."
    else:
        target_model = GEMMA_FAST_MODEL
        preset = "softpos_fast_gemma"
        sys_prompt = "You are a fast engineering reviewer for SoftPOS Platform."

    print(f"\n========================================================")
    print(f"🤖 LM STUDIO CONSULT: [{args.model.upper()}] ({target_model})")
    print(f"⚙️  Preset: {preset}")
    print(f"========================================================\n")

    res = mgr.chat_completion(
        model_key=target_model,
        messages=prompt_text,
        system_prompt=sys_prompt,
        temperature=args.temperature,
        max_tokens=args.tokens,
        preset=preset,
    )

    if res.get("thinking"):
        print("🧠 [Reasoning Chain / <think>]:")
        print(res["thinking"])
        print("\n--------------------------------------------------------\n")

    print("📄 [Respuesta]:")
    print(res["content"])
    print(f"\n========================================================\n")


if __name__ == "__main__":
    main()

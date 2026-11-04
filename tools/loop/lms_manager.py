#!/usr/bin/env python3
"""
lms_manager.py
Re-exports LM Studio Manager and Qwen 2.5 Coder connector for SoftPOS,
matching the Sabino Asset Management (SAM) architectural interface.
"""
from tools.loop.models_manager import (
    DEEPSEEK_R1_MODEL,
    GEMMA_FAST_MODEL,
    LM_STUDIO_API_KEY,
    LM_STUDIO_BASE_URL,
    LM_STUDIO_PRESETS,
    LMS_CLI_PATH,
    MODELS_CONFIG,
    QWEN_CODER_MODEL,
    SOFTPOS_PRESET_FILE,
    SOFTPOS_QWEN_SYSTEM_PROMPT,
    LMStudioManager,
    chat_completion,
    ensure_model,
    extract_json_object,
    extract_thinking_and_answer,
    get_loaded_model_identifier,
    strip_thinking_blocks,
)

__all__ = [
    "LMS_CLI_PATH",
    "LM_STUDIO_BASE_URL",
    "LM_STUDIO_API_KEY",
    "QWEN_CODER_MODEL",
    "DEEPSEEK_R1_MODEL",
    "GEMMA_FAST_MODEL",
    "SOFTPOS_PRESET_FILE",
    "SOFTPOS_QWEN_SYSTEM_PROMPT",
    "LM_STUDIO_PRESETS",
    "MODELS_CONFIG",
    "LMStudioManager",
    "extract_thinking_and_answer",
    "strip_thinking_blocks",
    "extract_json_object",
    "get_loaded_model_identifier",
    "ensure_model",
    "chat_completion",
]

if __name__ == "__main__":
    from tools.loop.models_manager import _DEFAULT_MANAGER
    import sys
    if len(sys.argv) > 1 and sys.argv[1] == "ask":
        prompt = " ".join(sys.argv[2:]) if len(sys.argv) > 2 else "¿Cuál es la regla de CVM para pagos mayores a $1,000 MXN en SoftPOS?"
        ans = _DEFAULT_MANAGER.ask_qwen_coder(prompt)
        print(ans["content"])
    else:
        print("LM Studio Connector for SoftPOS initialized.")

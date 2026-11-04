#!/usr/bin/env python3
"""
models_manager.py / lms_manager.py
LM Studio & Qwen 2.5 Coder 14B Orchestration Connector for SoftPOS Platform.
Manages sequential VRAM model loading on Apple Silicon (Mac Mini M4 Pro, 24 GB unified memory)
for `qwen2.5-coder-14b-instruct-mlx` (15.71 GB), `deepseek-r1-distill-qwen-14b` (8.99 GB),
and `google/gemma-4-12b-qat` (7.15 GB), ensuring heavy models are never loaded simultaneously.
"""
from __future__ import annotations

import json
import logging
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple, Union

logger = logging.getLogger("softpos.lms_manager")
if not logger.handlers:
    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(logging.Formatter("[%(levelname)s] %(name)s: %(message)s"))
    logger.addHandler(handler)
    logger.setLevel(logging.INFO)

# Base Paths & Environment Settings
LMS_CLI_PATH = os.getenv("LMS_CLI_PATH", "/Users/giuliosemino/.lmstudio/bin/lms")
LM_STUDIO_BASE_URL = os.getenv("LM_STUDIO_BASE_URL", "http://localhost:1234/v1").rstrip("/")
LM_STUDIO_API_KEY = os.getenv("LM_STUDIO_API_KEY", "lm-studio")

QWEN_CODER_MODEL = os.getenv("QWEN_CODER_MODEL", "qwen2.5-coder-14b-instruct-mlx")
DEEPSEEK_R1_MODEL = os.getenv("DEEPSEEK_R1_MODEL", "deepseek-r1-distill-qwen-14b")
GEMMA_FAST_MODEL = os.getenv("GEMMA_FAST_MODEL", "google/gemma-4-12b-qat")

SOFTPOS_PRESET_FILE = Path.home() / ".lmstudio" / "config-presets" / "SoftPOS-Payment-Coder.preset.json"

SOFTPOS_QWEN_SYSTEM_PROMPT = (
    "Eres el Arquitecto Principal de Software e Ingeniero de Sistemas Financieros de SoftPOS Platform, "
    "ejecutándote sobre `qwen2.5-coder-14b-instruct-mlx` en Apple Silicon (Mac Mini M4 Pro).\n\n"
    "REGLAS INQUEBRANTABLES DE INGENIERÍA EN SOFTPOS:\n"
    "1. ZERO-LAZY CODING: Entrega SIEMPRE código completo, funcional y listo para producción "
    "(Go 1.22+, Android Kotlin 2.0 o TypeScript) o JSON estrictamente válido según se solicite. "
    "Prohibido truncar bloques con `# TODO`, `// ... resto del código`, comentarios de elipsis o implementaciones vacías.\n"
    "2. ARITMÉTICA FINANCIERA DETERMINISTA: Cero números en punto flotante (float32/float64/Double/Float) para montos monetarios o centavos. "
    "Usa exclusivamente enteros de 64 bits (int64 en Go, Long en Kotlin) y puntos base (bps) con redondeo bancario half-up.\n"
    "3. SANITIZACIÓN ESTRICTA DE MEMORIA (PCI MPoC / SCRP): Búferes sensibles de tarjetas (PAN, CVV, tracks EMV) o PIN "
    "deben limpiarse explícitamente (ByteArray.fill(0) o sobrescritura con ceros) dentro de bloques finally.\n"
    "4. REGLAS CVM CONTACTLESS (MÉXICO / BANXICO):\n"
    "   - Montos <= $1,000.00 MXN (100,000 centavos): Aprobación sin CVM (NoCvmRequired).\n"
    "   - Montos > $1,000.00 MXN con tarjeta física: Requiere PIN-on-Glass con teclado aleatorizado (PinRequired).\n"
    "   - Montos > $1,000.00 MXN con billetera digital (Apple Pay / Google Pay): Aprobación directa vía biometría del comprador (CdcvmVerified) sin límite de $1,000 MXN.\n"
    "5. CONCURRENCIA, SEGURIDAD Y TIPADO ESTRICTO: En Go usa sync.Mutex/RWMutex, canales, manejo explícito de errores y cero data races (go test -race). "
    "En Android Kotlin usa Clean Architecture, data class, interfaces selladas, corrutinas, StateFlow, FLAG_SECURE y Google Play Integrity."
)

# Hardware- & Task-Optimized LM Studio Presets for SoftPOS (Mac Mini M4 Pro — 24 GB RAM)
LM_STUDIO_PRESETS: Dict[str, Dict[str, Any]] = {
    "softpos_coder_dev": {
        "model": QWEN_CODER_MODEL,
        "identifier": "qwen2.5-coder",
        "context_length": int(os.getenv("SOFTPOS_QWEN_CTX", "16384")),
        "gpu_offload": "max",
        "temperature": 0.15,
        "min_p": 0.05,
        "top_p": 0.92,
        "top_k": 20,
        "repeat_penalty": 1.05,
        "max_tokens": 4096,
    },
    "softpos_quant_json": {
        "model": QWEN_CODER_MODEL,
        "identifier": "qwen2.5-coder",
        "context_length": int(os.getenv("SOFTPOS_QWEN_CTX", "16384")),
        "gpu_offload": "max",
        "temperature": 0.12,
        "min_p": 0.05,
        "top_p": 0.90,
        "top_k": 20,
        "repeat_penalty": 1.05,
        "max_tokens": 2200,
    },
    "softpos_reasoning_r1": {
        "model": DEEPSEEK_R1_MODEL,
        "identifier": "deepseek-r1-distill-qwen-14b",
        "context_length": int(os.getenv("SOFTPOS_R1_CTX", "16384")),
        "gpu_offload": "max",
        "temperature": 0.50,
        "min_p": 0.05,
        "top_p": 0.95,
        "top_k": 40,
        "repeat_penalty": 1.05,
        "max_tokens": 4096,
    },
    "softpos_fast_gemma": {
        "model": GEMMA_FAST_MODEL,
        "identifier": "gemma-4",
        "context_length": int(os.getenv("SOFTPOS_GEMMA_CTX", "8192")),
        "gpu_offload": "max",
        "temperature": 0.20,
        "min_p": 0.05,
        "top_p": 0.90,
        "top_k": 20,
        "repeat_penalty": 1.05,
        "max_tokens": 2048,
    },
}

# Compatibility dictionary for legacy loop runner calls
MODELS_CONFIG: Dict[str, Dict[str, Any]] = {
    "qwen": {
        "key": QWEN_CODER_MODEL,
        "identifier": "qwen2.5-coder",
        "role": "The Coder (Implementation & TDD greening)",
        "default_temp": 0.15,
        "default_max_tokens": 4096,
        "preset_key": "softpos_coder_dev",
        "is_reasoning": False,
    },
    "deepseek": {
        "key": DEEPSEEK_R1_MODEL,
        "identifier": "deepseek-r1-distill-qwen-14b",
        "role": "The Auditor & Security Reasoner (Deep CoT verification, concurrency & SAT compliance)",
        "default_temp": 0.5,
        "default_max_tokens": 4096,
        "preset_key": "softpos_reasoning_r1",
        "is_reasoning": True,
    },
    "gemma": {
        "key": GEMMA_FAST_MODEL,
        "identifier": "gemma-4",
        "role": "Secondary Auditor (Fast review fallback)",
        "default_temp": 0.2,
        "default_max_tokens": 2048,
        "preset_key": "softpos_fast_gemma",
        "is_reasoning": False,
    },
}


def extract_thinking_and_answer(raw_text: str) -> Tuple[str, str]:
    """Separates `<think>...</think>` reasoning blocks (produced by DeepSeek-R1)
    from the final structured response.
    """
    if not raw_text:
        return "", ""
    think_matches = re.findall(r"<think>(.*?)</think>", raw_text, flags=re.DOTALL)
    thinking = "\n\n".join(m.strip() for m in think_matches if m.strip())
    clean_answer = re.sub(r"<think>.*?</think>", "", raw_text, flags=re.DOTALL).strip()
    return thinking, clean_answer


def strip_thinking_blocks(text: str) -> str:
    """Removes `<think>...</think>` blocks emitted by DeepSeek R1 models."""
    cleaned = re.sub(r"<think>.*?</think>", "", text, flags=re.DOTALL)
    if "<think>" in cleaned and "</think>" not in cleaned:
        parts = cleaned.split("<think>", 1)
        cleaned = parts[0]
    return cleaned.strip()


def extract_json_object(raw_text: str) -> Dict[str, Any]:
    """Extracts and parses a JSON object from an LLM response (handling markdown fences and <think> tags)."""
    _, cleaned = extract_thinking_and_answer(raw_text)
    fence_match = re.search(r"```(?:json)?\s*(\{.*?\})\s*```", cleaned, flags=re.DOTALL)
    candidate = fence_match.group(1) if fence_match else cleaned
    if not fence_match:
        brace_match = re.search(r"(\{.*\})", cleaned, flags=re.DOTALL)
        if brace_match:
            candidate = brace_match.group(1)
    return json.loads(candidate)


class LMStudioManager:
    """Controls LM Studio CLI (`lms`) and OpenAI-compatible inference server (`localhost:1234/v1`)
    with strict sequential VRAM protection for 24 GB Apple Silicon.
    """

    def __init__(
        self,
        lms_bin: str = LMS_CLI_PATH,
        base_url: str = LM_STUDIO_BASE_URL,
        api_key: str = LM_STUDIO_API_KEY,
        timeout: float = 360.0,
    ) -> None:
        self.lms_bin = lms_bin
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout
        self.presets = dict(LM_STUDIO_PRESETS)

    def _run_lms(self, args: List[str]) -> str:
        """Executes a sub-command via LM Studio CLI (`lms`)."""
        if not Path(self.lms_bin).exists():
            raise FileNotFoundError(f"LM Studio CLI no encontrado en {self.lms_bin}")
        cmd = [self.lms_bin] + args
        res = subprocess.run(cmd, capture_output=True, text=True, check=False)
        return (res.stdout or "") + "\n" + (res.stderr or "")

    def ensure_server_running(self) -> None:
        """Starts the LM Studio local server if it is not already listening."""
        status_out = self._run_lms(["status"])
        if "OFF" in status_out or "not running" in status_out.lower():
            logger.info("Iniciando servidor local LM Studio...")
            self._run_lms(["server", "start"])
            time.sleep(2.0)

    def get_loaded_models(self) -> List[str]:
        """Returns list of currently loaded model identifiers in LM Studio VRAM."""
        ps_out = self._run_lms(["ps"])
        loaded: List[str] = []
        for line in ps_out.splitlines():
            s = line.strip()
            if not s or s.startswith("IDENTIFIER") or "no models" in s.lower() or s.startswith("Loaded Models") or s.startswith("·"):
                continue
            parts = s.split()
            if parts:
                loaded.append(parts[0])
        return loaded

    def resolve_preset(
        self,
        model_key: str,
        preset: Optional[str] = None,
        json_schema: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        """Resolves optimal inference and VRAM preset parameters."""
        if preset and preset in self.presets:
            return dict(self.presets[preset])
        m_low = model_key.lower()
        if "qwen" in m_low or "coder" in m_low:
            key = "softpos_quant_json" if json_schema else "softpos_coder_dev"
            return dict(self.presets[key])
        if "deepseek" in m_low or "r1" in m_low:
            return dict(self.presets["softpos_reasoning_r1"])
        if "gemma" in m_low:
            return dict(self.presets["softpos_fast_gemma"])
        return {
            "model": model_key,
            "identifier": model_key,
            "context_length": 16384,
            "gpu_offload": "max",
            "temperature": 0.15,
            "min_p": 0.05,
            "top_p": 0.92,
            "top_k": 20,
            "repeat_penalty": 1.05,
            "max_tokens": 4096,
        }

    def switch_model(
        self,
        model_key: str,
        identifier: Optional[str] = None,
        context_length: Optional[int] = None,
        gpu_offload: str = "max",
    ) -> str:
        """Ensures ONLY `model_key` is loaded in VRAM with optimal context length and GPU offload.
        If another heavy model is loaded, executes `lms unload --all` first to prevent RAM swap.
        """
        self.ensure_server_running()
        preset_cfg = self.resolve_preset(model_key)
        target_id = identifier or preset_cfg.get("identifier") or model_key
        loaded = self.get_loaded_models()

        # If exact model is already the sole loaded model, reuse it immediately (0 reload overhead)
        if len(loaded) == 1 and (target_id in loaded[0] or model_key in loaded[0]):
            logger.info("Modelo '%s' ya activo en VRAM (0 recarga).", loaded[0])
            return loaded[0]

        if loaded:
            logger.info("Liberando VRAM en LM Studio (descargando: %s)...", ", ".join(loaded))
            self._run_lms(["unload", "--all"])
            time.sleep(1.5)

        ctx_len = context_length or int(preset_cfg.get("context_length", 16384))
        gpu_val = gpu_offload or str(preset_cfg.get("gpu_offload", "max"))

        logger.info("Cargando modelo '%s' en LM Studio VRAM (ctx=%d, gpu=%s)...", model_key, ctx_len, gpu_val)
        load_args = [
            "load",
            model_key,
            "--gpu",
            gpu_val,
            "--context-length",
            str(ctx_len),
            "-y",
        ]
        if target_id:
            load_args.extend(["--identifier", target_id])
        
        load_res = self._run_lms(load_args)
        time.sleep(2.0)
        return target_id

    def unload_all(self) -> None:
        """Unloads all models from LM Studio VRAM."""
        self._run_lms(["unload", "--all"])

    def chat_completion(
        self,
        model_key: str,
        messages: Union[str, List[Dict[str, str]]],
        system_prompt: Optional[str] = None,
        temperature: Optional[float] = None,
        max_tokens: Optional[int] = None,
        auto_switch_vram: bool = True,
        preset: Optional[str] = None,
        json_schema: Optional[Dict[str, Any]] = None,
        timeout: Optional[float] = None,
    ) -> Dict[str, Any]:
        """Executes a chat completion against LM Studio using SoftPOS's tuned sampling parameters
        (`min_p`, `top_p`, `top_k`, `repeat_penalty`, `temperature`, `max_tokens`).
        Returns `{'model': ..., 'preset': ..., 'thinking': ..., 'content': ..., 'raw': ...}`.
        """
        preset_cfg = self.resolve_preset(model_key, preset=preset, json_schema=json_schema)
        
        active_model = (
            self.switch_model(
                model_key,
                identifier=preset_cfg.get("identifier"),
                context_length=int(preset_cfg.get("context_length", 16384)),
                gpu_offload=str(preset_cfg.get("gpu_offload", "max")),
            )
            if auto_switch_vram
            else (preset_cfg.get("identifier") or model_key)
        )

        eff_temp = temperature if temperature is not None else float(preset_cfg.get("temperature", 0.15))
        eff_max_tokens = max_tokens if max_tokens is not None else int(preset_cfg.get("max_tokens", 4096))
        eff_timeout = timeout or self.timeout

        msg_list: List[Dict[str, str]] = []
        eff_sys = system_prompt or SOFTPOS_QWEN_SYSTEM_PROMPT
        if eff_sys:
            msg_list.append({"role": "system", "content": eff_sys})

        if isinstance(messages, str):
            msg_list.append({"role": "user", "content": messages})
        else:
            msg_list.extend(messages)

        payload: Dict[str, Any] = {
            "model": active_model,
            "messages": msg_list,
            "temperature": eff_temp,
            "min_p": float(preset_cfg.get("min_p", 0.05)),
            "top_p": float(preset_cfg.get("top_p", 0.92)),
            "top_k": int(preset_cfg.get("top_k", 20)),
            "repeat_penalty": float(preset_cfg.get("repeat_penalty", 1.05)),
            "max_tokens": eff_max_tokens,
            "stream": False,
        }

        if json_schema is not None:
            payload["response_format"] = {
                "type": "json_schema",
                "json_schema": json_schema,
            }

        headers = {
            "Authorization": f"Bearer {self.api_key}",
            "Content-Type": "application/json",
        }

        req = urllib.request.Request(
            f"{self.base_url}/chat/completions",
            data=json.dumps(payload).encode("utf-8"),
            headers=headers,
            method="POST",
        )

        try:
            with urllib.request.urlopen(req, timeout=eff_timeout) as resp:
                data = json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            err_body = e.read().decode("utf-8", errors="replace")
            # Fallback if MLX engine rejects response_format
            if e.code >= 400 and "response_format" in payload:
                logger.warning("Backend MLX rechazó response_format; reintentando sin restricción estricta de esquema...")
                payload.pop("response_format", None)
                req_fallback = urllib.request.Request(
                    f"{self.base_url}/chat/completions",
                    data=json.dumps(payload).encode("utf-8"),
                    headers=headers,
                    method="POST",
                )
                with urllib.request.urlopen(req_fallback, timeout=eff_timeout) as resp:
                    data = json.loads(resp.read().decode("utf-8"))
            else:
                raise RuntimeError(f"LM Studio HTTP error {e.code}: {err_body}") from e

        choice = data["choices"][0]["message"]
        raw_content = choice.get("content", "") or ""
        reasoning_field = choice.get("reasoning_content", "") or ""

        thinking, clean_answer = extract_thinking_and_answer(raw_content)
        if reasoning_field and not thinking:
            thinking = reasoning_field.strip()

        return {
            "model": model_key,
            "identifier": active_model,
            "preset": preset or ("softpos_quant_json" if json_schema else "softpos_coder_dev"),
            "thinking": thinking,
            "content": clean_answer if clean_answer else raw_content,
            "raw": raw_content,
        }

    def ask_qwen_coder(
        self,
        user_prompt: str,
        system_prompt: Optional[str] = None,
        max_tokens: int = 4096,
        temperature: float = 0.15,
    ) -> Dict[str, Any]:
        """Direct helper to invoke `qwen2.5-coder-14b-instruct-mlx` using the
        SoftPOS Coder Dev preset for code generation, bug fixing, and architecture tasks.
        """
        return self.chat_completion(
            model_key=QWEN_CODER_MODEL,
            messages=user_prompt,
            system_prompt=system_prompt or SOFTPOS_QWEN_SYSTEM_PROMPT,
            temperature=temperature,
            max_tokens=max_tokens,
            auto_switch_vram=True,
            preset="softpos_coder_dev",
        )


# Global default manager instance
_DEFAULT_MANAGER = LMStudioManager()


# Module-level backward-compatible API functions
def get_loaded_model_identifier() -> Optional[str]:
    """Check which model is currently loaded in memory via LM Studio CLI (`lms ps`)."""
    loaded = _DEFAULT_MANAGER.get_loaded_models()
    for name, cfg in MODELS_CONFIG.items():
        for l in loaded:
            if cfg["identifier"] in l or cfg["key"] in l:
                return cfg["identifier"]
    return loaded[0] if loaded else None


def ensure_model(model_name: str) -> str:
    """Ensure the requested model ('qwen', 'deepseek', or 'gemma') is loaded in LM Studio.
    Returns the identifier to use for inference.
    """
    if model_name not in MODELS_CONFIG:
        # Check if direct model_key was passed
        return _DEFAULT_MANAGER.switch_model(model_name)

    cfg = MODELS_CONFIG[model_name]
    target_key = cfg["key"]
    target_id = cfg["identifier"]
    preset_key = cfg.get("preset_key")
    preset_cfg = LM_STUDIO_PRESETS.get(preset_key, {})
    ctx = int(preset_cfg.get("context_length", 16384))

    return _DEFAULT_MANAGER.switch_model(
        model_key=target_key,
        identifier=target_id,
        context_length=ctx,
        gpu_offload="max",
    )


def chat_completion(
    model_name: str,
    messages: List[Dict[str, str]],
    temperature: Optional[float] = None,
    max_tokens: Optional[int] = None,
    system_prompt: Optional[str] = None,
    timeout: int = 180,
) -> str:
    """Backward-compatible chat completion wrapper for `loop_runner.py`.
    Returns the cleaned assistant text response.
    """
    model_key = MODELS_CONFIG.get(model_name, {}).get("key", model_name)
    preset_key = MODELS_CONFIG.get(model_name, {}).get("preset_key")

    res = _DEFAULT_MANAGER.chat_completion(
        model_key=model_key,
        messages=messages,
        system_prompt=system_prompt,
        temperature=temperature,
        max_tokens=max_tokens,
        auto_switch_vram=True,
        preset=preset_key,
        timeout=float(timeout),
    )
    return res["content"]


if __name__ == "__main__":
    if len(sys.argv) > 1:
        cmd = sys.argv[1].lower()
        if cmd == "test":
            target = sys.argv[2] if len(sys.argv) > 2 else "qwen"
            print(f"[ModelManager] Testing chat completion with '{target}'...")
            reply = chat_completion(
                model_name=target,
                messages=[{"role": "user", "content": "Respond only with: READY_FOR_SOFTPOS_ENGINEERING"}],
                max_tokens=100,
            )
            print(f"Response: {reply}")
        elif cmd == "ask":
            query = " ".join(sys.argv[2:]) if len(sys.argv) > 2 else "Verifica las reglas CVM de México en SoftPOS."
            print(f"[Qwen 2.5 Coder] Consultando: {query}\n")
            res = _DEFAULT_MANAGER.ask_qwen_coder(query)
            print(f"\n--- Respuesta de Qwen 2.5 Coder 14B ({res['identifier']}) ---")
            print(res["content"])
            print("----------------------------------------------------------------")
        elif cmd == "status":
            print(_DEFAULT_MANAGER._run_lms(["status"]))
        elif cmd == "ps":
            print(_DEFAULT_MANAGER._run_lms(["ps"]))
        else:
            print("Comandos disponibles: test [qwen|deepseek|gemma], ask <pregunta>, status, ps")
    else:
        print("Uso: python3 tools/loop/models_manager.py [test|ask|status|ps]")

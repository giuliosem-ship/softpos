"""
Unit tests for the LM Studio Preset & Qwen 2.5 Coder 14B Connector (tools/loop/models_manager.py).
Verifies:
1. LM Studio preset JSON file existence and schema validity.
2. Preset resolution for `qwen2.5-coder-14b-instruct-mlx`, `deepseek-r1-distill-qwen-14b`, and `google/gemma-4-12b-qat`.
3. `<think>` reasoning block separation, strip_thinking_blocks, and JSON object extraction.
4. Sequential VRAM switching (`lms unload --all` before loading heavy models) and `ask_qwen_coder` payload construction.
5. Backward compatibility with `chat_completion` and `ensure_model`.
"""
from __future__ import annotations

import io
import json
import os
import sys
from pathlib import Path

# Ensure repo root is on sys.path
REPO_ROOT = Path(__file__).resolve().parent.parent.parent
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

import unittest
from typing import Any, Dict, List
from unittest.mock import MagicMock, patch

from tools.loop.models_manager import (
    DEEPSEEK_R1_MODEL,
    GEMMA_FAST_MODEL,
    LM_STUDIO_PRESETS,
    MODELS_CONFIG,
    QWEN_CODER_MODEL,
    SOFTPOS_PRESET_FILE,
    SOFTPOS_QWEN_SYSTEM_PROMPT,
    LMStudioManager,
    chat_completion,
    ensure_model,
    extract_json_object,
    extract_thinking_and_answer,
    strip_thinking_blocks,
)


class TestLMSQwenConnector(unittest.TestCase):
    def test_lm_studio_preset_file_and_config_presets(self):
        """Ensures the SoftPOS-Payment-Coder.preset.json exists and adheres to LM Studio specifications."""
        self.assertTrue(SOFTPOS_PRESET_FILE.exists(), f"Preset file missing at {SOFTPOS_PRESET_FILE}")

        raw_preset = json.loads(SOFTPOS_PRESET_FILE.read_text(encoding="utf-8"))
        self.assertIn("Qwen 2.5 Coder 14B", raw_preset["name"])
        self.assertIn("SoftPOS", raw_preset["name"])

        load_fields = {f["key"]: f["value"] for f in raw_preset["load"]["fields"]}
        self.assertEqual(load_fields["llm.load.contextLength"], 16384)
        self.assertTrue(load_fields["llm.load.llama.flashAttention"])
        self.assertEqual(load_fields["llm.load.llama.acceleration.offloadRatio"], 1)

        op_fields = {f["key"]: f["value"] for f in raw_preset["operation"]["fields"]}
        self.assertEqual(op_fields["llm.prediction.temperature"], 0.15)
        self.assertEqual(op_fields["llm.prediction.minPSampling"]["value"], 0.05)
        self.assertEqual(op_fields["llm.prediction.topPSampling"]["value"], 0.92)
        self.assertEqual(op_fields["llm.prediction.topKSampling"], 20)
        self.assertEqual(op_fields["llm.prediction.repeatPenalty"]["value"], 1.05)
        self.assertEqual(op_fields["llm.prediction.maxPredictedTokens"]["value"], 4096)

        sys_prompt = op_fields["llm.prediction.systemPrompt"]
        self.assertIn("ZERO-LAZY CODING", sys_prompt)
        self.assertIn("ARITMÉTICA FINANCIERA DETERMINISTA", sys_prompt)
        self.assertIn("PCI MPoC / SCRP", sys_prompt)
        self.assertIn("REGLAS CVM CONTACTLESS", sys_prompt)

        # In-memory presets dictionary
        self.assertIn("softpos_coder_dev", LM_STUDIO_PRESETS)
        self.assertIn("softpos_quant_json", LM_STUDIO_PRESETS)
        self.assertIn("softpos_reasoning_r1", LM_STUDIO_PRESETS)
        self.assertIn("softpos_fast_gemma", LM_STUDIO_PRESETS)

    def test_resolve_preset_and_parsers(self):
        """Verifies preset resolution for Qwen Coder (dev vs JSON) and extraction helpers."""
        mgr = LMStudioManager()

        dev_preset = mgr.resolve_preset("qwen2.5-coder-14b-instruct-mlx")
        self.assertEqual(dev_preset["temperature"], 0.15)
        self.assertEqual(dev_preset["max_tokens"], 4096)
        self.assertEqual(dev_preset["context_length"], 16384)
        self.assertEqual(dev_preset["min_p"], 0.05)
        self.assertEqual(dev_preset["top_p"], 0.92)
        self.assertEqual(dev_preset["top_k"], 20)
        self.assertEqual(dev_preset["repeat_penalty"], 1.05)

        json_preset = mgr.resolve_preset(
            "qwen2.5-coder-14b-instruct-mlx",
            json_schema={"name": "test_schema", "strict": True, "schema": {"type": "object"}},
        )
        self.assertEqual(json_preset["temperature"], 0.12)
        self.assertEqual(json_preset["top_p"], 0.90)
        self.assertEqual(json_preset["max_tokens"], 2200)

        # Reasoning separation and strip helpers
        sample_cot = (
            "<think>\nVerificando regla CVM de $1,000 MXN y CDCVM para Apple Pay.\n</think>\n"
            "```json\n{\"cvm_required\": false, \"auth_mode\": \"CdcvmVerified\"}\n```"
        )
        thinking, clean = extract_thinking_and_answer(sample_cot)
        self.assertIn("Verificando regla CVM", thinking)
        self.assertNotIn("<think>", clean)

        stripped = strip_thinking_blocks(sample_cot)
        self.assertNotIn("<think>", stripped)
        self.assertIn("```json", stripped)

        parsed = extract_json_object(clean)
        self.assertEqual(parsed, {"cvm_required": False, "auth_mode": "CdcvmVerified"})

    @patch("time.sleep", return_value=None)
    def test_sequential_vram_switch_and_ask_qwen_coder(self, _mock_sleep):
        """Verifies that LMStudioManager unloads existing models before loading Qwen 2.5 Coder 14B."""
        mgr = LMStudioManager()
        lms_calls: List[List[str]] = []

        def fake_run_lms(args: List[str]) -> str:
            lms_calls.append(args)
            if args == ["status"]:
                return "Server: ON (port: 1234)"
            if args == ["ps"]:
                return "IDENTIFIER    MODEL\ngemma-4       google/gemma-4-12b-qat"
            return "OK"

        mgr._run_lms = fake_run_lms

        captured_payload: Dict[str, Any] = {}

        class DummyHTTPResponse:
            def __enter__(self):
                return self

            def __exit__(self, *args):
                pass

            def read(self):
                resp_dict = {
                    "choices": [
                        {
                            "message": {
                                "content": '{"status": "ready", "architecture": "SoftPOS Clean Kotlin"}'
                            }
                        }
                    ]
                }
                return json.dumps(resp_dict).encode("utf-8")

        def fake_urlopen(req, timeout=None):
            data = json.loads(req.data.decode("utf-8"))
            captured_payload.update(data)
            return DummyHTTPResponse()

        with patch("urllib.request.urlopen", fake_urlopen):
            res = mgr.ask_qwen_coder("Genera el contrato de StoreAndForwardQueue en Kotlin.")

        self.assertEqual(res["model"], QWEN_CODER_MODEL)
        self.assertEqual(res["preset"], "softpos_coder_dev")
        parsed = extract_json_object(res["content"])
        self.assertEqual(parsed["status"], "ready")

        # Verify sequential VRAM unload before loading Qwen 14B
        self.assertIn(["unload", "--all"], lms_calls)
        load_call = next(c for c in lms_calls if c[0] == "load")
        self.assertIn(QWEN_CODER_MODEL, load_call)
        self.assertIn("--context-length", load_call)
        self.assertIn("16384", load_call)
        self.assertIn("--gpu", load_call)
        self.assertIn("max", load_call)

        # Verify sampling parameters injected in payload
        self.assertEqual(captured_payload["temperature"], 0.15)
        self.assertEqual(captured_payload["min_p"], 0.05)
        self.assertEqual(captured_payload["top_p"], 0.92)
        self.assertEqual(captured_payload["top_k"], 20)
        self.assertEqual(captured_payload["repeat_penalty"], 1.05)
        self.assertEqual(captured_payload["messages"][0]["content"], SOFTPOS_QWEN_SYSTEM_PROMPT)

    @patch("time.sleep", return_value=None)
    def test_backwards_compatibility(self, _mock_sleep):
        """Verifies backward compatibility with existing loop_runner interface."""
        with patch.object(LMStudioManager, "_run_lms") as mock_lms:
            mock_lms.side_effect = lambda args: (
                "Server: ON (port: 1234)" if args == ["status"]
                else "IDENTIFIER    MODEL\nqwen2.5-coder qwen2.5-coder-14b-instruct-mlx" if args == ["ps"]
                else "OK"
            )

            class DummyHTTPResponse:
                def __enter__(self):
                    return self

                def __exit__(self, *args):
                    pass

                def read(self):
                    return json.dumps({
                        "choices": [{"message": {"content": "```kotlin\nclass TestModule\n```"}}]
                    }).encode("utf-8")

            with patch("urllib.request.urlopen", return_value=DummyHTTPResponse()):
                ident = ensure_model("qwen")
                self.assertEqual(ident, "qwen2.5-coder")

                resp = chat_completion(
                    model_name="qwen",
                    messages=[{"role": "user", "content": "Implement TestModule"}],
                )
                self.assertIn("class TestModule", resp)


if __name__ == "__main__":
    unittest.main()

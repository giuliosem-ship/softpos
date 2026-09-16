#!/usr/bin/env python3
"""
loop_runner.py
Autonomous Multi-Model Engineering Loop Orchestrator:
- Architect: Antigravity / Gemini (High-level specs & TDD tests)
- Coder: Qwen 2.5 Coder (via LM Studio hot-swap)
- Auditor: Gemma 4 (via LM Studio hot-swap)
- Test Runner: Python unittest / subprocess
- Version Control: Automated atomic Git commits
"""

import argparse
import datetime
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path
from typing import Dict, Any, Optional

try:
    from tools.loop.models_manager import chat_completion, ensure_model
except ImportError:
    from models_manager import chat_completion, ensure_model

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
TASKS_FILE = Path(__file__).resolve().parent / "tasks.json"

def load_tasks() -> Dict[str, Any]:
    with open(TASKS_FILE, "r", encoding="utf-8") as f:
        return json.load(f)

def save_tasks(data: Dict[str, Any]) -> None:
    with open(TASKS_FILE, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)

def extract_python_code(text: str) -> str:
    """Extract code block from LLM markdown response."""
    code_blocks = re.findall(r"```(?:python)?\s*\n(.*?)\n```", text, re.DOTALL)
    if code_blocks:
        # Return the largest code block if multiple exist
        return max(code_blocks, key=len).strip()
    return text.strip()

def run_tests_for_task(test_path: str) -> subprocess.CompletedProcess:
    """Run python unittest for the specified test file."""
    full_path = REPO_ROOT / test_path
    if not full_path.exists():
        raise FileNotFoundError(f"Test file not found: {full_path}")
    
    # Run test with PYTHONPATH set to project root
    env = os.environ.copy()
    env["PYTHONPATH"] = str(REPO_ROOT)
    
    proc = subprocess.run(
        [sys.executable, "-m", "unittest", str(full_path)],
        cwd=str(REPO_ROOT),
        capture_output=True,
        text=True,
        env=env,
        check=False
    )
    return proc

def coder_generate_implementation(task: Dict[str, Any], previous_error: Optional[str] = None) -> str:
    """Prompt Qwen 2.5 Coder to write or fix implementation."""
    impl_path = REPO_ROOT / task["files"]["implementation"]
    test_path = REPO_ROOT / task["files"]["tests"]
    
    with open(test_path, "r", encoding="utf-8") as f:
        test_code = f.read()

    current_code = ""
    if impl_path.exists():
        with open(impl_path, "r", encoding="utf-8") as f:
            current_code = f.read()

    system_prompt = (
        "You are a Principal Software Engineer implementing a high-throughput financial and fiscal platform in Mexico.\n"
        "Write pure, production-grade, type-annotated Python 3.12 code. Follow PEP8, clean architecture, and strict immutability.\n"
        "IMPORTANT RULES:\n"
        "- For immutable data models requiring initialization validation (such as enforcing balanced debits/credits or non-empty lists), use `@dataclass(frozen=True)`. Do NOT use `NamedTuple` with `__post_init__` because NamedTuple does NOT call `__post_init__`.\n"
        "- Ensure all imports, types, and exceptions match the specification and tests exactly.\n"
        "Return ONLY the complete, executable Python code inside a ```python ``` block."
    )

    prompt = f"TASK ID: {task['id']}\n"
    prompt += f"TITLE: {task['title']}\n"
    prompt += f"SPECIFICATION:\n{task['spec']}\n\n"
    prompt += f"TEST SUITE ({task['files']['tests']}):\n```python\n{test_code}\n```\n\n"

    if previous_error:
        prompt += f"CURRENT IMPLEMENTATION:\n```python\n{current_code}\n```\n\n"
        prompt += f"THE TEST FAILED WITH THIS ERROR TRACE:\n```\n{previous_error}\n```\n\n"
        prompt += "Please fix the implementation so that all unit tests pass completely."
    else:
        prompt += f"Implement the required module to be saved at `{task['files']['implementation']}` so that all tests pass."

    print(f"\n🤖 [Coder: Qwen 2.5 Coder] Generating implementation for {task['id']}...")
    reply = chat_completion(
        model_name="qwen",
        messages=[{"role": "user", "content": prompt}],
        system_prompt=system_prompt,
        temperature=0.1,
        max_tokens=3000
    )
    code = extract_python_code(reply)
    
    # Write to target file
    impl_path.parent.mkdir(parents=True, exist_ok=True)
    with open(impl_path, "w", encoding="utf-8") as f:
        f.write(code + "\n")
    print(f"💾 [Coder] Saved code to {task['files']['implementation']} ({len(code.splitlines())} lines).")
    return code

def auditor_review_code(task: Dict[str, Any]) -> str:
    """Prompt Gemma 4 to review implementation and tests."""
    impl_path = REPO_ROOT / task["files"]["implementation"]
    test_path = REPO_ROOT / task["files"]["tests"]

    with open(impl_path, "r", encoding="utf-8") as f:
        impl_code = f.read()
    with open(test_path, "r", encoding="utf-8") as f:
        test_code = f.read()

    system_prompt = (
        "You are a Senior Security Auditor and Financial Systems Architect.\n"
        "Review the code for: numerical precision (avoiding float for money), concurrency safety, SAT tax compliance, edge cases.\n"
        "Provide a concise, constructive audit report with 3-5 bullet points."
    )

    prompt = f"TASK: {task['title']}\n\n"
    prompt += f"IMPLEMENTATION:\n```python\n{impl_code}\n```\n\n"
    prompt += f"TESTS:\n```python\n{test_code}\n```\n\n"
    prompt += "Audit this implementation and summarize your findings."

    print(f"\n🔍 [Auditor: Gemma 4] Reviewing code for {task['id']}...")
    review = chat_completion(
        model_name="gemma",
        messages=[{"role": "user", "content": prompt}],
        system_prompt=system_prompt,
        temperature=0.2,
        max_tokens=1500
    )
    return review

def git_commit_task(task: Dict[str, Any]) -> None:
    """Stage files and create a clean git commit."""
    files_to_add = [
        task["files"]["tests"],
        task["files"]["implementation"],
        "tools/loop/tasks.json"
    ]
    for rel_path in files_to_add:
        full = REPO_ROOT / rel_path
        if full.exists():
            subprocess.run(["git", "add", str(full)], cwd=str(REPO_ROOT), check=False)

    msg = f"feat({task['component']}): {task['title']} [{task['id']}]\n\n- Implemented by Qwen 2.5 Coder\n- Audited by Gemma 4\n- Verified with unit tests"
    commit_proc = subprocess.run(["git", "commit", "-m", msg], cwd=str(REPO_ROOT), capture_output=True, text=True, check=False)
    if commit_proc.returncode == 0:
        print(f"📦 [Git] Committed changes: {msg.splitlines()[0]}")
    else:
        print(f"⚠️ [Git] Commit output: {commit_proc.stdout or commit_proc.stderr}")

def execute_task(task: Dict[str, Any], max_retries: int = 3) -> bool:
    print("=" * 70)
    print(f"🚀 EXECUTING TASK: {task['id']} - {task['title']}")
    print("=" * 70)

    test_path = REPO_ROOT / task["files"]["tests"]
    if not test_path.exists():
        print(f"❌ Test file missing: {test_path}. Architect must provide tests first.")
        return False

    task["status"] = "in_progress"
    save_tasks(load_tasks_and_update(task))

    err_trace = None
    tests_passed = False

    # Check if implementation already satisfies test suite
    initial_test = run_tests_for_task(task["files"]["tests"])
    if initial_test.returncode == 0:
        print("✅ [Runner] Implementation already satisfies tests! Proceeding to auditor review...")
        tests_passed = True
        task["test_results"] = "PASS: " + initial_test.stderr.strip()
    else:
        for attempt in range(1, max_retries + 1):
            print(f"\n--- [Iteration {attempt}/{max_retries}] ---")
            coder_generate_implementation(task, previous_error=err_trace)

            # Run test
            print(f"🧪 [Runner] Running test suite: {task['files']['tests']}...")
            test_proc = run_tests_for_task(task["files"]["tests"])
            
            if test_proc.returncode == 0:
                print("✅ [Runner] All unit tests PASSED successfully!")
                tests_passed = True
                task["test_results"] = "PASS: " + test_proc.stderr.strip()
                break
            else:
                print("❌ [Runner] Tests failed!")
                err_trace = (test_proc.stdout + "\n" + test_proc.stderr).strip()
                print(f"Error summary: {err_trace[-300:]}")
                task["retries"] = attempt

    if not tests_passed:
        task["status"] = "failed"
        save_tasks(load_tasks_and_update(task))
        print(f"💥 Task {task['id']} failed after {max_retries} attempts.")
        return False

    # Auditing with Gemma 4
    task["status"] = "review"
    audit = auditor_review_code(task)
    task["audit_notes"] = audit
    print("\n📋 [Auditor Report]:\n" + audit + "\n")

    # Complete & Commit
    task["status"] = "completed"
    task["completed_at"] = datetime.datetime.now().isoformat()
    save_tasks(load_tasks_and_update(task))
    git_commit_task(task)

    print(f"🎉 Task {task['id']} COMPLETED SUCCESSFULLY!\n")
    return True

def load_tasks_and_update(updated_task: Dict[str, Any]) -> Dict[str, Any]:
    data = load_tasks()
    for idx, t in enumerate(data["tasks"]):
        if t["id"] == updated_task["id"]:
            data["tasks"][idx] = updated_task
            break
    return data

def print_status():
    data = load_tasks()
    print("\n" + "=" * 60)
    print(f"📊 ENGINEERING LOOP STATUS: {data['project']}")
    print("=" * 60)
    for t in data["tasks"]:
        status_emoji = {
            "pending": "⏳",
            "in_progress": "⚙️",
            "review": "🔍",
            "completed": "✅",
            "failed": "❌"
        }.get(t["status"], "❓")
        print(f"{status_emoji} [{t['id']}] {t['title']} ({t['status'].upper()})")
        if t["completed_at"]:
            print(f"   Completed at: {t['completed_at']}")
    print("=" * 60 + "\n")

def main():
    parser = argparse.ArgumentParser(description="Autonomous Multi-Model Engineering Loop")
    parser.add_argument("--status", action="store_true", help="Print backlog status")
    parser.add_argument("--step", action="store_true", help="Execute the next pending task")
    parser.add_argument("--all", action="store_true", help="Execute all pending tasks sequentially")
    parser.add_argument("--task", type=str, help="Execute a specific task ID")
    parser.add_argument("--swap", choices=["qwen", "gemma"], help="Manually hot-swap model in LM Studio")
    args = parser.parse_args()

    if args.swap:
        ensure_model(args.swap)
        return

    if args.status or (not args.step and not args.all and not args.task):
        print_status()
        return

    data = load_tasks()
    tasks_to_run = []
    if args.task:
        tasks_to_run = [t for t in data["tasks"] if t["id"] == args.task]
    elif args.step:
        pending = [t for t in data["tasks"] if t["status"] in ("pending", "in_progress", "failed")]
        if pending:
            tasks_to_run = [pending[0]]
    elif args.all:
        tasks_to_run = [t for t in data["tasks"] if t["status"] in ("pending", "in_progress", "failed")]

    if not tasks_to_run:
        print("🎉 No pending tasks to run!")
        return

    for t in tasks_to_run:
        ok = execute_task(t)
        if not ok and not args.all:
            break

if __name__ == "__main__":
    main()

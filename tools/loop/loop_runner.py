#!/usr/bin/env python3
"""
loop_runner.py
Autonomous Multi-Model Engineering Loop Orchestrator:
- Architect: Antigravity / Gemini (High-level specs & TDD tests)
- Coder: Qwen 2.5 Coder (via LM Studio hot-swap)
- Auditor: DeepSeek R1 Distill 14B (via LM Studio hot-swap, with Gemma 4 fallback)
- Test Runner: Go test / Python unittest via subprocess
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
HANDOFF_FILE = Path(__file__).resolve().parent / "HANDOFF.md"

def load_tasks() -> Dict[str, Any]:
    with open(TASKS_FILE, "r", encoding="utf-8") as f:
        return json.load(f)

def save_tasks(data: Dict[str, Any]) -> None:
    with open(TASKS_FILE, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)

def detect_lang(path_str: str) -> str:
    if path_str.endswith(".kt"):
        return "kotlin"
    if path_str.endswith(".go"):
        return "go"
    return "python"

def extract_code(text: str, lang: str = "python") -> str:
    """Extract code block from LLM markdown response."""
    if lang == "kotlin":
        pattern = r"```(?:kotlin|kt)?\s*\n(.*?)```"
    elif lang == "go":
        pattern = r"```(?:go|golang)?\s*\n(.*?)```"
    else:
        pattern = r"```(?:python)?\s*\n(.*?)```"
    code_blocks = re.findall(pattern, text, re.DOTALL)
    if code_blocks:
        return max(code_blocks, key=len).strip()
    cleaned = re.sub(r"^```[a-zA-Z]*\s*", "", text.strip())
    cleaned = re.sub(r"```\s*$", "", cleaned).strip()
    return cleaned

def run_tests_for_task(test_path: str, impl_path: Optional[str] = None) -> subprocess.CompletedProcess:
    """Run tests for the specified test file (Go, Kotlin, or Python)."""
    full_path = REPO_ROOT / test_path
    if not full_path.exists():
        raise FileNotFoundError(f"Test file not found: {full_path}")
    
    lang = detect_lang(test_path)

    if lang == "go":
        test_dir = full_path.parent
        rel_pkg = "./" + str(test_dir.relative_to(REPO_ROOT))
        proc = subprocess.run(
            ["/opt/homebrew/bin/go", "test", "-v", rel_pkg],
            cwd=str(REPO_ROOT),
            capture_output=True,
            text=True,
            check=False
        )
        return proc
    elif lang == "kotlin":
        gradlew = REPO_ROOT / "mobile" / "android" / "gradlew"
        test_class_name = full_path.stem
        env = os.environ.copy()
        if Path("/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home").exists():
            env["JAVA_HOME"] = "/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home"
            env["PATH"] = f"{env['JAVA_HOME']}/bin:/opt/homebrew/bin:" + env.get("PATH", "")

        if gradlew.exists():
            proc = subprocess.run(
                [str(gradlew), "-p", str(REPO_ROOT / "mobile" / "android"), ":app:test", "--tests", f"*{test_class_name}"],
                cwd=str(REPO_ROOT),
                capture_output=True,
                text=True,
                env=env,
                check=False
            )
            return proc
        else:
            proc = subprocess.run(
                ["gradle", "-p", str(REPO_ROOT / "mobile" / "android"), ":app:test", "--tests", f"*{test_class_name}"],
                cwd=str(REPO_ROOT),
                capture_output=True,
                text=True,
                env=env,
                check=False
            )
            return proc
    else:
        env = os.environ.copy()
        env["PYTHONPATH"] = str(REPO_ROOT)
        venv_py = REPO_ROOT / ".venv" / "bin" / "python"
        py_exec = str(venv_py) if venv_py.exists() else sys.executable

        proc = subprocess.run(
            [py_exec, "-m", "unittest", str(full_path)],
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
    lang = detect_lang(str(impl_path))
    
    with open(test_path, "r", encoding="utf-8") as f:
        test_code = f.read()

    current_code = ""
    if impl_path.exists():
        with open(impl_path, "r", encoding="utf-8") as f:
            current_code = f.read()

    if lang == "kotlin":
        system_prompt = (
            "You are a Principal Android & Kotlin Software Engineer implementing a high-throughput financial & SoftPOS platform in Mexico.\n"
            "Write pure, production-grade, idiomatic Kotlin 2.0 code. Follow Clean Architecture, strict immutability (data classes, sealed interfaces, StateFlow), Long cents / basis points arithmetic (zero Double/Float for money), and PCI-DSS memory hygiene.\n"
            "IMPORTANT RULES:\n"
            "- Keep the exact package declaration required by the file path and test suite.\n"
            "- Ensure all classes, data classes, methods, and signatures match the JUnit 5 test suite and specification 100%.\n"
            "Return ONLY the complete, compilable Kotlin code inside a ```kotlin ``` block."
        )
        lang_tag = "kotlin"
    elif lang == "go":
        system_prompt = (
            "You are a Principal Go Software Engineer implementing a high-throughput, low-latency financial and fiscal platform in Mexico.\n"
            "Write pure, production-grade, idiomatic Go 1.22+ code. Follow effective Go, explicit error handling, strict concurrency safety (sync.Mutex / RWMutex), zero allocations in hot paths, and immutability where required.\n"
            "IMPORTANT RULES:\n"
            "- Use proper package name matching the directory (e.g. package ledger, package stp, package fiscal, package identity).\n"
            "- Ensure all types, structs, exported methods, and functions match the test suite and specification exactly.\n"
            "Return ONLY the complete, compilable Go code inside a ```go ``` block."
        )
        lang_tag = "go"
    else:
        system_prompt = (
            "You are a Principal Software Engineer implementing a high-throughput financial and fiscal platform in Mexico.\n"
            "Write pure, production-grade, type-annotated Python 3.12 code. Follow PEP8, clean architecture, and strict immutability.\n"
            "IMPORTANT RULES:\n"
            "- For immutable data models requiring initialization validation, use `@dataclass(frozen=True)`.\n"
            "- Ensure all imports, types, and exceptions match the specification and tests exactly.\n"
            "Return ONLY the complete, executable Python code inside a ```python ``` block."
        )
        lang_tag = "python"

    prompt = f"TASK ID: {task['id']}\n"
    prompt += f"TITLE: {task['title']}\n"
    prompt += f"SPECIFICATION:\n{task['spec']}\n\n"
    prompt += f"TEST SUITE ({task['files']['tests']}):\n```{lang_tag}\n{test_code}\n```\n\n"

    if previous_error:
        prompt += f"CURRENT IMPLEMENTATION:\n```{lang_tag}\n{current_code}\n```\n\n"
        prompt += f"THE TEST FAILED WITH THIS ERROR TRACE / COMPILER OUTPUT:\n```\n{previous_error}\n```\n\n"
        prompt += "Please fix the implementation so that all tests pass completely."
    else:
        prompt += f"Implement the required module to be saved at `{task['files']['implementation']}` so that all tests pass."

    print(f"\n🤖 [Coder: Qwen 2.5 Coder] Generating implementation for {task['id']} ({lang.upper()})...")
    reply = chat_completion(
        model_name="qwen",
        messages=[{"role": "user", "content": prompt}],
        system_prompt=system_prompt,
        temperature=0.1,
        max_tokens=3000
    )
    code = extract_code(reply, lang=lang)
    
    # Write to target file
    impl_path.parent.mkdir(parents=True, exist_ok=True)
    with open(impl_path, "w", encoding="utf-8") as f:
        f.write(code + "\n")
    print(f"💾 [Coder] Saved code to {task['files']['implementation']} ({len(code.splitlines())} lines).")
    return code

def auditor_review_code(task: Dict[str, Any], auditor_model: str = "deepseek") -> str:
    """Prompt DeepSeek R1 Distill (or Gemma 4) to review implementation and tests."""
    impl_path = REPO_ROOT / task["files"]["implementation"]
    test_path = REPO_ROOT / task["files"]["tests"]
    lang_tag = detect_lang(str(impl_path))

    with open(impl_path, "r", encoding="utf-8") as f:
        impl_code = f.read()
    with open(test_path, "r", encoding="utf-8") as f:
        test_code = f.read()
    system_prompt = (
        "You are a Principal Security Auditor and Financial Systems Architect.\n"
        "Perform a deep formal verification of the implementation and tests for:\n"
        "1. Numerical precision & overflow safety (int64 cents vs float, rounding invariants).\n"
        "2. Concurrency safety (mutexes, race conditions, idempotency, state mutations).\n"
        "3. Mexican SAT / Banxico (SPEI/CoDi/CFDI 4.0) regulatory & cryptographic compliance.\n"
        "4. Unhandled edge cases or invariant violations.\n"
        "Provide a concise, high-signal audit report with 3-5 actionable bullet points."
    )

    prompt = f"TASK: {task['title']}\n\n"
    prompt += f"SPECIFICATION:\n{task.get('spec', '')}\n\n"
    prompt += f"IMPLEMENTATION:\n```{lang_tag}\n{impl_code}\n```\n\n"
    prompt += f"TESTS:\n```{lang_tag}\n{test_code}\n```\n\n"
    prompt += "Audit this implementation rigorously and summarize your findings."

    label = "DeepSeek R1 Distill 14B" if auditor_model == "deepseek" else "Gemma 4"
    print(f"\n🧠 [Auditor: {label}] Reasoning & reviewing code for {task['id']}...")
    review = chat_completion(
        model_name=auditor_model,
        messages=[{"role": "user", "content": prompt}],
        system_prompt=system_prompt
    )
    return review

def git_commit_task(task: Dict[str, Any], auditor_model: str = "deepseek") -> None:
    """Stage files and create a clean git commit."""
    files_to_add = [
        task["files"]["tests"],
        task["files"]["implementation"],
        "tools/loop/tasks.json"
    ]
    if "extra_files" in task.get("files", {}):
        files_to_add.extend(task["files"]["extra_files"])
    
    # If Go package, ensure all package files are added
    impl_parent = Path(task["files"]["implementation"]).parent
    if impl_parent.name != "internal" and impl_parent.name != "pkg":
        files_to_add.append(str(impl_parent))

    for rel_path in files_to_add:
        full = REPO_ROOT / rel_path
        if full.exists():
            subprocess.run(["git", "add", str(full)], cwd=str(REPO_ROOT), check=False)

    auditor_label = "DeepSeek R1 Distill 14B" if auditor_model == "deepseek" else "Gemma 4"
    msg = f"feat({task['component']}): {task['title']} [{task['id']}]\n\n- Implemented by Qwen 2.5 Coder\n- Audited by {auditor_label}\n- Verified with unit tests"
    commit_proc = subprocess.run(["git", "commit", "-m", msg], cwd=str(REPO_ROOT), capture_output=True, text=True, check=False)
    if commit_proc.returncode == 0:
        print(f"📦 [Git] Committed changes: {msg.splitlines()[0]}")
    else:
        print(f"⚠️ [Git] Commit output: {commit_proc.stdout or commit_proc.stderr}")

def execute_task(task: Dict[str, Any], max_retries: int = 3, auditor_model: str = "deepseek") -> bool:
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

    # Auditing with DeepSeek R1 Distill (or selected auditor)
    task["status"] = "review"
    audit = auditor_review_code(task, auditor_model=auditor_model)
    task["audit_notes"] = audit
    print("\n📋 [Auditor Report]:\n" + audit + "\n")

    # Complete & Commit
    task["status"] = "completed"
    task["completed_at"] = datetime.datetime.now().isoformat()
    save_tasks(load_tasks_and_update(task))
    git_commit_task(task, auditor_model=auditor_model)

    print(f"🎉 Task {task['id']} COMPLETED SUCCESSFULLY!\n")
    return True

def write_handoff_summary(batch_tasks: Optional[list] = None) -> None:
    """Write a compact HANDOFF.md for fresh subagent/chat sessions with minimal context consumption."""
    data = load_tasks()
    completed = [t for t in data["tasks"] if t["status"] == "completed"]
    pending = [t for t in data["tasks"] if t["status"] in ("pending", "in_progress", "failed")]
    recent_completed = batch_tasks if batch_tasks else completed[-4:]

    git_log = subprocess.run(
        ["git", "log", "-n", "4", "--oneline"],
        cwd=str(REPO_ROOT),
        capture_output=True,
        text=True,
        check=False
    ).stdout.strip()

    lines = [
        "# Engineering Loop Handoff Checkpoint",
        f"**Updated at:** {datetime.datetime.now().isoformat()}",
        f"**Progress:** {len(completed)}/{len(data['tasks'])} tasks completed | {len(pending)} pending",
        "",
        "## Recent Git Commits",
        "```text",
        git_log or "No commits yet",
        "```",
        "",
        "## Last Completed Batch Summary"
    ]
    for t in recent_completed:
        lines.append(f"- **[{t['id']}] {t['title']}** (`{t['files']['implementation']}`) — Status: `{t['status'].upper()}`")
        if t.get("audit_notes"):
            first_lines = " ".join(line.strip() for line in t["audit_notes"].splitlines() if line.strip())[:240]
            lines.append(f"  - *Audit preview:* {first_lines}...")

    lines.extend(["", "## Next Pending Batch (Up to 4 Tasks)"])
    if pending:
        for t in pending[:4]:
            lines.append(f"- **[{t['id']}] {t['title']}**")
            lines.append(f"  - Test: `{t['files']['tests']}`")
            lines.append(f"  - Impl: `{t['files']['implementation']}`")
    else:
        lines.append("- All registered tasks are completed!")

    lines.extend([
        "",
        "## How to Resume in a New Chat / Subagent",
        "Run the next batch with Two-Phase Swap (1 load of Qwen Coder -> 1 load of DeepSeek R1 Auditor):",
        "```zsh",
        "python3 tools/loop/loop_runner.py --batch 4",
        "```"
    ])
    with open(HANDOFF_FILE, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    print(f"📝 [Handoff] Updated compact checkpoint at {HANDOFF_FILE.relative_to(REPO_ROOT)}")

def execute_batch_two_phase(tasks_batch: list, max_retries: int = 3, auditor_model: str = "deepseek") -> bool:
    """
    Two-Phase Batch Execution to minimize LM Studio RAM swaps and context window usage:
    - Phase 1 (Coder): Loads Qwen 2.5 Coder ONCE and turns all N tasks in the batch GREEN.
    - Phase 2 (Auditor): Loads DeepSeek R1 Distill ONCE, audits all N green tasks, and commits each.
    """
    print("=" * 70)
    print(f"⚡ TWO-PHASE BATCH EXECUTION ({len(tasks_batch)} TASKS)")
    print("=" * 70)

    green_tasks = []

    # PHASE 1: CODER (QWEN 2.5 CODER)
    print("\n" + "-" * 70)
    print(f"🛠️  PHASE 1/2: CODING & TDD GREENING ({len(tasks_batch)} tasks with Qwen 2.5 Coder)")
    print("-" * 70)

    # Temporarily hold pending Kotlin test files so unimplemented later tasks in the batch
    # do not break Gradle :app:compileTestKotlin for earlier tasks in the batch.
    held_tests = {}
    failed_held_tests = []
    for t in tasks_batch:
        t_test = REPO_ROOT / t["files"]["tests"]
        t_impl = REPO_ROOT / t["files"]["implementation"]
        if detect_lang(str(t_test)) == "kotlin" and t_test.exists():
            if t_impl.exists():
                t_impl.unlink()
            hold_path = t_test.with_suffix(".kt.hold")
            t_test.rename(hold_path)
            held_tests[t["id"]] = (t_test, hold_path)

    for task in tasks_batch:
        print(f"\n🚀 [Phase 1] Task {task['id']}: {task['title']}")
        if task["id"] in held_tests:
            orig_test, hold_path = held_tests.pop(task["id"])
            if hold_path.exists():
                hold_path.rename(orig_test)

        test_path = REPO_ROOT / task["files"]["tests"]
        if not test_path.exists():
            print(f"❌ Test file missing: {test_path}. Skipping task.")
            continue

        task["status"] = "in_progress"
        save_tasks(load_tasks_and_update(task))

        initial_test = run_tests_for_task(task["files"]["tests"])
        if initial_test.returncode == 0:
            print(f"✅ [Runner] {task['id']} already satisfies tests!")
            task["test_results"] = "PASS: " + (initial_test.stderr or initial_test.stdout).strip()[:500]
            task["status"] = "review"
            save_tasks(load_tasks_and_update(task))
            green_tasks.append(task)
            continue

        err_trace = None
        passed = False
        for attempt in range(1, max_retries + 1):
            print(f"--- [{task['id']} Iteration {attempt}/{max_retries}] ---")
            coder_generate_implementation(task, previous_error=err_trace)
            test_proc = run_tests_for_task(task["files"]["tests"])
            if test_proc.returncode == 0:
                print(f"✅ [Runner] {task['id']} unit tests PASSED!")
                task["test_results"] = "PASS: " + (test_proc.stderr or test_proc.stdout).strip()[:500]
                task["status"] = "review"
                save_tasks(load_tasks_and_update(task))
                passed = True
                green_tasks.append(task)
                break
            else:
                print(f"❌ [Runner] {task['id']} tests failed!")
                err_trace = (test_proc.stdout + "\n" + test_proc.stderr).strip()
                print(f"Error summary: {err_trace[:600]}")
                task["retries"] = attempt

        if not passed:
            task["status"] = "failed"
            save_tasks(load_tasks_and_update(task))
            print(f"💥 Task {task['id']} failed after {max_retries} attempts.")
            if detect_lang(str(test_path)) == "kotlin":
                t_impl = REPO_ROOT / task["files"]["implementation"]
                if t_impl.exists():
                    t_impl.unlink()
                if test_path.exists():
                    hold_path = test_path.with_suffix(".kt.hold")
                    test_path.rename(hold_path)
                    failed_held_tests.append((test_path, hold_path))

    for orig_test, hold_path in list(held_tests.values()) + failed_held_tests:
        if hold_path.exists():
            hold_path.rename(orig_test)

    if not green_tasks:
        write_handoff_summary(tasks_batch)
        return False

    # PHASE 2: AUDITOR (DEEPSEEK R1 DISTILL)
    print("\n" + "-" * 70)
    print(f"🧠 PHASE 2/2: DEEP AUDIT & GIT COMMIT ({len(green_tasks)} tasks with {auditor_model})")
    print("-" * 70)
    for task in green_tasks:
        audit = auditor_review_code(task, auditor_model=auditor_model)
        task["audit_notes"] = audit
        print(f"\n📋 [Auditor Report - {task['id']}]:\n{audit}\n")
        task["status"] = "completed"
        task["completed_at"] = datetime.datetime.now().isoformat()
        save_tasks(load_tasks_and_update(task))
        git_commit_task(task, auditor_model=auditor_model)

    write_handoff_summary(green_tasks)
    return len(green_tasks) == len(tasks_batch)

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
    parser.add_argument("--batch", type=int, help="Execute up to N pending tasks in Two-Phase mode and write HANDOFF.md")
    parser.add_argument("--handoff", action="store_true", help="Generate/refresh tools/loop/HANDOFF.md")
    parser.add_argument("--all", action="store_true", help="Execute all pending tasks sequentially")
    parser.add_argument("--task", type=str, help="Execute a specific task ID")
    parser.add_argument("--auditor", choices=["deepseek", "gemma"], default="deepseek", help="Auditor model to use (default: deepseek)")
    parser.add_argument("--swap", choices=["qwen", "deepseek", "gemma"], help="Manually hot-swap model in LM Studio")
    args = parser.parse_args()

    if args.swap:
        ensure_model(args.swap)
        return

    if args.handoff:
        write_handoff_summary()
        return

    if args.status or (not args.step and not args.batch and not args.all and not args.task):
        print_status()
        return

    data = load_tasks()
    if args.batch:
        pending = [t for t in data["tasks"] if t["status"] in ("pending", "in_progress", "failed")]
        if not pending:
            print("🎉 No pending tasks to run!")
            write_handoff_summary()
            return
        batch_slice = pending[:args.batch]
        execute_batch_two_phase(batch_slice, auditor_model=args.auditor)
        return

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
        ok = execute_task(t, auditor_model=args.auditor)
        if not ok and not args.all:
            break
    write_handoff_summary(tasks_to_run)

if __name__ == "__main__":
    main()

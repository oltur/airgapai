#!/usr/bin/env python3
"""Replace the system prompt in a copy of the gateway's chat.go with a candidate's, in place.
Usage: swap-prompt.py CHAT.GO NAME.prompt
Fails unless the prompt is found exactly once, and the candidate's has no backtick."""
import pathlib
import re
import sys

chat, prompt = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]).read_text().strip()
if "`" in prompt:
    sys.exit("a candidate prompt may not contain a backtick")
source = chat.read_text()
swapped, n = re.subn(r"const systemPrompt = `.*?`", lambda _: f"const systemPrompt = `{prompt}`",
                     source, flags=re.S)
if n != 1:
    sys.exit(f"found the system prompt {n} times in {chat}, not once")
chat.write_text(swapped)

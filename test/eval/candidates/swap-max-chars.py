#!/usr/bin/env python3
"""Replace the chunk size in a copy of the ingest program's chunk.go with a candidate's, in place.
Usage: swap-max-chars.py CHUNK.GO NAME.max-chars
Fails unless the size is found exactly once, and the candidate's is a whole number from 50 to
5000."""
import pathlib
import re
import sys

chunk, size = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]).read_text().strip()
if not re.fullmatch(r"[0-9]+", size) or not 50 <= int(size) <= 5000:
    sys.exit(f"a candidate chunk size must be a whole number from 50 to 5000, not {size!r}")
source = chunk.read_text()
swapped, n = re.subn(r"(?m)^(\tmaxChars = )[0-9]+$", lambda m: m.group(1) + size, source)
if n != 1:
    sys.exit(f"found the chunk size {n} times in {chunk}, not once")
chunk.write_text(swapped)

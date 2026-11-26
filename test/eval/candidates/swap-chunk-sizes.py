#!/usr/bin/env python3
"""Replace the chunk sizes in a copy of the ingest program's chunk.go with a candidate's, in place.
Usage: swap-chunk-sizes.py CHUNK.GO NAME.chunk-sizes
The file holds two whole numbers: maxChars, the most characters of a page in a chunk, and
minChars, the size at which a heading starts a new chunk, which must be smaller, or the heading rule
could never apply. Fails unless each constant is found exactly once."""
import pathlib
import re
import sys

chunk, sizes = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]).read_text().split()
if len(sizes) != 2 or not all(re.fullmatch(r"[0-9]+", s) for s in sizes):
    sys.exit(f"a candidate's chunk sizes must be two whole numbers, maxChars minChars, not {sizes}")
most, least = (int(s) for s in sizes)
if not 0 < least < most <= 5000:
    sys.exit(f"the chunk sizes must have 0 < minChars < maxChars <= 5000, not {most} {least}")
source = chunk.read_text()
for name, value in (("maxChars", most), ("minChars", least)):
    source, n = re.subn(rf"(?m)^(\t{name} = )[0-9]+$",
                        lambda m, v=value: m.group(1) + str(v), source)
    if n != 1:
        sys.exit(f"found {name} {n} times in {chunk}, not once")
chunk.write_text(source)

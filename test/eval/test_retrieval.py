"""The retrieval gate's comparison, without the VM."""
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from retrieval import compare  # noqa: E402


def run(**results):
    return {"results": results}


def hit(ok, leaked=0, **extra):
    return {"hit@3": ok, "technician_restricted": leaked, **extra}


class Compare(unittest.TestCase):
    def test_unchanged_passes(self):
        base = run(a=hit(True), b=hit(False))
        self.assertEqual(compare(base, base["results"], 0), [])

    def test_a_lost_hit_fails(self):
        failures = compare(run(a=hit(True), b=hit(True)), {"a": hit(True), "b": hit(False)}, 0)
        self.assertEqual(len(failures), 1)
        self.assertIn("b", failures[0])

    def test_tolerance_allows_that_many(self):
        base = run(a=hit(True), b=hit(True))
        now = {"a": hit(False), "b": hit(False)}
        self.assertEqual(compare(base, now, 2), [])
        self.assertEqual(len(compare(base, now, 1)), 1)

    def test_a_new_hit_or_an_old_miss_is_not_a_failure(self):
        base = run(a=hit(False), b=hit(False))
        self.assertEqual(compare(base, {"a": hit(True), "b": hit(False)}, 0), [])

    def test_a_leak_fails_even_with_tolerance(self):
        failures = compare(run(a=hit(True)), {"a": hit(True, leaked=1)}, 99)
        self.assertTrue(any("technician" in f for f in failures))

    def test_a_blind_leak_control_fails(self):
        base = run(l=hit(False, engineer_restricted=4))
        self.assertEqual(compare(base, {"l": hit(False, engineer_restricted=4)}, 0), [])
        failures = compare(base, {"l": hit(False, engineer_restricted=0)}, 0)
        self.assertTrue(any("shows nothing" in f for f in failures))

    def test_a_changed_golden_set_fails(self):
        self.assertEqual(len(compare(run(a=hit(True)), {"b": hit(True)}, 0)), 1)

    def test_a_changed_question_fails_without_crashing(self):
        base = run(a=hit(True, asked={"question": "q", "expect": ["x.md"]}))
        # The same id, now with no expected pages, so no hit is recorded for it.
        now = {"a": {"technician_restricted": 0, "asked": {"question": "q", "expect": []}}}
        failures = compare(base, now, 0)
        self.assertEqual(len(failures), 1)
        self.assertIn("changed", failures[0])

    def test_another_deployment_fails(self):
        base = {"deployed": {"images": {"embed": ["a"]}}, "results": {"a": hit(True)}}
        self.assertEqual(compare(base, {"a": hit(True)}, 0, base["deployed"]), [])
        failures = compare(base, {"a": hit(True)}, 0, {"images": {"embed": ["b"]}})
        self.assertTrue(any("deployed" in f for f in failures))


if __name__ == "__main__":
    unittest.main()

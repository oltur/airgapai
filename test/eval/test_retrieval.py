"""The retrieval gate's comparison, without the VM."""
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import retrieval  # noqa: E402

CANDIDATE_INGEST, compare, gate = retrieval.CANDIDATE_INGEST, retrieval.compare, retrieval.gate
candidate_deployed, candidate_expected = retrieval.candidate_deployed, retrieval.candidate_expected


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

    def test_another_deployment_still_reports_a_leak(self):
        base = {"deployed": {"images": {"embed": ["a"]}}, "results": {"a": hit(True)}}
        failures = compare(base, {"a": hit(True, leaked=2)}, 0, {"images": {"embed": ["b"]}})
        self.assertEqual(len(failures), 2)
        self.assertTrue(any("technician" in f for f in failures))


class Gate(unittest.TestCase):
    def test_another_deployment_is_kept_apart_from_worse_results(self):
        base = run(a=hit(True))
        base["deployed"] = {"images": {"embed": ["a"]}}
        other, worse = gate(base, {"a": hit(False, leaked=1)}, 0, {"images": {"embed": ["b"]}})
        self.assertEqual(len(other), 1)
        self.assertEqual(len(worse), 2)

    def test_worse_results_alone_are_not_other_failures(self):
        other, worse = gate(run(a=hit(True)), {"a": hit(False)}, 0)
        self.assertEqual(other, [])
        self.assertEqual(len(worse), 1)


if __name__ == "__main__":
    unittest.main()


BASE_DEPLOYED = {"images": {"retrieval": ["r"], "ingest": ["i"], "embed": ["e"]},
                 "rows": ["public=314", "restricted=193"]}


def measured(ingest="i", retrieval="r", rows="public=314 restricted=193"):
    return candidate_deployed({"images": {"embed": ["e"]}},
                              {"ingest": ingest, "retrieval": retrieval, "rows": rows})


class Candidate(unittest.TestCase):
    def check(self, candidate, now):
        expected, wrong = candidate_expected(BASE_DEPLOYED, candidate, now)
        base = {"deployed": expected, "results": {"a": hit(True)}}
        return wrong, compare(base, {"a": hit(True)}, 0, now)

    def test_the_control_matching_the_baseline_passes(self):
        self.assertEqual(self.check("deployed", measured()), (None, []))

    def test_the_control_with_other_chunks_fails_as_another_deployment(self):
        wrong, failures = self.check("deployed", measured(rows="public=1 restricted=1"))
        self.assertIsNone(wrong)
        self.assertTrue(any("deployed" in f for f in failures))

    def test_a_chunking_candidate_may_change_the_ingest_image_and_rows(self):
        now = measured(ingest=CANDIDATE_INGEST + "f" * 64, rows="public=1914 restricted=1091")
        self.assertEqual(self.check("worse-chunking", now), (None, []))

    def test_a_chunking_candidate_with_another_retrieval_image_fails(self):
        now = measured(ingest=CANDIDATE_INGEST + "f" * 64, retrieval="other")
        self.assertTrue(any("deployed" in f for f in self.check("worse-chunking", now)[1]))

    def test_a_chunking_candidate_running_the_deployed_ingest_image_is_wrong(self):
        wrong, _ = self.check("worse-chunking", measured(ingest="i"))
        self.assertIn("not an image from", wrong)

    def test_the_baseline_is_not_changed_in_place(self):
        now = measured(ingest=CANDIDATE_INGEST + "f" * 64)
        candidate_expected(BASE_DEPLOYED, "worse-chunking", now)
        self.assertEqual(BASE_DEPLOYED["images"]["ingest"], ["i"])


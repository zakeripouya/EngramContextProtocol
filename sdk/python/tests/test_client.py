"""Integration tests against a live Engram server, run through sdk/test.sh."""

import os
import subprocess
import threading
import unittest

from engram_ecp import Client, Codes, ECPError

URL = os.environ.get("ECP_URL", "")
TOKEN = os.environ.get("ECP_TOKEN", "")
BIN = os.environ.get("ENGRAM_BIN", "")


@unittest.skipUnless(URL and TOKEN and BIN, "run through sdk/test.sh")
class ClientTest(unittest.TestCase):
    def test_observe_recall_get_and_errors(self):
        with Client.http(URL, TOKEN, name="sdk-test") as c:
            self.assertEqual(c.init["serverInfo"]["name"], "engram")
            mem = c.observe("fact", "Python SDK: invoices are sent on the first business day")
            hits = c.recall("when are invoices sent")
            self.assertIn(mem["id"], [h["memory"]["id"] for h in hits])
            self.assertEqual(c.get(mem["id"])[0]["provenance"]["client"], "sdk-test")
            with self.assertRaises(ECPError) as e:
                c.observe("fact", "x", namespace="team/secret")
            self.assertEqual(e.exception.code, Codes.SCOPE_DENIED)
            with self.assertRaises(ECPError) as e:
                c.observe("decision", "x", author="agent", decision={"situation": "s", "choice": "c"})
            self.assertEqual(e.exception.code, Codes.INVALID_PROVENANCE)

    def test_sessions_attend_feedback(self):
        with Client.http(URL, TOKEN, name="sdk-test") as c:
            cp = c.checkpoint(goal="Python SDK task", next="publish to PyPI")
            self.assertEqual(c.resume(cp["sessionId"])["checkpoint"]["next"], "publish to PyPI")
            self.assertIn(cp["sessionId"], [s["sessionId"] for s in c.sessions("open")])
            a = c.attend("invoices", mode="detail")
            self.assertIsInstance(a["conflicts"], list)
            mem = c.observe("fact", "Python SDK feedback target")
            self.assertEqual(c.feedback([mem["id"]], "accepted", objective="succeeded")["direction"], "strengthened")

    def test_relevant_notification(self):
        with Client.http(URL, TOKEN, name="sdk-test", notifications={"relevant": True}) as c:
            got = threading.Event()
            seen = {}

            def handler(n):
                if n["method"] == "notifications/engram/relevant":
                    seen["n"] = n
                    got.set()

            c.on_notification(handler)
            c.listen()
            c.subscribe("nightly backup job", threshold=0.2)
            subprocess.run([BIN, "job", "report", "backup", "--status", "failed", "--summary", "nightly backup disk full"],
                           check=True, env=os.environ, capture_output=True)
            self.assertTrue(got.wait(10), "no notification within 10s")
            self.assertIn("disk full", str(seen["n"]["params"]))

    def test_stdio(self):
        with Client.stdio([BIN, "serve", "--client", "sdk-test"], name="sdk-test", env=dict(os.environ)) as c:
            self.assertTrue(c.observe("fact", "Python SDK over stdio")["id"].startswith("mem_"))


if __name__ == "__main__":
    unittest.main()

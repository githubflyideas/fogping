"""Tests for the DNS prober. Run: python3 -m unittest test_probe.py

They use a fake DNS server on localhost, so nothing leaves the machine. Note how a
prober is tested through its public contract: a request in, an answer out.
"""
import importlib.machinery
import importlib.util
import json
import socket
import struct
import threading
import unittest

loader = importlib.machinery.SourceFileLoader("probe", "./probe")  # the file has no .py extension
spec = importlib.util.spec_from_loader("probe", loader)
probe = importlib.util.module_from_spec(spec)
loader.exec_module(probe)


class FakeServer:
    """Answers every query: `mode` is "ok", "nodata", "nxdomain" or "wrongid"."""

    def __init__(self, mode):
        self.mode = mode
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock.bind(("127.0.0.1", 0))
        self.port = self.sock.getsockname()[1]
        threading.Thread(target=self.serve, daemon=True).start()

    def serve(self):
        while True:
            try:
                data, peer = self.sock.recvfrom(512)
            except OSError:
                return
            qid = struct.unpack("!H", data[:2])[0]
            if self.mode == "wrongid":
                qid ^= 0xFFFF
            rcode = 3 if self.mode == "nxdomain" else 0
            an = 1 if self.mode == "ok" else 0
            reply = struct.pack("!6H", qid, 0x8180 | rcode, 1, an, 0, 0) + data[12:]
            self.sock.sendto(reply, peer)

    def close(self):
        self.sock.close()


def request(port, **opts):
    return {"protocol": "fogping-probe/1", "type": "dns", "host": "127.0.0.1", "port": port,
            "packets": 3, "gap_ms": 1, "timeout_ms": 300,
            "options": dict({"query": "example.com"}, **opts)}


class Tests(unittest.TestCase):
    def received(self, mode, **opts):
        srv = FakeServer(mode)
        try:
            a = probe.probe(request(srv.port, **opts))
        finally:
            srv.close()
        return a

    def test_healthy_server(self):
        a = self.received("ok")
        self.assertEqual((a["sent"], a["received"], len(a["ms"])), (3, 3, 3))

    def test_nodata_is_loss_only_when_an_answer_is_required(self):
        self.assertEqual(self.received("nodata")["received"], 0)
        self.assertEqual(self.received("nodata", require_answer=False)["received"], 3)

    def test_error_codes_and_wrong_ids_are_loss(self):
        self.assertEqual(self.received("nxdomain")["received"], 0)
        self.assertEqual(self.received("wrongid")["received"], 0)

    def test_nothing_listening_is_loss_not_an_error(self):
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]  # bound but never answered
        a = probe.probe(request(port))
        s.close()
        self.assertEqual((a["received"], a["ms"]), (0, []))

    def test_own_failures_are_errors_with_no_measurement(self):
        for bad in (request(53, query=""), request(0), request(53, record="ZZZ"),
                    request(53, query="a" * 64 + ".com")):
            a = probe.probe(bad)
            self.assertIn("error", a)
            self.assertNotIn("sent", a)

    def test_wire_format(self):
        self.assertEqual(probe.encode_name("www.example.com"), b"\x03www\x07example\x03com\x00")
        q = probe.build_query("a.b", 1, 0x1234)
        self.assertEqual(q[:2], b"\x12\x34")
        self.assertEqual(q[-4:], b"\x00\x01\x00\x01")

    def test_answer_is_valid_json_for_the_protocol(self):
        a = probe.probe(request(1))  # nothing there: all lost
        self.assertEqual(json.loads(json.dumps(a))["ms"], [])


if __name__ == "__main__":
    unittest.main()

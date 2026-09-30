import contextlib
import io
import json
import struct
import unittest
from unittest import mock

import server


class DiagnosticsTest(unittest.TestCase):
    def test_constant_stages_numeric_ages_and_redaction(self):
        with (
            mock.patch.object(server.time, "monotonic", return_value=120),
            mock.patch.object(server.time, "time", return_value=220),
        ):
            diagnostic = server.Diagnostics(dict(observed=110, observed_wall=209))
            for stage in server.Diagnostics.stages:
                diagnostic.mark(stage)
                event = diagnostic.failure(ValueError("API observation too old"))
                self.assertEqual(stage, event["stage"])
                self.assertEqual("ValueError", event["exception"])
                self.assertEqual("API observation too old", event["message"])
                self.assertEqual(10, event["observation_age_seconds"])
                self.assertEqual(11, event["observation_wall_age_seconds"])
                self.assertEqual(0, event["duration_seconds"])
                self.assertEqual(0, event["stage_duration_seconds"])
            secret = "Bearer credential payload 192.0.2.1"
            errors = [
                ValueError(secret),
                OSError(secret),
                KeyError(secret),
                json.JSONDecodeError(secret, secret, 0),
                type(secret, (Exception,), {})(secret),
            ]
            for error in errors:
                with self.subTest(kind=type(error)):
                    event = diagnostic.failure(error)
                    self.assertNotIn(secret, json.dumps(event))
                    self.assertEqual("redacted", event["message"])
            diagnostic.mark(secret)
            self.assertEqual("cycle", diagnostic.stage)

    def test_missing_invalid_and_negative_ages(self):
        for observed in (None, "secret", float("nan"), float("inf"), True):
            diagnostic = server.Diagnostics(dict(observed=observed, observed_wall=observed))
            event = diagnostic.failure(RuntimeError("secret"))
            self.assertNotIn("observation_age_seconds", event)
            self.assertNotIn("observation_wall_age_seconds", event)
        with mock.patch.object(server.time, "monotonic", return_value=10):
            diagnostic = server.Diagnostics(dict(observed=11))
            self.assertEqual(-1, diagnostic.failure(ValueError())["observation_age_seconds"])

    def test_reconcile_failure_stages_preserve_exception_and_clear_cache(self):
        for stage in ("direct-get", "pin", "tick"):
            with self.subTest(stage=stage):
                api, host = mock.Mock(), mock.Mock()
                cache = dict(observed=10, observed_wall=20, proof={})
                diagnostic = server.Diagnostics(cache)
                error = ValueError("secret")
                with mock.patch.object(server, "pinned", return_value="uid") as pin:
                    target = {"direct-get": api.request, "pin": pin, "tick": host.tick}[stage]
                    target.side_effect = error
                    with self.assertRaises(ValueError) as raised:
                        server.reconcile(api, host, {}, "node", cache, diagnostic)
                self.assertIs(error, raised.exception)
                self.assertEqual(stage, diagnostic.stage)
                self.assertEqual({}, cache)
                self.assertEqual(10, diagnostic.observed)

    def test_uds_failure_stages_and_success_without_logging(self):
        for stage in ("peer", "receive", "decode", "request", "status", "send", "success"):
            with self.subTest(stage=stage):
                connection = mock.Mock()
                connection.getsockopt.return_value = struct.pack("3i", 1, 0, 0)
                connection.recv.return_value = b'{"ready":"node"}\n'
                diagnostic = server.Diagnostics()
                error = OSError("secret")
                if stage == "peer":
                    connection.getsockopt.side_effect = error
                elif stage == "receive":
                    connection.recv.side_effect = error
                elif stage == "decode":
                    connection.recv.return_value = b"secret\n"
                elif stage == "request":
                    connection.recv.return_value = b"{}\n"
                elif stage == "send":
                    connection.sendall.side_effect = error
                with (
                    mock.patch.object(server, "status", return_value={}) as status,
                    mock.patch("builtins.print") as output,
                ):
                    if stage == "status":
                        status.side_effect = error
                    if stage == "success":
                        server.respond(connection, None, None, {}, "node", {}, diagnostic)
                        connection.sendall.assert_called_once_with(b"{}\n")
                    else:
                        with self.assertRaises(Exception):
                            server.respond(connection, None, None, {}, "node", {}, diagnostic)
                        self.assertEqual(stage, diagnostic.stage)
                    output.assert_not_called()
                if stage != "peer":
                    connection.settimeout.assert_called_once_with(2)

    def test_startup_reply_expiry_stage(self):
        connection = mock.Mock()
        connection.getsockopt.return_value = struct.pack("3i", 1, 0, 0)
        connection.recv.return_value = json.dumps(dict(node="node", nonce="a" * 64)).encode() + b"\n"
        diagnostic = server.Diagnostics()
        with (
            mock.patch.object(server, "reconcile", return_value=dict(valid_until=100)),
            mock.patch.object(server.time, "time", return_value=100),
        ):
            with self.assertRaisesRegex(ValueError, "proof expired before reply"):
                server.respond(connection, None, None, {}, "node", {}, diagnostic)
        self.assertEqual("reply-check", diagnostic.stage)
        connection.sendall.assert_not_called()

    def test_main_emits_after_fail_closed_cleanup_with_original_alarms(self):
        # Entire server environment is mocked: no socket, filesystem, API or kernel writes.
        for stage in ("poll", "bootstrap-get", "bootstrap-install", "direct-get", "accept", "status"):
            with self.subTest(stage=stage), contextlib.ExitStack() as stack:

                def patch(target, **kwargs):
                    return stack.enter_context(mock.patch(target, **kwargs))

                patch("sys.argv", new=["server.py", "--policy", "unused"])
                patch("server.os.geteuid", return_value=0)
                patch("server.os.environ", new=dict(NODE_NAME="node", NODE_IP="unused"))
                patch("server.Path.read_text", return_value='{"monitors":[]}')
                host = patch("server.local.Host").return_value
                host.directory.lstat.return_value = mock.Mock(st_mode=0o40700, st_uid=0)
                host.directory.__truediv__.return_value.exists.return_value = False
                host.lock.return_value = contextlib.nullcontext()
                api = patch("server.w.API").return_value
                patch("server.os.chmod")
                patch("server.os.umask")
                patch("server.os.open", return_value=99)
                patch("server.os.close")
                patch("fcntl.flock")
                patch("server.os.stat", return_value=mock.Mock(st_ino=0xF0000000))
                patch("server.signal.signal")
                alarm = patch("server.signal.alarm")
                patch("server.time.sleep", side_effect=SystemExit)
                schedule = patch("server.next_poll_at", return_value=0)
                install = patch("bootstrap.install")
                patch("bootstrap.listeners", return_value=[])
                reconcile = patch("server.reconcile")
                listener = patch("server.socket.socket").return_value
                connection = mock.MagicMock()
                listener.accept.return_value = (connection, None)
                connection.__enter__.return_value = connection
                connection.getsockopt.return_value = struct.pack("3i", 1, 0, 0)
                connection.recv.return_value = b'{"ready":"node"}\n'
                status = patch("server.status")
                targets = {
                    "poll": schedule,
                    "bootstrap-get": api.request,
                    "bootstrap-install": install,
                    "accept": listener.accept,
                    "status": status,
                }
                if stage == "direct-get":

                    def failed_reconcile(*args):
                        args[-1].mark("direct-get")
                        raise ValueError("secret")

                    reconcile.side_effect = failed_reconcile
                else:
                    targets[stage].side_effect = ValueError("secret")
                host.close_admission.side_effect = OSError("cleanup secret")
                output = io.StringIO()
                with contextlib.redirect_stdout(output), self.assertRaises(SystemExit):
                    server.main()
                host.close_admission.assert_called_once()
                self.assertEqual([mock.call(25), mock.call(10), mock.call(0)], alarm.call_args_list)
                event = json.loads(output.getvalue())
                self.assertEqual(stage, event["stage"])
                self.assertEqual("ValueError", event["exception"])
                self.assertEqual("cleanup", event["cleanup"]["stage"])
                self.assertEqual("OSError", event["cleanup"]["exception"])
                self.assertNotIn("secret", output.getvalue())


if __name__ == "__main__":
    unittest.main()

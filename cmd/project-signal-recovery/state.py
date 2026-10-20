# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import sqlite3
from contextlib import contextmanager


class StateError(ValueError):
    pass


class SQLiteState:
    def __init__(self, path, read_only=False):
        if read_only:
            self.connection = sqlite3.connect(
                f"file:{path}?mode=ro", uri=True, isolation_level=None
            )
        else:
            self.connection = sqlite3.connect(path, isolation_level=None)
            self.connection.execute("PRAGMA journal_mode=WAL")
            self.connection.execute("PRAGMA synchronous=FULL")
            self._initialize()

    def _initialize(self):
        self.connection.executescript(
            """
            CREATE TABLE IF NOT EXISTS ack_consumptions (
                consumption_id TEXT PRIMARY KEY,
                proof_json TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS witness_heads (
                statement_id TEXT PRIMARY KEY,
                tree_head TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS latest_checkpoints (
                stream_id TEXT PRIMARY KEY,
                latest_index INTEGER NOT NULL,
                tree_head TEXT NOT NULL,
                observed_at_epoch INTEGER NOT NULL
            );
            CREATE TABLE IF NOT EXISTS receipt_authorizations (
                authorization_id TEXT PRIMARY KEY,
                receipt_json TEXT NOT NULL
            );
            """
        )

    def close(self):
        if self.connection is not None:
            self.connection.close()
            self.connection = None

    def __del__(self):
        self.close()

    def consume_ack(self, consumption_id, proof):
        encoded = json.dumps(proof, sort_keys=True, separators=(",", ":"))
        try:
            self.connection.execute("BEGIN IMMEDIATE")
            self.connection.execute(
                "INSERT INTO ack_consumptions(consumption_id, proof_json) VALUES (?, ?)",
                (consumption_id, encoded),
            )
            self.connection.execute("COMMIT")
        except sqlite3.IntegrityError as error:
            self.connection.execute("ROLLBACK")
            raise StateError("ACK was already consumed") from error
        except Exception:
            self.connection.execute("ROLLBACK")
            raise

    def ack_proof(self, consumption_id):
        row = self.connection.execute(
            "SELECT proof_json FROM ack_consumptions WHERE consumption_id = ?",
            (consumption_id,),
        ).fetchone()
        return None if row is None else json.loads(row[0])

    def record_witness_head(self, statement_id, tree_head):
        try:
            self.connection.execute("BEGIN IMMEDIATE")
            row = self.connection.execute(
                "SELECT tree_head FROM witness_heads WHERE statement_id = ?",
                (statement_id,),
            ).fetchone()
            if row is not None and row[0] != tree_head:
                raise StateError("witness rejected equivocation")
            if row is None:
                self.connection.execute(
                    "INSERT INTO witness_heads(statement_id, tree_head) VALUES (?, ?)",
                    (statement_id, tree_head),
                )
            self.connection.execute("COMMIT")
        except Exception:
            self.connection.execute("ROLLBACK")
            raise

    def observe_latest(self, stream_id, latest_index, tree_head, observed_at_epoch):
        try:
            self.connection.execute("BEGIN IMMEDIATE")
            row = self.connection.execute(
                """
                SELECT latest_index, tree_head, observed_at_epoch
                FROM latest_checkpoints WHERE stream_id = ?
                """,
                (stream_id,),
            ).fetchone()
            if row is not None:
                current_index, current_head, current_observed = row
                if latest_index < current_index:
                    raise StateError("latest checkpoint rollback")
                if latest_index == current_index and tree_head != current_head:
                    raise StateError("latest checkpoint fork")
                if observed_at_epoch < current_observed:
                    raise StateError("latest observation rollback")
            self.connection.execute(
                """
                INSERT INTO latest_checkpoints(
                    stream_id, latest_index, tree_head, observed_at_epoch
                ) VALUES (?, ?, ?, ?)
                ON CONFLICT(stream_id) DO UPDATE SET
                    latest_index = excluded.latest_index,
                    tree_head = excluded.tree_head,
                    observed_at_epoch = excluded.observed_at_epoch
                """,
                (stream_id, latest_index, tree_head, observed_at_epoch),
            )
            self.connection.execute("COMMIT")
        except Exception:
            self.connection.execute("ROLLBACK")
            raise

    def latest(self, stream_id):
        row = self.connection.execute(
            """
            SELECT latest_index, tree_head, observed_at_epoch
            FROM latest_checkpoints WHERE stream_id = ?
            """,
            (stream_id,),
        ).fetchone()
        if row is None:
            raise StateError("current latest checkpoint is unavailable")
        return {
            "latestIndex": row[0],
            "latestTreeHead": row[1],
            "observedAtEpoch": row[2],
        }

    @contextmanager
    def receipt_transaction(self):
        try:
            self.connection.execute("BEGIN IMMEDIATE")
            yield
            self.connection.execute("COMMIT")
        except Exception:
            self.connection.execute("ROLLBACK")
            raise

    def record_receipt_authorization(self, authorization_id, receipt):
        encoded = json.dumps(
            receipt, sort_keys=True, separators=(",", ":")
        )
        row = self.connection.execute(
            """
            SELECT receipt_json FROM receipt_authorizations
            WHERE authorization_id = ?
            """,
            (authorization_id,),
        ).fetchone()
        if row is not None:
            if row[0] != encoded:
                raise StateError(
                    "receipt authorization already exists with different content"
                )
            return
        self.connection.execute(
            """
            INSERT INTO receipt_authorizations(
                authorization_id, receipt_json
            ) VALUES (?, ?)
            """,
            (authorization_id, encoded),
        )

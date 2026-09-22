// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Streaming, hash-chained exact journals. A missing terminal is an incomplete run.
use super::Choice;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    fs::File,
    io::{BufRead, BufReader, Read, Write},
    path::Path,
};

#[derive(Clone, Copy, Debug, Serialize, Deserialize)]
pub(crate) struct Seeds {
    pub scenario: u64,
    pub workload: u64,
    pub faults: u64,
    pub scheduler: u64,
    pub timing: u64,
    pub entropy: u64,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::simulation::World;
    #[test]
    fn complete_journal_outlives_tail_and_rejects_divergence() {
        let directory = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/dst-contracts");
        std::fs::create_dir_all(&directory).unwrap();
        let path = directory.join(format!("journal-{}.jsonl", std::process::id()));
        let _ = std::fs::remove_file(&path);
        let run = |exact, count, singleton, outcome| {
            let world = World::new(19);
            world.enable_scheduler();
            world.limits(1, 100_000, 2);
            world.journal(Journal::open(
                &path,
                exact,
                json!({"fixture": "tail-overflow"}),
            ));
            for i in 0..count {
                world.choose_enabled("worker", &[i, i + 1]);
            }
            assert_eq!(world.choices().len(), 2);
            world.choose_enabled("terminal-singleton", &[singleton]);
            world.finish_journal(json!(outcome));
        };
        run(false, 65_537, 19, "pass");
        run(true, 65_537, 19, "pass");
        let rejects = |count, singleton, outcome| {
            assert!(
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| run(
                    true, count, singleton, outcome
                )))
                .is_err()
            )
        };
        rejects(65_538, 19, "pass");
        rejects(65_536, 19, "pass");
        rejects(65_537, 20, "pass");
        rejects(65_537, 19, "failure");
        let bytes = std::fs::read(&path).unwrap();
        std::fs::write(&path, &bytes[..bytes.len() - 10]).unwrap();
        rejects(65_537, 19, "pass");
        let mut corrupt = bytes.clone();
        corrupt[20] ^= 1;
        std::fs::write(&path, corrupt).unwrap();
        rejects(65_537, 19, "pass");
        let mut trailing = bytes;
        trailing.extend_from_slice(b"{}\n");
        std::fs::write(&path, trailing).unwrap();
        rejects(65_537, 19, "pass");
        std::fs::remove_file(&path).unwrap();
    }
}
impl Seeds {
    pub fn from_seed(seed: u64) -> Self {
        Self {
            scenario: Self::named(seed, "scenario"),
            workload: Self::named(seed, "workload"),
            faults: Self::named(seed, "faults"),
            scheduler: Self::named(seed, "scheduler"),
            timing: Self::named(seed, "timing"),
            entropy: Self::named(seed, "entropy"),
        }
    }
    pub fn named(seed: u64, name: &str) -> u64 {
        let mut h = blake3::Hasher::new();
        h.update(b"racer/dst/named-seed/v1");
        h.update(&seed.to_le_bytes());
        h.update(name.as_bytes());
        u64::from_le_bytes(h.finalize().as_bytes()[..8].try_into().unwrap())
    }
}

enum Stream {
    Record(File),
    Exact(BufReader<File>),
}
pub(crate) struct Journal {
    stream: Stream,
    chain: blake3::Hasher,
}
impl Journal {
    pub fn open(path: &Path, exact: bool, input: Value) -> Self {
        let stream = if exact {
            Stream::Exact(BufReader::new(
                File::open(path).expect("infrastructure: open journal"),
            ))
        } else {
            Stream::Record(File::create_new(path).expect("infrastructure: create journal"))
        };
        let mut journal = Self {
            stream,
            chain: blake3::Hasher::new(),
        };
        journal.observe("header", json!({"schema": 1, "model": 2,
            "scheduler": "managed-phases-v1", "prng": "splitmix64/named-blake3-v1", "input": input}));
        journal
    }
    fn exchange(&mut self, value: Value) -> Value {
        match &mut self.stream {
            Stream::Record(file) => {
                let payload = serde_json::to_string(&value).unwrap();
                self.chain.update(payload.as_bytes());
                let line = json!({"payload": payload, "checksum": self.chain.finalize().to_hex().to_string()});
                serde_json::to_writer(&mut *file, &line).expect("infrastructure: write journal");
                file.write_all(b"\n")
                    .expect("infrastructure: write journal delimiter");
                value
            }
            Stream::Exact(reader) => {
                let mut line = String::new();
                reader
                    .by_ref()
                    .take(1_048_577)
                    .read_line(&mut line)
                    .expect("infrastructure: read journal");
                assert!(
                    line.ends_with('\n') && line.len() <= 1_048_576,
                    "replay divergence: incomplete or oversized journal record"
                );
                let record: Value =
                    serde_json::from_str(&line).expect("replay divergence: malformed record");
                let payload = record["payload"]
                    .as_str()
                    .expect("replay divergence: missing payload");
                self.chain.update(payload.as_bytes());
                assert_eq!(
                    record["checksum"],
                    self.chain.finalize().to_hex().to_string(),
                    "replay divergence: journal checksum"
                );
                serde_json::from_str(payload).expect("replay divergence: malformed payload")
            }
        }
    }
    pub fn observe(&mut self, kind: &str, value: Value) {
        let expected = json!({"kind": kind, "value": value});
        assert_eq!(
            self.exchange(expected.clone()),
            expected,
            "replay divergence: {kind}"
        );
    }
    pub fn choice(&mut self, choice: Choice) -> Choice {
        let record = self.exchange(json!({"kind": "choice", "value": choice}));
        assert_eq!(
            record["kind"], "choice",
            "replay divergence: expected choice"
        );
        let expected: Choice = serde_json::from_value(record["value"].clone())
            .expect("replay divergence: malformed choice");
        assert_eq!(
            (expected.index, expected.enabled, expected.fingerprint),
            (choice.index, choice.enabled, choice.fingerprint),
            "replay divergence: enabled identities"
        );
        assert!(
            expected.selected < choice.enabled,
            "replay divergence: choice out of range"
        );
        expected
    }
    pub fn finish(mut self, outcome: Value) {
        self.observe("terminal", outcome);
        match &mut self.stream {
            Stream::Record(file) => file.sync_all().expect("infrastructure: sync journal"),
            Stream::Exact(reader) => assert!(
                reader
                    .fill_buf()
                    .expect("infrastructure: journal EOF")
                    .is_empty(),
                "replay divergence: trailing records"
            ),
        }
    }
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn counts(io: &crate::slab_io::Io) -> (u64, u64) {
    let mut text = String::new();
    io.render(&mut text);
    let counter = |name| {
        text.lines()
            .find_map(|line| line.strip_prefix(name))
            .unwrap()
            .parse()
            .unwrap()
    };
    (
        counter("racer_dataplane_slab_io_operations_total "),
        counter("racer_dataplane_slab_io_bytes_total "),
    )
}

#[test]
fn creation_recovery_and_replacement_share_setup_accounting() {
    let io = crate::slab_io::Io::testing(1000, 1000, false);
    let (fixture, allocator) = io.scope(Fixture::new);
    // Two initial root writes, one file sync, then two roots read both for
    // version validation and for checkpoint recovery.
    assert_eq!(counts(&io), (7, 6 * PAGE_SIZE as u64));
    allocator.space._file.sync_data().unwrap();
    assert_eq!(counts(&io), (8, 6 * PAGE_SIZE as u64));

    let replacement = Fixture(fixture.0.with_extension("resize"));
    let plan = LayoutPlan::new(32 * WIDE, 1).unwrap();
    let mut next = allocator
        .space
        ._file
        .io()
        .scope(|| Slab::prepare_replacement(&replacement.0, plan, CheckpointBudget::default()))
        .unwrap();
    let shards = next.prepare_empty_shards().unwrap();
    assert_eq!(counts(&io), (13, 10 * PAGE_SIZE as u64));
    // Both generations keep the shared policy outside the setup scope.
    shards[0].shard.file.sync_data().unwrap();
    allocator.space._file.sync_data().unwrap();
    assert_eq!(counts(&io), (15, 10 * PAGE_SIZE as u64));
    drop((shards, next, allocator));
    let reopened = io.scope(|| Slab::open(&fixture.0, 1)).unwrap();
    assert_eq!(counts(&io), (17, 12 * PAGE_SIZE as u64));
    drop(reopened);
}

#[test]
fn setup_chunks_large_io_and_accounts_short_read_before_eof() {
    let (fixture, allocator) = Fixture::new();
    drop(allocator);
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&fixture.0)
        .unwrap();
    let io = crate::slab_io::Io::testing(1, 4 * BUFFER_SIZE as u64, true);
    let file = SlabFile::Os(file, io.clone());
    let data = vec![0x71; BUFFER_SIZE + 7];
    file.write_all_at(&data, 0).unwrap();
    let mut read = vec![0; data.len()];
    file.read_exact_at(&mut read, 0).unwrap();
    assert_eq!(read, data);
    assert_eq!(counts(&io), (4, 2 * data.len() as u64));
    let mut eof = [0; 20];
    assert_eq!(
        file.read_exact_at(&mut eof, 32 * WIDE - 10)
            .unwrap_err()
            .kind(),
        io::ErrorKind::UnexpectedEof
    );
    assert_eq!(counts(&io), (6, 2 * data.len() as u64 + 10));
    file.sync_data().unwrap();
    assert_eq!(counts(&io), (7, 2 * data.len() as u64 + 10));
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn queued<O: Operation>(ring: &Ring, ticket: &Ticket<O>) -> abi::Sqe {
    ring.request(ticket)
        .unwrap()
        .slab_pending
        .as_ref()
        .map_or_else(
            || {
                ring.core
                    .as_ref()
                    .unwrap()
                    .raw
                    .unpublished(ticket.id)
                    .unwrap()
            },
            |pending| pending.sqe,
        )
}
fn offloaded<O: Operation>(ring: &Ring, ticket: &Ticket<O>) {
    assert_ne!(
        queued(ring, ticket).flags & abi::ASYNC,
        0,
        "file work may block its reactor at submission"
    );
}

#[test]
fn file_dispatch_preserves_async_flags_ownership_and_completions() {
    let Some(mut ring) = crate::conformance::kernel_ring(2, Config::default()) else {
        return;
    };
    let path = std::env::temp_dir().join(format!("racer-async-file-{}", std::process::id()));
    let os = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .open(&path)
        .unwrap();
    std::fs::remove_file(&path).unwrap();
    os.set_len(8192).unwrap();
    let file = File::new(os.into());
    let fixed = ring.register_file(file.clone()).unwrap();
    let mut page = ring
        .write_page(
            fixed.clone().into(),
            Box::new(Page([0x5a; 4096])),
            FileOffset::new(0).unwrap(),
        )
        .unwrap();
    offloaded(&ring, &page);
    assert_eq!(
        queued(&ring, &page).flags & 1,
        1,
        "fixed-file flag must survive"
    );
    drive(&mut ring, |r| {
        r.take_page(&mut page).unwrap().is_some_and(|done| {
            assert_eq!(done.result.unwrap(), 4096);
            true
        })
    });
    let mut read = ring
        .read_bytes(file.clone().into(), vec![0; 4096].into_boxed_slice(), 0)
        .unwrap();
    offloaded(&ring, &read);
    drive(&mut ring, |r| {
        r.take_bytes(&mut read).unwrap().is_some_and(|done| {
            assert_eq!(done.result.unwrap(), 4096);
            assert!(done.resource.iter().all(|&b| b == 0x5a));
            true
        })
    });
    let destination = fill(ring.pool(), 1);
    let mut read = ring
        .read(
            fixed.into(),
            destination,
            BufferRange::new(0..4096).unwrap(),
            FileOffset::new(0).unwrap(),
        )
        .unwrap();
    offloaded(&ring, &read);
    let mut buffer = None;
    drive(&mut ring, |r| {
        r.take_read(&mut read).unwrap().is_some_and(|done| {
            assert_eq!(done.result.unwrap(), 4096);
            buffer = Some(done.resource.publish(4096).unwrap());
            true
        })
    });
    let mut write = ring
        .write(
            file.clone().into(),
            buffer.take().unwrap(),
            BufferRange::new(0..4096).unwrap(),
            FileOffset::new(4096).unwrap(),
        )
        .unwrap();
    offloaded(&ring, &write);
    drive(&mut ring, |r| {
        r.take_write(&mut write).unwrap().is_some_and(|done| {
            assert_eq!(done.result.unwrap(), 4096);
            true
        })
    });
    let mut sync = ring.sync_data(file.clone().into()).unwrap();
    offloaded(&ring, &sync);
    drive(&mut ring, |r| {
        r.take_control(&mut sync).unwrap().is_some_and(|done| {
            done.result.unwrap();
            true
        })
    });
    let owner = Rc::new(());
    let weak = Rc::downgrade(&owner);
    let mut punch = ring
        .punch_hole(
            file.clone().into(),
            FileOffset::new(4096).unwrap(),
            4096,
            owner,
        )
        .unwrap();
    offloaded(&ring, &punch);
    assert!(weak.upgrade().is_some());
    drive(&mut ring, |r| {
        r.take_punch(&mut punch).unwrap().is_some_and(|done| {
            done.unwrap();
            true
        })
    });
    assert!(weak.upgrade().is_none());

    let (pipe_read, pipe_write) = File::pipe().unwrap();
    let mut splice = ring
        .splice(
            file.clone(),
            pipe_write.clone().into(),
            Some(FileOffset::new(0).unwrap()),
            4096,
            Rc::new(()),
        )
        .unwrap();
    offloaded(&ring, &splice);
    drive(&mut ring, |r| {
        r.take_splice(&mut splice).unwrap().is_some_and(|done| {
            assert_eq!(done.unwrap(), 4096);
            true
        })
    });
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let socket = File::new(socket.into());
    let mut drain = ring
        .splice(pipe_read, socket.clone().into(), None, 4096, Rc::new(()))
        .unwrap();
    assert_eq!(queued(&ring, &drain).flags & abi::ASYNC, 0);
    drive(&mut ring, |r| {
        r.take_splice(&mut drain).unwrap().is_some_and(|done| {
            assert_eq!(done.unwrap(), 4096);
            true
        })
    });
    let mut bytes = [0; 4096];
    peer.read_exact(&mut bytes).unwrap();
    assert!(bytes.iter().all(|&b| b == 0x5a));
    let mut recv = ring
        .recv_bytes(socket.into(), vec![0; 1].into_boxed_slice())
        .unwrap();
    assert_eq!(queued(&ring, &recv).flags & abi::ASYNC, 0);
    let mut cancel = ring.cancel(&recv).unwrap();
    drive(&mut ring, |r| r.take_cancel(&mut cancel).unwrap().is_some());
    drive(&mut ring, |r| {
        r.take_bytes(&mut recv).unwrap().is_some_and(|done| {
            assert!(done.result.is_err());
            true
        })
    });

    // Rate-limited SQEs retain the offload bit while parked in userspace, and
    // cancellation must keep the existing ownership/terminal-completion rules.
    let io = crate::slab_io::Io::testing(1, 1, false);
    io.reserve(0, crate::environment::now()).unwrap().finish(0);
    let throttled = file.with_slab_io(io);
    let mut read = ring
        .read_bytes(throttled.into(), vec![0; 4096].into_boxed_slice(), 0)
        .unwrap();
    offloaded(&ring, &read);
    assert!(ring.request(&read).unwrap().slab_pending.is_some());
    let mut cancel = ring.cancel(&read).unwrap();
    let done = ring.take_bytes(&mut read).unwrap().unwrap();
    assert_eq!(
        done.result.unwrap_err().raw_os_error(),
        Some(libc::ECANCELED)
    );
    assert_eq!(done.resource.len(), 4096);
    drive(&mut ring, |r| r.take_cancel(&mut cancel).unwrap().is_some());
    ring.shutdown().unwrap();
    ring.pool().assert_recovered();
}

#[test]
fn abandoned_async_file_reads_preserve_pins_until_terminal_collection() {
    let Some(mut ring) = crate::conformance::kernel_ring(1, Config::default()) else {
        return;
    };
    let path = std::env::temp_dir().join(format!("racer-async-cancel-{}", std::process::id()));
    let os = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .open(&path)
        .unwrap();
    std::fs::remove_file(&path).unwrap();
    os.set_len(4096).unwrap();
    let file = File::new(os.into());
    for submitted in [false, true] {
        let owner = Rc::new(());
        let weak = Rc::downgrade(&owner);
        let ticket = ring
            .read_bytes(file.clone().into(), vec![0; 4096].into_boxed_slice(), 0)
            .unwrap();
        offloaded(&ring, &ticket);
        ring.retain(&ticket, owner);
        let ticket = ticket.cancel_on_drop();
        if submitted {
            ring.progress().unwrap();
        }
        drop(ticket);
        assert!(
            weak.upgrade().is_some(),
            "dropping the ticket cannot free kernel-owned input"
        );
        drive(&mut ring, |_| weak.upgrade().is_none());
    }
    // An async filesystem failure is still collected normally; no false success
    // or leaked ticket/pin when the backing object cannot be read as file data.
    let directory = File::new(std::fs::File::open(std::env::temp_dir()).unwrap().into());
    let mut bad = ring
        .read_bytes(directory.into(), vec![0; 4096].into_boxed_slice(), 0)
        .unwrap();
    offloaded(&ring, &bad);
    drive(&mut ring, |r| {
        r.take_bytes(&mut bad).unwrap().is_some_and(|done| {
            assert_eq!(done.result.unwrap_err().raw_os_error(), Some(libc::EISDIR));
            assert_eq!(done.resource.len(), 4096);
            true
        })
    });
    ring.shutdown().unwrap();
    ring.pool().assert_recovered();
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod uds_slab {
    use super::*;
    use crate::{
        allocator::{Allocator, CheckpointBudget, FileValue, LayoutPlan, Slab},
        cache::CachedValue,
        slab_io::Io,
        workers::ShardId,
    };
    use std::{
        os::unix::{fs::MetadataExt, net::UnixStream},
        path::Path,
    };

    const LENGTH: usize = 256 * 1024 + 7;

    fn counter(io: &Io, name: &str) -> u64 {
        let mut text = String::new();
        io.render(&mut text);
        text.lines()
            .find_map(|line| {
                line.strip_prefix(&format!("racer_dataplane_slab_io_{name} "))?
                    .parse()
                    .ok()
            })
            .unwrap()
    }

    fn payload(ring: &mut Ring, allocator: &mut Allocator, byte: u8) -> FileValue {
        allocator
            .insert_payload([byte; 32], buffer(ring, byte, LENGTH), None)
            .unwrap();
        let lease = allocator.lookup(&[byte; 32], 0).unwrap();
        let value = drive(ring, |ring| {
            let work = allocator.poll(ring, 16)?;
            Ok(lease
                .ready()
                .map_or(Progress::Pending(work), Progress::Ready))
        })
        .unwrap();
        drive(ring, |ring| {
            let work = allocator.poll(ring, 16)?;
            Ok(if allocator.is_idle() {
                Progress::Ready(())
            } else {
                Progress::Pending(work)
            })
        })
        .unwrap();
        value
    }

    fn response(ring: &mut Ring, listener: &mut Listener, path: &Path) -> (BodyWriter, UnixStream) {
        let mut peer = UnixStream::connect(path).unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        peer.write_all(b"GET /retained HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
            .unwrap();
        let connection = drive(ring, |ring| listener.poll_accept(ring, 1)).unwrap();
        let request = request(ring, connection);
        let writer = body_writer(ring, request, LENGTH as u64);
        let mut head = Vec::new();
        while !head.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            peer.read_exact(&mut byte).unwrap();
            head.push(byte[0]);
            assert!(head.len() < 16384);
        }
        let head = String::from_utf8(head).unwrap().to_ascii_lowercase();
        assert!(head.starts_with("http/1.1 200 "));
        assert!(head.contains(&format!("content-length: {LENGTH}\r\n")));
        peer.set_nonblocking(true).unwrap();
        (writer, peer)
    }

    fn queue(ring: &mut Ring, writer: BodyWriter, value: FileValue) -> SendingBody {
        let mut sending = writer
            .send(BodyChunk::value(CachedValue::File(value), 0..LENGTH).unwrap())
            .unwrap();
        assert!(matches!(
            sending.poll(ring, 1).unwrap(),
            Progress::Pending(_)
        ));
        let file = sending.response.as_ref().unwrap().file.as_ref().unwrap();
        // The first splice is only queued. Constrain the actual kernel pipe so
        // every successful slab read must be shorter than the requested body.
        let size = unsafe { libc::fcntl(file.write.as_fd().as_raw_fd(), libc::F_SETPIPE_SZ, 4096) };
        assert_eq!(size, 4096);
        sending
    }

    fn receive(peer: &mut UnixStream, bytes: &mut Vec<u8>) -> bool {
        loop {
            let mut chunk = [0; 8192];
            match peer.read(&mut chunk) {
                Ok(0) => return true,
                Ok(n) => bytes.extend_from_slice(&chunk[..n]),
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => return false,
                Err(error) => panic!("UDS response read: {error}"),
            }
        }
    }

    #[test]
    fn limited_uds_replacement_kernel_integration() {
        cache_responses::kernel_child(
            "http_server::tests::uds_slab::limited_uds_replacement_child",
            "RACER_UDS_SLAB_CHILD",
        );
    }

    #[test]
    #[ignore = "run through bounded limited_uds_replacement_kernel_integration"]
    fn limited_uds_replacement_child() {
        if std::env::var_os("RACER_UDS_SLAB_CHILD").is_none() {
            return;
        }
        let Some(mut old_ring) = crate::conformance::kernel_ring(4, Default::default()) else {
            return;
        };
        let Some(mut new_ring) = crate::conformance::kernel_ring(4, Default::default()) else {
            return;
        };
        let dir = std::env::temp_dir().join(format!("uds-slab-{}", std::process::id()));
        std::fs::create_dir(&dir).unwrap();
        let path = dir.join("cache.slab");
        let candidate = dir.join("cache.slab.resize");
        let socket = dir.join("cache");
        let io = Io::testing(8 * 1024 * 1024, BUFFER_SIZE as u64, true);
        let mut old_slab = io.scope(|| Slab::create(&path, 512 << 20, 1)).unwrap();
        let old_inode = std::fs::metadata(&path).unwrap().ino();
        let retired = old_slab.retirement();
        let mut old_allocator = Allocator::open_inner(
            old_slab.take_shard(ShardId::at(0)).unwrap(),
            Default::default(),
        )
        .unwrap();
        let canceled = payload(&mut old_ring, &mut old_allocator, 71);
        let surviving = payload(&mut old_ring, &mut old_allocator, 72);
        let canceled_pin = canceled.weak_pin();
        let surviving_pin = surviving.weak_pin();

        // Use the production replacement constructor, including its durable
        // empty layout, and give the replacement the same process-wide limiter.
        let mut new_slab = io
            .scope(|| {
                Slab::prepare_replacement(
                    &candidate,
                    LayoutPlan::new(768 << 20, 1).unwrap(),
                    CheckpointBudget::default(),
                )
            })
            .unwrap();
        let new_inode = std::fs::metadata(&candidate).unwrap().ino();
        assert_ne!(old_inode, new_inode);
        let mut new_allocator = Allocator::open_inner(
            new_slab.take_shard(ShardId::at(0)).unwrap(),
            Default::default(),
        )
        .unwrap();
        let replacement = payload(&mut new_ring, &mut new_allocator, 73);
        let replacement_pin = replacement.weak_pin();
        let unix = crate::socket::UnixPath::new(socket.to_str().unwrap()).unwrap();
        let mut first = Listener::bind_unix(unix).unwrap();
        let mut second = Listener::bind_unix(unix).unwrap();
        let (cancel_writer, mut cancel_peer) = response(&mut old_ring, &mut first, &socket);
        let (old_writer, mut old_peer) = response(&mut old_ring, &mut first, &socket);
        let (new_writer, mut new_peer) = response(&mut new_ring, &mut second, &socket);

        // Freeze the exhausted shared bucket through cancellation so scheduling
        // delays cannot refill enough tokens to submit the canceled operation.
        let paused_refill = io.exhaust_and_pause_refill();
        let before_bytes = counter(&io, "bytes_total");
        let before_ops = counter(&io, "operations_total");
        let before_waits = counter(&io, "waits_total");
        let mut cancel_send = queue(&mut old_ring, cancel_writer, canceled);
        let mut old_send = queue(&mut old_ring, old_writer, surviving);
        let mut new_send = queue(&mut new_ring, new_writer, replacement);
        // Deliberately exceed the old ~31 ms admission race window. This pause
        // must not allow either inode to acquire an independent refill/burst.
        thread::sleep(Duration::from_millis(100));
        old_ring.progress().unwrap();
        new_ring.progress().unwrap();
        assert_eq!(
            counter(&io, "operations_total"),
            before_ops,
            "neither inode has a separate burst"
        );
        assert!(old_ring.slab_deadline().is_some() && new_ring.slab_deadline().is_some());

        // Publish a different inode while old HTTP owners are live. Runtime
        // fencing is covered separately; this verifies the transport/file lease
        // boundary even after allocator and pathname authority have moved on.
        std::fs::rename(&candidate, &path).unwrap();
        std::fs::File::open(&dir).unwrap().sync_all().unwrap();
        assert_eq!(std::fs::metadata(&path).unwrap().ino(), new_inode);
        drop((old_allocator, old_slab, new_allocator, new_slab));
        assert!(retired.upgrade().is_some());

        let mut target = cancel_send
            .response
            .as_mut()
            .unwrap()
            .file
            .as_mut()
            .unwrap()
            .ticket
            .take()
            .unwrap();
        let owner = Rc::downgrade(cancel_send.file_owner.as_ref().unwrap());
        let mut cancel = old_ring.cancel(&target).unwrap();
        cancel_send.cancel();
        drive(&mut old_ring, |ring| {
            Ok(ring
                .take_cancel(&mut cancel)?
                .map_or(pending(true, None), Progress::Ready))
        })
        .unwrap()
        .result
        .unwrap();
        assert!(
            owner.upgrade().is_some() && canceled_pin.upgrade().is_some(),
            "cancellation ack cannot release the target's extent"
        );
        let result = old_ring.take_splice(&mut target).unwrap().unwrap();
        assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::ECANCELED));
        assert!(owner.upgrade().is_none() && canceled_pin.upgrade().is_none());
        let mut canceled_bytes = Vec::new();
        assert!(receive(&mut cancel_peer, &mut canceled_bytes));
        assert!(
            canceled_bytes.is_empty(),
            "token-blocked cancellation emits no payload"
        );
        assert_eq!(counter(&io, "operations_total"), before_ops);
        assert_eq!(counter(&io, "bytes_total"), before_bytes);
        drop(paused_refill); // Subsequent transfers use the real refill clock.

        let end = Instant::now() + Duration::from_secs(5);
        let (mut old_done, mut new_done) = (false, false);
        let (mut old_bytes, mut new_bytes) = (Vec::new(), Vec::new());
        loop {
            old_ring.progress().unwrap();
            new_ring.progress().unwrap();
            for (ring, sending, done) in [
                (&mut old_ring, &mut old_send, &mut old_done),
                (&mut new_ring, &mut new_send, &mut new_done),
            ] {
                if !*done && let Progress::Ready(progress) = sending.poll(ring, 8).unwrap() {
                    let BodyProgress::Done(completed) = progress else {
                        panic!("early body boundary");
                    };
                    drop(completed);
                    *done = true;
                }
            }
            let old_eof = receive(&mut old_peer, &mut old_bytes);
            let new_eof = receive(&mut new_peer, &mut new_bytes);
            if old_done && new_done && old_eof && new_eof {
                break;
            }
            assert!(
                Instant::now() < end,
                "replacement or old-inode response stopped making progress"
            );
            thread::sleep(Duration::from_micros(100));
        }
        assert_eq!(
            old_bytes,
            vec![72; LENGTH],
            "rename must not redirect old reads to replacement content"
        );
        assert_eq!(new_bytes, vec![73; LENGTH]);
        assert_eq!(
            counter(&io, "bytes_total") - before_bytes,
            (2 * LENGTH) as u64,
            "charge only completed slab reads, not pipe/socket drains or canceled reads"
        );
        assert!(
            counter(&io, "operations_total") - before_ops >= 2 * LENGTH.div_ceil(4096) as u64,
            "real pipe capacity must force short splice completions"
        );
        assert!(counter(&io, "waits_total") > before_waits);
        drop((old_send, new_send, first));
        assert!(
            socket.exists(),
            "second ring retains the shared UDS endpoint"
        );
        drop(second);
        assert!(!socket.exists());
        old_ring.shutdown().unwrap();
        new_ring.shutdown().unwrap();
        old_ring.pool().assert_recovered();
        new_ring.pool().assert_recovered();
        assert!(surviving_pin.upgrade().is_none() && replacement_pin.upgrade().is_none());
        assert!(
            retired.upgrade().is_none(),
            "old slab must retire after its final HTTP/kernel owner"
        );
        eprintln!(
            "UDS replacement: exact old/new bytes={}, slab bytes={}, operations={}, canceled extent released after target collection",
            2 * LENGTH,
            counter(&io, "bytes_total") - before_bytes,
            counter(&io, "operations_total") - before_ops
        );
        drop((old_ring, new_ring, old_peer, new_peer, cancel_peer));
        std::fs::remove_dir_all(dir).unwrap();
    }
}

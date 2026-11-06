//! `host-read` into a memfd sealed against shrinking: sealed pages are
//! decrypted on a crypto worker straight into a mapping of the memfd.
use super::*;
use crate::telemetry::Event;

/// A memfd of `size` bytes filled with `fill`, sealed with `seals`.
fn sealed_memfd(size: usize, fill: u8, seals: libc::c_int) -> Memfd {
    let mut memfd = Memfd::new_with(libc::MFD_CLOEXEC | libc::MFD_ALLOW_SEALING);
    memfd.file.write_all(&vec![fill; size]).unwrap();
    if seals != 0 {
        assert_eq!(
            unsafe { libc::fcntl(memfd.file.as_raw_fd(), libc::F_ADD_SEALS, seals) },
            0
        );
    }
    memfd
}

fn host_fixture(env: &racer_io::Env, body: Vec<u8>) -> Fixture {
    direct_fixture_for(env, body, |responses, worker| {
        responses.with_host_crypto(worker.crypto())
    })
}

fn worker(fixture: &Fixture) -> &crate::test_support::ReadWorker {
    fixture.worker.as_ref().unwrap()
}

fn page_id(number: u64) -> crate::model::PageId {
    crate::model::PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: definition().id,
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        number: crate::model::PageNumber(number),
    }
}

fn body(length: usize) -> Vec<u8> {
    (0..length).map(|i| (i * 7 % 251) as u8).collect()
}

/// A host-read of `range` into `memfd` at `offset`, `length` bytes.
fn host_read(
    fixture: &Fixture,
    memfd: &Memfd,
    range: Option<(u64, u64)>,
    offset: u64,
    length: u64,
) -> (String, Vec<(u8, u64, u64, u32)>) {
    let header = range.map_or(String::new(), |(start, end)| {
        format!("Range: bytes={start}-{end}\r\n")
    });
    let output = exchange(
        fixture,
        &direct_request(
            "host-read",
            &format!(
                "{header}Racer-Destination-Offset: {offset}\r\nRacer-Destination-Length: {length}\r\n"
            ),
        ),
        &[memfd.file.as_raw_fd()],
    );
    let (head, body) = split(&output);
    let frames = frames(body);
    settle(fixture);
    (head, frames)
}

/// Fill the cache with an ordinary host-read, then leave each page's
/// ciphertext in memory, on disk, or both, and no plaintext.
fn arrange(fixture: &Fixture, pages: u64, length: u64, memory: bool, disk: bool) {
    let worker = worker(fixture);
    worker.retain(true);
    if disk {
        worker.open_disk();
    }
    let scratch = Memfd::new();
    let (head, _) = host_read(fixture, &scratch, None, 0, length);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    let copies: Vec<_> = (0..pages)
        .map(|number| {
            worker
                .memory()
                .ciphertext(&page_id(number))
                .unwrap()
                .unwrap()
        })
        .collect();
    worker.memory().remove_cache(&definition().id).unwrap();
    for copy in copies {
        if disk {
            worker.persist(copy.clone());
        }
        if memory {
            worker
                .memory()
                .publish_ciphertext(crate::memory::UnverifiedPage {
                    copy,
                    disk_token: None,
                })
                .unwrap();
        }
    }
}

/// Whole and partial pages from memory ciphertext decrypt into the memfd,
/// not through pooled plaintext and positioned writes.
#[test]
fn sealed_memfd_decrypts_memory_ciphertext_in_place() {
    let env = racer_io::Env::host();
    let length = 2 * PAGE_BYTES as usize + 4099;
    let data = body(length);
    let fixture = host_fixture(&env, data.clone());
    arrange(&fixture, 3, length as u64, true, false);
    let before = worker(&fixture).metric(Event::HostReadDirectDecrypt);
    let mut memfd = sealed_memfd(length + 128, b'.', libc::F_SEAL_SHRINK);
    let (head, frames) = host_read(&fixture, &memfd, None, 64, length as u64);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames.len(), 4, "{frames:?}");
    assert_eq!(frames[3], (2, 3, length as u64, 0));
    let contents = memfd.contents();
    assert!(contents[64..64 + length] == data[..], "destination matches");
    assert!(contents[..64].iter().all(|&b| b == b'.'));
    assert!(contents[64 + length..].iter().all(|&b| b == b'.'));
    assert_eq!(
        worker(&fixture).metric(Event::HostReadDirectDecrypt) - before,
        3
    );
    assert_eq!(
        worker(&fixture).metric(Event::HostReadDirectDecryptBytes),
        length as u64
    );
    assert_eq!(memfd.open_copies(), 1, "dataplane closed its copy");
    for number in 0..3 {
        assert!(
            worker(&fixture)
                .memory()
                .get(&page_id(number))
                .unwrap()
                .is_none(),
            "no plaintext was published"
        );
    }
}

/// A range inside one page, and a range across a page boundary, write only
/// the requested bytes at the destination offset.
#[test]
fn sealed_memfd_writes_partial_pages_only() {
    let env = racer_io::Env::host();
    let length = PAGE_BYTES as usize + 5000;
    let data = body(length);
    let fixture = host_fixture(&env, data.clone());
    arrange(&fixture, 2, length as u64, true, false);
    let mut memfd = sealed_memfd(8192, b'.', libc::F_SEAL_SHRINK);
    let (head, frames) = host_read(&fixture, &memfd, Some((100, 199)), 4000, 100);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, [(1, 0, 100, 100), (2, 1, 100, 0)]);
    let contents = memfd.contents();
    assert_eq!(&contents[4000..4100], &data[100..200]);
    assert!(contents[..4000].iter().all(|&b| b == b'.'));
    assert!(contents[4100..].iter().all(|&b| b == b'.'));

    let start = PAGE_BYTES as usize - 10;
    let mut memfd = sealed_memfd(64, b'.', libc::F_SEAL_SHRINK);
    let (head, frames) = host_read(
        &fixture,
        &memfd,
        Some((start as u64, start as u64 + 29)),
        1,
        30,
    );
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames.len(), 3, "{frames:?}");
    let contents = memfd.contents();
    assert_eq!(&contents[1..31], &data[start..start + 30]);
    assert_eq!(contents[0], b'.');
    assert!(contents[31..].iter().all(|&b| b == b'.'));
    assert_eq!(worker(&fixture).metric(Event::HostReadDirectDecrypt), 3);
}

/// Disk-only ciphertext is read through host memory and decrypted into the
/// memfd as well.
#[test]
fn sealed_memfd_decrypts_disk_pages_in_place() {
    let env = racer_io::Env::host();
    let length = PAGE_BYTES as usize + 3;
    let data = body(length);
    let fixture = host_fixture(&env, data.clone());
    arrange(&fixture, 2, length as u64, false, true);
    let mut memfd = sealed_memfd(length, 0, libc::F_SEAL_SHRINK);
    let (head, frames) = host_read(&fixture, &memfd, None, 0, length as u64);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames.len(), 3, "{frames:?}");
    assert!(memfd.contents() == data);
    assert_eq!(worker(&fixture).metric(Event::HostReadDirectDecrypt), 2);
}

/// A memfd without the shrink seal, with a write seal, too short for the
/// window, or open write-only keeps positioned writes and still gets the
/// bytes (except the write-sealed one, which cannot be written at all).
#[test]
fn unsuitable_destinations_keep_positioned_writes() {
    let env = racer_io::Env::host();
    let data = body(5000);
    let fixture = host_fixture(&env, data.clone());
    arrange(&fixture, 1, 5000, true, false);

    let mut unsealed = sealed_memfd(5000, 0, 0);
    let (head, frames) = host_read(&fixture, &unsealed, None, 0, 5000);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames.len(), 2, "{frames:?}");
    assert!(unsealed.contents() == data);

    // Too short: the positioned write grows the file.
    let mut short = sealed_memfd(100, 0, libc::F_SEAL_SHRINK);
    let (head, frames) = host_read(&fixture, &short, None, 0, 5000);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames.len(), 2, "{frames:?}");
    assert!(short.contents() == data);

    // Write-only: a shared writable mapping needs a readable descriptor.
    let sealed = sealed_memfd(5000, 0, libc::F_SEAL_SHRINK);
    let path = format!("/proc/self/fd/{}", sealed.file.as_raw_fd());
    let write_only = std::fs::OpenOptions::new().write(true).open(&path).unwrap();
    let output = exchange(
        &fixture,
        &direct_request(
            "host-read",
            "Racer-Destination-Offset: 0\r\nRacer-Destination-Length: 5000\r\n",
        ),
        &[write_only.as_raw_fd()],
    );
    let (head, frames) = split(&output);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(super::frames(frames).len(), 2);
    settle(&fixture);
    let mut sealed = sealed;
    assert!(sealed.contents() == data);

    assert_eq!(worker(&fixture).metric(Event::HostReadDirectDecrypt), 0);
}

/// Replace the retained ciphertext of page 0 with tampered bytes.
fn tamper_retained(fixture: &Fixture, in_tag: bool) {
    use crate::memory::CiphertextBytes;
    use crate::memory::CiphertextPage;
    let memory = worker(fixture).memory();
    let copy = memory.unverified(&page_id(0)).unwrap().unwrap().copy;
    let mut bytes = copy.ciphertext.bytes().to_vec();
    let index = if in_tag { bytes.len() - 1 } else { 3 };
    bytes[index] ^= 0x40;
    let tampered = CiphertextPage {
        pinned: true,
        provenance: None,
        inner: Arc::new(CiphertextBytes {
            pins: Default::default(),
            envelope: copy.ciphertext.envelope().clone(),
            storage: crate::memory::fixture_ciphertext_storage(
                &fixture.listeners.admission,
                Some(&definition().id),
                &bytes,
            ),
        }),
    };
    memory.remove_cache(&definition().id).unwrap();
    memory
        .publish_ciphertext(crate::memory::UnverifiedPage {
            copy: crate::memory::CiphertextCopy {
                metadata: copy.metadata,
                ciphertext: tampered,
            },
            disk_token: None,
        })
        .unwrap();
}

/// A tampered body or tag fails the read before its page frame, leaves no
/// plaintext in the destination, and drops the rejected copy.
#[test]
fn tampered_ciphertext_fails_without_a_page_frame_or_plaintext() {
    let env = racer_io::Env::host();
    for (in_tag, whole) in [(false, true), (true, true), (false, false), (true, false)] {
        let fixture = host_fixture(&env, body(5000));
        arrange(&fixture, 1, 5000, true, false);
        tamper_retained(&fixture, in_tag);
        let mut memfd = sealed_memfd(5000, b'.', libc::F_SEAL_SHRINK);
        let (range, length) = if whole {
            (None, 5000)
        } else {
            (Some((10, 19)), 10)
        };
        let (head, frames) = host_read(&fixture, &memfd, range, 0, length);
        assert!(head.starts_with("http/1.1 200"), "{head}");
        assert!(frames.is_empty(), "{frames:?}");
        let contents = memfd.contents();
        if whole {
            // The in-place decrypt zeroes what it wrote.
            assert!(contents.iter().all(|&b| b == 0), "in_tag {in_tag}");
        } else {
            // A partial page fails in scratch memory and writes nothing.
            assert!(contents.iter().all(|&b| b == b'.'), "in_tag {in_tag}");
        }
        assert!(
            worker(&fixture)
                .memory()
                .unverified(&page_id(0))
                .unwrap()
                .is_none(),
            "the rejected copy left memory"
        );
        assert_eq!(worker(&fixture).metric(Event::HostReadDirectDecrypt), 0);
        assert_eq!(worker(&fixture).metric(Event::CryptoDecryptAeadRejected), 1);
    }
}

/// Without a crypto client, a sealed memfd keeps positioned writes.
#[test]
fn without_crypto_sealed_memfd_keeps_positioned_writes() {
    let env = racer_io::Env::host();
    let data = body(5000);
    let fixture = direct_fixture(&env, data.clone(), None);
    worker(&fixture).retain(true);
    let mut memfd = sealed_memfd(5000, 0, libc::F_SEAL_SHRINK);
    let (head, frames) = host_read(&fixture, &memfd, None, 0, 5000);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames.len(), 2, "{frames:?}");
    assert!(memfd.contents() == data);
    assert_eq!(worker(&fixture).metric(Event::HostReadDirectDecrypt), 0);
}

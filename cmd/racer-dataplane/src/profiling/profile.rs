//! Raw pprof export. Symbolization belongs to the client with matching binaries.
use super::{MAX_OUTPUT, ProfileError as Error, Sample};
use prost::Message;
use std::{
    io::Read,
    path::Path,
    sync::atomic::AtomicBool,
    time::{Duration, Instant, SystemTime},
};

pub(super) fn read_bounded(path: &Path, limit: usize) -> Result<Vec<u8>, Error> {
    let file = std::fs::File::open(path).map_err(Error::os)?;
    let mut bytes = Vec::new();
    bytes
        .try_reserve_exact(limit + 1)
        .map_err(|_| Error::ResourceLimit)?;
    file.take(limit as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(Error::os)?;
    if bytes.len() > limit {
        return Err(Error::ResourceLimit);
    }
    Ok(bytes)
}

#[derive(Eq, PartialEq)]
pub(super) struct Mappings {
    entries: Vec<Map>,
}
#[derive(Eq, PartialEq)]
struct Map {
    start: u64,
    end: u64,
    offset: u64,
    file: String,
}

impl Mappings {
    pub(super) fn ensure_unchanged(&self, after: &Self) -> Result<(), Error> {
        if self == after {
            Ok(())
        } else {
            Err(Error::MappingChanged)
        }
    }

    pub(super) fn read(cancel: &AtomicBool, deadline: Instant) -> Result<Self, Error> {
        super::check(cancel, deadline)?;
        let bytes = read_bounded(Path::new("/proc/self/maps"), 1024 * 1024)?;
        let text = std::str::from_utf8(&bytes).map_err(|_| Error::MalformedRecord)?;
        let executable = std::fs::read_link("/proc/self/exe").map_err(Error::os)?;
        let executable = executable.to_string_lossy();
        let mut entries = Vec::new();
        entries
            .try_reserve_exact(4096)
            .map_err(|_| Error::ResourceLimit)?;
        for line in text.lines() {
            super::check(cancel, deadline)?;
            let mut fields = line.split_whitespace();
            let range = fields.next().ok_or(Error::MalformedRecord)?;
            let permissions = fields.next().ok_or(Error::MalformedRecord)?;
            let offset = fields.next().ok_or(Error::MalformedRecord)?;
            fields.next().ok_or(Error::MalformedRecord)?;
            fields.next().ok_or(Error::MalformedRecord)?;
            let mut file = String::new();
            file.try_reserve_exact(line.len())
                .map_err(|_| Error::ResourceLimit)?;
            for field in fields {
                if !file.is_empty() {
                    file.push(' ');
                }
                file.push_str(field);
            }
            if !permissions.contains('x') {
                continue;
            }
            let (start, end) = range.split_once('-').ok_or(Error::MalformedRecord)?;
            let number = |s| u64::from_str_radix(s, 16).map_err(|_| Error::MalformedRecord);
            let (start, end, offset) = (number(start)?, number(end)?, number(offset)?);
            if start >= end || file.len() > 4096 {
                return Err(Error::MalformedRecord);
            }
            if entries.len() == 4096 {
                return Err(Error::ResourceLimit);
            }
            entries.push(Map {
                start,
                end,
                offset,
                file,
            });
            if entries.len() > 4096 {
                return Err(Error::ResourceLimit);
            }
        }
        entries.sort_unstable_by_key(|m| (m.file != executable, m.start));
        Ok(Self { entries })
    }
}

// This handwritten wire subset uses the stable perftools.profiles field numbers.
// Unknown optional fields remain absent; no generated source is edited.
#[derive(Clone, PartialEq, Message)]
struct Profile {
    #[prost(message, repeated, tag = "1")]
    sample_type: Vec<ValueType>,
    #[prost(message, repeated, tag = "2")]
    sample: Vec<ProtoSample>,
    #[prost(message, repeated, tag = "3")]
    mapping: Vec<Mapping>,
    #[prost(message, repeated, tag = "4")]
    location: Vec<Location>,
    #[prost(string, repeated, tag = "6")]
    string_table: Vec<String>,
    #[prost(int64, tag = "9")]
    time_nanos: i64,
    #[prost(int64, tag = "10")]
    duration_nanos: i64,
    #[prost(message, optional, tag = "11")]
    period_type: Option<ValueType>,
    #[prost(int64, tag = "12")]
    period: i64,
    #[prost(int64, repeated, tag = "13")]
    comment: Vec<i64>,
}
#[derive(Clone, PartialEq, Message)]
struct ValueType {
    #[prost(int64, tag = "1")]
    ty: i64,
    #[prost(int64, tag = "2")]
    unit: i64,
}
#[derive(Clone, PartialEq, Message)]
struct ProtoSample {
    #[prost(uint64, repeated, tag = "1")]
    location_id: Vec<u64>,
    #[prost(int64, repeated, tag = "2")]
    value: Vec<i64>,
    #[prost(message, repeated, tag = "3")]
    label: Vec<Label>,
}
#[derive(Clone, PartialEq, Message)]
struct Label {
    #[prost(int64, tag = "1")]
    key: i64,
    #[prost(int64, tag = "3")]
    num: i64,
}
#[derive(Clone, PartialEq, Message)]
struct Mapping {
    #[prost(uint64, tag = "1")]
    id: u64,
    #[prost(uint64, tag = "2")]
    memory_start: u64,
    #[prost(uint64, tag = "3")]
    memory_limit: u64,
    #[prost(uint64, tag = "4")]
    file_offset: u64,
    #[prost(int64, tag = "5")]
    filename: i64,
    #[prost(int64, tag = "6")]
    build_id: i64,
}
#[derive(Clone, PartialEq, Message)]
struct Location {
    #[prost(uint64, tag = "1")]
    id: u64,
    #[prost(uint64, tag = "2")]
    mapping_id: u64,
    #[prost(uint64, tag = "3")]
    address: u64,
}

pub(super) fn encode(
    samples: &[Sample],
    maps: &Mappings,
    start: SystemTime,
    duration: Duration,
    cancel: &AtomicBool,
) -> Result<Vec<u8>, Error> {
    let deadline = Instant::now() + Duration::from_secs(5);
    super::check(cancel, deadline)?;
    if samples.len() > super::MAX_SAMPLES
        || samples.iter().map(|s| s.depth).sum::<usize>() > 1024 * 1024
    {
        return Err(Error::ResourceLimit);
    }
    // Preflight the graph before allocating it. Include an allowance per small
    // allocation, the hash index at four times its payload, both maps snapshots,
    // all frame references, and the final encoded buffer at the same time.
    let export_bytes = super::MAX_SAMPLES
        * (std::mem::size_of::<ProtoSample>() + 2 * 8 + 3 * std::mem::size_of::<Label>() + 128)
        + 1024 * 1024 * 8
        + 16_384 * (std::mem::size_of::<Location>() + 4 * 16)
        + 4096 * (std::mem::size_of::<Mapping>() + std::mem::size_of::<String>() + 128)
        + 4 * 1024 * 1024
        + MAX_OUTPUT;
    if export_bytes > 64 * 1024 * 1024 {
        return Err(Error::ResourceLimit);
    }
    let mut p = Profile {
        string_table: ["", "samples", "count", "cpu", "nanoseconds", "thread_id", "leaf_only", "userspace-only; raw addresses; supply matching binaries; build IDs not collected; task scan 100ms; collector excluded; callchains may be truncated; maps checked only at endpoints, transient changes may be missed; unmapped userspace frames retained with mapping_id=0, not assigned to a nearby mapping", "thread_start_ticks"].map(str::to_owned).to_vec(),
        sample_type: vec![ValueType { ty: 1, unit: 2 }, ValueType { ty: 3, unit: 4 }],
        time_nanos: start.duration_since(SystemTime::UNIX_EPOCH).map_err(|_| Error::Io)?.as_nanos().try_into().map_err(|_| Error::Io)?,
        duration_nanos: duration.as_nanos().try_into().map_err(|_| Error::Io)?,
        period_type: Some(ValueType { ty: 3, unit: 4 }), period: 1_000_000_000 / 49, comment: vec![7], ..Default::default()
    };
    p.mapping
        .try_reserve_exact(maps.entries.len())
        .map_err(|_| Error::ResourceLimit)?;
    p.string_table
        .try_reserve_exact(maps.entries.len() + 1)
        .map_err(|_| Error::ResourceLimit)?;
    p.location
        .try_reserve_exact(16_384)
        .map_err(|_| Error::ResourceLimit)?;
    for (index, map) in maps.entries.iter().enumerate() {
        super::check(cancel, deadline)?;
        let filename = p.string_table.len() as i64;
        let mut file = String::new();
        file.try_reserve_exact(map.file.len())
            .map_err(|_| Error::ResourceLimit)?;
        file.push_str(&map.file);
        p.string_table.push(file);
        p.mapping.push(Mapping {
            id: index as u64 + 1,
            memory_start: map.start,
            memory_limit: map.end,
            file_offset: map.offset,
            filename,
            build_id: 0,
        });
    }
    let mut locations = std::collections::HashMap::new();
    locations
        .try_reserve(16_384)
        .map_err(|_| Error::ResourceLimit)?;
    let mut frames = 0usize;
    let mut unmapped_locations = 0usize;
    let mut unmapped_frame_references = 0usize;
    let mut samples_with_unmapped_frames = 0usize;
    p.sample
        .try_reserve_exact(samples.len())
        .map_err(|_| Error::ResourceLimit)?;
    for sample in samples {
        super::check(cancel, deadline)?;
        frames += sample.depth;
        if frames > 1024 * 1024 {
            return Err(Error::ResourceLimit);
        }
        let mut ids = Vec::new();
        ids.try_reserve_exact(sample.depth)
            .map_err(|_| Error::ResourceLimit)?;
        let mut has_unmapped = false;
        for &address in &sample.frames[..sample.depth] {
            let mapping_id = maps
                .entries
                .iter()
                .position(|m| m.start <= address && address < m.end)
                .map_or(0, |index| index as u64 + 1);
            if mapping_id == 0 {
                unmapped_frame_references += 1;
                has_unmapped = true;
            }
            let id = if let Some(id) = locations.get(&address) {
                *id
            } else {
                if locations.len() == 16_384 {
                    return Err(Error::ResourceLimit);
                }
                let id = locations.len() as u64 + 1;
                locations.insert(address, id);
                unmapped_locations += usize::from(mapping_id == 0);
                p.location.push(Location {
                    id,
                    mapping_id,
                    address,
                });
                id
            };
            ids.push(id);
        }
        samples_with_unmapped_frames += usize::from(has_unmapped);
        let mut value = Vec::new();
        value
            .try_reserve_exact(2)
            .map_err(|_| Error::ResourceLimit)?;
        value.extend([
            i64::try_from(sample.count).map_err(|_| Error::ResourceLimit)?,
            i64::try_from(sample.period).map_err(|_| Error::ResourceLimit)?,
        ]);
        let mut label = Vec::new();
        label
            .try_reserve_exact(3)
            .map_err(|_| Error::ResourceLimit)?;
        label.extend([
            Label {
                key: 5,
                num: sample.tid.into(),
            },
            Label {
                key: 6,
                num: i64::from(sample.depth == 1),
            },
            Label {
                key: 8,
                num: sample.birth.try_into().map_err(|_| Error::ResourceLimit)?,
            },
        ]);
        p.sample.push(ProtoSample {
            location_id: ids,
            value,
            label,
        });
    }
    // Counts describe the exported graph, not weighted CPU time or lost samples.
    use std::fmt::Write;
    let mut counts = String::new();
    counts
        .try_reserve_exact(192)
        .map_err(|_| Error::ResourceLimit)?;
    write!(&mut counts, "unmapped_locations={unmapped_locations} unmapped_frame_references={unmapped_frame_references} aggregated_samples_with_unmapped_frames={samples_with_unmapped_frames}").map_err(|_| Error::ResourceLimit)?;
    p.comment
        .try_reserve_exact(1)
        .map_err(|_| Error::ResourceLimit)?;
    p.comment.push(p.string_table.len() as i64);
    p.string_table.push(counts);
    let length = p.encoded_len();
    if length > MAX_OUTPUT {
        return Err(Error::ResourceLimit);
    }
    let mut output = Vec::new();
    output
        .try_reserve_exact(length)
        .map_err(|_| Error::ResourceLimit)?;
    p.encode(&mut output).map_err(|_| Error::ResourceLimit)?;
    super::check(cancel, deadline)?;
    Ok(output)
}

#[cfg(test)]
pub(super) fn assert_native_threads(bytes: &[u8], tids: &[u32]) {
    let profile = Profile::decode(bytes).expect("valid native protobuf");
    assert!(!profile.sample.is_empty());
    assert!(!profile.location.is_empty());
    for tid in tids {
        assert!(
            profile.sample.iter().any(|sample| sample.value[0] > 0
                && sample.value[1] > 0
                && sample
                    .label
                    .iter()
                    .any(|label| label.key == 5 && label.num == i64::from(*tid))),
            "missing busy thread {tid}"
        );
    }
    assert!(
        profile
            .sample
            .iter()
            .any(|sample| sample.location_id.len() > 1),
        "native profile has only leaf samples"
    );
}

#[cfg(test)]
mod tests {
    use super::*;
    fn fixture_maps() -> Mappings {
        Mappings {
            entries: vec![Map {
                start: 0x1000,
                end: 0x2000,
                offset: 0,
                file: "binary".into(),
            }],
        }
    }
    fn fixture_sample(addresses: &[u64], count: u64, period: u64) -> Sample {
        let mut sample = Sample {
            tid: 17,
            birth: 123,
            count,
            period,
            depth: addresses.len(),
            frames: [0; 64],
        };
        sample.frames[..addresses.len()].copy_from_slice(addresses);
        sample
    }
    fn encoded(samples: &[Sample], maps: &Mappings) -> Profile {
        let bytes = encode(
            samples,
            maps,
            SystemTime::UNIX_EPOCH,
            Duration::from_secs(20),
            &AtomicBool::new(false),
        )
        .unwrap();
        Profile::decode(bytes.as_slice()).unwrap()
    }
    #[test]
    fn unknown_leaf_and_callers_preserve_addresses_recursion_and_weights() {
        let samples = [
            fixture_sample(&[0x1000, 0x2000, 0x1000, 0x2000], 3, 60),
            fixture_sample(&[0x9000, 0x1fff], 5, 100),
            fixture_sample(&[0x1fff], 2, 40),
        ];
        let p = encoded(&samples, &fixture_maps());
        assert_eq!(p.sample.len(), samples.len());
        for (actual, expected) in p.sample.iter().zip(&samples) {
            assert_eq!(
                actual.value,
                [expected.count as i64, expected.period as i64]
            );
            let locations: Vec<_> = actual
                .location_id
                .iter()
                .map(|id| {
                    p.location
                        .iter()
                        .find(|location| location.id == *id)
                        .unwrap()
                })
                .collect();
            assert_eq!(
                locations.iter().map(|l| l.address).collect::<Vec<_>>(),
                expected.frames[..expected.depth]
            );
            for location in locations {
                assert_eq!(
                    location.mapping_id,
                    if (0x1000..0x2000).contains(&location.address) {
                        1
                    } else {
                        0
                    }
                );
            }
        }
        assert_eq!(p.sample.iter().map(|s| s.value[0]).sum::<i64>(), 10);
        assert_eq!(p.sample.iter().map(|s| s.value[1]).sum::<i64>(), 200);
        assert_eq!(p.sample[0].location_id[0], p.sample[0].location_id[2]);
        assert_eq!(p.sample[0].location_id[1], p.sample[0].location_id[3]);
        assert!(p.comment.iter().map(|i| p.string_table[*i as usize].as_str()).any(|s| s == "unmapped_locations=2 unmapped_frame_references=3 aggregated_samples_with_unmapped_frames=2"));
        let mapped = encoded(&samples[2..], &fixture_maps());
        assert!(mapped.comment.iter().map(|i| mapped.string_table[*i as usize].as_str()).any(|s| s == "unmapped_locations=0 unmapped_frame_references=0 aggregated_samples_with_unmapped_frames=0"));
    }
    #[test]
    fn changed_executable_snapshot_still_fails_closed() {
        let before = fixture_maps();
        before.ensure_unchanged(&fixture_maps()).unwrap();
        for field in 0..5 {
            let mut after = fixture_maps();
            match field {
                0 => after.entries[0].start += 1,
                1 => after.entries[0].end += 1,
                2 => after.entries[0].offset += 1,
                3 => after.entries[0].file.push_str(".changed"),
                _ => after.entries.clear(),
            }
            assert_eq!(before.ensure_unchanged(&after), Err(Error::MappingChanged));
        }
    }
    #[test]
    fn diverse_stacks_keep_totals_with_unknown_frames() {
        let mut samples = Vec::new();
        for index in 0..6860u64 {
            let mut sample = fixture_sample(&[0; 64], 1, 20_408_163);
            sample.tid = 1 + (index % 15) as u32;
            for frame in 0..64 {
                sample.frames[frame] = if frame % 2 == 0 {
                    0x1000 + (index + frame as u64) % 4096
                } else {
                    0x10000 + index
                };
            }
            samples.push(sample);
        }
        let p = encoded(&samples, &fixture_maps());
        assert_eq!(p.sample.len(), 6860);
        assert_eq!(
            p.sample.iter().map(|s| s.value[1]).sum::<i64>(),
            6860 * 20_408_163
        );
        assert!(p.sample.iter().all(|s| s.location_id.len() == 64));
        assert_eq!(
            p.location.iter().filter(|l| l.mapping_id == 0).count(),
            6860
        );
    }
    #[test]
    fn unknown_locations_still_obey_the_existing_location_limit() {
        let mut samples: Vec<_> = (0..16_384)
            .map(|index| fixture_sample(&[0x10000 + index], 1, 1))
            .collect();
        let p = encoded(&samples, &fixture_maps());
        assert_eq!(p.location.len(), 16_384);
        assert!(p.location.iter().all(|location| location.mapping_id == 0));
        samples.push(fixture_sample(&[0x20000], 1, 1));
        assert_eq!(
            encode(
                &samples,
                &fixture_maps(),
                SystemTime::UNIX_EPOCH,
                Duration::from_secs(20),
                &AtomicBool::new(false)
            ),
            Err(Error::ResourceLimit)
        );
    }
    #[test]
    fn raw_profile_roundtrip_has_mappings_cpu_and_thread() {
        let maps = Mappings {
            entries: vec![Map {
                start: 0x1000,
                end: 0x2000,
                offset: 0x3000,
                file: "binary".into(),
            }],
        };
        let mut sample = Sample {
            tid: 17,
            birth: 123,
            count: 3,
            period: 12345,
            depth: 1,
            frames: [0; 64],
        };
        sample.frames[0] = 0x1234;
        let bytes = encode(
            &[sample.clone()],
            &maps,
            SystemTime::UNIX_EPOCH,
            Duration::from_secs(1),
            &AtomicBool::new(false),
        )
        .unwrap();
        let p = Profile::decode(bytes.as_slice()).unwrap();
        assert_eq!(p.sample[0].value, [3, 12345]);
        assert_eq!(p.sample[0].label[2].num, 123);
        assert_eq!(p.mapping[0].build_id, 0);
        assert_eq!(p.sample[0].label[0].num, 17);
        assert_eq!(p.mapping[0].file_offset, 0x3000);
        assert_eq!(p.location[0].address, 0x1234);
        assert_eq!(p.string_table[0], "");
        assert!(matches!(
            encode(
                &[sample],
                &maps,
                SystemTime::UNIX_EPOCH,
                Duration::ZERO,
                &AtomicBool::new(true)
            ),
            Err(Error::Cancelled)
        ));
    }
    #[test]
    fn own_executable_maps_and_build_id_are_readable() {
        assert!(
            !Mappings::read(
                &AtomicBool::new(false),
                Instant::now() + Duration::from_secs(1)
            )
            .unwrap()
            .entries
            .is_empty()
        );
        assert!(matches!(
            Mappings::read(
                &AtomicBool::new(true),
                Instant::now() + Duration::from_secs(1)
            ),
            Err(Error::Cancelled)
        ));
    }
}

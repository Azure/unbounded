// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use unbounded_storage::topology::Host;

pub fn block_annotation(host: &Host) -> Vec<u8> {
    let mut out = String::new();
    for (idx, dev) in host.block_devices.iter().enumerate() {
        if idx != 0 {
            out.push(',');
        }

        out.push_str("/dev/");
        out.push_str(&dev.dev_name);
        out.push_str("?name=");
        push_query_value(&mut out, &dev.dev_name);
        if let Some(size_bytes) = dev.size_bytes {
            out.push_str("&size_bytes=");
            out.push_str(&size_bytes.to_string());
        }
    }

    out.into_bytes()
}

fn push_query_value(out: &mut String, value: &str) {
    const HEX: &[u8; 16] = b"0123456789ABCDEF";

    for b in value.as_bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => {
                out.push(*b as char);
            }
            _ => {
                out.push('%');
                out.push(HEX[(b >> 4) as usize] as char);
                out.push(HEX[(b & 0x0f) as usize] as char);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use unbounded_storage::topology::BlockDevice;

    #[test]
    fn block_annotation_includes_name_and_size_only() {
        let host = Host {
            block_devices: vec![
                BlockDevice {
                    dev_name: "sdb".to_string(),
                    size_bytes: Some(4096),
                },
                BlockDevice {
                    dev_name: "nvme0n1".to_string(),
                    size_bytes: None,
                },
            ],
            ..Host::default()
        };

        let annotation = String::from_utf8(block_annotation(&host)).unwrap();

        assert_eq!(
            annotation,
            "/dev/sdb?name=sdb&size_bytes=4096,/dev/nvme0n1?name=nvme0n1"
        );
    }

    #[test]
    fn query_values_are_percent_encoded() {
        let host = Host {
            block_devices: vec![BlockDevice {
                dev_name: "disk by-id".to_string(),
                size_bytes: None,
            }],
            ..Host::default()
        };
        let block = String::from_utf8(block_annotation(&host)).unwrap();

        assert_eq!(block, "/dev/disk by-id?name=disk%20by-id");
    }
}

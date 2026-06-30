// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::collections::BTreeMap;

use unbounded_storage::topology::Host;

use crate::fabric_group::FabricUnitAddress;

pub fn rdma_annotation(host: &Host, unit_addresses: &[FabricUnitAddress]) -> Vec<u8> {
    let mut addrs_by_hca: BTreeMap<&str, Vec<String>> = BTreeMap::new();

    for unit in unit_addresses.iter().filter(|unit| unit.rdma) {
        addrs_by_hca
            .entry(unit.device_name.as_str())
            .or_default()
            .push(unit.addr.clone());
    }

    for addrs in addrs_by_hca.values_mut() {
        addrs.sort();
    }

    let mut out = String::new();
    for (idx, hca) in host.hcas.iter().enumerate() {
        if idx != 0 {
            out.push(',');
        }

        out.push_str(&hca.dev_name);
        if let Some(addrs) = addrs_by_hca.get(hca.dev_name.as_str()) {
            for (addr_idx, addr) in addrs.iter().enumerate() {
                out.push(if addr_idx == 0 { '?' } else { '&' });
                out.push_str("addr=");
                push_query_value(&mut out, addr);
            }
        }
    }

    out.into_bytes()
}

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

    use unbounded_storage::topology::{BlockDevice, Hca};

    #[test]
    fn rdma_annotation_is_single_line_and_includes_all_hcas() {
        let host = Host {
            hcas: vec![
                Hca {
                    dev_name: "mlx5_0".to_string(),
                    pci_bdf: Some("0000:af:00.0".to_string()),
                    pcie_root: Some("0000:ae:00.0".to_string()),
                    numa: Some(0),
                    ports_active: true,
                },
                Hca {
                    dev_name: "mlx5_1".to_string(),
                    pci_bdf: None,
                    pcie_root: None,
                    numa: None,
                    ports_active: false,
                },
            ],
            ..Host::default()
        };
        let units = vec![
            FabricUnitAddress {
                device_name: "mlx5_0".to_string(),
                rdma: true,
                addr: "hex:02".to_string(),
            },
            FabricUnitAddress {
                device_name: "lo".to_string(),
                rdma: false,
                addr: "127.0.0.1:9000".to_string(),
            },
            FabricUnitAddress {
                device_name: "mlx5_0".to_string(),
                rdma: true,
                addr: "hex:01".to_string(),
            },
        ];

        let annotation = String::from_utf8(rdma_annotation(&host, &units)).unwrap();

        assert!(!annotation.contains('\n'));
        assert_eq!(annotation, "mlx5_0?addr=hex%3A01&addr=hex%3A02,mlx5_1");
    }

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
            hcas: vec![Hca {
                dev_name: "mlx5_0".to_string(),
                pci_bdf: None,
                pcie_root: None,
                numa: None,
                ports_active: true,
            }],
            block_devices: vec![BlockDevice {
                dev_name: "disk by-id".to_string(),
                size_bytes: None,
            }],
            ..Host::default()
        };
        let units = vec![FabricUnitAddress {
            device_name: "mlx5_0".to_string(),
            rdma: true,
            addr: "[::]:60000".to_string(),
        }];

        let rdma = String::from_utf8(rdma_annotation(&host, &units)).unwrap();
        let block = String::from_utf8(block_annotation(&host)).unwrap();

        assert_eq!(rdma, "mlx5_0?addr=%5B%3A%3A%5D%3A60000");
        assert_eq!(block, "/dev/disk by-id?name=disk%20by-id");
    }
}

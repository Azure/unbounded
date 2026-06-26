// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::collections::BTreeMap;

use unbounded_storage::topology::Host;

use crate::fabric_group::FabricUnitAddress;

pub fn to_json(host: &Host, unit_addresses: &[FabricUnitAddress]) -> Vec<u8> {
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

    let mut out = String::from("{\"schemaVersion\":1,\"hcas\":[");
    for (idx, hca) in host.hcas.iter().enumerate() {
        if idx != 0 {
            out.push(',');
        }

        out.push_str("{\"name\":");
        push_json_string(&mut out, &hca.dev_name);
        if let Some(pci_bdf) = &hca.pci_bdf {
            out.push_str(",\"pciBdf\":");
            push_json_string(&mut out, pci_bdf);
        }
        if let Some(pcie_root) = &hca.pcie_root {
            out.push_str(",\"pcieRoot\":");
            push_json_string(&mut out, pcie_root);
        }
        if let Some(numa) = hca.numa {
            out.push_str(",\"numa\":");
            out.push_str(&numa.to_string());
        }
        out.push_str(",\"portsActive\":");
        out.push_str(if hca.ports_active { "true" } else { "false" });
        out.push_str(",\"addrs\":[");
        if let Some(addrs) = addrs_by_hca.get(hca.dev_name.as_str()) {
            for (addr_idx, addr) in addrs.iter().enumerate() {
                if addr_idx != 0 {
                    out.push(',');
                }
                push_json_string(&mut out, addr);
            }
        }
        out.push_str("]}");
    }
    out.push_str("]}");

    out.into_bytes()
}

fn push_json_string(out: &mut String, value: &str) {
    out.push('"');
    for c in value.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if c <= '\u{1f}' => {
                out.push_str("\\u");
                out.push_str(&format!("{:04x}", c as u32));
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

#[cfg(test)]
mod tests {
    use super::*;

    use unbounded_storage::topology::Hca;

    #[test]
    fn inventory_json_is_single_line_and_includes_all_hcas() {
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

        let json = String::from_utf8(to_json(&host, &units)).unwrap();

        assert!(!json.contains('\n'));
        assert_eq!(
            json,
            concat!(
                "{\"schemaVersion\":1,\"hcas\":[",
                "{\"name\":\"mlx5_0\",\"pciBdf\":\"0000:af:00.0\",",
                "\"pcieRoot\":\"0000:ae:00.0\",\"numa\":0,",
                "\"portsActive\":true,\"addrs\":[\"hex:01\",\"hex:02\"]},",
                "{\"name\":\"mlx5_1\",\"portsActive\":false,\"addrs\":[]}]}",
            )
        );
    }

    #[test]
    fn inventory_json_escapes_strings() {
        let host = Host {
            hcas: vec![Hca {
                dev_name: "mlx5_\"0".to_string(),
                pci_bdf: Some("0000\\af".to_string()),
                pcie_root: None,
                numa: None,
                ports_active: true,
            }],
            ..Host::default()
        };

        let json = String::from_utf8(to_json(&host, &[])).unwrap();

        assert_eq!(
            json,
            "{\"schemaVersion\":1,\"hcas\":[{\"name\":\"mlx5_\\\"0\",\"pciBdf\":\"0000\\\\af\",\"portsActive\":true,\"addrs\":[]}]}",
        );
    }
}

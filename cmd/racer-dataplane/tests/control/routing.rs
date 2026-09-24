// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn v1_requires_explicit_product_and_rejects_historical_formats() {
        let mut v = crate::control::product_routing::tests::volume(1, 1, vec![0], 0, vec![0]);
        let r = Routing::new(&[1; 32], &v).unwrap();
        let c = r.start_key(&[0; 32]);
        assert_eq!(c.encode().len(), 71);
        assert_eq!(c.algorithm.magic(), b"RR01");
        assert_eq!(Cursor::decode(&c.encode()).unwrap(), c);
        assert!(Cursor::decode(&c.encode()[..45]).is_err());
        let mut old = b"RR02".to_vec();
        old.extend(c.encode());
        old.extend(b"RD01\0/object");
        let old =
            crate::cache::peer_wire::with_budget(old, std::time::Duration::from_secs(1)).unwrap();
        assert!(crate::cache::peer_wire::routed_descriptor(&old).is_err());
        for algorithm in [None, Some(0), Some(2)] {
            v.topology.as_mut().unwrap().routing_algorithm = algorithm;
            assert!(Routing::new(&[1; 32], &v).is_err());
        }
        v.topology.as_mut().unwrap().routing_algorithm = Some(1);
        v.topology.as_mut().unwrap().product = None;
        assert!(Routing::new(&[1; 32], &v).is_err());
        v.topology = None;
        assert!(Routing::new(&[1; 32], &v).is_err());
    }
}

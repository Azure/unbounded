use crate::{Error, MAX_MEMBERS, Result};
use serde::de::{self, DeserializeSeed, MapAccess, SeqAccess, Visitor};
use std::{cell::Cell, fmt};

#[derive(Clone, Copy)]
pub(super) enum Shape {
    Text,
    Unsigned(u64),
    Object(&'static [Field]),
    Array(&'static Shape, usize),
}

pub(super) struct Field(pub &'static str, pub Shape, pub bool);

use Shape::{Array, Object, Text, Unsigned};
const U32: Shape = Unsigned(u32::MAX as u64);
const RAIL: Shape = Object(&[
    Field("device", Text, true),
    Field("port", Unsigned(u8::MAX as u64), true),
    Field("rail", Unsigned(u16::MAX as u64), true),
    Field("gid", Text, false),
    Field("numa_node", U32, false),
]);
const NICS: Shape = Array(&RAIL, 64);
const MEMBER: Shape = Object(&[
    Field("node", Text, true),
    Field("shares", U32, true),
    Field("peer_endpoint", Text, true),
    Field("rdma_nics", NICS, true),
    Field("site", Text, true),
]);
const CACHE: Shape = Object(&[
    Field("id", Text, true),
    Field("name", Text, true),
    Field("client_socket", Text, true),
    Field("origin_socket", Text, true),
]);
const CACHES: Shape = Array(&CACHE, usize::MAX);
pub(super) const PUBLICATION: Shape = Object(&[
    Field("schema_version", U32, true),
    Field("cluster", Text, true),
    Field("sequence", Text, true),
    Field("membership_version", Text, true),
    Field("members", Array(&MEMBER, MAX_MEMBERS), true),
    Field("caches", CACHES, true),
]);
pub(super) const REQUEST: Shape = Object(&[
    Field("shares", U32, true),
    Field("rdma_nics", NICS, true),
    Field("schema_version", U32, true),
    Field("cluster", Text, true),
    Field("enrollment", Text, true),
    Field("csr_der", Text, true),
]);
pub(super) const RESPONSE: Shape = Object(&[
    Field("schema_version", U32, true),
    Field("cluster", Text, true),
    Field("node", Text, true),
    Field("enrollment", Text, true),
    Field("certificate_chain", Array(&Text, usize::MAX), true),
    Field("block_devices", Text, false),
]);
pub(super) const DELTA: Shape = Object(&[
    Field("delta_version", U32, true),
    Field("cluster", Text, true),
    Field("base_sequence", Text, true),
    Field("base_hash", Text, true),
    Field("sequence", Text, true),
    Field("membership_version", Text, true),
    Field("content_hash", Text, true),
    Field("upsert_members", Array(&MEMBER, MAX_MEMBERS), true),
    Field("remove_members", Array(&Text, usize::MAX), true),
    Field("caches", CACHES, true),
]);
const KEY: Shape = Object(&[
    Field("cache", Text, true),
    Field("id", Text, true),
    Field("purpose", Text, true),
    Field("state", Text, true),
    Field("material", Text, true),
]);
pub(super) const BUNDLE: Shape = Object(&[
    Field("schema_version", U32, true),
    Field("cluster", Text, true),
    Field("generation", Text, true),
    Field("peer_trust_roots", Array(&Text, usize::MAX), true),
    Field("cache_keys", Array(&KEY, usize::MAX), true),
]);
pub(super) const FAILURE: Shape = Object(&[Field("code", Text, true)]);

struct Check<'a> {
    shape: Shape,
    overloaded: &'a Cell<bool>,
    excess: bool,
}

impl<'de> DeserializeSeed<'de> for Check<'_> {
    type Value = ();

    fn deserialize<D: serde::Deserializer<'de>>(self, d: D) -> std::result::Result<(), D::Error> {
        if self.excess {
            self.overloaded.set(true);
            return Err(de::Error::custom("array limit"));
        }
        d.deserialize_any(self)
    }
}

impl<'de> Visitor<'de> for Check<'_> {
    type Value = ();

    fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
        f.write_str("wire shape")
    }

    fn visit_str<E: de::Error>(self, _: &str) -> std::result::Result<(), E> {
        match self.shape {
            Text => Ok(()),
            _ => Err(E::custom("string shape")),
        }
    }

    // deserialize_any preserves integer tokens. Floats, exponents, and -0 reach
    // other visitor methods and are rejected, without rounding through f64.
    fn visit_u64<E: de::Error>(self, value: u64) -> std::result::Result<(), E> {
        match self.shape {
            Unsigned(max) if value <= max => Ok(()),
            _ => Err(E::custom("integer shape")),
        }
    }

    fn visit_seq<A: SeqAccess<'de>>(self, mut a: A) -> std::result::Result<(), A::Error> {
        let Array(element, max) = self.shape else {
            return Err(de::Error::custom("array shape"));
        };
        let mut count = 0;
        while a
            .next_element_seed(Check {
                shape: *element,
                overloaded: self.overloaded,
                excess: count >= max,
            })?
            .is_some()
        {
            count += 1;
        }
        Ok(())
    }

    fn visit_map<A: MapAccess<'de>>(self, mut a: A) -> std::result::Result<(), A::Error> {
        let Object(fields) = self.shape else {
            return Err(de::Error::custom("object shape"));
        };
        // Fixed schema bits, never an input-sized set of attacker field names.
        let mut seen = 0u64;
        while let Some(index) = a.next_key_seed(FieldIndex(fields))? {
            let bit = 1u64 << index;
            if seen & bit != 0 {
                return Err(de::Error::custom("duplicate"));
            }
            seen |= bit;
            a.next_value_seed(Check {
                shape: fields[index].1,
                overloaded: self.overloaded,
                excess: false,
            })?;
        }
        if fields
            .iter()
            .enumerate()
            .any(|(i, field)| field.2 && seen & (1 << i) == 0)
        {
            return Err(de::Error::custom("missing field"));
        }
        Ok(())
    }
}

struct FieldIndex(&'static [Field]);

impl<'de> DeserializeSeed<'de> for FieldIndex {
    type Value = usize;

    fn deserialize<D: serde::Deserializer<'de>>(
        self,
        d: D,
    ) -> std::result::Result<usize, D::Error> {
        d.deserialize_identifier(self)
    }
}

impl Visitor<'_> for FieldIndex {
    type Value = usize;

    fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
        f.write_str("wire field")
    }

    fn visit_str<E: de::Error>(self, name: &str) -> std::result::Result<usize, E> {
        self.0
            .iter()
            .position(|field| field.0 == name)
            .ok_or_else(|| E::custom("unknown field"))
    }
}

pub(super) fn validate(input: &[u8], shape: Shape) -> Result<()> {
    let overloaded = Cell::new(false);
    let mut d = serde_json::Deserializer::from_slice(input);
    Check {
        shape,
        overloaded: &overloaded,
        excess: false,
    }
    .deserialize(&mut d)
    .and_then(|()| d.end())
    .map_err(|_| {
        if overloaded.get() {
            Error::Overloaded
        } else {
            Error::InvalidRequest
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_subtrees_before_traversal() {
        for (prefix, suffix) in [
            ("[", "{}]"),
            (r#"{"unknown""#, ":[{}]}"),
            (r#"{"schema_version":["#, "{}]}"),
            (r#"{"members":{"#, r#""unknown":[]}}"#),
            (r#"{"members":[["#, "{}]]}"),
            (r#"{"schema_version":1,"schema_versi\u006fn""#, ":[{}]}"),
        ] {
            let input = format!("{prefix}{suffix}");
            let overloaded = Cell::new(false);
            let mut d = serde_json::Deserializer::from_slice(input.as_bytes());
            let error = Check {
                shape: PUBLICATION,
                overloaded: &overloaded,
                excess: false,
            }
            .deserialize(&mut d)
            .unwrap_err();
            assert_eq!(error.column(), prefix.len(), "{input}");
            assert!(!overloaded.get());
        }
    }

    #[test]
    fn array_limits_reject_before_excess_element() {
        for (element, shape, max) in [
            (r#"{"device":"a","port":1,"rail":0}"#, NICS, 64),
            (
                r#"{"node":"","shares":0,"peer_endpoint":"","rdma_nics":[],"site":""}"#,
                Array(&MEMBER, MAX_MEMBERS),
                MAX_MEMBERS,
            ),
        ] {
            let prefix = format!("[{}{}", format!("{element},").repeat(max - 1), element);
            assert_eq!(validate(format!("{prefix}]").as_bytes(), shape), Ok(()));
            // An invalid extra value must report size, not its shape or syntax.
            assert_eq!(
                validate(format!("{prefix},{{\"unvisited\":").as_bytes(), shape),
                Err(Error::Overloaded)
            );
        }
    }

    #[test]
    fn integer_tokens_are_exact() {
        for good in [
            "0",
            "4294967295",
            "9007199254740993",
            "18446744073709551615",
        ] {
            assert_eq!(validate(good.as_bytes(), Unsigned(u64::MAX)), Ok(()));
        }
        for bad in [
            "-0",
            "-1",
            "0.0",
            "1e0",
            "01",
            "+1",
            "18446744073709551616",
            "null",
            "true",
            "\"1\"",
        ] {
            assert_eq!(
                validate(bad.as_bytes(), Unsigned(u64::MAX)),
                Err(Error::InvalidRequest),
                "{bad}"
            );
        }
        assert_eq!(validate(b"4294967296", U32), Err(Error::InvalidRequest));
    }
}

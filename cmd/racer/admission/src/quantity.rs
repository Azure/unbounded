//! Kubernetes resource quantity parsing, limited to what the injector needs:
//! checking the syntax, checking the sign, and comparing two values.
//!
//! This follows `resource.ParseQuantity` from k8s.io/apimachinery. Values are
//! rounded up to whole nano units, as Kubernetes does, before they are compared.
//! Unlike Kubernetes, decimal exponents beyond +/-`MAX_EXPONENT` are rejected
//! so that comparison stays cheap.

use std::cmp::Ordering;

const MAX_EXPONENT: i64 = 1000;
const MAX_DIGITS: usize = 256;

/// A parsed quantity, held as an exact count of nano units.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Quantity {
    negative: bool,
    nanos: Big,
}

impl Quantity {
    pub(crate) fn is_positive(&self) -> bool {
        !self.negative && !self.nanos.is_zero()
    }

    /// Compares two positive quantities.
    pub(crate) fn cmp_positive(&self, other: &Self) -> Ordering {
        self.nanos.cmp(&other.nanos)
    }
}

pub(crate) fn parse(s: &str) -> Option<Quantity> {
    let b = s.as_bytes();
    let mut i = 0;
    let mut negative = false;
    if i < b.len() && (b[i] == b'+' || b[i] == b'-') {
        negative = b[i] == b'-';
        i += 1;
    }

    let mut digits = Vec::new();
    let mut frac = 0i64;
    while i < b.len() && b[i].is_ascii_digit() {
        digits.push(b[i] - b'0');
        i += 1;
    }
    if i < b.len() && b[i] == b'.' {
        i += 1;
        while i < b.len() && b[i].is_ascii_digit() {
            digits.push(b[i] - b'0');
            frac += 1;
            i += 1;
        }
    }
    if digits.is_empty() || digits.len() > MAX_DIGITS {
        return None;
    }

    let (exp10, exp2) = suffix(&s[i..])?;
    let p = exp10 - frac + 9;

    let mut n = Big::default();
    for d in digits {
        n.mul_small(10);
        n.add_small(u32::from(d));
    }
    for _ in 0..exp2 {
        n.mul_small(2);
    }
    if p >= 0 {
        for _ in 0..p {
            n.mul_small(10);
        }
    } else {
        let mut inexact = false;
        for _ in 0..-p {
            inexact |= n.div_small(10) != 0;
        }
        if inexact {
            n.add_small(1);
        }
    }

    Some(Quantity { negative, nanos: n })
}

/// Returns the decimal and binary exponents for a quantity suffix.
fn suffix(s: &str) -> Option<(i64, i64)> {
    let r = match s {
        "" => (0, 0),
        "n" => (-9, 0),
        "u" => (-6, 0),
        "m" => (-3, 0),
        "k" => (3, 0),
        "M" => (6, 0),
        "G" => (9, 0),
        "T" => (12, 0),
        "P" => (15, 0),
        "E" => (18, 0),
        "Ki" => (0, 10),
        "Mi" => (0, 20),
        "Gi" => (0, 30),
        "Ti" => (0, 40),
        "Pi" => (0, 50),
        "Ei" => (0, 60),
        _ => {
            let rest = s.strip_prefix(['e', 'E'])?;
            let digits = rest.strip_prefix(['+', '-']).unwrap_or(rest);
            if digits.is_empty() || digits.len() > 6 || !digits.bytes().all(|c| c.is_ascii_digit())
            {
                return None;
            }
            let e: i64 = rest.parse().ok()?;
            if e.abs() > MAX_EXPONENT {
                return None;
            }
            (e, 0)
        }
    };
    Some(r)
}

/// Minimal unsigned big integer, little-endian base 2^32 limbs.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
struct Big(Vec<u32>);

impl Big {
    fn is_zero(&self) -> bool {
        self.0.iter().all(|&l| l == 0)
    }

    fn mul_small(&mut self, m: u32) {
        let mut carry = 0u64;
        for l in &mut self.0 {
            let v = u64::from(*l) * u64::from(m) + carry;
            *l = v as u32;
            carry = v >> 32;
        }
        if carry != 0 {
            self.0.push(carry as u32);
        }
    }

    fn add_small(&mut self, a: u32) {
        let mut carry = u64::from(a);
        for l in &mut self.0 {
            if carry == 0 {
                return;
            }
            let v = u64::from(*l) + carry;
            *l = v as u32;
            carry = v >> 32;
        }
        if carry != 0 {
            self.0.push(carry as u32);
        }
    }

    /// Divides in place and returns the remainder.
    fn div_small(&mut self, d: u32) -> u32 {
        let mut rem = 0u64;
        for l in self.0.iter_mut().rev() {
            let v = (rem << 32) | u64::from(*l);
            *l = (v / u64::from(d)) as u32;
            rem = v % u64::from(d);
        }
        rem as u32
    }

    fn trimmed(&self) -> &[u32] {
        let n = self.0.iter().rposition(|&l| l != 0).map_or(0, |p| p + 1);
        &self.0[..n]
    }
}

impl PartialOrd for Big {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for Big {
    fn cmp(&self, other: &Self) -> Ordering {
        let (a, b) = (self.trimmed(), other.trimmed());
        a.len()
            .cmp(&b.len())
            .then_with(|| a.iter().rev().cmp(b.iter().rev()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn q(s: &str) -> Quantity {
        parse(s).unwrap_or_else(|| panic!("parse {s:?}"))
    }

    #[test]
    fn valid_forms() {
        for s in [
            "1", "100m", "64Mi", "1Gi", "0.5", ".5", "5.", "+1", "1e3", "1E3", "1e-3", "1e+3",
            "2Ei", "1n", "1u", "1k", "1M", "1G", "1T", "1P", "1E", "1Ki", "1Ti", "1Pi",
        ] {
            assert!(q(s).is_positive(), "{s}");
        }
    }

    #[test]
    fn invalid_forms() {
        for s in [
            "",
            "lots",
            ".",
            "+",
            "1K",
            "1KI",
            "1mi",
            "1e",
            "1e+",
            "1 Gi",
            "1..0",
            "1Gi2",
            "0x10",
            "1e1001",
            "1e9999999",
            "--1",
            "1b",
        ] {
            assert!(parse(s).is_none(), "{s:?}");
        }
    }

    #[test]
    fn sign() {
        assert!(!q("0").is_positive());
        assert!(!q("-1").is_positive());
        assert!(!q("-0").is_positive());
        assert!(!q("0Gi").is_positive());
        // Kubernetes rounds sub-nano values up to 1n.
        assert!(q("0.1n").is_positive());
        assert!(q("1e-12").is_positive());
    }

    #[test]
    fn compare() {
        use Ordering::*;
        for (a, b, want) in [
            ("1000m", "1", Equal),
            ("1", "999m", Greater),
            ("1Gi", "1G", Greater),
            ("1024Mi", "1Gi", Equal),
            ("2Gi", "1Gi", Greater),
            ("1e3", "1k", Equal),
            ("0.1n", "0.2n", Equal),
            ("1.5", "1500m", Equal),
            ("100m", "1", Less),
            ("1e1000", "1e999", Greater),
        ] {
            assert_eq!(q(a).cmp_positive(&q(b)), want, "{a} vs {b}");
        }
    }
}

//! Differential head parsing against `httparse` over a seeded generated corpus.
//!
//! Both parsers must agree on acceptance, and accepted fields must match after
//! trimming optional whitespace, except where the codec is intentionally
//! stricter. Those differences are listed in [`Strict`] and are only allowed in
//! the codec-rejects direction: the codec never accepts what `httparse` rejects.

use racer_wire::http::{Codec, StartLine, trim_ows};

/// Deterministic xorshift64* generator; no external dependency.
struct Rng(u64);

impl Rng {
    /// Next pseudo-random value.
    fn next(&mut self) -> u64 {
        self.0 ^= self.0 >> 12;
        self.0 ^= self.0 << 25;
        self.0 ^= self.0 >> 27;
        self.0.wrapping_mul(0x2545_f491_4f6c_dd1d)
    }

    /// Pick one element.
    fn pick<T: Copy>(&mut self, items: &[T]) -> T {
        items[(self.next() % items.len() as u64) as usize]
    }
}

/// Intentional codec strictness beyond `httparse`.
#[derive(Default)]
struct Strict {
    /// Only CRLF line endings are accepted.
    bare_lf: bool,

    /// Only HTTP/1.1 is accepted.
    version: bool,

    /// Transfer coding, duplicate or non-decimal Content-Length, duplicate Host,
    /// and Connection tokens naming framing fields are rejected.
    framing: bool,

    /// Response status codes must be within 100 through 599.
    status: bool,
}

impl Strict {
    /// Whether any intentional difference applies.
    fn any(&self) -> bool {
        self.bare_lf || self.version || self.framing || self.status
    }
}

/// Generate one head and the strictness it triggers.
fn generate(rng: &mut Rng) -> (Vec<u8>, Strict) {
    let mut strict = Strict::default();
    let mut wire = Vec::new();
    let response = rng.next().is_multiple_of(2);
    let version = rng.pick(&["HTTP/1.1", "HTTP/1.1", "HTTP/1.1", "HTTP/1.0", "HTTP/2.0"]);
    if version == "HTTP/1.0" {
        strict.version = true;
    }
    if response {
        let status = rng.pick(&["200", "404", "100", "599", "099", "20", "2000", "abc"]);
        if status == "099" {
            strict.status = true;
        }
        let reason = rng.pick(&["", " OK", " Not Found", " \t", " r\u{80}"]);
        wire.extend_from_slice(format!("{version} {status}{reason}").as_bytes());
    } else {
        let method = rng.pick(&["GET", "POST", "M-SEARCH", "G T", ""]);
        let target = rng.pick(&["/", "/a?b=c", "*", "/\x7f", "/%20"]);
        wire.extend_from_slice(format!("{method} {target} {version}").as_bytes());
    }
    let crlf = |rng: &mut Rng, strict: &mut Strict| -> &'static [u8] {
        if rng.next().is_multiple_of(40) {
            strict.bare_lf = true;
            b"\n"
        } else {
            b"\r\n"
        }
    };
    let end = crlf(rng, &mut strict);
    wire.extend_from_slice(end);
    let mut content_length = 0;
    let mut host = 0;
    for _ in 0..rng.next() % 6 {
        let name = rng.pick(&[
            "X-A",
            "x-b",
            "Host",
            "Content-Length",
            "Transfer-Encoding",
            "Connection",
            "Bad Name",
            "",
            "N\u{e9}",
            "a{b",
        ]);
        let separator = rng.pick(&[":", ": ", ":\t", ":  ", " :", ": \t "]);
        let value: &[u8] = match name {
            "Content-Length" => rng.pick(&[&b"0"[..], b"12", b"+1", b"x", b"1 "]),
            "Connection" => rng.pick(&[
                &b"close"[..],
                b"keep-alive, Upgrade",
                b"te,,close",
                b"content-length",
            ]),
            _ => rng.pick(&[
                &b"v"[..],
                b"",
                b"a b",
                b"a\tb",
                b"tail \t",
                b"\x80\xff",
                b"ctl\x01",
                b"del\x7f",
                b"cr\rx",
            ]),
        };
        match name {
            "Content-Length" => {
                content_length += 1;
                if content_length > 1 || !trim_ows(value).iter().all(u8::is_ascii_digit) {
                    strict.framing = true;
                }
            }
            "Host" => {
                host += 1;
                if host > 1 {
                    strict.framing = true;
                }
            }
            "Transfer-Encoding" => strict.framing = true,
            "Connection" if value == b"content-length" => strict.framing = true,
            _ => {}
        }
        wire.extend_from_slice(name.as_bytes());
        wire.extend_from_slice(separator.as_bytes());
        wire.extend_from_slice(value);
        let end = crlf(rng, &mut strict);
        wire.extend_from_slice(end);
        if rng.next().is_multiple_of(50) {
            // Obsolete line folding; both parsers must reject or agree.
            wire.extend_from_slice(b" folded\r\n");
        }
    }
    let end = crlf(rng, &mut strict);
    wire.extend_from_slice(end);
    (wire, strict)
}

/// Fields accepted by `httparse`, or `None` if it rejects or needs more bytes.
#[allow(clippy::type_complexity)]
fn reference(wire: &[u8]) -> Option<(Option<u16>, Vec<(String, Vec<u8>)>, usize)> {
    let mut slots = [httparse::EMPTY_HEADER; 32];
    let collect = |headers: &[httparse::Header<'_>]| {
        headers
            .iter()
            .map(|h| (h.name.to_owned(), trim_ows(h.value).to_vec()))
            .collect::<Vec<_>>()
    };
    if wire.starts_with(b"HTTP/") {
        let mut response = httparse::Response::new(&mut slots);
        match response.parse(wire) {
            Ok(httparse::Status::Complete(n)) => {
                Some((response.code, collect(response.headers), n))
            }
            _ => None,
        }
    } else {
        let mut request = httparse::Request::new(&mut slots);
        match request.parse(wire) {
            Ok(httparse::Status::Complete(n)) => Some((None, collect(request.headers), n)),
            _ => None,
        }
    }
}

/// Every generated head yields the same verdict and fields, modulo strictness.
#[test]
fn codec_agrees_with_httparse_on_generated_heads() {
    let codec = Codec::<()>::new(8192).trim_ows();
    let mut rng = Rng(0x5eed_1234_abcd_ef01);
    let (mut accepted, mut strict_rejects) = (0, 0);
    for case in 0..20_000 {
        let (wire, strict) = generate(&mut rng);
        let ours = codec.decode_head(&wire);
        let theirs = reference(&wire);
        match (&ours, &theirs) {
            (Ok(Some((head, used))), Some((status, fields, n))) => {
                assert!(!strict.any(), "case {case}: accepted strict head {wire:?}");
                assert_eq!(used, n, "case {case}: {wire:?}");
                if let StartLine::Response { status: ours } = head.start {
                    assert_eq!(Some(ours), *status, "case {case}");
                }
                let ours: Vec<_> = head
                    .headers
                    .iter()
                    .map(|h| (h.name.clone(), h.value.clone()))
                    .collect();
                assert_eq!(&ours, fields, "case {case}: {wire:?}");
                accepted += 1;
            }
            (Ok(Some(_)), None) => panic!("case {case}: codec accepted {wire:?}"),
            (Ok(None), _) => panic!("case {case}: complete head reported incomplete {wire:?}"),
            (Err(_), Some(_)) => {
                assert!(strict.any(), "case {case}: codec rejected {wire:?}");
                strict_rejects += 1;
            }
            (Err(_), None) => {}
        }
    }
    // The corpus must exercise both agreement and intentional strictness.
    assert!(accepted > 1_000, "accepted {accepted}");
    assert!(strict_rejects > 100, "strict rejects {strict_rejects}");
}

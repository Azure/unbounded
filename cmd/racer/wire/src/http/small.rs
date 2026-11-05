//! Bounded decoding of small response bodies into a capped buffer.
//!
//! This is a pure codec: callers feed byte slices as they arrive and report end
//! of stream. It never reads sockets or allocates beyond the configured body
//! cap plus fixed bookkeeping. Three framings are supported:
//!
//! - chunked transfer coding (RFC 9112 section 7.1), with chunk extensions
//!   ignored after a syntax check, trailer fields bounded and discarded, and
//!   overflow-checked hexadecimal sizes;
//! - close-delimited bodies, which end at end of stream;
//! - fixed `Content-Length` bodies.
//!
//! [`Framing::from_response`] is the explicit opt-in for transfer coding. The
//! fixed-length request and response paths keep using
//! [`crate::http::MessageHead::content_length`], which rejects every
//! `Transfer-Encoding` field.

use crate::http::{MessageHead, is_token, trim_ows};

/// Size bounds for one decoded body.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Limits {
    /// Maximum decoded body bytes retained in the buffer.
    pub body: usize,

    /// Maximum bytes in one chunk-size line, including extensions but not CRLF.
    pub line: usize,

    /// Maximum total trailer section bytes, including line endings.
    pub trailers: usize,
}

impl Default for Limits {
    /// Conservative bounds suitable for control-plane sized responses.
    fn default() -> Self {
        Self {
            body: 64 * 1024,
            line: 256,
            trailers: 4 * 1024,
        }
    }
}

/// The message framing selected for a small response body.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Framing {
    /// `Transfer-Encoding: chunked` as the sole coding.
    Chunked,

    /// A body of exactly this many bytes.
    Length(u64),

    /// A body that ends when the peer closes the connection.
    Close,
}

/// Body decoding failures. None of them include body bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The bytes violate the framing grammar.
    Malformed,

    /// A body, chunk line, or trailer section exceeds its limit.
    TooLarge,

    /// End of stream arrived before the framing completed.
    Truncated,
}

impl std::fmt::Display for Error {
    /// Display the stable variant name.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}

impl std::error::Error for Error {}

/// Progress after feeding bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Status {
    /// Every input byte was consumed and more are required.
    Need,

    /// The body is complete; this many input bytes belonged to it and any
    /// remaining bytes are read-ahead that the caller must handle.
    Done(usize),
}

impl Framing {
    /// Select framing for a final response head, opting in to chunked coding.
    ///
    /// The only accepted transfer coding is a single `chunked`; combining it with
    /// `Content-Length` is rejected as a smuggling vector. Without either field
    /// the body is close-delimited.
    pub fn from_response(head: &MessageHead) -> Result<Self, Error> {
        let mut codings = head.values("transfer-encoding");
        if let Some(first) = codings.next() {
            if codings.next().is_some()
                || !trim_ows(first).eq_ignore_ascii_case(b"chunked")
                || head.values("content-length").next().is_some()
            {
                return Err(Error::Malformed);
            }
            return Ok(Self::Chunked);
        }
        match head.content_length() {
            Ok(Some(length)) => Ok(Self::Length(length)),
            Ok(None) => Ok(Self::Close),
            Err(_) => Err(Error::Malformed),
        }
    }
}

/// Chunked decoding position.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Chunk {
    /// Reading hexadecimal size digits.
    Size { value: u64, digits: usize },

    /// Whitespace after the size, before an extension or line end.
    SizeSpace { value: u64 },

    /// Inside chunk extensions, which are checked for control bytes and ignored.
    Extension { value: u64, quoted: bool },

    /// A CR ended the size line; LF must follow.
    SizeLf { value: u64 },

    /// Copying chunk data.
    Data { remaining: u64 },

    /// CR after chunk data.
    DataCr,

    /// LF after chunk data.
    DataLf,

    /// At the start of a trailer line or the final empty line.
    TrailerStart,

    /// Inside a trailer field name.
    TrailerName { length: usize },

    /// Inside a trailer field value.
    TrailerValue,

    /// A CR ended a trailer line; LF must follow.
    TrailerLf,

    /// A CR began the final empty line; LF must follow.
    EndLf,

    /// The chunked body is complete.
    Done,
}

/// Decoder state for the selected framing.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum State {
    /// Chunked transfer coding.
    Chunked {
        /// Grammar position.
        chunk: Chunk,

        /// Bytes in the current size line.
        line: usize,

        /// Bytes in the trailer section so far.
        trailers: usize,
    },

    /// Fixed length with this many bytes outstanding.
    Length(u64),

    /// Close-delimited; complete only at end of stream.
    Close,

    /// A previous error poisoned the decoder.
    Failed(Error),
}

/// A pure incremental small-body decoder with a capped output buffer.
#[derive(Debug)]
pub struct Decoder {
    /// Framing state.
    state: State,

    /// Size limits.
    limits: Limits,

    /// Decoded body bytes.
    body: Vec<u8>,
}

impl Decoder {
    /// Construct a decoder, rejecting fixed lengths that already exceed the cap.
    pub fn new(framing: Framing, limits: Limits) -> Result<Self, Error> {
        let state = match framing {
            Framing::Chunked => State::Chunked {
                chunk: Chunk::Size {
                    value: 0,
                    digits: 0,
                },
                line: 0,
                trailers: 0,
            },
            Framing::Length(length) => {
                if usize::try_from(length).map_or(true, |n| n > limits.body) {
                    return Err(Error::TooLarge);
                }
                State::Length(length)
            }
            Framing::Close => State::Close,
        };
        Ok(Self {
            state,
            limits,
            body: Vec::new(),
        })
    }

    /// Borrow the decoded body bytes so far.
    pub fn body(&self) -> &[u8] {
        &self.body
    }

    /// Take the decoded body bytes.
    pub fn into_body(self) -> Vec<u8> {
        self.body
    }

    /// Take the decoded body bytes so far, leaving the decoder running. The
    /// body cap applies to bytes not yet taken, so a caller streaming an
    /// unbounded body (for example a watch) bounds each undrained window.
    pub fn take_body(&mut self) -> Vec<u8> {
        std::mem::take(&mut self.body)
    }

    /// Whether the framing already completed without needing end of stream.
    pub fn is_done(&self) -> bool {
        matches!(
            self.state,
            State::Length(0)
                | State::Chunked {
                    chunk: Chunk::Done,
                    ..
                }
        )
    }

    /// Feed the next input bytes. After an error, every call returns that error.
    pub fn feed(&mut self, input: &[u8]) -> Result<Status, Error> {
        let result = self.step(input);
        if let Err(error) = result {
            self.state = State::Failed(error);
        }
        result
    }

    /// Report end of stream. Close-delimited bodies complete here; other framings
    /// must already be done or the body is truncated.
    pub fn finish(&mut self) -> Result<(), Error> {
        match self.state {
            State::Failed(error) => Err(error),
            State::Close => Ok(()),
            _ if self.is_done() => Ok(()),
            _ => {
                self.state = State::Failed(Error::Truncated);
                Err(Error::Truncated)
            }
        }
    }

    /// Append bytes, enforcing the body cap.
    fn append(&mut self, bytes: &[u8]) -> Result<(), Error> {
        if bytes.len() > self.limits.body - self.body.len() {
            return Err(Error::TooLarge);
        }
        self.body.extend_from_slice(bytes);
        Ok(())
    }

    /// Advance the state machine over `input`.
    fn step(&mut self, input: &[u8]) -> Result<Status, Error> {
        match self.state {
            State::Failed(error) => Err(error),
            State::Close => {
                self.append(input)?;
                Ok(Status::Need)
            }
            State::Length(remaining) => {
                let take = usize::try_from(remaining)
                    .unwrap_or(usize::MAX)
                    .min(input.len());
                self.append(&input[..take])?;
                let remaining = remaining - take as u64;
                self.state = State::Length(remaining);
                Ok(if remaining == 0 {
                    Status::Done(take)
                } else {
                    Status::Need
                })
            }
            State::Chunked { .. } => self.chunked(input),
        }
    }

    /// Decode chunked coding until done or input is exhausted.
    fn chunked(&mut self, input: &[u8]) -> Result<Status, Error> {
        let State::Chunked {
            mut chunk,
            mut line,
            mut trailers,
        } = self.state
        else {
            return Err(Error::Malformed);
        };
        let mut at = 0;
        while at < input.len() {
            if chunk == Chunk::Done {
                break;
            }
            if let Chunk::Data { remaining } = chunk {
                let take = usize::try_from(remaining)
                    .unwrap_or(usize::MAX)
                    .min(input.len() - at);
                self.append(&input[at..at + take])?;
                at += take;
                let remaining = remaining - take as u64;
                chunk = if remaining == 0 {
                    Chunk::DataCr
                } else {
                    Chunk::Data { remaining }
                };
                continue;
            }
            let byte = input[at];
            at += 1;
            if matches!(
                chunk,
                Chunk::Size { .. } | Chunk::SizeSpace { .. } | Chunk::Extension { .. }
            ) && byte != b'\r'
            {
                line += 1;
                if line > self.limits.line {
                    return Err(Error::TooLarge);
                }
            }
            if matches!(
                chunk,
                Chunk::TrailerStart
                    | Chunk::TrailerName { .. }
                    | Chunk::TrailerValue
                    | Chunk::TrailerLf
                    | Chunk::EndLf
            ) {
                trailers += 1;
                if trailers > self.limits.trailers {
                    return Err(Error::TooLarge);
                }
            }
            chunk = self.chunk_byte(chunk, byte)?;
            if matches!(chunk, Chunk::SizeLf { .. }) {
                line = 0;
            }
        }
        self.state = State::Chunked {
            chunk,
            line,
            trailers,
        };
        Ok(if chunk == Chunk::Done {
            Status::Done(at)
        } else {
            Status::Need
        })
    }

    /// Apply one non-data byte to the chunked grammar.
    fn chunk_byte(&self, chunk: Chunk, byte: u8) -> Result<Chunk, Error> {
        let remaining_cap = (self.limits.body - self.body.len()) as u64;
        Ok(match chunk {
            Chunk::Size { value, digits } => match byte {
                b'0'..=b'9' | b'a'..=b'f' | b'A'..=b'F' => {
                    let digit = (byte as char).to_digit(16).unwrap_or(0) as u64;
                    let value = value
                        .checked_mul(16)
                        .and_then(|n| n.checked_add(digit))
                        .ok_or(Error::TooLarge)?;
                    if value > remaining_cap {
                        return Err(Error::TooLarge);
                    }
                    Chunk::Size {
                        value,
                        digits: digits + 1,
                    }
                }
                _ if digits == 0 => return Err(Error::Malformed),
                b' ' | b'\t' => Chunk::SizeSpace { value },
                b';' => Chunk::Extension {
                    value,
                    quoted: false,
                },
                b'\r' => Chunk::SizeLf { value },
                _ => return Err(Error::Malformed),
            },
            Chunk::SizeSpace { value } => match byte {
                b' ' | b'\t' => chunk,
                b';' => Chunk::Extension {
                    value,
                    quoted: false,
                },
                b'\r' => Chunk::SizeLf { value },
                _ => return Err(Error::Malformed),
            },
            Chunk::Extension { value, quoted } => match byte {
                b'\r' if !quoted => Chunk::SizeLf { value },
                b'"' => Chunk::Extension {
                    value,
                    quoted: !quoted,
                },
                b'\t' => chunk,
                0..=31 | 127 => return Err(Error::Malformed),
                _ => chunk,
            },
            Chunk::SizeLf { value } => match byte {
                b'\n' if value == 0 => Chunk::TrailerStart,
                b'\n' => Chunk::Data { remaining: value },
                _ => return Err(Error::Malformed),
            },
            Chunk::Data { .. } => return Err(Error::Malformed),
            Chunk::DataCr => match byte {
                b'\r' => Chunk::DataLf,
                _ => return Err(Error::Malformed),
            },
            Chunk::DataLf => match byte {
                b'\n' => Chunk::Size {
                    value: 0,
                    digits: 0,
                },
                _ => return Err(Error::Malformed),
            },
            Chunk::TrailerStart => match byte {
                b'\r' => Chunk::EndLf,
                _ if is_token(byte) => Chunk::TrailerName { length: 1 },
                _ => return Err(Error::Malformed),
            },
            Chunk::TrailerName { length } => match byte {
                b':' => Chunk::TrailerValue,
                _ if is_token(byte) => Chunk::TrailerName { length: length + 1 },
                _ => return Err(Error::Malformed),
            },
            Chunk::TrailerValue => match byte {
                b'\r' => Chunk::TrailerLf,
                b'\t' => chunk,
                0..=31 | 127 => return Err(Error::Malformed),
                _ => chunk,
            },
            Chunk::TrailerLf | Chunk::EndLf => match byte {
                b'\n' if chunk == Chunk::EndLf => Chunk::Done,
                b'\n' => Chunk::TrailerStart,
                _ => return Err(Error::Malformed),
            },
            Chunk::Done => return Err(Error::Malformed),
        })
    }
}

/// Framing selection, grammar, limits, and fragmentation contracts.
#[cfg(test)]
mod tests {
    use super::*;
    use crate::http::Codec;

    /// Small limits that keep edge cases short.
    const LIMITS: Limits = Limits {
        body: 16,
        line: 12,
        trailers: 24,
    };

    /// Decode `wire` in one call and finish, returning the body and consumed length.
    fn whole(framing: Framing, wire: &[u8]) -> Result<(Vec<u8>, usize), Error> {
        let mut decoder = Decoder::new(framing, LIMITS)?;
        let used = match decoder.feed(wire)? {
            Status::Done(used) => used,
            Status::Need => wire.len(),
        };
        decoder.finish()?;
        Ok((decoder.into_body(), used))
    }

    /// Decode `wire` split at `at`, returning the body and total consumed length.
    fn split(framing: Framing, wire: &[u8], at: usize) -> Result<(Vec<u8>, usize), Error> {
        let mut decoder = Decoder::new(framing, LIMITS)?;
        let mut used = 0;
        for part in [&wire[..at], &wire[at..]] {
            if decoder.is_done() {
                break;
            }
            match decoder.feed(part)? {
                Status::Done(n) => {
                    used += n;
                    break;
                }
                Status::Need => used += part.len(),
            }
        }
        decoder.finish()?;
        Ok((decoder.into_body(), used))
    }

    /// Taking the body streams an unbounded chunked body in capped windows.
    #[test]
    fn take_body_streams_in_capped_windows() {
        let mut decoder = Decoder::new(Framing::Chunked, LIMITS).unwrap();
        let mut seen = Vec::new();
        for _ in 0..4 {
            assert_eq!(decoder.feed(b"c\r\nabcdefghijkl\r\n"), Ok(Status::Need));
            seen.extend(decoder.take_body());
            assert!(decoder.body().is_empty());
        }
        assert_eq!(seen.len(), 48);
        assert_eq!(decoder.feed(b"0\r\n\r\n"), Ok(Status::Done(5)));
        assert!(decoder.take_body().is_empty());
        // An undrained window still hits the cap.
        let mut decoder = Decoder::new(Framing::Chunked, LIMITS).unwrap();
        assert_eq!(decoder.feed(b"c\r\nabcdefghijkl\r\n"), Ok(Status::Need));
        assert_eq!(decoder.feed(b"c\r\n"), Err(Error::TooLarge));
    }

    /// Valid chunked bodies decode identically at every split and stop at the end.
    #[test]
    fn chunked_bodies_decode_at_every_split() {
        for (wire, body) in [
            (&b"0\r\n\r\n"[..], &b""[..]),
            (b"3\r\nabc\r\n0\r\n\r\n", b"abc"),
            (
                b"3;x=y\r\nabc\r\n2 ; q=\"a;b\"\r\nde\r\n0\r\n\r\n",
                b"abcde",
            ),
            (
                b"A\r\n0123456789\r\n0\r\nX-T: v\r\nY:\r\n\r\n",
                b"0123456789",
            ),
            (
                b"00010\r\n0123456789abcdef\r\n000\r\n\r\n",
                b"0123456789abcdef",
            ),
            (b"1\t\r\nz\r\n0 \r\n\r\n", b"z"),
        ] {
            let mut extended = wire.to_vec();
            extended.extend_from_slice(b"NEXT");
            assert_eq!(
                whole(Framing::Chunked, &extended),
                Ok((body.to_vec(), wire.len()))
            );
            for at in 0..=wire.len() {
                assert_eq!(
                    split(Framing::Chunked, &extended, at),
                    Ok((body.to_vec(), wire.len())),
                    "split {at} of {wire:?}"
                );
            }
            let mut decoder = Decoder::new(Framing::Chunked, LIMITS).unwrap();
            for (i, byte) in wire.iter().enumerate() {
                let status = decoder.feed(std::slice::from_ref(byte)).unwrap();
                let expected = if i + 1 == wire.len() {
                    Status::Done(1)
                } else {
                    Status::Need
                };
                assert_eq!(status, expected);
            }
            assert_eq!(decoder.feed(b"more"), Ok(Status::Done(0)));
            assert_eq!(decoder.body(), body);
        }
    }

    /// Malformed, oversized, and truncated chunked bodies fail with stable errors.
    #[test]
    fn chunked_failures_are_classified() {
        for (wire, error) in [
            (&b"\r\n"[..], Error::Malformed),
            (b"g\r\n", Error::Malformed),
            (b";x\r\n", Error::Malformed),
            (b"-1\r\n", Error::Malformed),
            (b"0x1\r\n", Error::Malformed),
            (b"1\nx\r\n0\r\n\r\n", Error::Malformed),
            (b"1\r\nxy\r\n0\r\n\r\n", Error::Malformed),
            (b"1\r\nx\n0\r\n\r\n", Error::Malformed),
            (b"1 2\r\n", Error::Malformed),
            (b"1;\x01\r\n", Error::Malformed),
            (b"0\r\n\n", Error::Malformed),
            (b"0\r\nbad field\r\n\r\n", Error::Malformed),
            (b"0\r\n: v\r\n\r\n", Error::Malformed),
            (b"0\r\nX: \x00\r\n\r\n", Error::Malformed),
            (b"0\r\nX: v\n\r\n", Error::Malformed),
            (b"11\r\n", Error::TooLarge),
            (b"10\r\n0123456789abcdef\r\n1\r\nx\r\n", Error::TooLarge),
            (b"ffffffffffffffffff\r\n", Error::TooLarge),
            (b"0000000000001\r\n", Error::TooLarge),
            (b"1;aaaaaaaaaaaaa\r\n", Error::TooLarge),
            (
                b"0\r\nX: aaaaaaaaaaaaaaaaaaaaaaaaaa\r\n\r\n",
                Error::TooLarge,
            ),
            (b"", Error::Truncated),
            (b"3\r\nab", Error::Truncated),
            (b"3\r\nabc\r\n", Error::Truncated),
            (b"0\r\n", Error::Truncated),
            (b"0\r\nX: v\r\n", Error::Truncated),
        ] {
            assert_eq!(whole(Framing::Chunked, wire), Err(error), "{wire:?}");
            for at in 0..=wire.len() {
                assert_eq!(
                    split(Framing::Chunked, wire, at),
                    Err(error),
                    "{wire:?} at {at}"
                );
            }
        }
    }

    /// A failed decoder stays failed.
    #[test]
    fn errors_poison_the_decoder() {
        let mut decoder = Decoder::new(Framing::Chunked, LIMITS).unwrap();
        assert_eq!(decoder.feed(b"z"), Err(Error::Malformed));
        assert_eq!(decoder.feed(b"0\r\n\r\n"), Err(Error::Malformed));
        assert_eq!(decoder.finish(), Err(Error::Malformed));
        let mut decoder = Decoder::new(Framing::Length(2), LIMITS).unwrap();
        assert_eq!(decoder.finish(), Err(Error::Truncated));
        assert_eq!(decoder.feed(b"ab"), Err(Error::Truncated));
    }

    /// Close-delimited bodies end at end of stream and respect the cap.
    #[test]
    fn close_delimited_bodies_are_capped() {
        assert_eq!(whole(Framing::Close, b""), Ok((Vec::new(), 0)));
        assert_eq!(
            whole(Framing::Close, b"0123456789abcdef"),
            Ok((b"0123456789abcdef".to_vec(), 16))
        );
        assert_eq!(
            whole(Framing::Close, b"0123456789abcdefg"),
            Err(Error::TooLarge)
        );
        let mut decoder = Decoder::new(Framing::Close, LIMITS).unwrap();
        for _ in 0..16 {
            assert_eq!(decoder.feed(b"x"), Ok(Status::Need));
        }
        assert!(!decoder.is_done());
        assert_eq!(decoder.feed(b"x"), Err(Error::TooLarge));
    }

    /// Fixed lengths stop at the declared length and reject oversized declarations.
    #[test]
    fn fixed_lengths_stop_and_cap() {
        assert_eq!(whole(Framing::Length(0), b"rest"), Ok((Vec::new(), 0)));
        for at in 0..=6 {
            assert_eq!(
                split(Framing::Length(3), b"abcdef", at),
                Ok((b"abc".to_vec(), 3))
            );
        }
        assert_eq!(whole(Framing::Length(3), b"ab"), Err(Error::Truncated));
        assert_eq!(
            Decoder::new(Framing::Length(17), LIMITS).err(),
            Some(Error::TooLarge)
        );
        assert_eq!(
            Decoder::new(Framing::Length(u64::MAX), LIMITS).err(),
            Some(Error::TooLarge)
        );
    }

    /// Framing selection opts in to chunked coding only for a sole `chunked`.
    #[test]
    fn framing_selection_from_response_heads() {
        let codec = Codec::<()>::new(1024);
        let pick = |fields: &str| {
            // The codec rejects transfer coding, so build the head by hand.
            let mut headers = Vec::new();
            for line in fields.split("\r\n").filter(|l| !l.is_empty()) {
                let (name, value) = line.split_once(':').unwrap();
                headers.push(crate::http::Header::new(name, value.trim()));
            }
            Framing::from_response(&MessageHead {
                start: crate::http::StartLine::Response { status: 200 },
                headers,
            })
        };
        assert_eq!(pick("Transfer-Encoding: chunked\r\n"), Ok(Framing::Chunked));
        assert_eq!(
            pick("transfer-encoding:  CHUNKED \r\n"),
            Ok(Framing::Chunked)
        );
        assert_eq!(pick("Content-Length: 5\r\n"), Ok(Framing::Length(5)));
        assert_eq!(pick(""), Ok(Framing::Close));
        for fields in [
            "Transfer-Encoding: gzip, chunked\r\n",
            "Transfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n",
            "Transfer-Encoding: chunked\r\nContent-Length: 1\r\n",
            "Transfer-Encoding: identity\r\n",
            "Content-Length: 1\r\nContent-Length: 1\r\n",
            "Content-Length: x\r\n",
        ] {
            assert_eq!(pick(fields), Err(Error::Malformed), "{fields:?}");
        }
        // Existing fixed-length paths still reject transfer coding.
        assert!(
            codec
                .decode_head(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
                .is_err()
        );
    }
}

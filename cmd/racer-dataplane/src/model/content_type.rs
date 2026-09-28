//! Bounded ASCII MIME metadata, separate from HTTP transport framing.
use crate::error::{Error, Result};

pub const CONTENT_TYPE_HEADER: &str = "Racer-Content-Type";
pub const MAX_CONTENT_TYPE_BYTES: usize = 256;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ContentType(String);

impl ContentType {
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        if bytes.is_empty()
            || bytes.len() > MAX_CONTENT_TYPE_BYTES
            || bytes.iter().any(|b| !(0x20..=0x7e).contains(b))
            || bytes.first() == Some(&b' ')
            || bytes.last() == Some(&b' ')
        {
            return Err(Error::InvalidRequest);
        }
        let mut rest = bytes;
        token(&mut rest)?;
        consume(&mut rest, b'/')?;
        token(&mut rest)?;
        let mut parameters: Vec<&[u8]> = Vec::new();
        while !rest.is_empty() {
            spaces(&mut rest);
            consume(&mut rest, b';')?;
            spaces(&mut rest);
            let name = token(&mut rest)?;
            if parameters.iter().any(|old| old.eq_ignore_ascii_case(name)) {
                return Err(Error::InvalidRequest);
            }
            parameters.push(name);
            spaces(&mut rest);
            consume(&mut rest, b'=')?;
            spaces(&mut rest);
            if rest.first() == Some(&b'"') {
                rest = &rest[1..];
                loop {
                    let byte = *rest.first().ok_or(Error::InvalidRequest)?;
                    rest = &rest[1..];
                    match byte {
                        b'"' => break,
                        b'\\' => {
                            rest = rest.get(1..).ok_or(Error::InvalidRequest)?;
                        }
                        _ => {}
                    }
                }
            } else {
                token(&mut rest)?;
            }
        }
        Ok(Self(
            String::from_utf8(bytes.to_vec()).map_err(|_| Error::InvalidRequest)?,
        ))
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
    pub fn as_bytes(&self) -> &[u8] {
        self.0.as_bytes()
    }
}
fn spaces(rest: &mut &[u8]) {
    while rest.first() == Some(&b' ') {
        *rest = &rest[1..];
    }
}
fn consume(rest: &mut &[u8], byte: u8) -> Result<()> {
    if rest.first() != Some(&byte) {
        return Err(Error::InvalidRequest);
    }
    *rest = &rest[1..];
    Ok(())
}
fn token<'a>(rest: &mut &'a [u8]) -> Result<&'a [u8]> {
    let length = rest
        .iter()
        .take_while(|b| b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(b))
        .count();
    if length == 0 {
        return Err(Error::InvalidRequest);
    }
    let value = &rest[..length];
    *rest = &rest[length..];
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn bounded_mime_values_preserve_bytes_and_reject_malformed_metadata() {
        for value in [
            "application/vnd.oci.image.manifest.v1+json",
            "text/plain; charset=utf-8",
            "text/plain; x=\"a;b\\\"c\"",
        ] {
            assert_eq!(
                ContentType::parse(value.as_bytes()).unwrap().as_str(),
                value
            );
        }
        for value in [
            "",
            "text",
            "text/",
            "/plain",
            " text/plain",
            "text/plain ",
            "text/plain, text/html",
            "text/plain;",
            "text/plain;x",
            "text/plain;x=",
            "text/plain;x=\"",
            "text/plain;x=a;X=b",
            "text/plain\r\nx:y",
            "text/\tplain",
            "text/pläin",
        ] {
            assert!(ContentType::parse(value.as_bytes()).is_err(), "{value:?}");
        }
        assert!(ContentType::parse(format!("a/{}", "b".repeat(254)).as_bytes()).is_ok());
        assert!(ContentType::parse(format!("a/{}", "b".repeat(255)).as_bytes()).is_err());
    }
}

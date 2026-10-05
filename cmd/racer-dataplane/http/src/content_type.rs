use crate::{Error, is_token};
type Result<T> = std::result::Result<T, Error>;

/// Byte-preserving ASCII MIME value with unique case-insensitive parameter names.
/// No tabs, edge spaces, lists, or non-ASCII bytes. Callers impose field-size caps.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ContentType(String);

impl ContentType {
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        if bytes.is_empty()
            || bytes.iter().any(|b| !(0x20..=0x7e).contains(b))
            || bytes.first() == Some(&b' ')
            || bytes.last() == Some(&b' ')
        {
            return Err(Error::Malformed);
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
                return Err(Error::Malformed);
            }
            parameters.push(name);
            spaces(&mut rest);
            consume(&mut rest, b'=')?;
            spaces(&mut rest);
            if rest.first() == Some(&b'"') {
                rest = &rest[1..];
                loop {
                    let byte = *rest.first().ok_or(Error::Malformed)?;
                    rest = &rest[1..];
                    match byte {
                        b'"' => break,
                        b'\\' => {
                            rest = rest.get(1..).ok_or(Error::Malformed)?;
                        }
                        _ => {}
                    }
                }
            } else {
                token(&mut rest)?;
            }
        }
        Ok(Self(
            String::from_utf8(bytes.to_vec()).map_err(|_| Error::Malformed)?,
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
        return Err(Error::Malformed);
    }
    *rest = &rest[1..];
    Ok(())
}
fn token<'a>(rest: &mut &'a [u8]) -> Result<&'a [u8]> {
    let length = rest.iter().take_while(|b| is_token(**b)).count();
    if length == 0 {
        return Err(Error::Malformed);
    }
    let value = &rest[..length];
    *rest = &rest[length..];
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn mime_preserves_case_spaces_quotes_and_escapes() {
        for value in [
            "Text/Plain",
            "text/plain ; Charset = utf-8",
            "text/plain;x=\"\"",
            "text/plain;x=\"a;b\\\"c\";y=z",
            "text/plain;x=\" \\\\ \"",
        ] {
            let parsed = ContentType::parse(value.as_bytes()).unwrap();
            assert_eq!(parsed.as_str(), value);
            assert_eq!(parsed.as_bytes(), value.as_bytes());
        }
        assert!(ContentType::parse(format!("a/{}", "b".repeat(300)).as_bytes()).is_ok());
    }
    #[test]
    fn malformed_mime_is_rejected_without_normalization() {
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
            "text/plain;x=\"\\",
            "text/plain;x=a;X=b",
            "text/plain\r\nx:y",
            "text/\tplain",
            "text/pläin",
            "text/plain;x=\"a\"junk",
        ] {
            assert_eq!(
                ContentType::parse(value.as_bytes()),
                Err(Error::Malformed),
                "{value:?}"
            );
        }
    }
}

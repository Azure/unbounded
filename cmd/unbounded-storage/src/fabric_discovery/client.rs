// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::io::{self, Read, Write};
use std::net::{IpAddr, SocketAddr, TcpStream};
use std::time::Duration;

use super::{MAX_RESPONSE_BYTES, PATH};

const IO_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_RESPONSE_HEADER_BYTES: usize = 8 * 1024;
const MAX_HEADERS: usize = 64;

/// Fetch fabric endpoint candidates from a discovery server.
pub fn fetch(discovery_addr: SocketAddr) -> io::Result<Vec<String>> {
    let mut stream = TcpStream::connect_timeout(&discovery_addr, IO_TIMEOUT)?;
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    stream.set_write_timeout(Some(IO_TIMEOUT))?;

    let host = format_host(discovery_addr);
    let request = format!("GET {PATH} HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n\r\n");
    stream.write_all(request.as_bytes())?;

    let (head, mut body) = read_response_head(&mut stream)?;
    let response = parse_response_head(&head)?;
    if response.status != 200 {
        return Err(invalid_data(format!(
            "fabric discovery returned HTTP {}",
            response.status
        )));
    }
    if response.chunked {
        return Err(invalid_data(
            "fabric discovery does not support chunked responses",
        ));
    }
    if let Some(length) = response.content_length {
        if length > MAX_RESPONSE_BYTES {
            return Err(invalid_data("fabric discovery response exceeds 256 KiB"));
        }
        if body.len() > length {
            return Err(invalid_data(
                "fabric discovery response exceeds Content-Length",
            ));
        }
        while body.len() < length {
            if !read_bounded(&mut stream, &mut body, length)? {
                return Err(invalid_data(
                    "fabric discovery response ended before Content-Length",
                ));
            }
        }
    } else {
        while body.len() <= MAX_RESPONSE_BYTES {
            if !read_bounded(&mut stream, &mut body, MAX_RESPONSE_BYTES + 1)? {
                break;
            }
        }
        if body.len() > MAX_RESPONSE_BYTES {
            return Err(invalid_data("fabric discovery response exceeds 256 KiB"));
        }
    }

    parse_candidates(body)
}

fn format_host(addr: SocketAddr) -> String {
    match addr.ip() {
        IpAddr::V4(ip) => format!("{ip}:{}", addr.port()),
        IpAddr::V6(ip) => format!("[{ip}]:{}", addr.port()),
    }
}

struct ResponseHead {
    status: u16,
    content_length: Option<usize>,
    chunked: bool,
}

fn read_response_head(stream: &mut TcpStream) -> io::Result<(Vec<u8>, Vec<u8>)> {
    let mut bytes = Vec::with_capacity(1024);
    loop {
        if let Some(end) = bytes.windows(4).position(|window| window == b"\r\n\r\n") {
            let body = bytes.split_off(end + 4);
            return Ok((bytes, body));
        }
        if bytes.len() == MAX_RESPONSE_HEADER_BYTES {
            return Err(invalid_data(
                "fabric discovery response header exceeds 8 KiB",
            ));
        }

        let mut chunk = [0_u8; 1024];
        let available = (MAX_RESPONSE_HEADER_BYTES - bytes.len()).min(chunk.len());
        let read = stream.read(&mut chunk[..available])?;
        if read == 0 {
            return Err(invalid_data("incomplete fabric discovery response"));
        }
        bytes.extend_from_slice(&chunk[..read]);
    }
}

fn parse_response_head(bytes: &[u8]) -> io::Result<ResponseHead> {
    let mut headers = [httparse::EMPTY_HEADER; MAX_HEADERS];
    let mut response = httparse::Response::new(&mut headers);
    if !matches!(response.parse(bytes), Ok(httparse::Status::Complete(_))) {
        return Err(invalid_data("invalid fabric discovery response"));
    }

    let status = response
        .code
        .ok_or_else(|| invalid_data("fabric discovery response has no status"))?;
    let mut content_length = None;
    let mut chunked = false;
    for header in response.headers.iter() {
        if header.name.eq_ignore_ascii_case("content-length") {
            if content_length.is_some() {
                return Err(invalid_data(
                    "fabric discovery response has duplicate Content-Length",
                ));
            }
            let value = std::str::from_utf8(header.value)
                .map_err(|_| invalid_data("invalid fabric discovery Content-Length"))?;
            content_length = Some(
                value
                    .trim()
                    .parse::<usize>()
                    .map_err(|_| invalid_data("invalid fabric discovery Content-Length"))?,
            );
        } else if header.name.eq_ignore_ascii_case("transfer-encoding") {
            chunked = true;
        }
    }
    Ok(ResponseHead {
        status,
        content_length,
        chunked,
    })
}

fn read_bounded(stream: &mut TcpStream, bytes: &mut Vec<u8>, limit: usize) -> io::Result<bool> {
    let mut chunk = [0_u8; 8192];
    let available = (limit - bytes.len()).min(chunk.len());
    if available == 0 {
        return Ok(false);
    }
    let read = stream.read(&mut chunk[..available])?;
    bytes.extend_from_slice(&chunk[..read]);
    Ok(read != 0)
}

fn parse_candidates(bytes: Vec<u8>) -> io::Result<Vec<String>> {
    let text = String::from_utf8(bytes)
        .map_err(|_| invalid_data("fabric discovery response is not UTF-8"))?;
    if text.is_empty() {
        return Ok(Vec::new());
    }

    let mut lines: Vec<&str> = text.split('\n').collect();
    if lines.last() == Some(&"") {
        lines.pop();
    }
    if lines
        .iter()
        .any(|line| line.is_empty() || line.contains('\r'))
    {
        return Err(invalid_data(
            "fabric discovery response contains an empty or invalid line",
        ));
    }
    Ok(lines.into_iter().map(str::to_owned).collect())
}

fn invalid_data(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn host_header_formats_both_ip_families() {
        assert_eq!(
            format_host("192.0.2.1:8080".parse().unwrap()),
            "192.0.2.1:8080"
        );
        assert_eq!(format_host("[::1]:8080".parse().unwrap()), "[::1]:8080");
    }

    #[test]
    fn candidate_parser_rejects_empty_lines_and_invalid_utf8() {
        assert!(parse_candidates(b"one\n\ntwo\n".to_vec()).is_err());
        assert!(parse_candidates(vec![0xff]).is_err());
        assert_eq!(
            parse_candidates(b"one\ntwo\n".to_vec()).unwrap(),
            vec!["one", "two"]
        );
    }
}

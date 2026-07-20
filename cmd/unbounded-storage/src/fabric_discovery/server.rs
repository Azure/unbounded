// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::io::{self, Read, Write};
use std::net::{SocketAddr, TcpListener, TcpStream};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::Duration;

use super::{MAX_REQUEST_BYTES, MAX_RESPONSE_BYTES, PATH};

const IO_TIMEOUT: Duration = Duration::from_secs(5);
const SHUTDOWN_POLL: Duration = Duration::from_millis(100);
const MAX_HEADERS: usize = 64;

/// A dedicated blocking HTTP server for fabric endpoint discovery.
pub struct Server {
    listener: TcpListener,
    body: Vec<u8>,
}

impl Server {
    /// Bind a discovery server and prepare a deterministically sorted response.
    pub fn bind(
        addr: SocketAddr,
        candidates: impl IntoIterator<Item = String>,
    ) -> io::Result<Self> {
        let mut candidates: Vec<String> = candidates.into_iter().collect();
        if candidates
            .iter()
            .any(|candidate| candidate.is_empty() || candidate.contains(['\r', '\n']))
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "fabric discovery candidates must be non-empty single lines",
            ));
        }
        candidates.sort_unstable();

        let mut body = candidates.join("\n").into_bytes();
        if !body.is_empty() {
            body.push(b'\n');
        }
        if body.len() > MAX_RESPONSE_BYTES {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "fabric discovery response exceeds 256 KiB",
            ));
        }

        let listener = TcpListener::bind(addr)?;
        listener.set_nonblocking(true)?;
        Ok(Self { listener, body })
    }

    /// Return the address selected by the operating system when binding.
    pub fn local_addr(&self) -> io::Result<SocketAddr> {
        self.listener.local_addr()
    }

    /// Serve requests until `shutdown` is set, polling it between accepts.
    pub fn serve(self, shutdown: Arc<AtomicBool>) -> io::Result<()> {
        while !shutdown.load(Ordering::Acquire) {
            match self.listener.accept() {
                Ok((stream, _)) => {
                    let _ = handle_connection(stream, &self.body);
                }
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                    thread::sleep(SHUTDOWN_POLL);
                }
                Err(error) => return Err(error),
            }
        }
        Ok(())
    }
}

fn handle_connection(mut stream: TcpStream, body: &[u8]) -> io::Result<()> {
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    stream.set_write_timeout(Some(IO_TIMEOUT))?;

    let request = match read_request(&mut stream) {
        Ok(request) => request,
        Err(error) => {
            let status = if error.kind() == io::ErrorKind::InvalidData {
                "400 Bad Request"
            } else {
                return Err(error);
            };
            return write_response(&mut stream, status, b"");
        }
    };

    let (status, response_body) = if request.method != "GET" {
        ("405 Method Not Allowed", &[][..])
    } else if request.path != PATH {
        ("404 Not Found", &[][..])
    } else {
        ("200 OK", body)
    };
    write_response(&mut stream, status, response_body)
}

struct Request {
    method: String,
    path: String,
}

fn read_request(stream: &mut TcpStream) -> io::Result<Request> {
    let mut bytes = Vec::with_capacity(1024);
    loop {
        if bytes.len() == MAX_REQUEST_BYTES {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "fabric discovery request exceeds 8 KiB",
            ));
        }

        let mut chunk = [0_u8; 1024];
        let available = (MAX_REQUEST_BYTES - bytes.len()).min(chunk.len());
        let read = stream.read(&mut chunk[..available])?;
        if read == 0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "incomplete fabric discovery request",
            ));
        }
        bytes.extend_from_slice(&chunk[..read]);

        let mut headers = [httparse::EMPTY_HEADER; MAX_HEADERS];
        let mut request = httparse::Request::new(&mut headers);
        match request.parse(&bytes) {
            Ok(httparse::Status::Complete(_)) => {
                let method = request.method.ok_or_else(invalid_request)?;
                let path = request.path.ok_or_else(invalid_request)?;
                return Ok(Request {
                    method: method.to_owned(),
                    path: path.to_owned(),
                });
            }
            Ok(httparse::Status::Partial) => {}
            Err(_) => return Err(invalid_request()),
        }
    }
}

fn invalid_request() -> io::Error {
    io::Error::new(
        io::ErrorKind::InvalidData,
        "invalid fabric discovery request",
    )
}

fn write_response(stream: &mut TcpStream, status: &str, body: &[u8]) -> io::Result<()> {
    let head = format!(
        "HTTP/1.1 {status}\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    stream.write_all(head.as_bytes())?;
    stream.write_all(body)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bind_rejects_multiline_candidates() {
        let error = Server::bind(
            "127.0.0.1:0".parse().unwrap(),
            ["node-a:1\nnode-b:2".to_owned()],
        )
        .err()
        .unwrap();
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
    }
}

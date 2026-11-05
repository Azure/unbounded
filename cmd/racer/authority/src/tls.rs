//! Userspace TLS (rustls) over reactor sockets, for clients and servers.

use crate::rt::Tcp;
use rustls::pki_types::ServerName;
use std::{
    io::{self, Read, Write},
    sync::Arc,
};

/// TLS over a nonblocking TCP stream.
pub struct TlsStream {
    tcp: Tcp,
    conn: rustls::Connection,
    /// Received ciphertext not yet taken by rustls.
    rx: Vec<u8>,
}

fn invalid<E>(_: E) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, "tls")
}

impl TlsStream {
    /// Connect TLS as a client and finish the handshake.
    pub async fn client(
        tcp: Tcp,
        config: Arc<rustls::ClientConfig>,
        name: ServerName<'static>,
    ) -> io::Result<Self> {
        let conn = rustls::ClientConnection::new(config, name).map_err(invalid)?;
        let mut s = Self {
            tcp,
            conn: conn.into(),
            rx: Vec::new(),
        };
        s.handshake().await?;
        Ok(s)
    }

    /// Accept TLS as a server and finish the handshake.
    pub async fn server(tcp: Tcp, config: Arc<rustls::ServerConfig>) -> io::Result<Self> {
        let conn = rustls::ServerConnection::new(config).map_err(invalid)?;
        let mut s = Self {
            tcp,
            conn: conn.into(),
            rx: Vec::new(),
        };
        s.handshake().await?;
        Ok(s)
    }

    async fn handshake(&mut self) -> io::Result<()> {
        while self.conn.is_handshaking() {
            self.flush().await?;
            if self.conn.is_handshaking() && self.conn.wants_read() && self.read_tls().await? == 0 {
                return Err(io::ErrorKind::UnexpectedEof.into());
            }
        }
        self.flush().await
    }

    /// DER chain sent by the peer, if any.
    pub fn peer_certificates(&self) -> Vec<Vec<u8>> {
        self.conn
            .peer_certificates()
            .map(|c| c.iter().map(|c| c.to_vec()).collect())
            .unwrap_or_default()
    }

    async fn read_tls(&mut self) -> io::Result<usize> {
        if self.rx.is_empty() && self.tcp.recv_into(&mut self.rx).await? == 0 {
            return Ok(0);
        }
        let mut slice = &self.rx[..];
        let n = self.conn.read_tls(&mut slice)?;
        self.rx.drain(..n);
        if let Err(e) = self.conn.process_new_packets() {
            let _ = self.flush().await;
            return Err(invalid(e));
        }
        // Buffered ciphertext counts as progress even if rustls took none.
        Ok(n.max(1))
    }

    /// Write all pending TLS records.
    pub async fn flush(&mut self) -> io::Result<()> {
        while self.conn.wants_write() {
            let mut out = Vec::new();
            while self.conn.wants_write() {
                self.conn.write_tls(&mut out)?;
            }
            self.tcp.send_all(out).await?;
        }
        Ok(())
    }

    /// Read plaintext. Returns 0 at a clean end of stream.
    pub async fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        loop {
            match self.conn.reader().read(buf) {
                Ok(n) => return Ok(n),
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {}
                Err(e) if e.kind() == io::ErrorKind::UnexpectedEof => return Ok(0),
                Err(e) => return Err(e),
            }
            self.flush().await?;
            if self.read_tls().await? == 0 {
                return Ok(0);
            }
        }
    }

    /// Write and flush all plaintext.
    pub async fn write_all(&mut self, mut data: &[u8]) -> io::Result<()> {
        while !data.is_empty() {
            let n = self
                .conn
                .writer()
                .write(&data[..data.len().min(64 * 1024)])?;
            data = &data[n..];
            self.flush().await?;
        }
        self.flush().await
    }

    /// Send close_notify. Errors are ignored.
    pub async fn close(&mut self) {
        self.conn.send_close_notify();
        let _ = self.flush().await;
    }
}

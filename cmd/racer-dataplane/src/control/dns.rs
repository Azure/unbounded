//! Bounded UDP DNS on the control reactor. TLS, never DNS, authenticates the host.
use super::transport::ControlIo;
use crate::{
    error::{Error, Result},
    runtime::deadline::RequestScope,
};
use std::{
    net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr, UdpSocket},
    os::fd::OwnedFd,
    rc::Rc,
    time::{Duration, Instant},
};

pub(super) async fn resolve(
    io: &dyn ControlIo,
    host: &str,
    port: u16,
    scope: &RequestScope,
) -> Result<Vec<SocketAddr>> {
    scope.check()?;
    // Bounded synchronous configuration reads; DNS network operations never block.
    let config = super::files::read_path(std::path::Path::new("/etc/resolv.conf"), 64 * 1024)?;
    let text = std::str::from_utf8(&config).map_err(|_| Error::InvalidConfiguration)?;
    let mut servers = Vec::new();
    let mut search = Vec::new();
    let mut ndots = 1;
    for line in text.lines() {
        let mut fields = line
            .split(['#', ';'])
            .next()
            .unwrap_or("")
            .split_whitespace();
        match fields.next() {
            Some("nameserver") => {
                if let Some(ip) = fields.next().and_then(|s| s.parse::<IpAddr>().ok()) {
                    servers.push(SocketAddr::new(ip, 53));
                }
            }
            Some("search" | "domain") => search = fields.take(6).map(str::to_owned).collect(),
            Some("options") => {
                for option in fields {
                    if let Some(n) = option
                        .strip_prefix("ndots:")
                        .and_then(|s| s.parse::<usize>().ok())
                    {
                        ndots = n.min(15);
                    }
                }
            }
            _ => (),
        }
    }
    servers.truncate(3);
    if servers.is_empty() {
        return Err(Error::Unavailable);
    }
    let mut names = Vec::new();
    let absolute_first =
        host.ends_with('.') || host.bytes().filter(|b| *b == b'.').count() >= ndots;
    if absolute_first {
        names.push(host.trim_end_matches('.').to_owned());
    }
    if !host.ends_with('.') {
        for suffix in search {
            names.push(format!("{host}.{suffix}"));
        }
    }
    if !absolute_first {
        names.push(host.to_owned());
    }
    for name in names {
        for server in &servers {
            let mut addresses = Vec::new();
            for kind in [1u16, 28] {
                let mut attempt = scope.clone();
                attempt.deadline.0 = attempt
                    .deadline
                    .0
                    .min(Instant::now() + Duration::from_secs(2));
                match query(io, *server, &name, kind, &attempt).await {
                    Ok(ips) => {
                        addresses.extend(ips.into_iter().map(|ip| SocketAddr::new(ip, port)))
                    }
                    Err(Error::Cancelled) => return Err(Error::Cancelled),
                    Err(_) => {
                        scope.check()?;
                    }
                }
            }
            if !addresses.is_empty() {
                addresses.truncate(64);
                return Ok(addresses);
            }
        }
    }
    Err(Error::Unavailable)
}
async fn query(
    io: &dyn ControlIo,
    server: SocketAddr,
    name: &str,
    kind: u16,
    scope: &RequestScope,
) -> Result<Vec<IpAddr>> {
    let mut id = [0; 2];
    getrandom::getrandom(&mut id).map_err(|_| Error::Io)?;
    let mut request = vec![id[0], id[1], 1, 0, 0, 1, 0, 0, 0, 0, 0, 0];
    if name.len() > 253 {
        return Err(Error::InvalidConfiguration);
    }
    for label in name.split('.') {
        if label.is_empty() || label.len() > 63 || !label.is_ascii() {
            return Err(Error::InvalidConfiguration);
        }
        request.push(label.len() as u8);
        request.extend_from_slice(label.as_bytes());
    }
    request.push(0);
    request.extend_from_slice(&kind.to_be_bytes());
    request.extend_from_slice(&[0, 1]);
    let socket = UdpSocket::bind(if server.is_ipv4() {
        "0.0.0.0:0"
    } else {
        "[::]:0"
    })
    .map_err(|_| Error::Io)?;
    socket.set_nonblocking(true).map_err(|_| Error::Io)?;
    socket.connect(server).map_err(|_| Error::Io)?;
    let fd = Rc::new(OwnedFd::from(socket.try_clone().map_err(|_| Error::Io)?));
    loop {
        scope.check()?;
        match socket.send(&request) {
            Ok(n) if n == request.len() => break,
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                io.ready(fd.clone(), false, true, scope).await?
            }
            _ => return Err(Error::Io),
        }
    }
    let mut response = [0; 4096];
    loop {
        scope.check()?;
        match socket.recv(&mut response) {
            Ok(n) => return parse(&response[..n], &request, kind),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                io.ready(fd.clone(), true, false, scope).await?
            }
            _ => return Err(Error::Io),
        }
    }
}
fn u16_at(b: &[u8], p: usize) -> Result<u16> {
    Ok(u16::from_be_bytes(
        b.get(p..p + 2)
            .ok_or(Error::InvalidRequest)?
            .try_into()
            .unwrap(),
    ))
}
fn skip_name(b: &[u8], p: &mut usize) -> Result<()> {
    for _ in 0..128 {
        let n = *b.get(*p).ok_or(Error::InvalidRequest)?;
        *p += 1;
        if n == 0 {
            return Ok(());
        }
        if n & 0xc0 == 0xc0 {
            b.get(*p).ok_or(Error::InvalidRequest)?;
            *p += 1;
            return Ok(());
        }
        if n > 63 {
            return Err(Error::InvalidRequest);
        }
        *p += n as usize;
        if *p > b.len() {
            return Err(Error::InvalidRequest);
        }
    }
    Err(Error::InvalidRequest)
}
fn parse(b: &[u8], request: &[u8], kind: u16) -> Result<Vec<IpAddr>> {
    if b.len() < request.len()
        || b[..2] != request[..2]
        || b[2] & 0xfa != 0x80
        || b[3] & 0x0f != 0
        || u16_at(b, 4)? != 1
        || b[12..request.len()] != request[12..]
    {
        return Err(Error::Unavailable);
    }
    let mut p = request.len();
    let count = u16_at(b, 6)? as usize;
    if count > 128 {
        return Err(Error::Overloaded);
    }
    let mut addresses = Vec::new();
    for _ in 0..count {
        skip_name(b, &mut p)?;
        let rr = u16_at(b, p)?;
        let class = u16_at(b, p + 2)?;
        let len = u16_at(b, p + 8)? as usize;
        p += 10;
        let data = b.get(p..p + len).ok_or(Error::InvalidRequest)?;
        p += len;
        if class == 1 && rr == kind {
            match (rr, len) {
                (1, 4) => addresses.push(IpAddr::V4(Ipv4Addr::from(
                    <[u8; 4]>::try_from(data).unwrap(),
                ))),
                (28, 16) => addresses.push(IpAddr::V6(Ipv6Addr::from(
                    <[u8; 16]>::try_from(data).unwrap(),
                ))),
                _ => return Err(Error::InvalidRequest),
            }
        }
    }
    Ok(addresses)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn rejects_truncation_wrong_question_and_malformed_answers() {
        let request = [1, 2, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, b'a', 0, 0, 1, 0, 1];
        let mut response = request.to_vec();
        response[2] = 0x81;
        response[3] = 0x80;
        response[7] = 1;
        response.extend_from_slice(&[0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, 192, 0, 2, 1]);
        assert_eq!(
            parse(&response, &request, 1).unwrap(),
            vec!["192.0.2.1".parse::<IpAddr>().unwrap()]
        );
        response[2] |= 2;
        assert!(parse(&response, &request, 1).is_err());
        response[2] &= !2;
        response[13] = b'b';
        assert!(parse(&response, &request, 1).is_err());
        response[13] = b'a';
        response.pop();
        assert!(parse(&response, &request, 1).is_err());
    }
}

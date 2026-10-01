use super::*;

#[test]
fn connect_observation_retains_errno_before_generic_boundary_mapping() {
    for errno in [
        libc::ENOBUFS,
        libc::ENOMEM,
        libc::EADDRNOTAVAIL,
        libc::ECONNREFUSED,
        libc::ECONNRESET,
    ] {
        let observation = Cell::new(None);
        let result = KernelResult::Value(-errno);
        result.observe_errno(&observation);
        assert_eq!(observation.get(), Some(errno));
        assert_eq!(result.value(), Err(Error::Io));
    }
    let observation = Cell::new(None);
    KernelResult::Value(0).observe_errno(&observation);
    assert_eq!(observation.get(), None);
}
#[test]
fn sockaddr_encoding_is_owned_and_validated() {
    let (address, len) =
        encode_address(SocketAddress::Inet("127.0.0.1:1234".parse().unwrap())).unwrap();
    assert_eq!(len as usize, std::mem::size_of::<libc::sockaddr_in>());
    let value = unsafe { &*address.as_ptr().cast::<libc::sockaddr_in>() };
    assert_eq!(value.sin_port, 1234u16.to_be());
    assert_eq!(value.sin_addr.s_addr.to_ne_bytes(), [127, 0, 0, 1]);
    let (address, _) = encode_address(SocketAddress::Inet("[::1]:4321".parse().unwrap())).unwrap();
    assert_eq!(
        address.into_inner().ss_family,
        libc::AF_INET6 as libc::sa_family_t
    );
    let (address, len) = encode_address(SocketAddress::Unix("/a/b".into())).unwrap();
    assert_eq!(
        address.into_inner().ss_family,
        libc::AF_UNIX as libc::sa_family_t
    );
    assert_eq!(
        len as usize,
        std::mem::offset_of!(libc::sockaddr_un, sun_path) + 5
    );
    for path in [
        PathBuf::new(),
        PathBuf::from("a\0b"),
        PathBuf::from("x".repeat(108)),
    ] {
        assert!(matches!(
            encode_address(SocketAddress::Unix(path)),
            Err(Error::InvalidRequest)
        ));
    }
}

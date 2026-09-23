// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

#[test]
fn filesystem_path_bounds_and_encoding() {
    for path in [
        "",
        "relative",
        "@abstract",
        "/nul\0path",
        &format!("/{}", "a".repeat(107)),
    ] {
        assert!(UnixPath::new(path).is_err(), "accepted {path:?}");
    }
    let text = format!("/{}", "a".repeat(106));
    let path = UnixPath::new(&text).unwrap();
    assert_eq!(path.as_str(), text);
    let address = path.sockaddr();
    assert_eq!(address.sun_family, libc::AF_UNIX as libc::sa_family_t);
    assert_eq!(address.sun_path[107], 0);
    assert_eq!(
        path.sockaddr_len(),
        std::mem::size_of::<libc::sockaddr_un>()
    );
}

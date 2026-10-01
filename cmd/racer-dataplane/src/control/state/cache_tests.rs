use super::*;

#[test]
fn replacement_definitions_and_socket_paths_are_validated() {
    let mut defs =
        crate::control::wire::decode_publication(include_bytes!("../testdata/publication.json"))
            .unwrap()
            .caches;
    validate_definitions(&defs).unwrap();
    defs[0].id.0 = "66666666-6666-4666-8666-666666666666".into();
    validate_definitions(&defs).unwrap();
    let mut bad = defs.clone();
    bad[0].name = "..".into();
    assert!(validate_definitions(&bad).is_err());
    validate_definitions(&defs).unwrap();
    defs[0].name = "renamed".into();
    (defs[0].client_socket, defs[0].origin_socket) = canonical_socket_paths("renamed").unwrap();
    validate_definitions(&defs).unwrap();
    for name in ["", ".", "..", "a/b", "A", "-a", "a-", "a..b"] {
        assert!(canonical_socket_paths(name).is_err());
    }
    let maximum = format!("{}.{}", "a".repeat(63), "b".repeat(18));
    assert_eq!(
        canonical_socket_paths(&maximum)
            .unwrap()
            .0
            .as_os_str()
            .len(),
        107
    );
    assert!(canonical_socket_paths(&(maximum + "b")).is_err());
}

use super::*;
use racer_control_wire::CacheId;
use crate::model::CacheKey;
#[test]
fn local_context_is_independently_charged_and_keeps_exact_sensitive_fields() {
    let keys = Rc::new(crate::test_support::security::keys());
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let crypto = CredentialCrypto::new(keys, admission.clone());
    let scope = RequestScope::new(
        RequestId([4; 16]),
        std::time::Instant::now() + std::time::Duration::from_secs(5),
    )
    .unwrap();
    let context = OriginContext {
        object: ObjectId {
            cache: CacheId("00000000-0000-4000-8000-000000000001".into()),
            key: CacheKey([3; 32]),
        },
        metadata: Some(OpaqueMetadata::from_header(b"meta\xff").unwrap()),
        authorization: Some(Authorization::from_header(b"opaque\x80").unwrap()),
    };
    let first = crypto.local_context(&context, &scope).unwrap();
    let used = admission.used(ResourceClass::RequestContext);
    let second = crypto.local_context(&context, &scope).unwrap();
    assert_eq!(admission.used(ResourceClass::RequestContext), 2 * used);
    drop(context);
    assert_eq!(
        first.authorization.as_ref().unwrap().expose_for_origin(),
        b"opaque\x80"
    );
    assert_eq!(second.metadata.as_ref().unwrap().as_header(), b"meta\xff");
    scope.cancel().unwrap();
    assert!(matches!(
        crypto.local_context(&first, &scope),
        Err(Error::Cancelled)
    ));
    drop((first, second));
    assert_eq!(admission.used(ResourceClass::RequestContext), 0);
}
fn scope() -> RequestScope {
    RequestScope::new(
        RequestId([1; 16]),
        std::time::Instant::now() + std::time::Duration::from_secs(10),
    )
    .unwrap()
}
fn origin() -> OriginContext {
    OriginContext {
        object: ObjectId {
            cache: CacheId(crate::test_support::security::CACHE.into()),
            key: CacheKey([3; 32]),
        },
        metadata: Some(OpaqueMetadata::from_header(b"opaque,  \xff").unwrap()),
        authorization: Some(Authorization::from_header(b"Bearer exact  \xfe").unwrap()),
    }
}
#[test]
fn exact_roundtrip_nonce_freshness_admission_and_cancellation() {
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let crypto = CredentialCrypto::new(
        Rc::new(crate::test_support::security::keys()),
        admission.clone(),
    );
    let original = origin();
    let scope = scope();
    let attempt = AttemptId([4; 16]);
    let first = crypto.seal(&original, attempt, &scope).unwrap();
    let second = crypto.seal(&original, attempt, &scope).unwrap();
    assert_ne!(
        first.authorization.as_ref().unwrap().nonce,
        second.authorization.as_ref().unwrap().nonce
    );
    drop(second);
    let clear = crypto.open_charged(first, scope.request, attempt).unwrap();
    assert_eq!(
        clear.authorization.as_ref().unwrap().expose_for_origin(),
        b"Bearer exact  \xfe"
    );
    assert_eq!(
        clear.metadata.as_ref().unwrap().as_header(),
        b"opaque,  \xff"
    );
    assert!(admission.used(ResourceClass::RequestContext) > 0);
    drop(clear);
    assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    scope.cancel().unwrap();
    assert!(matches!(
        crypto.seal(&original, attempt, &scope),
        Err(Error::Cancelled)
    ));
    assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.request_context_bytes = std::num::NonZeroUsize::new(1).unwrap();
    let crypto = CredentialCrypto::new(
        Rc::new(crate::test_support::security::keys()),
        Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits))),
    );
    assert!(matches!(
        crypto.seal(&original, attempt, &self::scope()),
        Err(Error::Overloaded)
    ));
}
#[test]
fn absent_authorization_is_charged_and_expired_context_is_rejected() {
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let crypto = CredentialCrypto::new(
        Rc::new(crate::test_support::security::keys()),
        admission.clone(),
    );
    let mut original = origin();
    original.authorization = None;
    let scope = scope();
    let attempt = AttemptId([4; 16]);
    let sealed = crypto.seal(&original, attempt, &scope).unwrap();
    assert!(sealed.authorization.is_none());
    assert!(admission.used(ResourceClass::RequestContext) > 0);
    let clear = crypto.open_charged(sealed, scope.request, attempt).unwrap();
    assert!(clear.authorization.is_none());
    assert_eq!(
        clear.metadata.as_ref().unwrap().as_header(),
        b"opaque,  \xff"
    );
    drop(clear);
    assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    let sealed = crypto.seal(&original, attempt, &scope).unwrap();
    scope.cancel().unwrap();
    assert!(matches!(
        crypto.open_charged(sealed, scope.request, attempt),
        Err(Error::Cancelled)
    ));
    assert_eq!(admission.used(ResourceClass::RequestContext), 0);
}
#[test]
fn rejects_substitution_and_distinguishes_absent_from_empty() {
    let crypto = CredentialCrypto::new(
        Rc::new(crate::test_support::security::keys()),
        Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ))),
    );
    let original = origin();
    let scope = scope();
    let attempt = AttemptId([4; 16]);
    for index in 0..8 {
        let mut sealed = crypto.seal(&original, attempt, &scope).unwrap();
        match index {
            0 => sealed.object.key.0[0] ^= 1,
            1 => sealed.metadata = None,
            2 => sealed.authorization.as_mut().unwrap().ciphertext[0] ^= 1,
            3 => sealed.authorization.as_mut().unwrap().nonce.0[0] ^= 1,
            4 => sealed.request.0[0] ^= 1,
            5 => sealed.attempt.0[0] ^= 1,
            6 => sealed
                .authorization
                .as_mut()
                .unwrap()
                .ciphertext
                .truncate(aead::TAG_LEN - 1),
            _ => sealed
                .authorization
                .as_mut()
                .unwrap()
                .ciphertext
                .truncate(aead::TAG_LEN),
        }
        assert!(crypto.open_charged(sealed, scope.request, attempt).is_err());
    }
    assert_ne!(
        *credential_aad(
            KeyId([1; 16]),
            &original.object,
            scope.request,
            attempt,
            None
        )
        .unwrap(),
        *credential_aad(
            KeyId([1; 16]),
            &original.object,
            scope.request,
            attempt,
            Some(b"")
        )
        .unwrap()
    );
}
#[test]
fn seal_borrows_inputs_and_returns_an_independent_owner() {
    // Independent input lifetimes; the result cannot borrow any of them.
    let _: for<'crypto, 'origin, 'scope> fn(
        &'crypto CredentialCrypto,
        &'origin OriginContext,
        AttemptId,
        &'scope RequestScope,
    ) -> Result<PeerOriginContext> = CredentialCrypto::seal;
    fn owned<T: Send + 'static>() {}
    owned::<PeerOriginContext>();
}

use racer_crypto::{
    aead::{self, TAG_LEN},
    ct_eq, hmac_sha256,
};

#[test]
fn exchange_records_with_reusable_caller_buffers() {
    let key = [7; 32];
    let mut send = [0xa5; 128];
    let mut receive = [0x5a; 128];

    // Each record has a distinct caller-supplied nonce. Empty records still
    // authenticate their context. Only the selected output slices may change.
    for (sequence, plaintext) in [b"first record".as_slice(), b"", b"next record"]
        .into_iter()
        .enumerate()
    {
        let mut nonce = [0; 24];
        nonce[..8].copy_from_slice(&(sequence as u64).to_le_bytes());
        let context = b"example/records/v1";
        let sealed_end = 3 + plaintext.len() + TAG_LEN;
        let opened_end = 5 + plaintext.len();
        let previous_send = send;
        let previous_receive = receive;

        aead::seal(&key, &nonce, context, plaintext, &mut send[3..sealed_end]).unwrap();
        assert_eq!(&send[..3], &previous_send[..3]);
        assert_eq!(&send[sealed_end..], &previous_send[sealed_end..]);

        // A rejected record must not destroy an earlier result in reused output.
        assert!(
            aead::open(
                &key,
                &nonce,
                b"example/other-context/v1",
                &send[3..sealed_end],
                &mut receive[5..opened_end],
            )
            .is_err()
        );
        assert_eq!(receive, previous_receive);

        // Retry the original record into that same buffer without resetting it.
        let transmitted = send;
        aead::open(
            &key,
            &nonce,
            context,
            &send[3..sealed_end],
            &mut receive[5..opened_end],
        )
        .unwrap();
        assert_eq!(&receive[5..opened_end], plaintext);
        assert_eq!(&receive[..5], &previous_receive[..5]);
        assert_eq!(&receive[opened_end..], &previous_receive[opened_end..]);
        assert_eq!(send, transmitted);
    }
}

#[test]
fn authenticate_messages_with_a_shared_key() {
    let sender_key = [0x37; 32];
    let receiver_key = sender_key;
    let long_message = [0x81; 129];
    for message in [b"hello".as_slice(), b"", long_message.as_slice()] {
        let transmitted_tag = hmac_sha256(&sender_key, message);
        let expected = hmac_sha256(&receiver_key, message);
        assert!(ct_eq(&transmitted_tag, &expected));

        let mut changed_message = message.to_vec();
        changed_message.push(0);
        assert!(!ct_eq(
            &transmitted_tag,
            &hmac_sha256(&receiver_key, &changed_message)
        ));
        assert!(!ct_eq(&transmitted_tag, &hmac_sha256(&[0x38; 32], message)));
        assert!(!ct_eq(&transmitted_tag[..31], &expected));
        let mut changed_tag = transmitted_tag;
        changed_tag[31] ^= 1;
        assert!(!ct_eq(&changed_tag, &expected));

        // A failed comparison does not consume the key or the expected tag.
        assert!(ct_eq(
            &transmitted_tag,
            &hmac_sha256(&receiver_key, message)
        ));
    }
}

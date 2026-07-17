/*
 * Copyright (c) Microsoft Corporation.
 * Licensed under the MIT License.
 *
 * Minimal C shim for the unbounded-storage OpenSSL/kTLS transport.
 *
 * Several OpenSSL entry points the TLS module needs are exposed only as
 * preprocessor macros over `SSL_ctrl`/`SSL_CTX_ctrl` (for example
 * `SSL_CTX_set_options`, `SSL_CTX_set_min_proto_version`,
 * `SSL_set_tlsext_host_name`) or as macros over `BIO_ctrl`
 * (`BIO_get_ktls_send`/`BIO_get_ktls_recv`). Macros have no exported
 * symbols, so Rust FFI cannot call them directly. This shim is the only
 * piece of C the tls module ships; everything else is Rust.
 *
 * The shim also owns PEM object parsing so Rust only passes borrowed byte
 * slices and never handles OpenSSL X509 or EVP_PKEY ownership directly. It
 * also hides OpenSSL constants
 * (`SSL_OP_ENABLE_KTLS`, `SSL_FILETYPE_PEM`, `TLS1_2_VERSION`,
 * `TLS1_3_VERSION`) behind
 * plain functions so the Rust side never freezes a numeric value against
 * a moving header.
 */

#include <limits.h>
#include <openssl/bio.h>
#include <openssl/err.h>
#include <openssl/pem.h>
#include <openssl/ssl.h>
#include <openssl/x509err.h>
#include <openssl/x509_vfy.h>
#include <openssl/x509v3.h>

#include <string.h>

static BIO *ub_bio_from_bytes(const unsigned char *data, size_t len) {
    if (data == NULL || len == 0 || len > INT_MAX) {
        return NULL;
    }
    return BIO_new_mem_buf(data, (int)len);
}

static int ub_bio_remaining_is_whitespace(BIO *bio, long position) {
    unsigned char buffer[256];
    int count;
    int i;

    if (position < 0 || BIO_seek(bio, position) < 0) {
        return 0;
    }
    while ((count = BIO_read(bio, buffer, sizeof(buffer))) > 0) {
        for (i = 0; i < count; i++) {
            if (buffer[i] != ' ' && buffer[i] != '\t' &&
                buffer[i] != '\r' && buffer[i] != '\n') {
                return 0;
            }
        }
    }
    return count == 0;
}

static int ub_pem_finished(BIO *bio, long position, unsigned long error,
                           int parsed) {
    return parsed > 0 && ERR_GET_LIB(error) == ERR_LIB_PEM &&
           ERR_GET_REASON(error) == PEM_R_NO_START_LINE &&
           ub_bio_remaining_is_whitespace(bio, position);
}

static int ub_no_password(char *buf, int size, int rwflag, void *userdata) {
    (void)buf;
    (void)size;
    (void)rwflag;
    (void)userdata;
    return 0;
}

/* SSL_CTX_set_options is a macro over SSL_CTX_ctrl. */
unsigned long ub_ssl_ctx_set_options(SSL_CTX *ctx, unsigned long op) {
    return SSL_CTX_set_options(ctx, op);
}

/* SSL_CTX_set_min_proto_version is a macro over SSL_CTX_ctrl. */
int ub_ssl_ctx_set_min_proto_version(SSL_CTX *ctx, int version) {
    return (int)SSL_CTX_set_min_proto_version(ctx, version);
}

/* SSL_CTX_set_max_proto_version is a macro over SSL_CTX_ctrl. */
int ub_ssl_ctx_set_max_proto_version(SSL_CTX *ctx, int version) {
    return (int)SSL_CTX_set_max_proto_version(ctx, version);
}

/* Peer sockets leave OpenSSL after the handshake, so disable TLS 1.3 tickets. */
int ub_ssl_ctx_set_num_tickets(SSL_CTX *ctx, size_t tickets) {
    return SSL_CTX_set_num_tickets(ctx, tickets);
}

int ub_ssl_ctx_load_ca_pem(SSL_CTX *ctx, const unsigned char *pem, size_t len) {
    BIO *bio = NULL;
    X509_STORE *store;
    X509 *cert = NULL;
    long position;
    int parsed = 0;
    int ok = 0;

    ERR_clear_error();
    if (ctx == NULL || (bio = ub_bio_from_bytes(pem, len)) == NULL) {
        goto out;
    }
    store = SSL_CTX_get_cert_store(ctx);
    if (store == NULL) {
        goto out;
    }

    for (;;) {
        position = BIO_tell(bio);
        cert = PEM_read_bio_X509(bio, NULL, NULL, NULL);
        if (cert == NULL) {
            break;
        }
        if (X509_STORE_add_cert(store, cert) != 1) {
            unsigned long error = ERR_peek_last_error();
            if (ERR_GET_LIB(error) != ERR_LIB_X509 ||
                ERR_GET_REASON(error) != X509_R_CERT_ALREADY_IN_HASH_TABLE) {
                goto out;
            }
            ERR_clear_error();
        }
        X509_free(cert);
        cert = NULL;
        parsed++;
    }

    if (ub_pem_finished(bio, position, ERR_peek_last_error(), parsed)) {
        ERR_clear_error();
        ok = 1;
    }

out:
    X509_free(cert);
    BIO_free(bio);
    return ok;
}

int ub_ssl_ctx_use_certificate_chain_pem(SSL_CTX *ctx,
                                         const unsigned char *pem,
                                         size_t len) {
    BIO *bio = NULL;
    X509 *cert = NULL;
    long position;
    int parsed = 0;
    int ok = 0;

    ERR_clear_error();
    if (ctx == NULL || (bio = ub_bio_from_bytes(pem, len)) == NULL) {
        goto out;
    }

    cert = PEM_read_bio_X509_AUX(bio, NULL, NULL, NULL);
    if (cert == NULL || SSL_CTX_use_certificate(ctx, cert) != 1 ||
        SSL_CTX_clear_chain_certs(ctx) != 1) {
        goto out;
    }
    X509_free(cert);
    cert = NULL;
    parsed = 1;

    for (;;) {
        position = BIO_tell(bio);
        cert = PEM_read_bio_X509(bio, NULL, NULL, NULL);
        if (cert == NULL) {
            break;
        }
        if (SSL_CTX_add1_chain_cert(ctx, cert) != 1) {
            goto out;
        }
        X509_free(cert);
        cert = NULL;
        parsed++;
    }

    if (ub_pem_finished(bio, position, ERR_peek_last_error(), parsed)) {
        ERR_clear_error();
        ok = 1;
    }

out:
    X509_free(cert);
    BIO_free(bio);
    return ok;
}

int ub_ssl_ctx_use_private_key_pem(SSL_CTX *ctx,
                                   const unsigned char *pem,
                                   size_t len) {
    BIO *bio = NULL;
    EVP_PKEY *key = NULL;
    int ok = 0;

    ERR_clear_error();
    if (ctx == NULL || (bio = ub_bio_from_bytes(pem, len)) == NULL) {
        goto out;
    }
    key = PEM_read_bio_PrivateKey(bio, NULL, ub_no_password, NULL);
    if (key != NULL && SSL_CTX_use_PrivateKey(ctx, key) == 1 &&
        ub_bio_remaining_is_whitespace(bio, BIO_tell(bio))) {
        ok = 1;
    }

out:
    EVP_PKEY_free(key);
    BIO_free(bio);
    return ok;
}

/* SSL_set_tlsext_host_name is a macro over SSL_ctrl; sets the SNI name. */
long ub_ssl_set_tlsext_host_name(SSL *ssl, const char *name) {
    return SSL_set_tlsext_host_name(ssl, name);
}

/* Peer identities must come from DNS SANs, never a legacy subject CN. */
void ub_ssl_set_dns_san_only(SSL *ssl) {
    SSL_set_hostflags(ssl, X509_CHECK_FLAG_NEVER_CHECK_SUBJECT);
}

X509 *ub_ssl_get1_peer_certificate(SSL *ssl) {
    return SSL_get1_peer_certificate(ssl);
}

int ub_x509_check_dns_san(X509 *cert, const char *name) {
    return X509_check_host(cert, name, 0, X509_CHECK_FLAG_NEVER_CHECK_SUBJECT,
                           NULL);
}

int ub_x509_dns_san_count(X509 *cert) {
    GENERAL_NAMES *sans;
    int count = 0;
    int i;

    sans = X509_get_ext_d2i(cert, NID_subject_alt_name, NULL, NULL);
    if (sans == NULL) {
        return 0;
    }
    for (i = 0; i < sk_GENERAL_NAME_num(sans); i++) {
        const GENERAL_NAME *name = sk_GENERAL_NAME_value(sans, i);
        if (name->type == GEN_DNS) {
            count++;
        }
    }
    GENERAL_NAMES_free(sans);
    return count;
}

/*
 * Copy the indexed DNS SAN. A NULL output reports its byte length. Embedded
 * NULs are rejected so Rust can safely expose the result as a String.
 */
int ub_x509_dns_san_copy(X509 *cert, int index, char *out, size_t capacity) {
    GENERAL_NAMES *sans;
    int dns_index = 0;
    int result = -1;
    int i;

    if (index < 0) {
        return -1;
    }
    sans = X509_get_ext_d2i(cert, NID_subject_alt_name, NULL, NULL);
    if (sans == NULL) {
        return -1;
    }
    for (i = 0; i < sk_GENERAL_NAME_num(sans); i++) {
        const GENERAL_NAME *name = sk_GENERAL_NAME_value(sans, i);
        const unsigned char *data;
        int len;

        if (name->type != GEN_DNS || dns_index++ != index) {
            continue;
        }
        data = ASN1_STRING_get0_data(name->d.dNSName);
        len = ASN1_STRING_length(name->d.dNSName);
        if (len < 0 || memchr(data, '\0', (size_t)len) != NULL) {
            break;
        }
        if (out == NULL) {
            result = len;
        } else if (capacity > (size_t)len) {
            memcpy(out, data, (size_t)len);
            out[len] = '\0';
            result = len;
        }
        break;
    }
    GENERAL_NAMES_free(sans);
    return result;
}

/*
 * BIO_get_ktls_send/BIO_get_ktls_recv are macros over BIO_ctrl. They
 * return 1 only when the kernel TLS data path is actually engaged for
 * that direction (correct cipher negotiated, kernel `tls` ULP active).
 * The handshake asserts both are 1 afterwards; otherwise it would
 * silently fall back to a non-zero-copy userspace path, which we
 * refuse.
 */
int ub_ssl_ktls_send_enabled(SSL *ssl) {
    BIO *w = SSL_get_wbio(ssl);
    return w ? BIO_get_ktls_send(w) : 0;
}

int ub_ssl_ktls_recv_enabled(SSL *ssl) {
    BIO *r = SSL_get_rbio(ssl);
    return r ? BIO_get_ktls_recv(r) : 0;
}

/* Constants that live in headers; surfaced as functions for Rust. */
unsigned long ub_ssl_op_enable_ktls(void) { return SSL_OP_ENABLE_KTLS; }

int ub_ssl_filetype_pem(void) { return SSL_FILETYPE_PEM; }

int ub_tls1_2_version(void) { return TLS1_2_VERSION; }

int ub_tls1_3_version(void) { return TLS1_3_VERSION; }

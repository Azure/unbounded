// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#include <errno.h>
#include <stdint.h>
#include <openssl/ssl.h>
#include <openssl/err.h>
#include <openssl/x509v3.h>

#if OPENSSL_VERSION_NUMBER < 0x30500000L
#error Racer requires OpenSSL >= 3.5 for safe kTLS KeyUpdate handling
#endif
#ifdef OPENSSL_NO_KTLS
#error Racer requires OpenSSL built with enable-ktls
#endif

struct racer_tls_result {
    int64_t transferred;
    int status; /* 0 complete, 1 read, 2 write, 3 orderly EOF, 4 error, 5 abrupt EOF */
    int system_error;
};

static struct racer_tls_result result(SSL *ssl, int rc, int64_t n) {
    int saved_errno = errno;
    struct racer_tls_result r = {n, 0, 0};
    if (rc > 0) return r;
    int error = SSL_get_error(ssl, rc);
    switch (error) {
    case SSL_ERROR_WANT_READ: r.status = 1; break;
    case SSL_ERROR_WANT_WRITE: r.status = 2; break;
    case SSL_ERROR_ZERO_RETURN: r.status = 3; break;
    default:
        r.status = 4;
        /* Newer record layers can append a write/alert failure after the EOF.
         * Classify the original cause, never an arbitrary error string. */
        unsigned long reason = ERR_peek_error();
        if (error == SSL_ERROR_SSL && ERR_GET_LIB(reason) == ERR_LIB_SSL &&
            ERR_GET_REASON(reason) == SSL_R_UNEXPECTED_EOF_WHILE_READING)
            r.status = 5;
        if (error == SSL_ERROR_SYSCALL) r.system_error = saved_errno;
        break;
    }
    return r;
}

int racer_tls_configure(SSL_CTX *ctx) {
    /* Check the loaded library too when linking an external OpenSSL. */
    if (OpenSSL_version_num() < 0x30500000L) {
        ERR_raise_data(ERR_LIB_SSL, ERR_R_UNSUPPORTED,
                       "Racer requires OpenSSL >= 3.5 built with enable-ktls");
        return 0;
    }
    /* Always attempt offload, including on Linux 5.15. The session requires
     * actual TX and RX kTLS before admitting application data. OpenSSL >= 3.5
     * replaces the active kTLS record layer on KeyUpdate. If the kernel cannot
     * replace its keys, ssl_set_new_record_layer fails fatally: fallback is
     * allowed only when changing methods, never from an already-active kTLS
     * method to software TLS. The Rust session latches that error permanently.
     */
    SSL_CTX_set_options(ctx, SSL_OP_NO_TICKET | SSL_OP_NO_RENEGOTIATION |
                             SSL_OP_ENABLE_KTLS);
    SSL_CTX_set_session_cache_mode(ctx, SSL_SESS_CACHE_OFF);
    SSL_CTX_set_mode(ctx, SSL_MODE_ENABLE_PARTIAL_WRITE);
    return SSL_CTX_set_num_tickets(ctx, 0) &&
           SSL_CTX_set_max_early_data(ctx, 0) &&
           SSL_CTX_set_recv_max_early_data(ctx, 0);
}

int racer_tls_set_fd(SSL *ssl, int fd, int server) {
    if (SSL_set_fd(ssl, fd) != 1) return 0;
    if (server) SSL_set_accept_state(ssl);
    else SSL_set_connect_state(ssl);
    return 1;
}

struct racer_tls_result racer_tls_handshake(SSL *ssl) {
    ERR_clear_error(); errno = 0;
    int rc = SSL_do_handshake(ssl);
    return result(ssl, rc, 0);
}

struct racer_tls_result racer_tls_read(SSL *ssl, void *buf, size_t len) {
    size_t n = 0;
    ERR_clear_error(); errno = 0;
    int rc = SSL_read_ex(ssl, buf, len, &n);
    return result(ssl, rc, (int64_t)n);
}

struct racer_tls_result racer_tls_write(SSL *ssl, const void *buf, size_t len) {
    size_t n = 0;
    ERR_clear_error(); errno = 0;
    int rc = SSL_write_ex(ssl, buf, len, &n);
    return result(ssl, rc, (int64_t)n);
}

struct racer_tls_result racer_tls_sendfile(SSL *ssl, int fd, int64_t offset, size_t count) {
    ERR_clear_error(); errno = 0;
    ossl_ssize_t n = SSL_sendfile(ssl, fd, (off_t)offset, count, 0);
    return result(ssl, n >= 0 ? 1 : (int)n, n >= 0 ? (int64_t)n : 0);
}

struct racer_tls_result racer_tls_shutdown(SSL *ssl) {
    ERR_clear_error(); errno = 0;
    int rc = SSL_shutdown(ssl);
    if (rc == 0) {
        struct racer_tls_result r = {0, 1, 0};
        return r;
    }
    return result(ssl, rc, 0);
}

int racer_tls_offload(SSL *ssl) {
    return (BIO_get_ktls_send(SSL_get_wbio(ssl)) ? 1 : 0) |
           (BIO_get_ktls_recv(SSL_get_rbio(ssl)) ? 2 : 0);
}

int racer_tls_is_ca(X509 *cert) { return X509_check_ca(cert) > 0; }

/* Reject missing, duplicate, wildcard, and unrelated role usages. */
int racer_tls_role_eku(X509 *cert, int node) {
    int critical = -1;
    EXTENDED_KEY_USAGE *usage = X509_get_ext_d2i(cert, NID_ext_key_usage, &critical, NULL);
    if (usage == NULL) return 0;
    int server = 0, client = 0, invalid = 0;
    for (int i = 0; i < sk_ASN1_OBJECT_num(usage); i++) {
        switch (OBJ_obj2nid(sk_ASN1_OBJECT_value(usage, i))) {
        case NID_server_auth: server++; break;
        case NID_client_auth: client++; break;
        default: invalid = 1;
        }
    }
    EXTENDED_KEY_USAGE_free(usage);
    return !invalid && server == 1 && client == (node ? 1 : 0);
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <sys/utsname.h>
#include <openssl/ssl.h>
#include <openssl/err.h>
#include <openssl/x509v3.h>

#if OPENSSL_VERSION_MAJOR < 3
#error Racer requires OpenSSL 3
#endif

struct racer_tls_result {
    int64_t transferred;
    int status; /* 0 complete, 1 read, 2 write, 3 orderly EOF, 4 error */
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
        if (error == SSL_ERROR_SYSCALL) r.system_error = saved_errno;
        break;
    }
    return r;
}

static int ktls_rekey_supported(void) {
    /* OpenSSL 3.0 updates software secrets but leaves the TX kernel key stale
     * after a peer-requested KeyUpdate. Never enable offload on that path.
     * Use a conservative baseline with record-layer rekeying in OpenSSL and
     * TLS_TX/TLS_RX key replacement in Linux (documented in Linux 6.14's
     * Documentation/networking/tls.rst). Older/backported stacks use software
     * TLS for the entire connection, not a midstream plaintext downgrade.
     */
    struct utsname kernel;
    unsigned int major = 0, minor = 0;
    if (OpenSSL_version_num() < 0x30500000L || uname(&kernel) != 0)
        return 0;
    if (sscanf(kernel.release, "%u.%u", &major, &minor) != 2)
        return 0;
    return major > 6 || (major == 6 && minor >= 14);
}

int racer_tls_configure(SSL_CTX *ctx, int ktls) {
    SSL_CTX_set_options(ctx, SSL_OP_NO_TICKET | SSL_OP_NO_RENEGOTIATION);
    SSL_CTX_clear_options(ctx, SSL_OP_ENABLE_KTLS);
    if (ktls && ktls_rekey_supported())
        SSL_CTX_set_options(ctx, SSL_OP_ENABLE_KTLS);
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

/*
 * Copyright (c) Microsoft Corporation.
 * Licensed under the MIT License.
 *
 * Minimal C shim for the unbounded-storage OpenSSL/kTLS client.
 *
 * Several OpenSSL entry points the backend needs are exposed only as
 * preprocessor macros over `SSL_ctrl`/`SSL_CTX_ctrl` (for example
 * `SSL_CTX_set_options`, `SSL_CTX_set_min_proto_version`,
 * `SSL_set_tlsext_host_name`) or as macros over `BIO_ctrl`
 * (`BIO_get_ktls_send`/`BIO_get_ktls_recv`). Macros have no exported
 * symbols, so Rust FFI cannot call them directly. This shim is the only
 * piece of C the backend module ships; everything else is Rust.
 *
 * The shim also hides a couple of OpenSSL constants
 * (`SSL_OP_ENABLE_KTLS`, `TLS1_2_VERSION`) behind plain functions so the
 * Rust side never freezes a numeric value against a moving header.
 */

#include <openssl/bio.h>
#include <openssl/err.h>
#include <openssl/ssl.h>

/* SSL_CTX_set_options is a macro over SSL_CTX_ctrl. */
unsigned long ub_ssl_ctx_set_options(SSL_CTX *ctx, unsigned long op) {
    return SSL_CTX_set_options(ctx, op);
}

/* SSL_CTX_set_min_proto_version is a macro over SSL_CTX_ctrl. */
int ub_ssl_ctx_set_min_proto_version(SSL_CTX *ctx, int version) {
    return (int)SSL_CTX_set_min_proto_version(ctx, version);
}

/* SSL_set_tlsext_host_name is a macro over SSL_ctrl; sets the SNI name. */
long ub_ssl_set_tlsext_host_name(SSL *ssl, const char *name) {
    return SSL_set_tlsext_host_name(ssl, name);
}

/*
 * BIO_get_ktls_send/BIO_get_ktls_recv are macros over BIO_ctrl. They
 * return 1 only when the kernel TLS data path is actually engaged for
 * that direction (correct cipher negotiated, kernel `tls` ULP active).
 * The backend asserts both are 1 after the handshake; otherwise it
 * would silently fall back to a non-zero-copy userspace path, which we
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

int ub_tls1_2_version(void) { return TLS1_2_VERSION; }

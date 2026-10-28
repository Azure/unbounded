/* Serial host emulation of the GPU decrypt kernels.
 *
 * Runs the same gcm_core.h steps in the same order the kernels do: prepare,
 * one partial per chunk, XOR reduction, finalize, then CTR only on a passing
 * verdict. The CPU tests compare it with the racer-crypto crate, and the
 * simulation backend uses it as its "kernel". Linked statically into the Rust
 * crate by build.rs; never part of libracer_gpu_cuda.so.1. */
#include "gcm_core.h"

#include <stdlib.h>
#include <string.h>

static const uint8_t host_sbox[256] = GCM_SBOX_INIT;

/* Wipe memory the compiler may not elide. */
static void wipe(void *p, size_t n) {
    volatile uint8_t *v = (volatile uint8_t *)p;
    while (n--) *v++ = 0;
}

/* CRC-64/XZ through the chunked combine path. */
uint64_t racer_gcm_host_crc64(const uint8_t *data, uint64_t length) {
    uint64_t table[256], crc = 0;
    gcm_crc64_table(table);
    if (!length) return gcm_crc64(table, data, 0);
    for (uint64_t start = 0; start < length; start += GCM_CHUNK) {
        uint64_t end = start + GCM_CHUNK < length ? start + GCM_CHUNK : length;
        crc ^= gcm_crc_shift(gcm_crc64(table, data + start, end - start), length - end);
    }
    return crc;
}

/* One AES-256 block, for known-answer tests. */
void racer_gcm_host_aes256(const uint8_t key[32], const uint8_t in[16], uint8_t out[16]) {
    uint8_t rk[240];
    gcm_aes256_expand(host_sbox, key, rk);
    gcm_aes256_encrypt(host_sbox, rk, in, out);
    wipe(rk, sizeof(rk));
}

/* Verify and decrypt `sealed` (n ciphertext bytes then a 16-byte tag) into
 * `out` (n bytes). `out` is written only when the verdict passes.
 * Returns 0, or -1 if scratch memory cannot be allocated. */
int racer_gcm_host_decrypt(const uint8_t key[32], const uint8_t nonce[12], const uint8_t *aad,
                           uint64_t aad_len, const uint8_t *sealed, uint64_t n,
                           uint32_t check_crc, uint64_t expected_crc, uint8_t *out,
                           struct gcm_verdict *verdict) {
    struct gcm_key k;
    uint64_t table[256];
    uint64_t chunks = gcm_chunks(n);
    struct gcm_partial *partials = calloc(chunks, sizeof(*partials));
    verdict->status = GCM_VERDICT_NONE;
    if (!partials) return -1;
    gcm_crc64_table(table);
    gcm_prepare(host_sbox, key, nonce, aad, aad_len, n, &k);
    for (uint64_t j = 0; j < chunks; ++j) gcm_chunk_partial(table, &k, sealed, n, j, &partials[j]);
    uint8_t ghash[16] = {0};
    uint64_t crc = 0;
    for (uint64_t j = 0; j < chunks; ++j) {
        for (int i = 0; i < 16; ++i) ghash[i] ^= partials[j].ghash[i];
        crc ^= partials[j].crc;
    }
    gcm_finalize(&k, sealed, n, ghash, crc, check_crc, expected_crc, verdict);
    if (verdict->status == GCM_VERDICT_PASS)
        for (uint64_t i = 0; i < (n + 15) / 16; ++i) gcm_ctr_block(host_sbox, &k, sealed, n, i, out);
    wipe(&k, sizeof(k));
    wipe(partials, chunks * sizeof(*partials));
    free(partials);
    return 0;
}

/* AES-256-GCM decrypt-with-verification and CRC-64/XZ, shared by the CUDA
 * kernels (gcm_kernel.cu), the host shim (gpu_cuda.c), the simulation backend
 * and the CPU tests (gcm_host.c). It compiles as C99 and as CUDA.
 *
 * Racer pages are sealed with XAES-256-GCM (https://c2sp.org/XAES-256-GCM).
 * The CPU derives the per-page AES-256 key and 12-byte GCM nonce, so this file
 * only implements plain AES-256-GCM (NIST SP 800-38D) on the derived values.
 * The sealed page is ciphertext followed by the 16-byte tag; the stored
 * checksum is CRC-64/XZ over the whole sealed page (racer_crypto::crc64 over
 * CiphertextPage::bytes()).
 *
 * The work is split so it runs in parallel:
 *
 *   gcm_prepare        host: key schedule, H, E(K, J0), and the GHASH terms
 *                      that do not depend on the ciphertext (AAD, lengths).
 *   gcm_chunk_partial  one per GCM_CHUNK bytes: CRC of the chunk shifted to
 *                      its final position, and the chunk's GHASH sum
 *                      multiplied by the power of H it has in the full sum.
 *   gcm_finalize       XOR of all partials (both sums are linear), then the
 *                      constant-time tag compare and the CRC compare.
 *   gcm_ctr_block      one per 16-byte block: CTR decryption.
 *
 * GHASH is sum(X_i * H^(m-i+1)) over m blocks, so chunk j with last block
 * b_j contributes Horner(chunk) * H^(nc-b_j+1), where nc is the number of
 * ciphertext blocks. CRC-64/XZ composes like zlib's crc32_combine:
 * crc(A||B) = crc(A) * x^(8|B|) mod P xor crc(B), so each chunk is shifted by
 * the number of bytes after it and all chunks are XORed.
 *
 * Timing: GF(2^128) multiplication is constant time (no secret-dependent
 * branches or indexes). AES uses an S-box lookup with secret indexes, so it is
 * not constant time with respect to cache or shared-memory bank timing. The
 * kernels keep the S-box in shared memory, where only bank conflicts (not a
 * cache hierarchy shared with other tenants) can leak timing, and the key is a
 * per-page derived key. A bitsliced AES would remove this; it is future work.
 */
#ifndef RACER_GCM_CORE_H
#define RACER_GCM_CORE_H

#include <stdint.h>
#include <stddef.h>

#ifdef __CUDACC__
#define GCM_FN __host__ __device__ static inline
#else
#define GCM_FN static inline
#endif

/* Bytes of sealed input each partial covers. A multiple of 16. */
#define GCM_CHUNK 4096u
/* Reflected CRC-64/XZ (ECMA-182) polynomial. */
#define GCM_CRC64_POLY 0xC96C5795D7870F42ull
/* Verdict values. Only GCM_VERDICT_PASS lets CTR decryption write output. */
#define GCM_VERDICT_NONE 0u
#define GCM_VERDICT_PASS 0x600Du
#define GCM_VERDICT_CRC 2u
#define GCM_VERDICT_TAG 3u

/* FIPS-197 S-box. */
#define GCM_SBOX_INIT { \
    0x63,0x7c,0x77,0x7b,0xf2,0x6b,0x6f,0xc5,0x30,0x01,0x67,0x2b,0xfe,0xd7,0xab,0x76, \
    0xca,0x82,0xc9,0x7d,0xfa,0x59,0x47,0xf0,0xad,0xd4,0xa2,0xaf,0x9c,0xa4,0x72,0xc0, \
    0xb7,0xfd,0x93,0x26,0x36,0x3f,0xf7,0xcc,0x34,0xa5,0xe5,0xf1,0x71,0xd8,0x31,0x15, \
    0x04,0xc7,0x23,0xc3,0x18,0x96,0x05,0x9a,0x07,0x12,0x80,0xe2,0xeb,0x27,0xb2,0x75, \
    0x09,0x83,0x2c,0x1a,0x1b,0x6e,0x5a,0xa0,0x52,0x3b,0xd6,0xb3,0x29,0xe3,0x2f,0x84, \
    0x53,0xd1,0x00,0xed,0x20,0xfc,0xb1,0x5b,0x6a,0xcb,0xbe,0x39,0x4a,0x4c,0x58,0xcf, \
    0xd0,0xef,0xaa,0xfb,0x43,0x4d,0x33,0x85,0x45,0xf9,0x02,0x7f,0x50,0x3c,0x9f,0xa8, \
    0x51,0xa3,0x40,0x8f,0x92,0x9d,0x38,0xf5,0xbc,0xb6,0xda,0x21,0x10,0xff,0xf3,0xd2, \
    0xcd,0x0c,0x13,0xec,0x5f,0x97,0x44,0x17,0xc4,0xa7,0x7e,0x3d,0x64,0x5d,0x19,0x73, \
    0x60,0x81,0x4f,0xdc,0x22,0x2a,0x90,0x88,0x46,0xee,0xb8,0x14,0xde,0x5e,0x0b,0xdb, \
    0xe0,0x32,0x3a,0x0a,0x49,0x06,0x24,0x5c,0xc2,0xd3,0xac,0x62,0x91,0x95,0xe4,0x79, \
    0xe7,0xc8,0x37,0x6d,0x8d,0xd5,0x4e,0xa9,0x6c,0x56,0xf4,0xea,0x65,0x7a,0xae,0x08, \
    0xba,0x78,0x25,0x2e,0x1c,0xa6,0xb4,0xc6,0xe8,0xdd,0x74,0x1f,0x4b,0xbd,0x8b,0x8a, \
    0x70,0x3e,0xb5,0x66,0x48,0x03,0xf6,0x0e,0x61,0x35,0x57,0xb9,0x86,0xc1,0x1d,0x9e, \
    0xe1,0xf8,0x98,0x11,0x69,0xd9,0x8e,0x94,0x9b,0x1e,0x87,0xe9,0xce,0x55,0x28,0xdf, \
    0x8c,0xa1,0x89,0x0d,0xbf,0xe6,0x42,0x68,0x41,0x99,0x2d,0x0f,0xb0,0x54,0xbb,0x16 }

/* Everything the device needs for one page. Wiped after use. */
struct gcm_key {
    uint8_t rk[240];   /* AES-256 round keys */
    uint8_t h[16];     /* E(K, 0^128) */
    uint8_t ekj0[16];  /* E(K, J0), the tag mask */
    uint8_t prefix[16]; /* GHASH terms for AAD and the length block */
    uint8_t nonce[12];
    uint32_t reserved;
};

/* One chunk's contribution. */
struct gcm_partial {
    uint8_t ghash[16];
    uint64_t crc;
    uint64_t reserved;
};

/* Result of verification. Same layout as struct racer_gpu_verdict. */
struct gcm_verdict {
    uint32_t status;
    uint32_t reserved;
    uint64_t crc;
};

/* GF(2^128) element in GCM bit order, as two big-endian halves. */
struct gcm_block {
    uint64_t hi, lo;
};

GCM_FN uint64_t gcm_load64(const uint8_t *p) {
    uint64_t v = 0;
    for (int i = 0; i < 8; ++i) v = (v << 8) | p[i];
    return v;
}

GCM_FN void gcm_store64(uint8_t *p, uint64_t v) {
    for (int i = 7; i >= 0; --i) { p[i] = (uint8_t)v; v >>= 8; }
}

GCM_FN struct gcm_block gcm_load(const uint8_t *p) {
    struct gcm_block b;
    b.hi = gcm_load64(p);
    b.lo = gcm_load64(p + 8);
    return b;
}

/* Load up to 16 bytes, zero padding the rest. */
GCM_FN struct gcm_block gcm_load_partial(const uint8_t *p, uint64_t n) {
    uint8_t tmp[16];
    for (unsigned i = 0; i < 16; ++i) tmp[i] = i < n ? p[i] : 0;
    return gcm_load(tmp);
}

GCM_FN void gcm_store(uint8_t *p, struct gcm_block b) {
    gcm_store64(p, b.hi);
    gcm_store64(p + 8, b.lo);
}

/* X * Y in GF(2^128), constant time (SP 800-38D algorithm 1). */
GCM_FN struct gcm_block gcm_mul(struct gcm_block x, struct gcm_block y) {
    struct gcm_block z = {0, 0}, v = y;
    for (int i = 0; i < 128; ++i) {
        uint64_t bit = i < 64 ? (x.hi >> (63 - i)) & 1 : (x.lo >> (127 - i)) & 1;
        uint64_t mask = (uint64_t)0 - bit;
        z.hi ^= v.hi & mask;
        z.lo ^= v.lo & mask;
        uint64_t lsb = (uint64_t)0 - (v.lo & 1);
        v.lo = (v.lo >> 1) | (v.hi << 63);
        v.hi = (v.hi >> 1) ^ (0xE100000000000000ull & lsb);
    }
    return z;
}

/* H^k. k is public; the multiplications are constant time in H. */
GCM_FN struct gcm_block gcm_pow(struct gcm_block h, uint64_t k) {
    struct gcm_block r = {0x8000000000000000ull, 0}; /* multiplicative one */
    while (k) {
        if (k & 1) r = gcm_mul(r, h);
        h = gcm_mul(h, h);
        k >>= 1;
    }
    return r;
}

GCM_FN uint8_t gcm_xtime(uint8_t x) {
    return (uint8_t)((x << 1) ^ (0x1b & (uint8_t)(0u - (x >> 7))));
}

/* FIPS-197 AES-256 key expansion into 15 round keys. */
GCM_FN void gcm_aes256_expand(const uint8_t *sbox, const uint8_t key[32], uint8_t rk[240]) {
    uint8_t rcon = 1;
    for (int i = 0; i < 32; ++i) rk[i] = key[i];
    for (int i = 8; i < 60; ++i) {
        uint8_t t[4];
        for (int j = 0; j < 4; ++j) t[j] = rk[4 * (i - 1) + j];
        if (i % 8 == 0) {
            uint8_t first = t[0];
            t[0] = (uint8_t)(sbox[t[1]] ^ rcon);
            t[1] = sbox[t[2]];
            t[2] = sbox[t[3]];
            t[3] = sbox[first];
            rcon = gcm_xtime(rcon);
        } else if (i % 8 == 4) {
            for (int j = 0; j < 4; ++j) t[j] = sbox[t[j]];
        }
        for (int j = 0; j < 4; ++j) rk[4 * i + j] = (uint8_t)(rk[4 * (i - 8) + j] ^ t[j]);
    }
}

/* Encrypt one block with AES-256 round keys `rk`. */
GCM_FN void gcm_aes256_encrypt(const uint8_t *sbox, const uint8_t *rk, const uint8_t in[16],
                               uint8_t out[16]) {
    uint8_t s[16], t[16];
    for (int i = 0; i < 16; ++i) s[i] = (uint8_t)(in[i] ^ rk[i]);
    for (int round = 1; round <= 14; ++round) {
        /* SubBytes and ShiftRows. Byte r + 4c is row r, column c. */
        for (int c = 0; c < 4; ++c)
            for (int r = 0; r < 4; ++r) t[r + 4 * c] = sbox[s[r + 4 * ((c + r) & 3)]];
        if (round != 14) {
            for (int c = 0; c < 4; ++c) {
                uint8_t a0 = t[4 * c], a1 = t[4 * c + 1], a2 = t[4 * c + 2], a3 = t[4 * c + 3];
                uint8_t all = (uint8_t)(a0 ^ a1 ^ a2 ^ a3);
                s[4 * c] = (uint8_t)(a0 ^ all ^ gcm_xtime((uint8_t)(a0 ^ a1)));
                s[4 * c + 1] = (uint8_t)(a1 ^ all ^ gcm_xtime((uint8_t)(a1 ^ a2)));
                s[4 * c + 2] = (uint8_t)(a2 ^ all ^ gcm_xtime((uint8_t)(a2 ^ a3)));
                s[4 * c + 3] = (uint8_t)(a3 ^ all ^ gcm_xtime((uint8_t)(a3 ^ a0)));
            }
        } else {
            for (int i = 0; i < 16; ++i) s[i] = t[i];
        }
        for (int i = 0; i < 16; ++i) s[i] ^= rk[16 * round + i];
    }
    for (int i = 0; i < 16; ++i) out[i] = s[i];
}

/* Entry `i` of the reflected CRC-64/XZ byte table. */
GCM_FN uint64_t gcm_crc64_entry(unsigned i) {
    uint64_t c = i;
    for (int k = 0; k < 8; ++k) c = (c & 1) ? (c >> 1) ^ GCM_CRC64_POLY : c >> 1;
    return c;
}

/* Reflected CRC-64/XZ byte table. */
GCM_FN void gcm_crc64_table(uint64_t table[256]) {
    for (unsigned i = 0; i < 256; ++i) table[i] = gcm_crc64_entry(i);
}

/* Complete CRC-64/XZ (init and xorout all ones) of `n` bytes. */
GCM_FN uint64_t gcm_crc64(const uint64_t *table, const uint8_t *p, uint64_t n) {
    uint64_t c = ~(uint64_t)0;
    for (uint64_t i = 0; i < n; ++i) c = table[(c ^ p[i]) & 0xff] ^ (c >> 8);
    return ~c;
}

/* a * b mod P in the reflected representation (bit 63 is x^0). */
GCM_FN uint64_t gcm_crc_mulmod(uint64_t a, uint64_t b) {
    uint64_t m = 1ull << 63, p = 0;
    if (!a) return 0;
    for (;;) {
        if (a & m) {
            p ^= b;
            if ((a & (m - 1)) == 0) break;
        }
        m >>= 1;
        b = (b & 1) ? (b >> 1) ^ GCM_CRC64_POLY : b >> 1;
    }
    return p;
}

/* crc * x^(8n) mod P: moves a complete CRC past n more bytes. */
GCM_FN uint64_t gcm_crc_shift(uint64_t crc, uint64_t n) {
    uint64_t p = 1ull << 63, base = 1ull << (63 - 8);
    while (n) {
        if (n & 1) p = gcm_crc_mulmod(base, p);
        base = gcm_crc_mulmod(base, base);
        n >>= 1;
    }
    return gcm_crc_mulmod(p, crc);
}

/* Number of partials for a page of `n` plaintext bytes. */
GCM_FN uint64_t gcm_chunks(uint64_t n) {
    return (n + 16 + GCM_CHUNK - 1) / GCM_CHUNK;
}

/* Host side: derive everything the kernels need from the derived key,
 * nonce, AAD and plaintext length. */
GCM_FN void gcm_prepare(const uint8_t *sbox, const uint8_t key[32], const uint8_t nonce[12],
                        const uint8_t *aad, uint64_t aad_len, uint64_t n, struct gcm_key *out) {
    uint8_t block[16];
    gcm_aes256_expand(sbox, key, out->rk);
    for (int i = 0; i < 16; ++i) block[i] = 0;
    gcm_aes256_encrypt(sbox, out->rk, block, out->h);
    for (int i = 0; i < 12; ++i) block[i] = out->nonce[i] = nonce[i];
    block[12] = 0; block[13] = 0; block[14] = 0; block[15] = 1;
    gcm_aes256_encrypt(sbox, out->rk, block, out->ekj0);
    out->reserved = 0;

    struct gcm_block h = gcm_load(out->h), y = {0, 0};
    for (uint64_t i = 0; i < aad_len; i += 16) {
        struct gcm_block x = gcm_load_partial(aad + i, aad_len - i);
        y.hi ^= x.hi; y.lo ^= x.lo;
        y = gcm_mul(y, h);
    }
    uint64_t nc = (n + 15) / 16;
    struct gcm_block prefix = gcm_mul(y, gcm_pow(h, nc + 1));
    struct gcm_block lengths = {aad_len * 8, n * 8};
    struct gcm_block lh = gcm_mul(lengths, h);
    prefix.hi ^= lh.hi; prefix.lo ^= lh.lo;
    gcm_store(out->prefix, prefix);
    for (int i = 0; i < 16; ++i) block[i] = 0;
}

/* Partial `j` over the sealed page (`n` ciphertext bytes then a 16-byte tag). */
GCM_FN void gcm_chunk_partial(const uint64_t *crc_table, const struct gcm_key *key,
                              const uint8_t *sealed, uint64_t n, uint64_t j,
                              struct gcm_partial *out) {
    uint64_t total = n + 16;
    uint64_t start = j * GCM_CHUNK;
    uint64_t end = start + GCM_CHUNK < total ? start + GCM_CHUNK : total;
    out->crc = gcm_crc_shift(gcm_crc64(crc_table, sealed + start, end - start), total - end);
    out->reserved = 0;

    struct gcm_block y = {0, 0};
    uint64_t gend = end < n ? end : n;
    if (start < gend) {
        struct gcm_block h = gcm_load(key->h);
        for (uint64_t i = start; i < gend; i += 16) {
            struct gcm_block x = gcm_load_partial(sealed + i, gend - i);
            y.hi ^= x.hi; y.lo ^= x.lo;
            y = gcm_mul(y, h);
        }
        uint64_t nc = (n + 15) / 16, last = (gend + 15) / 16;
        y = gcm_mul(y, gcm_pow(h, nc - last + 1));
    }
    gcm_store(out->ghash, y);
}

/* Combine reduced partials, check the tag in constant time and the CRC. */
GCM_FN void gcm_finalize(const struct gcm_key *key, const uint8_t *sealed, uint64_t n,
                         const uint8_t ghash[16], uint64_t crc, uint32_t check_crc,
                         uint64_t expected_crc, struct gcm_verdict *out) {
    uint8_t diff = 0;
    for (int i = 0; i < 16; ++i)
        diff |= (uint8_t)(key->prefix[i] ^ ghash[i] ^ key->ekj0[i] ^ sealed[n + i]);
    int crc_ok = !check_crc || crc == expected_crc;
    out->crc = crc;
    out->reserved = 0;
    out->status = !crc_ok ? GCM_VERDICT_CRC : diff ? GCM_VERDICT_TAG : GCM_VERDICT_PASS;
}

/* CTR-decrypt block `i` into `out`. Call only after a passing verdict. */
GCM_FN void gcm_ctr_block(const uint8_t *sbox, const struct gcm_key *key, const uint8_t *sealed,
                          uint64_t n, uint64_t i, uint8_t *out) {
    uint8_t counter[16], stream[16];
    uint32_t c = (uint32_t)(i + 2);
    for (int k = 0; k < 12; ++k) counter[k] = key->nonce[k];
    counter[12] = (uint8_t)(c >> 24); counter[13] = (uint8_t)(c >> 16);
    counter[14] = (uint8_t)(c >> 8); counter[15] = (uint8_t)c;
    gcm_aes256_encrypt(sbox, key->rk, counter, stream);
    uint64_t base = i * 16, len = n - base < 16 ? n - base : 16;
    for (uint64_t k = 0; k < len; ++k) out[base + k] = (uint8_t)(sealed[base + k] ^ stream[k]);
    for (int k = 0; k < 16; ++k) stream[k] = 0;
}

#endif

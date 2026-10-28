/* CUDA kernels for GPU page decryption. See gcm_core.h for the math.
 *
 * Built to PTX by images/racer/build-native-cuda.sh with
 * `nvcc -ptx -arch=compute_80`. compute_80 (Ampere) is the baseline: the
 * driver JIT-compiles the PTX for the installed GPU, so newer GPUs work and
 * older ones fail at cuModuleLoadData (the shim then reports no kernels).
 *
 * Launch order on one stream, all set up by racer_gpu_decrypt in gpu_cuda.c:
 *   1. racer_gcm_partials  one thread per GCM_CHUNK of the sealed page
 *   2. racer_gcm_finalize  one block of 256 threads; writes the verdict
 *   3. racer_gcm_ctr       one thread per 16-byte block; writes plaintext only
 *                          when the verdict on the device is GCM_VERDICT_PASS
 * No host round trip sits between verification and decryption. */
#include "gcm_core.h"

__device__ const uint8_t racer_gcm_sbox[256] = GCM_SBOX_INIT;

#define FINALIZE_THREADS 256

extern "C" __global__ void racer_gcm_partials(const uint8_t *sealed, uint64_t n,
                                              const struct gcm_key *key,
                                              struct gcm_partial *partials, uint64_t chunks) {
    __shared__ uint64_t table[256];
    for (unsigned i = threadIdx.x; i < 256; i += blockDim.x) table[i] = gcm_crc64_entry(i);
    __syncthreads();
    uint64_t j = (uint64_t)blockIdx.x * blockDim.x + threadIdx.x;
    if (j < chunks) gcm_chunk_partial(table, key, sealed, n, j, &partials[j]);
}

extern "C" __global__ void racer_gcm_finalize(const uint8_t *sealed, uint64_t n,
                                              const struct gcm_key *key,
                                              const struct gcm_partial *partials, uint64_t chunks,
                                              uint32_t check_crc, uint64_t expected_crc,
                                              struct gcm_verdict *verdict) {
    __shared__ uint64_t hi[FINALIZE_THREADS], lo[FINALIZE_THREADS], crc[FINALIZE_THREADS];
    unsigned t = threadIdx.x;
    uint64_t h = 0, l = 0, c = 0;
    for (uint64_t j = t; j < chunks; j += FINALIZE_THREADS) {
        struct gcm_block b = gcm_load(partials[j].ghash);
        h ^= b.hi;
        l ^= b.lo;
        c ^= partials[j].crc;
    }
    hi[t] = h;
    lo[t] = l;
    crc[t] = c;
    __syncthreads();
    for (unsigned stride = FINALIZE_THREADS / 2; stride; stride >>= 1) {
        if (t < stride) {
            hi[t] ^= hi[t + stride];
            lo[t] ^= lo[t + stride];
            crc[t] ^= crc[t + stride];
        }
        __syncthreads();
    }
    if (t == 0) {
        uint8_t ghash[16];
        struct gcm_block b = {hi[0], lo[0]};
        gcm_store(ghash, b);
        gcm_finalize(key, sealed, n, ghash, crc[0], check_crc, expected_crc, verdict);
    }
}

extern "C" __global__ void racer_gcm_ctr(const uint8_t *sealed, uint64_t n,
                                         const struct gcm_key *key,
                                         const struct gcm_verdict *verdict, uint8_t *out) {
    __shared__ uint8_t sbox[256];
    __shared__ struct gcm_key k;
    /* Same value for every thread, so the whole block leaves together. */
    if (verdict->status != GCM_VERDICT_PASS) return;
    uint8_t *raw = (uint8_t *)&k;
    const uint8_t *from = (const uint8_t *)key;
    for (unsigned i = threadIdx.x; i < 256; i += blockDim.x) sbox[i] = racer_gcm_sbox[i];
    for (unsigned i = threadIdx.x; i < sizeof(k); i += blockDim.x) raw[i] = from[i];
    __syncthreads();
    uint64_t i = (uint64_t)blockIdx.x * blockDim.x + threadIdx.x;
    if (i < (n + 15) / 16) gcm_ctr_block(sbox, &k, sealed, n, i, out);
    __syncthreads();
    /* Shared memory outlives the block; do not leave round keys behind. */
    for (unsigned j = threadIdx.x; j < sizeof(k); j += blockDim.x) raw[j] = 0;
}

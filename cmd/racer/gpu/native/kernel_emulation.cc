// Runs the CUDA decrypt kernels (gcm_kernel.cu) on the CPU.
//
// Hosted CI has no GPU and no nvcc. This harness compiles the kernel source as
// C++ with one std::thread per CUDA thread, one block at a time, so
// __shared__ data is per block and __syncthreads is a real barrier. It uses
// the launch geometry from gpu_cuda.c and checks the kernels against the
// serial gcm_core.h path, which the Rust tests check against racer-crypto.
//
// Build and run: run-kernel-emulation.sh OUTPUT_DIR
#include <barrier>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <functional>
#include <memory>
#include <thread>
#include <vector>

#define __CUDACC__ 1
#define __host__
#define __device__
#define __global__
#define __shared__ static
struct emu_dim3 {
    unsigned x, y, z;
};
static thread_local emu_dim3 threadIdx;
static thread_local emu_dim3 blockIdx;
static thread_local emu_dim3 blockDim;
static std::barrier<> *emu_barrier;
static void __syncthreads() { emu_barrier->arrive_and_wait(); }

#include "gcm_kernel.cu"

static const uint8_t sbox[256] = GCM_SBOX_INIT;
static int failures;

#define CHECK(cond, ...)                                     \
    do {                                                     \
        if (!(cond)) {                                       \
            std::fprintf(stderr, "FAIL %s:%d: ", __FILE__, __LINE__); \
            std::fprintf(stderr, __VA_ARGS__);               \
            std::fprintf(stderr, "\n");                      \
            ++failures;                                      \
        }                                                    \
    } while (0)

// Emulate <<<grid, threads>>>.
static void launch(uint64_t grid, unsigned threads, const std::function<void()> &kernel) {
    for (uint64_t b = 0; b < grid; ++b) {
        auto barrier = std::make_unique<std::barrier<>>(threads);
        emu_barrier = barrier.get();
        std::vector<std::thread> pool;
        for (unsigned t = 0; t < threads; ++t)
            pool.emplace_back([&, t] {
                threadIdx = {t, 0, 0};
                blockIdx = {(unsigned)b, 0, 0};
                blockDim = {threads, 1, 1};
                kernel();
            });
        for (auto &thread : pool) thread.join();
        emu_barrier = nullptr;
    }
}

// The gpu_cuda.c sequence: partials, finalize, ctr.
static gcm_verdict run(const gcm_key &key, const std::vector<uint8_t> &sealed, uint64_t n,
                       uint32_t check, uint64_t expected, std::vector<uint8_t> &out) {
    uint64_t chunks = gcm_chunks(n);
    std::vector<gcm_partial> partials(chunks);
    gcm_verdict verdict = {};
    launch((chunks + 127) / 128, 128,
           [&] { racer_gcm_partials(sealed.data(), n, &key, partials.data(), chunks); });
    launch(1, 256, [&] {
        racer_gcm_finalize(sealed.data(), n, &key, partials.data(), chunks, check, expected, &verdict);
    });
    uint64_t blocks = (n + 15) / 16;
    if (blocks)
        launch((blocks + 255) / 256, 256,
               [&] { racer_gcm_ctr(sealed.data(), n, &key, &verdict, out.data()); });
    return verdict;
}

int main() {
    uint64_t state = 0x243f6a8885a308d3ull;
    auto next = [&] {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        return (uint8_t)state;
    };
    uint64_t table[256];
    gcm_crc64_table(table);
    for (uint64_t n : {0ull, 1ull, 15ull, 16ull, 17ull, 4079ull, 4080ull, 4081ull, 8192ull,
                       20000ull, 70001ull}) {
        uint8_t raw_key[32], nonce[12], aad[77];
        for (auto &b : raw_key) b = next();
        for (auto &b : nonce) b = next();
        for (auto &b : aad) b = next();
        std::vector<uint8_t> plaintext(n), sealed(n + 16);
        for (auto &b : plaintext) b = next();
        gcm_key key;
        gcm_prepare(sbox, raw_key, nonce, aad, sizeof(aad), n, &key);

        // Seal: CTR is its own inverse, then the tag from the serial core.
        for (uint64_t i = 0; i < (n + 15) / 16; ++i) gcm_ctr_block(sbox, &key, plaintext.data(), n, i, sealed.data());
        gcm_block s = gcm_load(key.prefix);
        for (uint64_t j = 0; j < gcm_chunks(n); ++j) {
            gcm_partial p;
            gcm_chunk_partial(table, &key, sealed.data(), n, j, &p);
            gcm_block g = gcm_load(p.ghash);
            s.hi ^= g.hi;
            s.lo ^= g.lo;
        }
        uint8_t tag[16];
        gcm_store(tag, s);
        for (int i = 0; i < 16; ++i) sealed[n + i] = tag[i] ^ key.ekj0[i];
        uint64_t crc = gcm_crc64(table, sealed.data(), n + 16);

        std::vector<uint8_t> out(n, 0xa5);
        gcm_verdict v = run(key, sealed, n, 1, crc, out);
        CHECK(v.status == GCM_VERDICT_PASS && v.crc == crc, "n=%llu pass status=%u", (unsigned long long)n, v.status);
        CHECK(out == plaintext, "n=%llu plaintext", (unsigned long long)n);

        std::vector<uint8_t> untouched(n, 0xa5);
        out = untouched;
        v = run(key, sealed, n, 1, crc ^ 1, out);
        CHECK(v.status == GCM_VERDICT_CRC && out == untouched, "n=%llu crc", (unsigned long long)n);

        auto tampered = sealed;
        tampered[n / 2] ^= 4;
        v = run(key, tampered, n, 0, 0, out);
        CHECK(v.status == GCM_VERDICT_TAG && out == untouched, "n=%llu tag", (unsigned long long)n);
    }
    if (failures) {
        std::fprintf(stderr, "%d kernel emulation failures\n", failures);
        return 1;
    }
    std::printf("kernel emulation passed\n");
    return 0;
}

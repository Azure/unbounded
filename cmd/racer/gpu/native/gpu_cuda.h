/* C ABI of libracer_gpu_cuda.so.1, loaded by racer-gpu (src/ffi.rs) with
 * dlopen. The simulation backend (src/simulation.rs) implements the same
 * functions in Rust.
 *
 * Bump RACER_GPU_ABI_VERSION whenever a struct, signature, or result
 * contract changes. Rust refuses any other version.
 *
 * Every int result is RACER_GPU_OK or one of the negative codes below. A
 * failed free leaves the resource in place and returns a code; Rust then
 * leaks the resource on purpose because the GPU may still use it. Every call
 * that takes an offset and length checks it against the buffer size, so the
 * shim never touches memory outside a buffer. */
#ifndef RACER_GPU_CUDA_H
#define RACER_GPU_CUDA_H

#include <stddef.h>
#include <stdint.h>

#define RACER_GPU_ABI_VERSION 2u

#define RACER_GPU_OK 0
#define RACER_GPU_EINVAL (-1)       /* bad argument or handle type */
#define RACER_GPU_ERANGE (-2)       /* offset or length outside a buffer */
#define RACER_GPU_EUNAVAILABLE (-3) /* driver library missing or init failed */
#define RACER_GPU_EDEVICE (-4)      /* allocation or buffer on another device */
#define RACER_GPU_EDRIVER (-5)      /* CUDA, NVML or cuFile call failed */
#define RACER_GPU_ENOMEM (-6)       /* host or device allocation failed */
#define RACER_GPU_ENOKERNEL (-7)    /* shim built without decrypt kernels */
#define RACER_GPU_EIO (-8)          /* cuFile read failed or was short */
#define RACER_GPU_EBUSY (-9)        /* wrong state, for example no verdict to read */
#define RACER_GPU_ELEAKED (-10)     /* failed, and cleanup leaked an internal resource */
#define RACER_GPU_EUNSUPPORTED (-11) /* cuFile does not support this file or its device */

/* Device capability bits in racer_gpu_device.flags. */
#define RACER_GPU_FLAG_VMM 1u      /* virtual memory management */
#define RACER_GPU_FLAG_POSIX_FD 2u /* POSIX fd shareable handles */
#define RACER_GPU_FLAG_DMA_BUF 4u  /* dma-buf export */
#define RACER_GPU_FLAG_GDR 8u      /* GPUDirect RDMA */

/* CUcomputemode values (cuda.h). */
#define RACER_GPU_COMPUTE_DEFAULT 0
#define RACER_GPU_COMPUTE_PROHIBITED 2
#define RACER_GPU_COMPUTE_EXCLUSIVE_PROCESS 3

/* One CUDA device. Mirrors `RawDevice` in ffi.rs. */
struct racer_gpu_device {
    uint8_t uuid[16];
    char pci_bus_id[32]; /* NUL terminated, for example "0000:3b:00.0" */
    int32_t ordinal;
    int32_t numa_node; /* -1 when unknown */
    int32_t compute_mode;
    uint32_t flags;
    uint64_t total_memory;
};

/* One page to verify and decrypt. Mirrors `RawDecrypt` in ffi.rs.
 * `key` and `nonce` are the XAES-derived AES-256-GCM key and nonce. The
 * source holds `length` ciphertext bytes then the 16-byte tag. */
struct racer_gpu_decrypt {
    uint8_t key[32];
    uint8_t nonce[12];
    uint32_t check_crc; /* 1 to compare CRC-64/XZ with expected_crc */
    uint64_t expected_crc;
    uint64_t length;
    const uint8_t *aad;
    uint64_t aad_length;
};

/* Mirrors `RawVerdict` in gcm.rs and `struct gcm_verdict`. */
struct racer_gpu_verdict {
    uint32_t status;
    uint32_t reserved;
    uint64_t crc;
};

_Static_assert(sizeof(struct racer_gpu_device) == 72, "device ABI size");
_Static_assert(offsetof(struct racer_gpu_device, ordinal) == 48, "device ABI offset");
_Static_assert(offsetof(struct racer_gpu_device, total_memory) == 64, "device ABI offset");
_Static_assert(sizeof(struct racer_gpu_decrypt) == 80, "decrypt ABI size");
_Static_assert(offsetof(struct racer_gpu_decrypt, check_crc) == 44, "decrypt ABI offset");
_Static_assert(offsetof(struct racer_gpu_decrypt, aad) == 64, "decrypt ABI offset");
_Static_assert(sizeof(struct racer_gpu_verdict) == 16, "verdict ABI size");

/* Largest page one decrypt call accepts (1 GiB). */
#define RACER_GPU_MAX_DECRYPT (1ull << 30)
/* Largest AAD one decrypt call accepts. */
#define RACER_GPU_MAX_AAD 4096u

uint32_t racer_gpu_abi(void);
/* CUresult, NVML or cuFile status of this thread's last failed driver call. */
int32_t racer_gpu_last_driver_error(void);
/* Load libcuda.so.1 and call cuInit. Safe to call more than once. */
int racer_gpu_init(void);
/* Fill up to `capacity` devices. Returns the total count, which may exceed
 * `capacity`, or a negative code. Reports every device, including ones in
 * EXCLUSIVE_PROCESS or PROHIBITED mode; Rust decides which are usable. */
int racer_gpu_discover(struct racer_gpu_device *out, uint32_t capacity);

/* Retain the device's primary context. */
int racer_gpu_context(int32_t ordinal, void **out);
int racer_gpu_context_free(void *context);

int racer_gpu_stream(void *context, void **out);
int racer_gpu_stream_sync(void *stream);
/* Synchronizes, then frees the stream and its decrypt workspace. */
int racer_gpu_stream_free(void *stream);

/* Import a POSIX fd shareable handle of `size` bytes, check it lives on the
 * context's device, map all of it and grant this context read-write access.
 * The fd stays owned by the caller. */
int racer_gpu_import(void *context, int fd, uint64_t size, void **out);
/* Unmap, free the address range and release the handle. The caller must
 * have synchronized every stream that used it. */
int racer_gpu_import_free(void *allocation);

/* Allocate device staging memory, page aligned, usable with cuFile and
 * dma-buf export. `size` is rounded up to 2 MiB. */
int racer_gpu_staging(void *context, uint64_t size, void **out);
int racer_gpu_staging_free(void *staging);

/* Queue a copy of host bytes into an import or staging buffer. The copy is
 * asynchronous: for page-locked memory CUDA may still read the host buffer
 * after this returns, so the caller must keep it alive and unchanged until
 * the stream is synchronized. The Rust wrapper owns the source until then. */
int racer_gpu_copy_h2d(void *stream, void *dst, uint64_t offset, const void *src, uint64_t length);
/* Queue a device copy. `src` must be a staging buffer. */
int racer_gpu_copy_d2d(void *stream, void *dst, uint64_t dst_offset, void *src, uint64_t src_offset,
                       uint64_t length);
/* Let `dst` read memory owned by `src` (cuCtxEnablePeerAccess). Sets
 * *access to 1 when `dst` can now read `src` staging memory and 0 when the
 * pair has no peer path, which is not an error. The same context, or two
 * contexts on one device, report 1. Ordinals must be below
 * RACER_GPU_MAX_PEER_ORDINAL. */
#define RACER_GPU_MAX_PEER_ORDINAL 256
int racer_gpu_peer(void *dst, void *src, int *access);
/* Queue a device copy from a staging buffer on another context, which the
 * stream's context must have enabled with racer_gpu_peer. `dst` must be on
 * the stream's context, as for racer_gpu_copy_d2d. Use racer_gpu_copy_d2d
 * when both are on the stream's context; this returns RACER_GPU_EDEVICE. */
int racer_gpu_copy_peer(void *stream, void *dst, uint64_t dst_offset, void *src, uint64_t src_offset,
                        uint64_t length);
/* Copy staging bytes to the host and wait. Imports are never read. */
int racer_gpu_copy_d2h(void *stream, void *dst, void *src, uint64_t src_offset, uint64_t length);

/* Queue verify-then-decrypt of the sealed page at `src + src_offset` (a
 * staging buffer) into `dst + dst_offset`. The destination is written only if
 * the CRC (when requested) and the tag match; the decision is made on the
 * device. One decrypt per stream may be outstanding; read its verdict with
 * racer_gpu_verdict after racer_gpu_stream_sync. */
int racer_gpu_decrypt(void *stream, const struct racer_gpu_decrypt *params, void *src,
                      uint64_t src_offset, void *dst, uint64_t dst_offset);
int racer_gpu_verdict(void *stream, struct racer_gpu_verdict *out);

/* racer_gpu_file_open flags. */
#define RACER_GPU_FILE_PCI_P2PDMA 1u /* set cuFile use_pci_p2pdma before the driver opens */

/* Open the cuFile driver (libcufile.so.0). Safe to call more than once;
 * `flags` take effect only on the call that opens the driver. */
int racer_gpu_file_open(uint32_t flags);
/* Register an O_DIRECT fd with cuFile. The fd stays owned by the caller. */
int racer_gpu_file(int fd, void **out);
int racer_gpu_file_free(void *file);
/* Register or deregister a staging buffer with cuFile. */
int racer_gpu_staging_register(void *staging);
int racer_gpu_staging_unregister(void *staging);
/* Read `length` bytes at `file_offset` into staging at `offset`. Returns the
 * byte count (always `length` on success) or a negative code. */
int64_t racer_gpu_file_read(void *file, void *staging, uint64_t offset, uint64_t file_offset,
                            uint64_t length);
/* Export staging bytes as a dma-buf fd. Offset and length must be multiples
 * of the host page size. */
int racer_gpu_dmabuf(void *staging, uint64_t offset, uint64_t length, int *fd);

#endif

/* Thin C shim between racer-gpu (Rust) and the CUDA driver API, NVML and
 * cuFile. Rust loads it as libracer_gpu_cuda.so.1; see gpu_cuda.h for the
 * ABI and result contract.
 *
 * The shim itself dlopens libcuda.so.1, libnvidia-ml.so.1 and libcufile.so.0
 * with the declarations in cuda_private.h, so it builds without the CUDA
 * toolkit. The decrypt kernels are PTX embedded at build time when
 * RACER_GPU_PTX names a PTX file (see build-native-cuda.sh); without it, every
 * other call works and racer_gpu_decrypt returns RACER_GPU_ENOKERNEL.
 *
 * Every call runs on the dataplane's native helper thread. Each call pushes
 * the owning context and pops it before returning. */
#define _GNU_SOURCE
#include "gpu_cuda.h"
#include "cuda_private.h"
#include "gcm_core.h"

#include <dlfcn.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#ifdef RACER_GPU_PTX
/* Embed the PTX image, NUL terminated as cuModuleLoadData requires. */
__asm__(".section .rodata.racer_gpu_ptx,\"a\"\n"
        ".globl racer_gpu_ptx_image\n"
        ".hidden racer_gpu_ptx_image\n"
        "racer_gpu_ptx_image:\n"
        ".incbin \"" RACER_GPU_PTX "\"\n"
        ".byte 0\n"
        ".previous\n");
extern const char racer_gpu_ptx_image[];
#define PTX_IMAGE racer_gpu_ptx_image
#else
#define PTX_IMAGE NULL
#endif

/* Function pointer types and tables for each library. */
#define X_TYPE(name, symbol, ret, args) typedef ret(*name##_fn) args;
#define X_FIELD(name, symbol, ret, args) name##_fn name;
#define X_LOAD(name, symbol, ret, args) \
    if (!(table.name = (name##_fn)dlsym(table.lib, symbol))) goto missing;
RACER_CUDA_FUNCTIONS(X_TYPE)
RACER_NVML_FUNCTIONS(X_TYPE)
RACER_CUFILE_FUNCTIONS(X_TYPE)

struct cuda_table { void *lib; RACER_CUDA_FUNCTIONS(X_FIELD) };
struct nvml_table { void *lib; RACER_NVML_FUNCTIONS(X_FIELD) };
struct cufile_table { void *lib; int opened; RACER_CUFILE_FUNCTIONS(X_FIELD) };

static struct cuda_table cu;
static struct cufile_table cf;
static pthread_mutex_t load_lock = PTHREAD_MUTEX_INITIALIZER;
static __thread int32_t last_error;

static const uint8_t shim_sbox[256] = GCM_SBOX_INIT;

#define KIND_IMPORT 0x494d5054u
#define KIND_STAGING 0x53544147u
#define STAGING_ALIGN (2ull << 20)
/* Workspace layout: key, verdict, then one partial per chunk. */
#define WORK_KEY 0u
#define WORK_VERDICT 512u
#define WORK_PARTIALS 1024u
#define PARTIAL_THREADS 128u
#define FINALIZE_THREADS 256u
#define CTR_THREADS 256u

_Static_assert(sizeof(struct gcm_key) <= WORK_VERDICT, "workspace key slot");
_Static_assert(sizeof(struct gcm_partial) == 32, "partial layout");
_Static_assert(sizeof(struct gcm_verdict) == sizeof(struct racer_gpu_verdict), "verdict layout");

struct gpu_context {
    CUcontext ctx;
    CUdevice dev;
    int32_t ordinal;
    int module_state; /* 0 untried, 1 loaded, -1 unavailable */
    CUmodule module;
    CUfunction partials, finalize, ctr;
};

struct gpu_buffer {
    uint32_t kind;
    struct gpu_context *context;
    CUdeviceptr base;
    uint64_t size;
};

struct gpu_import {
    struct gpu_buffer b;
    CUmemGenericAllocationHandle handle;
    int has_handle, reserved, mapped;
};

struct gpu_staging {
    struct gpu_buffer b;
    uint64_t allocated;
    int registered;
};

struct gpu_stream {
    struct gpu_context *context;
    CUstream stream;
    CUdeviceptr work;
    uint64_t work_size;
    struct racer_gpu_verdict *verdict; /* pinned host memory */
    int decrypt;                      /* 0 none, 1 queued, 2 synchronized */
};

struct gpu_file {
    CUfileHandle_t handle;
};

uint32_t racer_gpu_abi(void) { return RACER_GPU_ABI_VERSION; }

int32_t racer_gpu_last_driver_error(void) { return last_error; }

/* Map a driver result to a shim code, remembering the raw value. */
static int drv(CUresult result) {
    if (result == CUDA_SUCCESS) return RACER_GPU_OK;
    last_error = result;
    return RACER_GPU_EDRIVER;
}

static void wipe(void *p, size_t n) {
    volatile uint8_t *v = (volatile uint8_t *)p;
    while (n--) *v++ = 0;
}

int racer_gpu_init(void) {
    int rc = RACER_GPU_OK;
    pthread_mutex_lock(&load_lock);
    if (!cu.lib) {
        struct cuda_table table;
        memset(&table, 0, sizeof(table));
        table.lib = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
        if (!table.lib) {
            rc = RACER_GPU_EUNAVAILABLE;
            goto out;
        }
        RACER_CUDA_FUNCTIONS(X_LOAD)
        if (drv(table.cuInit(0))) {
            dlclose(table.lib);
            rc = RACER_GPU_EUNAVAILABLE;
            goto out;
        }
        cu = table;
        goto out;
    missing:
        dlclose(table.lib);
        rc = RACER_GPU_EUNAVAILABLE;
    }
out:
    pthread_mutex_unlock(&load_lock);
    return rc;
}

/* Load NVML for one discovery. Returns 0 when usable. */
static int nvml_open(struct nvml_table *out) {
    struct nvml_table table;
    memset(&table, 0, sizeof(table));
    table.lib = dlopen("libnvidia-ml.so.1", RTLD_NOW | RTLD_LOCAL);
    if (!table.lib) return -1;
    RACER_NVML_FUNCTIONS(X_LOAD)
    if (table.nvmlInit() != NVML_SUCCESS) goto missing;
    *out = table;
    return 0;
missing:
    dlclose(table.lib);
    return -1;
}

/* NUMA node from sysfs for a lowercase PCI address, or -1. */
static int32_t sysfs_numa(const char *pci) {
    char path[96];
    int node = -1;
    if (snprintf(path, sizeof(path), "/sys/bus/pci/devices/%s/numa_node", pci) >= (int)sizeof(path))
        return -1;
    FILE *f = fopen(path, "re");
    if (!f) return -1;
    if (fscanf(f, "%d", &node) != 1) node = -1;
    fclose(f);
    return node < 0 ? -1 : node;
}

static int describe(int ordinal, struct racer_gpu_device *d, struct nvml_table *nvml, int have_nvml) {
    CUdevice dev;
    CUuuid uuid;
    size_t total = 0;
    int value = 0;
    memset(d, 0, sizeof(*d));
    d->ordinal = ordinal;
    d->numa_node = -1;
    d->compute_mode = -1;
    if (drv(cu.cuDeviceGet(&dev, ordinal)) || drv(cu.cuDeviceGetUuid(&uuid, dev)) ||
        drv(cu.cuDeviceGetPCIBusId(d->pci_bus_id, sizeof(d->pci_bus_id), dev)) ||
        drv(cu.cuDeviceTotalMem(&total, dev)))
        return -1;
    d->pci_bus_id[sizeof(d->pci_bus_id) - 1] = 0;
    for (char *p = d->pci_bus_id; *p; ++p)
        if (*p >= 'A' && *p <= 'F') *p = (char)(*p - 'A' + 'a');
    memcpy(d->uuid, uuid.bytes, 16);
    d->total_memory = total;
    static const struct { int attribute; uint32_t flag; } caps[] = {
        {CU_DEVICE_ATTRIBUTE_VIRTUAL_MEMORY_MANAGEMENT_SUPPORTED, RACER_GPU_FLAG_VMM},
        {CU_DEVICE_ATTRIBUTE_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR_SUPPORTED, RACER_GPU_FLAG_POSIX_FD},
        {CU_DEVICE_ATTRIBUTE_DMA_BUF_SUPPORTED, RACER_GPU_FLAG_DMA_BUF},
        {CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_SUPPORTED, RACER_GPU_FLAG_GDR},
    };
    for (size_t i = 0; i < sizeof(caps) / sizeof(caps[0]); ++i)
        if (!cu.cuDeviceGetAttribute(&value, caps[i].attribute, dev) && value) d->flags |= caps[i].flag;
    unsigned int node;
    nvmlDevice_t handle;
    if (have_nvml && nvml->nvmlDeviceGetHandleByPciBusId(d->pci_bus_id, &handle) == NVML_SUCCESS &&
        nvml->nvmlDeviceGetNumaNodeId(handle, &node) == NVML_SUCCESS && node <= INT32_MAX)
        d->numa_node = (int32_t)node;
    else
        d->numa_node = sysfs_numa(d->pci_bus_id);
    /* Last, so any earlier failure leaves the device unusable (-1). */
    if (drv(cu.cuDeviceGetAttribute(&value, CU_DEVICE_ATTRIBUTE_COMPUTE_MODE, dev))) return -1;
    d->compute_mode = value;
    return 0;
}

int racer_gpu_discover(struct racer_gpu_device *out, uint32_t capacity) {
    int count = 0;
    struct nvml_table nvml;
    if (!cu.lib) return RACER_GPU_EUNAVAILABLE;
    if (drv(cu.cuDeviceGetCount(&count))) return RACER_GPU_EDRIVER;
    int have_nvml = nvml_open(&nvml) == 0;
    for (int i = 0; i < count && (uint32_t)i < capacity; ++i) (void)describe(i, &out[i], &nvml, have_nvml);
    if (have_nvml) {
        (void)nvml.nvmlShutdown();
        dlclose(nvml.lib);
    }
    return count;
}

static int push(struct gpu_context *c) { return drv(cu.cuCtxPushCurrent(c->ctx)); }

static void pop(void) {
    CUcontext previous;
    (void)cu.cuCtxPopCurrent(&previous);
}

int racer_gpu_context(int32_t ordinal, void **out) {
    if (!cu.lib) return RACER_GPU_EUNAVAILABLE;
    if (!out || ordinal < 0) return RACER_GPU_EINVAL;
    *out = NULL;
    struct gpu_context *c = calloc(1, sizeof(*c));
    if (!c) return RACER_GPU_ENOMEM;
    c->ordinal = ordinal;
    int rc = drv(cu.cuDeviceGet(&c->dev, ordinal));
    if (!rc) rc = drv(cu.cuDevicePrimaryCtxRetain(&c->ctx, c->dev));
    if (rc) {
        free(c);
        return rc;
    }
    *out = c;
    return RACER_GPU_OK;
}

int racer_gpu_context_free(void *context) {
    struct gpu_context *c = context;
    if (!c) return RACER_GPU_EINVAL;
    if (c->module_state == 1) {
        int rc = push(c);
        if (rc) return rc;
        rc = drv(cu.cuModuleUnload(c->module));
        pop();
        if (rc) return rc;
        c->module_state = 0;
    }
    int rc = drv(cu.cuDevicePrimaryCtxRelease(c->dev));
    if (!rc) free(c);
    return rc;
}

/* Load the decrypt kernels into a context. Context must be current. */
static int kernels(struct gpu_context *c) {
    if (c->module_state == 0) {
        const char *image = PTX_IMAGE;
        c->module_state = -1;
        if (!image) return RACER_GPU_ENOKERNEL;
        if (drv(cu.cuModuleLoadData(&c->module, image))) return RACER_GPU_ENOKERNEL;
        if (drv(cu.cuModuleGetFunction(&c->partials, c->module, "racer_gcm_partials")) ||
            drv(cu.cuModuleGetFunction(&c->finalize, c->module, "racer_gcm_finalize")) ||
            drv(cu.cuModuleGetFunction(&c->ctr, c->module, "racer_gcm_ctr"))) {
            /* Keep it as loaded (state 1) if unload fails so free retries. */
            if (drv(cu.cuModuleUnload(c->module))) c->module_state = 1;
            return RACER_GPU_ENOKERNEL;
        }
        c->module_state = 1;
    }
    return c->module_state == 1 && c->ctr ? RACER_GPU_OK : RACER_GPU_ENOKERNEL;
}

int racer_gpu_stream(void *context, void **out) {
    struct gpu_context *c = context;
    if (!c || !out) return RACER_GPU_EINVAL;
    *out = NULL;
    struct gpu_stream *s = calloc(1, sizeof(*s));
    if (!s) return RACER_GPU_ENOMEM;
    s->context = c;
    int rc = push(c);
    if (rc) {
        free(s);
        return rc;
    }
    rc = drv(cu.cuStreamCreate(&s->stream, CU_STREAM_NON_BLOCKING));
    if (!rc) {
        rc = drv(cu.cuMemAllocHost((void **)&s->verdict, sizeof(*s->verdict)));
        if (rc && drv(cu.cuStreamDestroy(s->stream))) rc = RACER_GPU_ELEAKED;
    }
    pop();
    if (rc) {
        free(s);
        return rc;
    }
    memset(s->verdict, 0, sizeof(*s->verdict));
    *out = s;
    return RACER_GPU_OK;
}

int racer_gpu_stream_sync(void *stream) {
    struct gpu_stream *s = stream;
    if (!s) return RACER_GPU_EINVAL;
    int rc = push(s->context);
    if (rc) return rc;
    rc = drv(cu.cuStreamSynchronize(s->stream));
    pop();
    if (!rc && s->decrypt == 1) s->decrypt = 2;
    return rc;
}

int racer_gpu_stream_free(void *stream) {
    struct gpu_stream *s = stream;
    if (!s) return RACER_GPU_EINVAL;
    int rc = push(s->context);
    if (rc) return rc;
    rc = drv(cu.cuStreamSynchronize(s->stream));
    if (!rc && s->work) {
        rc = drv(cu.cuMemFree(s->work));
        if (!rc) s->work = 0, s->work_size = 0;
    }
    if (!rc && s->verdict) {
        rc = drv(cu.cuMemFreeHost(s->verdict));
        if (!rc) s->verdict = NULL;
    }
    if (!rc) rc = drv(cu.cuStreamDestroy(s->stream));
    pop();
    if (!rc) free(s);
    return rc;
}

/* Undo an import as far as possible. Context must be current. */
static int import_release(struct gpu_import *a) {
    if (a->mapped) {
        if (drv(cu.cuMemUnmap(a->b.base, a->b.size))) return RACER_GPU_EDRIVER;
        a->mapped = 0;
    }
    if (a->reserved) {
        if (drv(cu.cuMemAddressFree(a->b.base, a->b.size))) return RACER_GPU_EDRIVER;
        a->reserved = 0;
    }
    if (a->has_handle) {
        if (drv(cu.cuMemRelease(a->handle))) return RACER_GPU_EDRIVER;
        a->has_handle = 0;
    }
    return RACER_GPU_OK;
}

int racer_gpu_import(void *context, int fd, uint64_t size, void **out) {
    struct gpu_context *c = context;
    if (!c || fd < 0 || !size || size > SIZE_MAX || !out) return RACER_GPU_EINVAL;
    *out = NULL;
    struct gpu_import *a = calloc(1, sizeof(*a));
    if (!a) return RACER_GPU_ENOMEM;
    a->b.kind = KIND_IMPORT;
    a->b.context = c;
    a->b.size = size;
    int rc = push(c);
    if (rc) {
        free(a);
        return rc;
    }
    CUmemAllocationProp prop;
    size_t granularity = 0;
    memset(&prop, 0, sizeof(prop));
    rc = drv(cu.cuMemImportFromShareableHandle(&a->handle, (void *)(uintptr_t)fd,
                                               CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR));
    if (rc) goto fail;
    a->has_handle = 1;
    rc = drv(cu.cuMemGetAllocationPropertiesFromHandle(&prop, a->handle));
    if (rc) goto fail;
    if (prop.location.type != CU_MEM_LOCATION_TYPE_DEVICE || prop.location.id != c->ordinal) {
        rc = RACER_GPU_EDEVICE;
        goto fail;
    }
    rc = drv(cu.cuMemGetAllocationGranularity(&granularity, &prop, CU_MEM_ALLOC_GRANULARITY_MINIMUM));
    if (rc) goto fail;
    if (!granularity || size % granularity) {
        rc = RACER_GPU_EINVAL;
        goto fail;
    }
    rc = drv(cu.cuMemAddressReserve(&a->b.base, size, 0, 0, 0));
    if (rc) goto fail;
    a->reserved = 1;
    /* Fails if `size` exceeds the allocation, so we never map past it. */
    rc = drv(cu.cuMemMap(a->b.base, size, 0, a->handle, 0));
    if (rc) goto fail;
    a->mapped = 1;
    CUmemAccessDesc access;
    access.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    access.location.id = c->ordinal;
    access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
    rc = drv(cu.cuMemSetAccess(a->b.base, size, &access, 1));
    if (rc) goto fail;
    pop();
    *out = a;
    return RACER_GPU_OK;
fail:
    if (import_release(a)) rc = RACER_GPU_ELEAKED;
    else free(a);
    pop();
    return rc;
}

int racer_gpu_import_free(void *allocation) {
    struct gpu_import *a = allocation;
    if (!a || a->b.kind != KIND_IMPORT) return RACER_GPU_EINVAL;
    int rc = push(a->b.context);
    if (rc) return rc;
    rc = import_release(a);
    pop();
    if (!rc) {
        a->b.kind = 0;
        free(a);
    }
    return rc;
}

int racer_gpu_staging(void *context, uint64_t size, void **out) {
    struct gpu_context *c = context;
    if (!c || !size || size > UINT64_MAX - STAGING_ALIGN || !out) return RACER_GPU_EINVAL;
    *out = NULL;
    uint64_t allocated = (size + STAGING_ALIGN - 1) & ~(STAGING_ALIGN - 1);
    if (allocated > SIZE_MAX) return RACER_GPU_EINVAL;
    struct gpu_staging *b = calloc(1, sizeof(*b));
    if (!b) return RACER_GPU_ENOMEM;
    b->b.kind = KIND_STAGING;
    b->b.context = c;
    b->b.size = size;
    b->allocated = allocated;
    int rc = push(c);
    if (rc) {
        free(b);
        return rc;
    }
    rc = drv(cu.cuMemAlloc(&b->b.base, allocated));
    /* dma-buf export and cuFile want page alignment; 2 MiB sizes get 2 MiB
     * aligned addresses in practice, but check rather than assume. */
    if (!rc && b->b.base % 65536) rc = drv(cu.cuMemFree(b->b.base)) ? RACER_GPU_ELEAKED : RACER_GPU_EINVAL;
    pop();
    if (rc) {
        if (rc != RACER_GPU_ELEAKED) free(b);
        return rc;
    }
    *out = b;
    return RACER_GPU_OK;
}

int racer_gpu_staging_free(void *staging) {
    struct gpu_staging *b = staging;
    if (!b || b->b.kind != KIND_STAGING) return RACER_GPU_EINVAL;
    if (b->registered) return RACER_GPU_EBUSY;
    int rc = push(b->b.context);
    if (rc) return rc;
    rc = drv(cu.cuMemFree(b->b.base));
    pop();
    if (!rc) {
        b->b.kind = 0;
        free(b);
    }
    return rc;
}

/* Nonempty [offset, offset + length) inside the buffer. */
static int span(const struct gpu_buffer *b, uint64_t offset, uint64_t length) {
    return length && offset <= b->size && length <= b->size - offset ? RACER_GPU_OK : RACER_GPU_ERANGE;
}

static int buffer(void *p, struct gpu_buffer **out) {
    struct gpu_buffer *b = p;
    if (!b || (b->kind != KIND_IMPORT && b->kind != KIND_STAGING)) return RACER_GPU_EINVAL;
    *out = b;
    return RACER_GPU_OK;
}

int racer_gpu_copy_h2d(void *stream, void *dst, uint64_t offset, const void *src, uint64_t length) {
    struct gpu_stream *s = stream;
    struct gpu_buffer *d;
    if (!s || !src || buffer(dst, &d)) return RACER_GPU_EINVAL;
    if (d->context != s->context) return RACER_GPU_EDEVICE;
    int rc = span(d, offset, length);
    if (rc) return rc;
    if ((rc = push(s->context))) return rc;
    rc = drv(cu.cuMemcpyHtoDAsync(d->base + offset, src, length, s->stream));
    pop();
    return rc;
}

int racer_gpu_copy_d2d(void *stream, void *dst, uint64_t dst_offset, void *src, uint64_t src_offset,
                       uint64_t length) {
    struct gpu_stream *s = stream;
    struct gpu_buffer *d, *from;
    if (!s || buffer(dst, &d) || buffer(src, &from) || from->kind != KIND_STAGING) return RACER_GPU_EINVAL;
    if (d->context != s->context || from->context != s->context) return RACER_GPU_EDEVICE;
    int rc = span(d, dst_offset, length);
    if (!rc) rc = span(from, src_offset, length);
    if (rc) return rc;
    if ((rc = push(s->context))) return rc;
    rc = drv(cu.cuMemcpyDtoDAsync(d->base + dst_offset, from->base + src_offset, length, s->stream));
    pop();
    return rc;
}

int racer_gpu_copy_d2h(void *stream, void *dst, void *src, uint64_t src_offset, uint64_t length) {
    struct gpu_stream *s = stream;
    struct gpu_buffer *from;
    if (!s || !dst || buffer(src, &from) || from->kind != KIND_STAGING) return RACER_GPU_EINVAL;
    if (from->context != s->context) return RACER_GPU_EDEVICE;
    int rc = span(from, src_offset, length);
    if (rc) return rc;
    if ((rc = push(s->context))) return rc;
    rc = drv(cu.cuMemcpyDtoHAsync(dst, from->base + src_offset, length, s->stream));
    if (!rc) rc = drv(cu.cuStreamSynchronize(s->stream));
    pop();
    if (!rc && s->decrypt == 1) s->decrypt = 2;
    return rc;
}

static int launch(CUfunction f, uint64_t grid, unsigned threads, CUstream stream, void **args) {
    return drv(cu.cuLaunchKernel(f, (unsigned)grid, 1, 1, threads, 1, 1, 0, stream, args, NULL));
}

int racer_gpu_decrypt(void *stream, const struct racer_gpu_decrypt *p, void *src, uint64_t src_offset,
                      void *dst, uint64_t dst_offset) {
    struct gpu_stream *s = stream;
    struct gpu_buffer *d, *from;
    if (!s || !p || buffer(dst, &d) || buffer(src, &from) || from->kind != KIND_STAGING)
        return RACER_GPU_EINVAL;
    if (p->length > RACER_GPU_MAX_DECRYPT || p->aad_length > RACER_GPU_MAX_AAD ||
        (!p->aad && p->aad_length) || p->check_crc > 1)
        return RACER_GPU_EINVAL;
    if (d->context != s->context || from->context != s->context) return RACER_GPU_EDEVICE;
    if (s->decrypt == 1) return RACER_GPU_EBUSY;
    uint64_t n = p->length;
    int rc = span(from, src_offset, n + 16);
    if (!rc && n) rc = span(d, dst_offset, n);
    if (!rc && !n && dst_offset > d->size) rc = RACER_GPU_ERANGE;
    if (rc) return rc;
    if ((rc = push(s->context))) return rc;
    if ((rc = kernels(s->context))) goto out;

    uint64_t chunks = gcm_chunks(n);
    uint64_t need = WORK_PARTIALS + chunks * sizeof(struct gcm_partial);
    if (s->work_size < need) {
        /* Earlier work may still read the old workspace. */
        if ((rc = drv(cu.cuStreamSynchronize(s->stream)))) goto out;
        if (s->work) {
            if ((rc = drv(cu.cuMemFree(s->work)))) goto out;
            s->work = 0;
            s->work_size = 0;
        }
        if ((rc = drv(cu.cuMemAlloc(&s->work, need)))) goto out;
        s->work_size = need;
    }

    struct gcm_key key;
    gcm_prepare(shim_sbox, p->key, p->nonce, p->aad, p->aad_length, n, &key);
    /* Pageable source: the driver has copied `key` when this returns. */
    rc = drv(cu.cuMemcpyHtoDAsync(s->work + WORK_KEY, &key, sizeof(key), s->stream));
    wipe(&key, sizeof(key));
    if (rc) goto out;
    s->decrypt = 1;
    CUdeviceptr sealed = from->base + src_offset, out_ptr = d->base + dst_offset;
    CUdeviceptr key_ptr = s->work + WORK_KEY, verdict = s->work + WORK_VERDICT;
    CUdeviceptr partials = s->work + WORK_PARTIALS;
    uint32_t check = p->check_crc;
    uint64_t expected = p->expected_crc;
    void *partial_args[] = {&sealed, &n, &key_ptr, &partials, &chunks};
    void *finalize_args[] = {&sealed, &n, &key_ptr, &partials, &chunks, &check, &expected, &verdict};
    void *ctr_args[] = {&sealed, &n, &key_ptr, &verdict, &out_ptr};
    uint64_t blocks = (n + 15) / 16;
    rc = drv(cu.cuMemsetD8Async(verdict, 0, sizeof(struct gcm_verdict), s->stream));
    if (!rc)
        rc = launch(s->context->partials, (chunks + PARTIAL_THREADS - 1) / PARTIAL_THREADS,
                    PARTIAL_THREADS, s->stream, partial_args);
    if (!rc) rc = launch(s->context->finalize, 1, FINALIZE_THREADS, s->stream, finalize_args);
    if (!rc && blocks)
        rc = launch(s->context->ctr, (blocks + CTR_THREADS - 1) / CTR_THREADS, CTR_THREADS, s->stream,
                    ctr_args);
    /* Always queue the wipe of key material and partials, even on failure. */
    int wiped = drv(cu.cuMemsetD8Async(key_ptr, 0, sizeof(struct gcm_key), s->stream));
    if (!wiped)
        wiped = drv(cu.cuMemsetD8Async(partials, 0, chunks * sizeof(struct gcm_partial), s->stream));
    if (!rc) rc = wiped;
    if (!rc)
        rc = drv(cu.cuMemcpyDtoHAsync(s->verdict, verdict, sizeof(*s->verdict), s->stream));
out:
    pop();
    return rc;
}

int racer_gpu_verdict(void *stream, struct racer_gpu_verdict *out) {
    struct gpu_stream *s = stream;
    if (!s || !out) return RACER_GPU_EINVAL;
    if (s->decrypt != 2) return RACER_GPU_EBUSY;
    *out = *s->verdict;
    memset(s->verdict, 0, sizeof(*s->verdict));
    s->decrypt = 0;
    return RACER_GPU_OK;
}

static int file_error(CUfileError_t e) {
    if (e.err == CU_FILE_SUCCESS) return RACER_GPU_OK;
    last_error = e.err;
    return RACER_GPU_EDRIVER;
}

int racer_gpu_file_open(void) {
    int rc = RACER_GPU_OK;
    pthread_mutex_lock(&load_lock);
    if (!cf.opened) {
        struct cufile_table table;
        memset(&table, 0, sizeof(table));
        table.lib = dlopen("libcufile.so.0", RTLD_NOW | RTLD_LOCAL);
        if (!table.lib) {
            rc = RACER_GPU_EUNAVAILABLE;
            goto out;
        }
        RACER_CUFILE_FUNCTIONS(X_LOAD)
        /* Without nvidia-fs or P2PDMA, cuFile's compatibility mode does a
         * POSIX read into a bounce buffer and a copy on this helper thread,
         * so every GPU job waits behind the disk. Racer's own host path does
         * that work on I/O threads. Turn it off unless the operator chose a
         * value with CUFILE_ALLOW_COMPAT_MODE; cuFileDriverOpen then fails
         * without GPUDirect Storage and Racer uses its host path. */
        cuFileSetParameterBool_fn set_bool =
            (cuFileSetParameterBool_fn)dlsym(table.lib, "cuFileSetParameterBool");
        if (set_bool && !getenv("CUFILE_ALLOW_COMPAT_MODE"))
            set_bool(CUFILE_PARAM_PROPERTIES_ALLOW_COMPAT_MODE, false);
        if (file_error(table.cuFileDriverOpen())) {
            dlclose(table.lib);
            rc = RACER_GPU_EUNAVAILABLE;
            goto out;
        }
        /* Left open for the life of the process. */
        table.opened = 1;
        cf = table;
        goto out;
    missing:
        dlclose(table.lib);
        rc = RACER_GPU_EUNAVAILABLE;
    }
out:
    pthread_mutex_unlock(&load_lock);
    return rc;
}

int racer_gpu_file(int fd, void **out) {
    if (!cf.opened) return RACER_GPU_EUNAVAILABLE;
    if (fd < 0 || !out) return RACER_GPU_EINVAL;
    *out = NULL;
    struct gpu_file *f = calloc(1, sizeof(*f));
    if (!f) return RACER_GPU_ENOMEM;
    CUfileDescr_t descr;
    memset(&descr, 0, sizeof(descr));
    descr.type = CU_FILE_HANDLE_TYPE_OPAQUE_FD;
    descr.handle.fd = fd;
    int rc = file_error(cf.cuFileHandleRegister(&f->handle, &descr));
    if (rc) {
        free(f);
        return rc;
    }
    *out = f;
    return RACER_GPU_OK;
}

int racer_gpu_file_free(void *file) {
    struct gpu_file *f = file;
    if (!f || !cf.opened) return RACER_GPU_EINVAL;
    cf.cuFileHandleDeregister(f->handle);
    free(f);
    return RACER_GPU_OK;
}

static int staging(void *p, struct gpu_staging **out) {
    struct gpu_staging *b = p;
    if (!b || b->b.kind != KIND_STAGING) return RACER_GPU_EINVAL;
    *out = b;
    return RACER_GPU_OK;
}

int racer_gpu_staging_register(void *p) {
    struct gpu_staging *b;
    if (!cf.opened) return RACER_GPU_EUNAVAILABLE;
    if (staging(p, &b)) return RACER_GPU_EINVAL;
    if (b->registered) return RACER_GPU_EBUSY;
    int rc = push(b->b.context);
    if (rc) return rc;
    rc = file_error(cf.cuFileBufRegister((const void *)(uintptr_t)b->b.base, b->allocated, 0));
    pop();
    if (!rc) b->registered = 1;
    return rc;
}

int racer_gpu_staging_unregister(void *p) {
    struct gpu_staging *b;
    if (!cf.opened || staging(p, &b)) return RACER_GPU_EINVAL;
    if (!b->registered) return RACER_GPU_EBUSY;
    int rc = push(b->b.context);
    if (rc) return rc;
    rc = file_error(cf.cuFileBufDeregister((const void *)(uintptr_t)b->b.base));
    pop();
    if (!rc) b->registered = 0;
    return rc;
}

int64_t racer_gpu_file_read(void *file, void *p, uint64_t offset, uint64_t file_offset, uint64_t length) {
    struct gpu_file *f = file;
    struct gpu_staging *b;
    if (!cf.opened) return RACER_GPU_EUNAVAILABLE;
    if (!f || staging(p, &b) || file_offset > (uint64_t)INT64_MAX - length) return RACER_GPU_EINVAL;
    int rc = span(&b->b, offset, length);
    if (rc) return rc;
    if ((rc = push(b->b.context))) return rc;
    ssize_t n = cf.cuFileRead(f->handle, (void *)(uintptr_t)b->b.base, length, (off_t)file_offset,
                              (off_t)offset);
    pop();
    if (n < 0) {
        last_error = (int32_t)n;
        return RACER_GPU_EIO;
    }
    return (uint64_t)n == length ? (int64_t)n : RACER_GPU_EIO;
}

int racer_gpu_dmabuf(void *p, uint64_t offset, uint64_t length, int *fd) {
    struct gpu_staging *b;
    long page = sysconf(_SC_PAGESIZE);
    if (staging(p, &b) || !fd || page <= 0) return RACER_GPU_EINVAL;
    if (!length || offset % (uint64_t)page || length % (uint64_t)page || offset > b->allocated ||
        length > b->allocated - offset)
        return RACER_GPU_ERANGE;
    *fd = -1;
    int rc = push(b->b.context);
    if (rc) return rc;
    int handle = -1;
    rc = drv(cu.cuMemGetHandleForAddressRange(&handle, b->b.base + offset, length,
                                              CU_MEM_RANGE_HANDLE_TYPE_DMA_BUF_FD, 0));
    pop();
    if (!rc) *fd = handle;
    return rc;
}

/* The few CUDA driver, NVML and cuFile declarations gpu_cuda.c needs.
 *
 * The shim loads every library with dlopen, so it builds without the CUDA
 * toolkit. Values and layouts are copied from the vendor headers:
 *
 *   cuda.h (CUDA 12.9, CUDA_VERSION 12090):
 *     https://gitlab.com/nvidia/headers/cuda-individual/cudart/-/raw/main/cuda.h
 *     https://docs.nvidia.com/cuda/cuda-driver-api/
 *   nvml.h:
 *     https://gitlab.com/nvidia/headers/cuda-individual/nvml_dev/-/raw/main/nvml.h
 *     https://docs.nvidia.com/deploy/nvml-api/
 *   cufile.h (GDS cuFile API reference, section 4.1):
 *     https://docs.nvidia.com/gpudirect-storage/api-reference-guide/index.html
 *
 * Symbol names include the version suffix cuda.h maps the plain name to (for
 * example cuMemAlloc -> cuMemAlloc_v2), because dlsym sees the real export.
 * Only append to this file; never change a value without the vendor header. */
#ifndef RACER_GPU_CUDA_PRIVATE_H
#define RACER_GPU_CUDA_PRIVATE_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <sys/types.h>

/* ---- CUDA driver API (cuda.h) ---- */
typedef int CUresult;                 /* enum cudaError_enum; CUDA_SUCCESS = 0 */
typedef int CUdevice;                 /* CUdevice_v1 */
typedef unsigned long long CUdeviceptr; /* CUdeviceptr_v2 on 64-bit hosts */
typedef unsigned long long CUmemGenericAllocationHandle;
typedef struct CUctx_st *CUcontext;
typedef struct CUstream_st *CUstream;
typedef struct CUmod_st *CUmodule;
typedef struct CUfunc_st *CUfunction;
typedef struct { char bytes[16]; } CUuuid;

#define CUDA_SUCCESS 0

/* CUdevice_attribute */
#define CU_DEVICE_ATTRIBUTE_COMPUTE_MODE 20
#define CU_DEVICE_ATTRIBUTE_VIRTUAL_MEMORY_MANAGEMENT_SUPPORTED 102
#define CU_DEVICE_ATTRIBUTE_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR_SUPPORTED 103
#define CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_SUPPORTED 116
#define CU_DEVICE_ATTRIBUTE_DMA_BUF_SUPPORTED 124

/* CUstream_flags */
#define CU_STREAM_NON_BLOCKING 0x1
/* CUmemAllocationHandleType */
#define CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR 0x1
/* CUmemAccess_flags */
#define CU_MEM_ACCESS_FLAGS_PROT_READWRITE 0x3
/* CUmemLocationType */
#define CU_MEM_LOCATION_TYPE_DEVICE 0x1
/* CUmemAllocationType */
#define CU_MEM_ALLOCATION_TYPE_PINNED 0x1
/* CUmemAllocationGranularity_flags */
#define CU_MEM_ALLOC_GRANULARITY_MINIMUM 0x0
/* CUmemRangeHandleType */
#define CU_MEM_RANGE_HANDLE_TYPE_DMA_BUF_FD 0x1

typedef struct {
    int type; /* CUmemLocationType */
    int id;
} CUmemLocation;

typedef struct {
    int type;                 /* CUmemAllocationType */
    int requestedHandleTypes; /* CUmemAllocationHandleType */
    CUmemLocation location;
    void *win32HandleMetaData;
    struct {
        unsigned char compressionType;
        unsigned char gpuDirectRDMACapable;
        unsigned short usage;
        unsigned char reserved[4];
    } allocFlags;
} CUmemAllocationProp;

typedef struct {
    CUmemLocation location;
    int flags; /* CUmemAccess_flags */
} CUmemAccessDesc;

_Static_assert(sizeof(CUmemLocation) == 8, "CUmemLocation layout");
_Static_assert(sizeof(CUmemAllocationProp) == 32, "CUmemAllocationProp layout");
_Static_assert(offsetof(CUmemAllocationProp, win32HandleMetaData) == 16, "CUmemAllocationProp layout");
_Static_assert(sizeof(CUmemAccessDesc) == 12, "CUmemAccessDesc layout");

/* X(name, symbol, return, arguments) for every driver function. */
#define RACER_CUDA_FUNCTIONS(X) \
    X(cuInit, "cuInit", CUresult, (unsigned int flags)) \
    X(cuDeviceGetCount, "cuDeviceGetCount", CUresult, (int *count)) \
    X(cuDeviceGet, "cuDeviceGet", CUresult, (CUdevice *device, int ordinal)) \
    X(cuDeviceGetUuid, "cuDeviceGetUuid_v2", CUresult, (CUuuid *uuid, CUdevice dev)) \
    X(cuDeviceGetPCIBusId, "cuDeviceGetPCIBusId", CUresult, (char *id, int len, CUdevice dev)) \
    X(cuDeviceGetAttribute, "cuDeviceGetAttribute", CUresult, (int *pi, int attrib, CUdevice dev)) \
    X(cuDeviceTotalMem, "cuDeviceTotalMem_v2", CUresult, (size_t *bytes, CUdevice dev)) \
    X(cuDevicePrimaryCtxRetain, "cuDevicePrimaryCtxRetain", CUresult, (CUcontext *ctx, CUdevice dev)) \
    X(cuDevicePrimaryCtxRelease, "cuDevicePrimaryCtxRelease_v2", CUresult, (CUdevice dev)) \
    X(cuCtxPushCurrent, "cuCtxPushCurrent_v2", CUresult, (CUcontext ctx)) \
    X(cuCtxPopCurrent, "cuCtxPopCurrent_v2", CUresult, (CUcontext *ctx)) \
    X(cuMemImportFromShareableHandle, "cuMemImportFromShareableHandle", CUresult, \
      (CUmemGenericAllocationHandle *handle, void *os_handle, int type)) \
    X(cuMemGetAllocationPropertiesFromHandle, "cuMemGetAllocationPropertiesFromHandle", CUresult, \
      (CUmemAllocationProp *prop, CUmemGenericAllocationHandle handle)) \
    X(cuMemGetAllocationGranularity, "cuMemGetAllocationGranularity", CUresult, \
      (size_t *granularity, const CUmemAllocationProp *prop, int option)) \
    X(cuMemAddressReserve, "cuMemAddressReserve", CUresult, \
      (CUdeviceptr *ptr, size_t size, size_t alignment, CUdeviceptr addr, unsigned long long flags)) \
    X(cuMemAddressFree, "cuMemAddressFree", CUresult, (CUdeviceptr ptr, size_t size)) \
    X(cuMemMap, "cuMemMap", CUresult, \
      (CUdeviceptr ptr, size_t size, size_t offset, CUmemGenericAllocationHandle handle, \
       unsigned long long flags)) \
    X(cuMemUnmap, "cuMemUnmap", CUresult, (CUdeviceptr ptr, size_t size)) \
    X(cuMemSetAccess, "cuMemSetAccess", CUresult, \
      (CUdeviceptr ptr, size_t size, const CUmemAccessDesc *desc, size_t count)) \
    X(cuMemRelease, "cuMemRelease", CUresult, (CUmemGenericAllocationHandle handle)) \
    X(cuMemAlloc, "cuMemAlloc_v2", CUresult, (CUdeviceptr *ptr, size_t size)) \
    X(cuMemFree, "cuMemFree_v2", CUresult, (CUdeviceptr ptr)) \
    X(cuMemAllocHost, "cuMemAllocHost_v2", CUresult, (void **ptr, size_t size)) \
    X(cuMemFreeHost, "cuMemFreeHost", CUresult, (void *ptr)) \
    X(cuMemcpyHtoDAsync, "cuMemcpyHtoDAsync_v2", CUresult, \
      (CUdeviceptr dst, const void *src, size_t n, CUstream stream)) \
    X(cuMemcpyDtoDAsync, "cuMemcpyDtoDAsync_v2", CUresult, \
      (CUdeviceptr dst, CUdeviceptr src, size_t n, CUstream stream)) \
    X(cuMemcpyDtoHAsync, "cuMemcpyDtoHAsync_v2", CUresult, \
      (void *dst, CUdeviceptr src, size_t n, CUstream stream)) \
    X(cuMemsetD8Async, "cuMemsetD8Async", CUresult, \
      (CUdeviceptr dst, unsigned char value, size_t n, CUstream stream)) \
    X(cuStreamCreate, "cuStreamCreate", CUresult, (CUstream *stream, unsigned int flags)) \
    X(cuStreamSynchronize, "cuStreamSynchronize", CUresult, (CUstream stream)) \
    X(cuStreamDestroy, "cuStreamDestroy_v2", CUresult, (CUstream stream)) \
    X(cuModuleLoadData, "cuModuleLoadData", CUresult, (CUmodule *module, const void *image)) \
    X(cuModuleUnload, "cuModuleUnload", CUresult, (CUmodule module)) \
    X(cuModuleGetFunction, "cuModuleGetFunction", CUresult, \
      (CUfunction *fn, CUmodule module, const char *name)) \
    X(cuLaunchKernel, "cuLaunchKernel", CUresult, \
      (CUfunction f, unsigned int gx, unsigned int gy, unsigned int gz, unsigned int bx, \
       unsigned int by, unsigned int bz, unsigned int shared, CUstream stream, void **params, \
       void **extra)) \
    X(cuMemGetHandleForAddressRange, "cuMemGetHandleForAddressRange", CUresult, \
      (void *handle, CUdeviceptr ptr, size_t size, int type, unsigned long long flags))

/* ---- NVML (nvml.h), optional; used only for the NUMA node ---- */
typedef int nvmlReturn_t; /* NVML_SUCCESS = 0 */
typedef struct nvmlDevice_st *nvmlDevice_t;
#define NVML_SUCCESS 0

#define RACER_NVML_FUNCTIONS(X) \
    X(nvmlInit, "nvmlInit_v2", nvmlReturn_t, (void)) \
    X(nvmlShutdown, "nvmlShutdown", nvmlReturn_t, (void)) \
    X(nvmlDeviceGetHandleByPciBusId, "nvmlDeviceGetHandleByPciBusId_v2", nvmlReturn_t, \
      (const char *id, nvmlDevice_t *device)) \
    X(nvmlDeviceGetNumaNodeId, "nvmlDeviceGetNumaNodeId", nvmlReturn_t, \
      (nvmlDevice_t device, unsigned int *node))

/* ---- cuFile (cufile.h), optional ---- */
typedef struct {
    int err;    /* CUfileOpError; CU_FILE_SUCCESS = 0 */
    int cu_err; /* CUresult */
} CUfileError_t;
typedef void *CUfileHandle_t;
typedef struct {
    int type; /* CUfileFileHandleType */
    union {
        int fd;
        void *handle;
    } handle;
    const void *fs_ops;
} CUfileDescr_t;

#define CU_FILE_SUCCESS 0
#define CU_FILE_HANDLE_TYPE_OPAQUE_FD 1

_Static_assert(sizeof(CUfileError_t) == 8, "CUfileError_t layout");
_Static_assert(sizeof(CUfileDescr_t) == 24, "CUfileDescr_t layout");
_Static_assert(offsetof(CUfileDescr_t, handle) == 8, "CUfileDescr_t layout");

/* cuFileSetParameterBool (cuFile 1.10+); resolved optionally. */
#define CUFILE_PARAM_PROPERTIES_ALLOW_COMPAT_MODE 1
typedef CUfileError_t (*cuFileSetParameterBool_fn)(int param, bool value);

#define RACER_CUFILE_FUNCTIONS(X) \
    X(cuFileDriverOpen, "cuFileDriverOpen", CUfileError_t, (void)) \
    X(cuFileHandleRegister, "cuFileHandleRegister", CUfileError_t, \
      (CUfileHandle_t *fh, CUfileDescr_t *descr)) \
    X(cuFileHandleDeregister, "cuFileHandleDeregister", void, (CUfileHandle_t fh)) \
    X(cuFileBufRegister, "cuFileBufRegister", CUfileError_t, \
      (const void *base, size_t size, int flags)) \
    X(cuFileBufDeregister, "cuFileBufDeregister", CUfileError_t, (const void *base)) \
    X(cuFileRead, "cuFileRead", ssize_t, \
      (CUfileHandle_t fh, void *base, size_t size, off_t file_offset, off_t base_offset))

#endif

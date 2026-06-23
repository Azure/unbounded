/*
 * Copyright (c) Microsoft Corporation.
 * Licensed under the MIT License.
 *
 * Minimal C shim for the unbounded-storage libfabric transport.
 *
 * libfabric exposes most operational entry points (`fi_close`,
 * `fi_endpoint`, `fi_av_open`, `fi_cq_open`, `fi_enable`,
 * `fi_ep_bind`, `fi_cq_read`, `fi_cq_readerr`, `fi_getname`, ...)
 * as `static inline` wrappers around the fid ops vtable. They have
 * no exported symbols, so Rust FFI cannot call them directly. This
 * file is the only piece of C we ship for the fabric module; the
 * rest is Rust.
 *
 * The shim also encapsulates the layout of `fi_info` and its
 * sub-attribute structs: Rust never reads or writes those fields
 * directly. Instead it asks this shim to "build hints with these
 * knobs" or "ask this info struct for its fabric_attr", which
 * keeps the unsafe Rust surface independent of libfabric's exact
 * struct layout across minor versions.
 */

#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <netdb.h>
#include <netinet/in.h>
#include <arpa/inet.h>

#include <rdma/fabric.h>
#include <rdma/fi_cm.h>
#include <rdma/fi_domain.h>
#include <rdma/fi_endpoint.h>
#include <rdma/fi_eq.h>
#include <rdma/fi_errno.h>
#include <rdma/fi_rma.h>
#include <rdma/fi_tagged.h>

struct ub_fi_cq_err_entry {
    void *op_context;
    uint64_t flags;
    size_t len;
    void *buf;
    uint64_t data;
    uint64_t tag;
    size_t olen;
    int err;
    int prov_errno;
    void *err_data;
    size_t err_data_size;
};

/* ------------------------------------------------------------------
 * Inline-function wrappers.
 * ------------------------------------------------------------------ */

int ub_fi_close(struct fid *fid) {
    if (!fid) {
        return 0;
    }
    return fi_close(fid);
}

int ub_fi_endpoint(struct fid_domain *domain, struct fi_info *info,
                   struct fid_ep **ep, void *context) {
    return fi_endpoint(domain, info, ep, context);
}

struct fi_info *ub_fi_dupinfo_with_dest(const struct fi_info *base,
                                        const void *dest_addr,
                                        size_t dest_addrlen) {
    if (!base || !dest_addr || dest_addrlen == 0) {
        return NULL;
    }

    struct fi_info *copy = fi_dupinfo(base);
    if (!copy) {
        return NULL;
    }

    void *dest = malloc(dest_addrlen);
    if (!dest) {
        fi_freeinfo(copy);
        return NULL;
    }
    memcpy(dest, dest_addr, dest_addrlen);

    free(copy->dest_addr);
    copy->dest_addr = dest;
    copy->dest_addrlen = dest_addrlen;

    const struct sockaddr *sa = (const struct sockaddr *)dest_addr;
    switch (sa->sa_family) {
    case AF_INET:
        copy->addr_format = FI_SOCKADDR_IN;
        break;
    case AF_INET6:
        copy->addr_format = FI_SOCKADDR_IN6;
        break;
    default:
        copy->addr_format = FI_SOCKADDR_IB;
        break;
    }

    return copy;
}

int ub_fi_domain(struct fid_fabric *fabric, struct fi_info *info,
                 struct fid_domain **domain, void *context) {
    return fi_domain(fabric, info, domain, context);
}

int ub_fi_enable(struct fid_ep *ep) {
    return fi_enable(ep);
}

int ub_fi_av_open(struct fid_domain *domain, struct fi_av_attr *attr,
                  struct fid_av **av, void *context) {
    return fi_av_open(domain, attr, av, context);
}

int ub_fi_cq_open(struct fid_domain *domain, struct fi_cq_attr *attr,
                  struct fid_cq **cq, void *context) {
    return fi_cq_open(domain, attr, cq, context);
}

int ub_fi_ep_bind(struct fid_ep *ep, struct fid *bfid, uint64_t flags) {
    return fi_ep_bind(ep, bfid, flags);
}

ssize_t ub_fi_cq_read(struct fid_cq *cq, void *buf, size_t count) {
    return fi_cq_read(cq, buf, count);
}

ssize_t ub_fi_cq_readfrom(struct fid_cq *cq, void *buf, size_t count,
                          fi_addr_t *src_addr) {
    return fi_cq_readfrom(cq, buf, count, src_addr);
}

ssize_t ub_fi_cq_readerr(struct fid_cq *cq, struct ub_fi_cq_err_entry *buf,
                         uint64_t flags) {
    struct fi_cq_err_entry native;
    ssize_t rc = fi_cq_readerr(cq, &native, flags);
    if (rc > 0 && buf != NULL) {
        buf->op_context = native.op_context;
        buf->flags = native.flags;
        buf->len = native.len;
        buf->buf = native.buf;
        buf->data = native.data;
        buf->tag = native.tag;
        buf->olen = native.olen;
        buf->err = native.err;
        buf->prov_errno = native.prov_errno;
        buf->err_data = native.err_data;
        buf->err_data_size = native.err_data_size;
    }
    return rc;
}

int ub_fi_getname(struct fid *fid, void *addr, size_t *addrlen) {
    return fi_getname(fid, addr, addrlen);
}

/* ------------------------------------------------------------------
 * Connection-manager wrappers (FI_EP_MSG).
 *
 * The native verbs and tcp MSG endpoint types are connection oriented:
 * a passive endpoint listens, active endpoints connect, and connection
 * state transitions (FI_CONNREQ / FI_CONNECTED / FI_SHUTDOWN) are
 * delivered on an event queue. None of these entry points exist on the
 * connectionless RDM path, so they live behind their own shim section.
 * ------------------------------------------------------------------ */

int ub_fi_eq_open(struct fid_fabric *fabric, struct fi_eq_attr *attr,
                  struct fid_eq **eq, void *context) {
    return fi_eq_open(fabric, attr, eq, context);
}

int ub_fi_passive_ep(struct fid_fabric *fabric, struct fi_info *info,
                     struct fid_pep **pep, void *context) {
    return fi_passive_ep(fabric, info, pep, context);
}

int ub_fi_pep_bind(struct fid_pep *pep, struct fid *bfid, uint64_t flags) {
    return fi_pep_bind(pep, bfid, flags);
}

int ub_fi_listen(struct fid_pep *pep) {
    return fi_listen(pep);
}

int ub_fi_connect(struct fid_ep *ep, const void *addr, const void *param,
                  size_t paramlen) {
    return fi_connect(ep, addr, param, paramlen);
}

int ub_fi_accept(struct fid_ep *ep, const void *param, size_t paramlen) {
    return fi_accept(ep, param, paramlen);
}

ssize_t ub_fi_eq_sread(struct fid_eq *eq, uint32_t *event, void *buf,
                       size_t len, int timeout, uint64_t flags) {
    return fi_eq_sread(eq, event, buf, len, timeout, flags);
}

ssize_t ub_fi_eq_read(struct fid_eq *eq, uint32_t *event, void *buf, size_t len,
                      uint64_t flags) {
    return fi_eq_read(eq, event, buf, len, flags);
}

ssize_t ub_fi_eq_readerr(struct fid_eq *eq, struct fi_eq_err_entry *buf,
                         uint64_t flags) {
    return fi_eq_readerr(eq, buf, flags);
}

/*
 * Connection-management event discriminants, exposed as functions so the
 * Rust side never hardcodes the `enum` values (libfabric could renumber
 * them across versions).
 */
uint32_t ub_fi_connreq(void) { return FI_CONNREQ; }
uint32_t ub_fi_connected(void) { return FI_CONNECTED; }
uint32_t ub_fi_shutdown(void) { return FI_SHUTDOWN; }

int ub_fi_ep_bind_eq(struct fid_ep *ep, struct fid_eq *eq, uint64_t flags) {
    return fi_ep_bind(ep, &eq->fid, flags);
}

/* ------------------------------------------------------------------
 * Hints builders. Keep Rust away from `fi_info` layout entirely.
 * ------------------------------------------------------------------ */

/*
 * Allocate an `fi_info` (via fi_allocinfo == fi_dupinfo(NULL)) and
 * populate it with the knobs Phase 3 needs:
 *   caps                  = FI_MSG | FI_RMA | FI_TAGGED | FI_SOURCE
 *   ep_attr.type          = FI_EP_RDM
 *   domain_attr.mr_mode   = FI_MR_LOCAL | FI_MR_VIRT_ADDR
 *                         | FI_MR_ALLOCATED | FI_MR_PROV_KEY
 *   domain_attr.av_type   = FI_AV_TABLE
 *   domain_attr.control_progress / data_progress = FI_PROGRESS_MANUAL
 *   domain_attr.threading = FI_THREAD_SAFE
 *   fabric_attr.prov_name = strdup(prov_name) when non-NULL
 *
 * FI_THREAD_SAFE is requested because a single shared per-HCA domain is
 * driven concurrently by multiple serving shards posting outbound RMA
 * from their own cores while NIC-worker threads progress completions.
 * The tcp and verbs RDM providers both support FI_THREAD_SAFE; if a
 * provider negotiates a weaker mode the Rust side detects it via
 * `ub_fi_info_threading` and warns at bring-up.
 *
 * The returned pointer must be freed with fi_freeinfo when the
 * caller is done with it.
 */
struct fi_info *ub_fi_build_hints(const char *prov_name) {
    struct fi_info *hints = fi_allocinfo();
    if (!hints) {
        return NULL;
    }
    hints->caps = FI_MSG | FI_RMA | FI_TAGGED | FI_SOURCE;
    if (hints->ep_attr) {
        hints->ep_attr->type = FI_EP_RDM;
    }
    if (hints->domain_attr) {
        hints->domain_attr->mr_mode = FI_MR_LOCAL | FI_MR_VIRT_ADDR
                                    | FI_MR_ALLOCATED | FI_MR_PROV_KEY;
        hints->domain_attr->av_type = FI_AV_TABLE;
        hints->domain_attr->control_progress = FI_PROGRESS_MANUAL;
        hints->domain_attr->data_progress = FI_PROGRESS_MANUAL;
        hints->domain_attr->threading = FI_THREAD_SAFE;
    }
    if (prov_name && hints->fabric_attr) {
        /* libfabric frees prov_name with `free(3)` inside fi_freeinfo. */
        size_t n = strlen(prov_name) + 1;
        char *dup = (char *)malloc(n);
        if (!dup) {
            fi_freeinfo(hints);
            return NULL;
        }
        memcpy(dup, prov_name, n);
        hints->fabric_attr->prov_name = dup;
    }
    return hints;
}

/*
 * Build hints for the connection-oriented MSG transport (native verbs
 * or tcp). Differs from the RDM hints above: ep type is FI_EP_MSG, the
 * caps drop FI_TAGGED (untagged demux moves into our wire header) and
 * FI_SOURCE (peer identity comes from the owning connection, not the
 * completion source), and there is no AV. mr_mode is requested the same
 * way; the provider pares it down (tcp negotiates an empty mr_mode and
 * uses 0-based RMA offsets, verbs keeps FI_MR_VIRT_ADDR and uses
 * absolute addresses). addr_format is left unspecified so socket
 * addresses remain the simple default while provider-native address
 * bytes can still be passed to fi_connect as an escape hatch.
 *
 * Freed with fi_freeinfo by the caller.
 */
struct fi_info *ub_fi_build_msg_hints(const char *prov_name) {
    struct fi_info *hints = fi_allocinfo();
    if (!hints) {
        return NULL;
    }
    hints->caps = FI_MSG | FI_RMA;
    hints->addr_format = FI_FORMAT_UNSPEC;
    if (hints->ep_attr) {
        hints->ep_attr->type = FI_EP_MSG;
    }
    if (hints->domain_attr) {
        hints->domain_attr->mr_mode = FI_MR_LOCAL | FI_MR_VIRT_ADDR
                                    | FI_MR_ALLOCATED | FI_MR_PROV_KEY;
        hints->domain_attr->control_progress = FI_PROGRESS_MANUAL;
        hints->domain_attr->data_progress = FI_PROGRESS_MANUAL;
        hints->domain_attr->threading = FI_THREAD_SAFE;
    }
    if (prov_name && hints->fabric_attr) {
        size_t n = strlen(prov_name) + 1;
        char *dup = (char *)malloc(n);
        if (!dup) {
            fi_freeinfo(hints);
            return NULL;
        }
        memcpy(dup, prov_name, n);
        hints->fabric_attr->prov_name = dup;
    }
    return hints;
}

/*
 * Pin `hints->domain_attr->name` to a specific provider domain so
 * fi_getinfo returns only the matching HCA's info entries. Without this,
 * a multi-HCA host (for example 8x mlx5_N verbs domains) returns every
 * domain in the info chain and the caller, taking the chain head, binds
 * every fabric instance to the first domain. The verbs/rxm domain name
 * is exactly the device name ("mlx5_0".."mlx5_7"). Returns 0 on success,
 * -1 if hints has no domain_attr or the allocation fails. The string is
 * freed by fi_freeinfo with free(3), matching how prov_name is handled.
 */
int ub_fi_hints_set_domain(struct fi_info *hints, const char *name) {
    if (!hints || !hints->domain_attr || !name) {
        return -1;
    }
    size_t n = strlen(name) + 1;
    char *dup = (char *)malloc(n);
    if (!dup) {
        return -1;
    }
    memcpy(dup, name, n);
    free(hints->domain_attr->name);
    hints->domain_attr->name = dup;
    return 0;
}

/*
 * Seed a provider-native source address on hints for FI_SOURCE lookups.
 * This is the listen-side companion to passing raw native bytes to
 * fi_connect: socket listeners still use node/service strings, while
 * non-socket RDMA fabrics can bind using controller-generated fi_getname
 * bytes. The copy is owned by hints and released by fi_freeinfo.
 */
int ub_fi_hints_set_src_addr(struct fi_info *hints, const void *addr,
                             size_t addrlen) {
    if (!hints || !addr || addrlen == 0) {
        return -1;
    }
    void *copy = malloc(addrlen);
    if (!copy) {
        return -1;
    }
    memcpy(copy, addr, addrlen);
    free(hints->src_addr);
    hints->src_addr = copy;
    hints->src_addrlen = addrlen;
    return 0;
}

/*
 * Accessors for the sub-attribute pointers `fi_fabric` /
 * `fi_domain` need from a chosen `fi_info`. Returning the embedded
 * pointer is safe because the caller still owns the parent `fi_info`.
 */
struct fi_fabric_attr *ub_fi_info_fabric_attr(struct fi_info *info) {
    return info ? info->fabric_attr : NULL;
}

/*
 * The negotiated `domain_attr->mr_mode` the provider selected. Used to
 * decide remote RMA addressing: when FI_MR_VIRT_ADDR is set the remote
 * target address is the registered virtual address; otherwise it is a
 * 0-based offset into the MR.
 */
int ub_fi_info_mr_mode(struct fi_info *info) {
    if (!info || !info->domain_attr) {
        return 0;
    }
    return info->domain_attr->mr_mode;
}

/*
 * The negotiated `domain_attr->threading` the provider selected,
 * returned as the raw `enum fi_threading` discriminant. The Rust side
 * compares it against `ub_fi_thread_safe_value()` to decide whether a
 * single shared domain may be posted to concurrently from multiple
 * shard cores without external serialization. Returns FI_THREAD_UNSPEC
 * (0) when the info or its domain_attr is NULL.
 */
int ub_fi_info_threading(struct fi_info *info) {
    if (!info || !info->domain_attr) {
        return FI_THREAD_UNSPEC;
    }
    return (int)info->domain_attr->threading;
}

/*
 * The `enum fi_threading` value for FI_THREAD_SAFE. Exposed as a
 * function so the Rust side never hardcodes the enum discriminant,
 * which keeps it correct if libfabric ever renumbers the enum.
 */
int ub_fi_thread_safe_value(void) {
    return (int)FI_THREAD_SAFE;
}

/*
 * The provider's negotiated `domain_attr->mr_cnt`: the maximum number
 * of memory regions that may be registered against this domain. A
 * value of 0 means the provider reports no fixed limit. Used at
 * bring-up to check that the expected per-domain MR registrations
 * (each shard registers its pool backing plus an RPC scratch region)
 * fit within the shared per-HCA domain's capacity.
 */
size_t ub_fi_info_mr_cnt(struct fi_info *info) {
    if (!info || !info->domain_attr) {
        return 0;
    }
    return info->domain_attr->mr_cnt;
}

/*
 * `fi_fabric` wants `struct fi_fabric_attr *`; provided via accessor
 * above. `fi_domain` wants the whole `fi_info`; nothing to do here.
 */

/* Errno constants we surface to Rust. */
int ub_fi_eagain(void) { return FI_EAGAIN; }
int ub_fi_eavail(void) { return FI_EAVAIL; }
int ub_fi_enodata(void) { return FI_ENODATA; }

/* Layout probes for Rust's hand-written repr(C) libfabric structs. */
#define UB_FI_LAYOUT_FI_CQ_ATTR 1
#define UB_FI_LAYOUT_FI_AV_ATTR 2
#define UB_FI_LAYOUT_FI_CQ_DATA_ENTRY 3
#define UB_FI_LAYOUT_FI_CQ_TAGGED_ENTRY 4
#define UB_FI_LAYOUT_FI_CQ_ERR_ENTRY 5
#define UB_FI_LAYOUT_FI_RMA_IOV 6
#define UB_FI_LAYOUT_FI_EQ_ATTR 7
#define UB_FI_LAYOUT_FI_EQ_CM_ENTRY 8
#define UB_FI_LAYOUT_FI_EQ_ERR_ENTRY 9

#define UB_FI_FIELD_SIZE 0
#define UB_FI_FIELD_CQ_ATTR_SIZE 1
#define UB_FI_FIELD_CQ_ATTR_FLAGS 2
#define UB_FI_FIELD_CQ_ATTR_FORMAT 3
#define UB_FI_FIELD_CQ_ATTR_WAIT_OBJ 4
#define UB_FI_FIELD_CQ_ATTR_SIGNALING_VECTOR 5
#define UB_FI_FIELD_CQ_ATTR_WAIT_COND 6
#define UB_FI_FIELD_CQ_ATTR_WAIT_SET 7
#define UB_FI_FIELD_AV_ATTR_TYPE 8
#define UB_FI_FIELD_AV_ATTR_RX_CTX_BITS 9
#define UB_FI_FIELD_AV_ATTR_COUNT 10
#define UB_FI_FIELD_AV_ATTR_EP_PER_NODE 11
#define UB_FI_FIELD_AV_ATTR_NAME 12
#define UB_FI_FIELD_AV_ATTR_MAP_ADDR 13
#define UB_FI_FIELD_AV_ATTR_FLAGS 14
#define UB_FI_FIELD_CQ_ENTRY_OP_CONTEXT 15
#define UB_FI_FIELD_CQ_ENTRY_FLAGS 16
#define UB_FI_FIELD_CQ_ENTRY_LEN 17
#define UB_FI_FIELD_CQ_ENTRY_BUF 18
#define UB_FI_FIELD_CQ_ENTRY_DATA 19
#define UB_FI_FIELD_CQ_ENTRY_TAG 20
#define UB_FI_FIELD_CQ_ERR_ENTRY_OLEN 21
#define UB_FI_FIELD_CQ_ERR_ENTRY_ERR 22
#define UB_FI_FIELD_CQ_ERR_ENTRY_PROV_ERRNO 23
#define UB_FI_FIELD_CQ_ERR_ENTRY_ERR_DATA 24
#define UB_FI_FIELD_CQ_ERR_ENTRY_ERR_DATA_SIZE 25
#define UB_FI_FIELD_RMA_IOV_ADDR 27
#define UB_FI_FIELD_RMA_IOV_LEN 28
#define UB_FI_FIELD_RMA_IOV_KEY 29
#define UB_FI_FIELD_EQ_ATTR_SIZE 30
#define UB_FI_FIELD_EQ_ATTR_FLAGS 31
#define UB_FI_FIELD_EQ_ATTR_WAIT_OBJ 32
#define UB_FI_FIELD_EQ_ATTR_SIGNALING_VECTOR 33
#define UB_FI_FIELD_EQ_ATTR_WAIT_SET 34
#define UB_FI_FIELD_EQ_CM_ENTRY_FID 35
#define UB_FI_FIELD_EQ_CM_ENTRY_INFO 36
#define UB_FI_FIELD_EQ_ERR_ENTRY_FID 37
#define UB_FI_FIELD_EQ_ERR_ENTRY_CONTEXT 38
#define UB_FI_FIELD_EQ_ERR_ENTRY_DATA 39
#define UB_FI_FIELD_EQ_ERR_ENTRY_ERR 40
#define UB_FI_FIELD_EQ_ERR_ENTRY_PROV_ERRNO 41
#define UB_FI_FIELD_EQ_ERR_ENTRY_ERR_DATA 42
#define UB_FI_FIELD_EQ_ERR_ENTRY_ERR_DATA_SIZE 43

size_t ub_fi_layout(int type, int field) {
    if (field == UB_FI_FIELD_SIZE) {
        switch (type) {
        case UB_FI_LAYOUT_FI_CQ_ATTR: return sizeof(struct fi_cq_attr);
        case UB_FI_LAYOUT_FI_AV_ATTR: return sizeof(struct fi_av_attr);
        case UB_FI_LAYOUT_FI_CQ_DATA_ENTRY: return sizeof(struct fi_cq_data_entry);
        case UB_FI_LAYOUT_FI_CQ_TAGGED_ENTRY: return sizeof(struct fi_cq_tagged_entry);
        case UB_FI_LAYOUT_FI_CQ_ERR_ENTRY: return sizeof(struct ub_fi_cq_err_entry);
        case UB_FI_LAYOUT_FI_RMA_IOV: return sizeof(struct fi_rma_iov);
        case UB_FI_LAYOUT_FI_EQ_ATTR: return sizeof(struct fi_eq_attr);
        case UB_FI_LAYOUT_FI_EQ_CM_ENTRY: return sizeof(struct fi_eq_cm_entry);
        case UB_FI_LAYOUT_FI_EQ_ERR_ENTRY: return sizeof(struct fi_eq_err_entry);
        default: return (size_t)-1;
        }
    }

    switch (type) {
    case UB_FI_LAYOUT_FI_CQ_ATTR:
        switch (field) {
        case UB_FI_FIELD_CQ_ATTR_SIZE: return offsetof(struct fi_cq_attr, size);
        case UB_FI_FIELD_CQ_ATTR_FLAGS: return offsetof(struct fi_cq_attr, flags);
        case UB_FI_FIELD_CQ_ATTR_FORMAT: return offsetof(struct fi_cq_attr, format);
        case UB_FI_FIELD_CQ_ATTR_WAIT_OBJ: return offsetof(struct fi_cq_attr, wait_obj);
        case UB_FI_FIELD_CQ_ATTR_SIGNALING_VECTOR: return offsetof(struct fi_cq_attr, signaling_vector);
        case UB_FI_FIELD_CQ_ATTR_WAIT_COND: return offsetof(struct fi_cq_attr, wait_cond);
        case UB_FI_FIELD_CQ_ATTR_WAIT_SET: return offsetof(struct fi_cq_attr, wait_set);
        default: return (size_t)-1;
        }
    case UB_FI_LAYOUT_FI_AV_ATTR:
        switch (field) {
        case UB_FI_FIELD_AV_ATTR_TYPE: return offsetof(struct fi_av_attr, type);
        case UB_FI_FIELD_AV_ATTR_RX_CTX_BITS: return offsetof(struct fi_av_attr, rx_ctx_bits);
        case UB_FI_FIELD_AV_ATTR_COUNT: return offsetof(struct fi_av_attr, count);
        case UB_FI_FIELD_AV_ATTR_EP_PER_NODE: return offsetof(struct fi_av_attr, ep_per_node);
        case UB_FI_FIELD_AV_ATTR_NAME: return offsetof(struct fi_av_attr, name);
        case UB_FI_FIELD_AV_ATTR_MAP_ADDR: return offsetof(struct fi_av_attr, map_addr);
        case UB_FI_FIELD_AV_ATTR_FLAGS: return offsetof(struct fi_av_attr, flags);
        default: return (size_t)-1;
        }
    case UB_FI_LAYOUT_FI_CQ_DATA_ENTRY:
        switch (field) {
        case UB_FI_FIELD_CQ_ENTRY_OP_CONTEXT: return offsetof(struct fi_cq_data_entry, op_context);
        case UB_FI_FIELD_CQ_ENTRY_FLAGS: return offsetof(struct fi_cq_data_entry, flags);
        case UB_FI_FIELD_CQ_ENTRY_LEN: return offsetof(struct fi_cq_data_entry, len);
        case UB_FI_FIELD_CQ_ENTRY_BUF: return offsetof(struct fi_cq_data_entry, buf);
        case UB_FI_FIELD_CQ_ENTRY_DATA: return offsetof(struct fi_cq_data_entry, data);
        default: return (size_t)-1;
        }
    case UB_FI_LAYOUT_FI_CQ_TAGGED_ENTRY:
        switch (field) {
        case UB_FI_FIELD_CQ_ENTRY_OP_CONTEXT: return offsetof(struct fi_cq_tagged_entry, op_context);
        case UB_FI_FIELD_CQ_ENTRY_FLAGS: return offsetof(struct fi_cq_tagged_entry, flags);
        case UB_FI_FIELD_CQ_ENTRY_LEN: return offsetof(struct fi_cq_tagged_entry, len);
        case UB_FI_FIELD_CQ_ENTRY_BUF: return offsetof(struct fi_cq_tagged_entry, buf);
        case UB_FI_FIELD_CQ_ENTRY_DATA: return offsetof(struct fi_cq_tagged_entry, data);
        case UB_FI_FIELD_CQ_ENTRY_TAG: return offsetof(struct fi_cq_tagged_entry, tag);
        default: return (size_t)-1;
        }
    case UB_FI_LAYOUT_FI_CQ_ERR_ENTRY:
        switch (field) {
        case UB_FI_FIELD_CQ_ENTRY_OP_CONTEXT: return offsetof(struct ub_fi_cq_err_entry, op_context);
        case UB_FI_FIELD_CQ_ENTRY_FLAGS: return offsetof(struct ub_fi_cq_err_entry, flags);
        case UB_FI_FIELD_CQ_ENTRY_LEN: return offsetof(struct ub_fi_cq_err_entry, len);
        case UB_FI_FIELD_CQ_ENTRY_BUF: return offsetof(struct ub_fi_cq_err_entry, buf);
        case UB_FI_FIELD_CQ_ENTRY_DATA: return offsetof(struct ub_fi_cq_err_entry, data);
        case UB_FI_FIELD_CQ_ENTRY_TAG: return offsetof(struct ub_fi_cq_err_entry, tag);
        case UB_FI_FIELD_CQ_ERR_ENTRY_OLEN: return offsetof(struct ub_fi_cq_err_entry, olen);
        case UB_FI_FIELD_CQ_ERR_ENTRY_ERR: return offsetof(struct ub_fi_cq_err_entry, err);
        case UB_FI_FIELD_CQ_ERR_ENTRY_PROV_ERRNO: return offsetof(struct ub_fi_cq_err_entry, prov_errno);
        case UB_FI_FIELD_CQ_ERR_ENTRY_ERR_DATA: return offsetof(struct ub_fi_cq_err_entry, err_data);
        case UB_FI_FIELD_CQ_ERR_ENTRY_ERR_DATA_SIZE: return offsetof(struct ub_fi_cq_err_entry, err_data_size);
        default: return (size_t)-1;
        }
    case UB_FI_LAYOUT_FI_RMA_IOV:
        switch (field) {
        case UB_FI_FIELD_RMA_IOV_ADDR: return offsetof(struct fi_rma_iov, addr);
        case UB_FI_FIELD_RMA_IOV_LEN: return offsetof(struct fi_rma_iov, len);
        case UB_FI_FIELD_RMA_IOV_KEY: return offsetof(struct fi_rma_iov, key);
        default: return (size_t)-1;
        }
    case UB_FI_LAYOUT_FI_EQ_ATTR:
        switch (field) {
        case UB_FI_FIELD_EQ_ATTR_SIZE: return offsetof(struct fi_eq_attr, size);
        case UB_FI_FIELD_EQ_ATTR_FLAGS: return offsetof(struct fi_eq_attr, flags);
        case UB_FI_FIELD_EQ_ATTR_WAIT_OBJ: return offsetof(struct fi_eq_attr, wait_obj);
        case UB_FI_FIELD_EQ_ATTR_SIGNALING_VECTOR: return offsetof(struct fi_eq_attr, signaling_vector);
        case UB_FI_FIELD_EQ_ATTR_WAIT_SET: return offsetof(struct fi_eq_attr, wait_set);
        default: return (size_t)-1;
        }
    case UB_FI_LAYOUT_FI_EQ_CM_ENTRY:
        switch (field) {
        case UB_FI_FIELD_EQ_CM_ENTRY_FID: return offsetof(struct fi_eq_cm_entry, fid);
        case UB_FI_FIELD_EQ_CM_ENTRY_INFO: return offsetof(struct fi_eq_cm_entry, info);
        default: return (size_t)-1;
        }
    case UB_FI_LAYOUT_FI_EQ_ERR_ENTRY:
        switch (field) {
        case UB_FI_FIELD_EQ_ERR_ENTRY_FID: return offsetof(struct fi_eq_err_entry, fid);
        case UB_FI_FIELD_EQ_ERR_ENTRY_CONTEXT: return offsetof(struct fi_eq_err_entry, context);
        case UB_FI_FIELD_EQ_ERR_ENTRY_DATA: return offsetof(struct fi_eq_err_entry, data);
        case UB_FI_FIELD_EQ_ERR_ENTRY_ERR: return offsetof(struct fi_eq_err_entry, err);
        case UB_FI_FIELD_EQ_ERR_ENTRY_PROV_ERRNO: return offsetof(struct fi_eq_err_entry, prov_errno);
        case UB_FI_FIELD_EQ_ERR_ENTRY_ERR_DATA: return offsetof(struct fi_eq_err_entry, err_data);
        case UB_FI_FIELD_EQ_ERR_ENTRY_ERR_DATA_SIZE: return offsetof(struct fi_eq_err_entry, err_data_size);
        default: return (size_t)-1;
        }
    default:
        return (size_t)-1;
    }
}

/* ------------------------------------------------------------------
 * Connection (AV) and memory registration wrappers.
 * ------------------------------------------------------------------ */

int ub_fi_av_insert(struct fid_av *av, const void *addr, size_t count,
                    fi_addr_t *fi_addr, uint64_t flags, void *context) {
    int rc = fi_av_insert(av, addr, count, fi_addr, flags, context);
    /* fi_av_insert returns the number of successfully inserted entries on
     * success (>= 0) and a negative errno on failure. Translate the
     * "inserted 0 of 1" case into a clear error so the Rust side does
     * not have to special-case the return-value semantics. */
    if (rc >= 0 && (size_t)rc != count) {
        return -FI_EINVAL;
    }
    if (rc >= 0) {
        return 0;
    }
    return rc;
}

int ub_fi_av_remove(struct fid_av *av, fi_addr_t *fi_addr, size_t count,
                    uint64_t flags) {
    return fi_av_remove(av, fi_addr, count, flags);
}

int ub_fi_mr_reg(struct fid_domain *domain, const void *buf, size_t len,
                 uint64_t access, uint64_t offset, uint64_t requested_key,
                 uint64_t flags, struct fid_mr **mr, void *context) {
    return fi_mr_reg(domain, buf, len, access, offset, requested_key, flags,
                     mr, context);
}

uint64_t ub_fi_mr_key(struct fid_mr *mr) {
    return fi_mr_key(mr);
}

void *ub_fi_mr_desc(struct fid_mr *mr) {
    return fi_mr_desc(mr);
}

/* ------------------------------------------------------------------
 * Tagged message send/recv wrappers.
 * ------------------------------------------------------------------ */

ssize_t ub_fi_tsend(struct fid_ep *ep, const void *buf, size_t len, void *desc,
                    fi_addr_t dest_addr, uint64_t tag, void *context) {
    return fi_tsend(ep, buf, len, desc, dest_addr, tag, context);
}

ssize_t ub_fi_trecv(struct fid_ep *ep, void *buf, size_t len, void *desc,
                    fi_addr_t src_addr, uint64_t tag, uint64_t ignore,
                    void *context) {
    return fi_trecv(ep, buf, len, desc, src_addr, tag, ignore, context);
}

ssize_t ub_fi_send(struct fid_ep *ep, const void *buf, size_t len, void *desc,
                   fi_addr_t dest_addr, void *context) {
    if (!ep) {
        return -FI_EINVAL;
    }
    return fi_send(ep, buf, len, desc, dest_addr, context);
}

ssize_t ub_fi_recv(struct fid_ep *ep, void *buf, size_t len, void *desc,
                   fi_addr_t src_addr, void *context) {
    if (!ep) {
        return -FI_EINVAL;
    }
    return fi_recv(ep, buf, len, desc, src_addr, context);
}

/* ------------------------------------------------------------------
 * Sockaddr parsing for the tcp provider.
 *
 * Returns the number of bytes written to `out` on success; a negative
 * value on failure. The output is a raw `sockaddr_in` / `sockaddr_in6`
 * (in network byte order) suitable to hand to fi_av_insert when using
 * the tcp provider.
 *
 * `s` is expected to be "host:port"; IPv6 literals must be bracketed.
 * ------------------------------------------------------------------ */
ssize_t ub_fi_parse_sockaddr(const char *s, uint8_t *out, size_t out_cap) {    if (!s || !out) {
        return -FI_EINVAL;
    }
    /* Split host and port. Handle "[ipv6]:port", "host:port", and
     * "host" (port=0). */
    const char *host_start;
    const char *host_end;
    const char *port_start = NULL;
    char host_buf[256];
    if (s[0] == '[') {
        const char *rb = strchr(s, ']');
        if (!rb) return -FI_EINVAL;
        host_start = s + 1;
        host_end = rb;
        if (rb[1] == ':') {
            port_start = rb + 2;
        }
    } else {
        const char *colon = strrchr(s, ':');
        host_start = s;
        if (colon) {
            host_end = colon;
            port_start = colon + 1;
        } else {
            host_end = s + strlen(s);
        }
    }
    size_t hlen = (size_t)(host_end - host_start);
    if (hlen >= sizeof(host_buf)) {
        return -FI_EINVAL;
    }
    memcpy(host_buf, host_start, hlen);
    host_buf[hlen] = '\0';

    struct addrinfo hints;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;
    hints.ai_flags = AI_NUMERICSERV;

    struct addrinfo *res = NULL;
    int rc = getaddrinfo(host_buf, port_start ? port_start : "0", &hints, &res);
    if (rc != 0 || !res) {
        if (res) freeaddrinfo(res);
        return -FI_EINVAL;
    }
    ssize_t written = -FI_EINVAL;
    if (res->ai_addrlen <= out_cap) {
        memcpy(out, res->ai_addr, res->ai_addrlen);
        written = (ssize_t)res->ai_addrlen;
    } else {
        written = -FI_ETOOSMALL;
    }
    freeaddrinfo(res);
    return written;
}

/* ------------------------------------------------------------------
 * Format a raw sockaddr (as handed back by fi_getname on a bound MSG
 * endpoint, or carried in a CONNREQ cm_entry) into a numeric
 * "ip:port" string. Used to learn the locally bound listen address
 * (which port the provider chose for a ":0" bind) and to render peer
 * addresses for logging.
 *
 * Returns the string length written (excluding the NUL) on success, a
 * negative value on failure. `out` is always NUL-terminated when cap > 0.
 * ------------------------------------------------------------------ */
ssize_t ub_fi_format_sockaddr(const void *addr, size_t addrlen, char *out,
                              size_t cap) {
    if (!addr || !out || cap == 0) {
        return -FI_EINVAL;
    }
    sa_family_t family = ((const struct sockaddr *)addr)->sa_family;
    if (family != AF_INET && family != AF_INET6) {
        out[0] = '\0';
        return -FI_EINVAL;
    }
    char host[NI_MAXHOST];
    char serv[NI_MAXSERV];
    int rc = getnameinfo((const struct sockaddr *)addr, (socklen_t)addrlen,
                         host, sizeof(host), serv, sizeof(serv),
                         NI_NUMERICHOST | NI_NUMERICSERV);
    if (rc != 0) {
        out[0] = '\0';
        return -FI_EINVAL;
    }
    int is_v6 = ((const struct sockaddr *)addr)->sa_family == AF_INET6;
    int n = is_v6 ? snprintf(out, cap, "[%s]:%s", host, serv)
                  : snprintf(out, cap, "%s:%s", host, serv);
    if (n < 0 || (size_t)n >= cap) {
        out[0] = '\0';
        return -FI_ETOOSMALL;
    }
    return (ssize_t)n;
}

/* ------------------------------------------------------------------
 * RMA write + cancel wrappers used by the fabric RPC layer.
 * ------------------------------------------------------------------ */

ssize_t ub_fi_write(struct fid_ep *ep, const void *buf, size_t len, void *desc,
                    fi_addr_t dest_addr, uint64_t addr, uint64_t key,
                    void *context) {
    if (!ep) {
        return -FI_EINVAL;
    }
    return fi_write(ep, buf, len, desc, dest_addr, addr, key, context);
}

ssize_t ub_fi_writedata(struct fid_ep *ep, const void *buf, size_t len,
                        void *desc, uint64_t data, fi_addr_t dest_addr,
                        uint64_t addr, uint64_t key, void *context) {
    if (!ep) {
        return -FI_EINVAL;
    }
    return fi_writedata(ep, buf, len, desc, data, dest_addr, addr, key,
                        context);
}

ssize_t ub_fi_cancel(struct fid *fid, void *context) {
    if (!fid) {
        return -FI_EINVAL;
    }
    return fi_cancel(fid, context);
}

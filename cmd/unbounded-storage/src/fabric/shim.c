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

ssize_t ub_fi_cq_readerr(struct fid_cq *cq, struct fi_cq_err_entry *buf,
                         uint64_t flags) {
    return fi_cq_readerr(cq, buf, flags);
}

int ub_fi_getname(struct fid *fid, void *addr, size_t *addrlen) {
    return fi_getname(fid, addr, addrlen);
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
#define UB_FI_FIELD_CQ_ERR_ENTRY_SRC_ADDR 26
#define UB_FI_FIELD_RMA_IOV_ADDR 27
#define UB_FI_FIELD_RMA_IOV_LEN 28
#define UB_FI_FIELD_RMA_IOV_KEY 29

size_t ub_fi_layout(int type, int field) {
    if (field == UB_FI_FIELD_SIZE) {
        switch (type) {
        case UB_FI_LAYOUT_FI_CQ_ATTR: return sizeof(struct fi_cq_attr);
        case UB_FI_LAYOUT_FI_AV_ATTR: return sizeof(struct fi_av_attr);
        case UB_FI_LAYOUT_FI_CQ_DATA_ENTRY: return sizeof(struct fi_cq_data_entry);
        case UB_FI_LAYOUT_FI_CQ_TAGGED_ENTRY: return sizeof(struct fi_cq_tagged_entry);
        case UB_FI_LAYOUT_FI_CQ_ERR_ENTRY: return sizeof(struct fi_cq_err_entry);
        case UB_FI_LAYOUT_FI_RMA_IOV: return sizeof(struct fi_rma_iov);
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
        case UB_FI_FIELD_CQ_ENTRY_OP_CONTEXT: return offsetof(struct fi_cq_err_entry, op_context);
        case UB_FI_FIELD_CQ_ENTRY_FLAGS: return offsetof(struct fi_cq_err_entry, flags);
        case UB_FI_FIELD_CQ_ENTRY_LEN: return offsetof(struct fi_cq_err_entry, len);
        case UB_FI_FIELD_CQ_ENTRY_BUF: return offsetof(struct fi_cq_err_entry, buf);
        case UB_FI_FIELD_CQ_ENTRY_DATA: return offsetof(struct fi_cq_err_entry, data);
        case UB_FI_FIELD_CQ_ENTRY_TAG: return offsetof(struct fi_cq_err_entry, tag);
        case UB_FI_FIELD_CQ_ERR_ENTRY_OLEN: return offsetof(struct fi_cq_err_entry, olen);
        case UB_FI_FIELD_CQ_ERR_ENTRY_ERR: return offsetof(struct fi_cq_err_entry, err);
        case UB_FI_FIELD_CQ_ERR_ENTRY_PROV_ERRNO: return offsetof(struct fi_cq_err_entry, prov_errno);
        case UB_FI_FIELD_CQ_ERR_ENTRY_ERR_DATA: return offsetof(struct fi_cq_err_entry, err_data);
        case UB_FI_FIELD_CQ_ERR_ENTRY_ERR_DATA_SIZE: return offsetof(struct fi_cq_err_entry, err_data_size);
        case UB_FI_FIELD_CQ_ERR_ENTRY_SRC_ADDR: return offsetof(struct fi_cq_err_entry, src_addr);
        default: return (size_t)-1;
        }
    case UB_FI_LAYOUT_FI_RMA_IOV:
        switch (field) {
        case UB_FI_FIELD_RMA_IOV_ADDR: return offsetof(struct fi_rma_iov, addr);
        case UB_FI_FIELD_RMA_IOV_LEN: return offsetof(struct fi_rma_iov, len);
        case UB_FI_FIELD_RMA_IOV_KEY: return offsetof(struct fi_rma_iov, key);
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
    return fi_send(ep, buf, len, desc, dest_addr, context);
}

ssize_t ub_fi_recv(struct fid_ep *ep, void *buf, size_t len, void *desc,
                   fi_addr_t src_addr, void *context) {
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
 * RMA write + cancel wrappers used by the fabric RPC layer.
 * ------------------------------------------------------------------ */

ssize_t ub_fi_write(struct fid_ep *ep, const void *buf, size_t len, void *desc,
                    fi_addr_t dest_addr, uint64_t addr, uint64_t key,
                    void *context) {
    return fi_write(ep, buf, len, desc, dest_addr, addr, key, context);
}

ssize_t ub_fi_cancel(struct fid *fid, void *context) {
    return fi_cancel(fid, context);
}

/*
 * Named segments: create / open, existing-name rules, safe unlink (SPEC 11.3,
 * 11.4). Ports named_link.go.
 */
#define HQQ_BUILDING 1
#include "hqq_internal.h"

static const hqq_allocator *opt_alloc(const hqq_open_req *req) { return req->alloc_set ? &req->alloc : NULL; }

/*
 * Does the name still refer to this endpoint's instance of the segment, so
 * that removing it cannot remove somebody else's segment? n->lock must be
 * held and the descriptor and mapping must still be open (SPEC 11.4).
 */
static int owns_name(hqq_named *n) {
    int known = 0, same = 0;
    uint64_t state = 0, token = 0;
    hqq_status st;

    if (!hqq_plat_names_persist()) {
        return 0;
    }
    st = hqq_plat_shm_name_refers_to_self(n->shm, &known, &same);
    if (st != HQQ_OK) {
        return 0;
    }
    if (known && !same) {
        return 0;
    }
    if (known && n->map != NULL) {
        const hqq_superblock *sb = (const hqq_superblock *)n->map;
        state = atomic_load_explicit(&sb->state, memory_order_seq_cst);
        token = atomic_load_explicit(&sb->creator_token, memory_order_seq_cst);
    } else if (known) {
        return n->fresh; /* same object, never mapped: ours if we created it */
    } else {
        int found = 0;
        st = hqq_plat_shm_peek(n->name, (size_t)hqq_plat_page_size(), &state, &token, &found);
        if (st != HQQ_OK || !found) {
            return 0;
        }
    }
    if (token == n->instance && token != 0) {
        return 1;
    }
    /* A segment that was created but never claimed has no token yet. */
    return n->fresh && state == HQQ_STATE_EMPTY;
}

hqq_status hqq_named_release(const hqq_allocator *a, hqq_named *n) {
    hqq_status st = HQQ_OK;
    int remove = 0;

    if (n == NULL) {
        return HQQ_OK;
    }
    hqq_spin_lock(&n->lock);
    if (n->shm != NULL && n->unlink_on_close) {
        remove = owns_name(n); /* decided while the mapping and descriptor are open */
    }
    if (n->map != NULL) {
        hqq_plat_shm_unmap(n->shm, n->map, n->map_size);
        n->map = NULL;
    }
    if (n->shm != NULL) {
        hqq_plat_shm_close(a, n->shm);
        n->shm = NULL;
        if (remove) {
            hqq_status rc = hqq_plat_unlink(n->name);
            if (rc != HQQ_OK && rc != HQQ_ERR_NOT_EXIST) {
                st = rc;
            }
        }
    }
    hqq_spin_unlock(&n->lock);
    hqq_free(a, n->name, n->name_size);
    hqq_free(a, n, sizeof *n);
    return st;
}

hqq_status hqq_named_unlink(hqq_named *n) {
    hqq_status st = HQQ_OK;

    hqq_spin_lock(&n->lock);
    if (n->shm != NULL) {
        n->unlink_on_close = 0;
        if (owns_name(n)) {
            st = hqq_plat_shm_delete(n->shm);
            if (st == HQQ_ERR_NOT_EXIST) {
                st = HQQ_OK;
            }
        }
    }
    hqq_spin_unlock(&n->lock);
    return st;
}

hqq_status hqq_unlink(const char *name) {
    if (name == NULL) {
        return HQQ_ERR_INVALID_NAME;
    }
    return hqq_plat_unlink(name);
}

/*
 * Opens (created == 0) or exclusively creates (created != 0) the object and
 * maps `size` bytes. The creator sizes the object right after creating it; an
 * opener checks the object size BEFORE mapping (touching a mapping beyond the
 * end of the object raises SIGBUS).
 */
static hqq_status map_named(const hqq_allocator *a, hqq_named *n, size_t size, int created, unsigned perm,
                            int *os_err) {
    hqq_status st = hqq_plat_shm_open(a, n->name, size, created, perm, &n->shm, os_err);

    if (st != HQQ_OK) {
        return st;
    }
    n->unlink_on_close = created;
    n->fresh = created;
    if (!created) {
        int64_t cur = hqq_plat_shm_size(n->shm, os_err);
        if (cur < 0) {
            return HQQ_ERR_SYSTEM;
        }
        if (cur == 0) {
            return HQQ_ERR_SEGMENT_NOT_READY;
        }
        if (cur < (int64_t)size) {
            return HQQ_ERR_INVALID_SIZE;
        }
    }
    st = hqq_plat_shm_map(n->shm, size, &n->map, os_err);
    if (st != HQQ_OK) {
        return st;
    }
    n->map_size = size;
    if ((uintptr_t)n->map % hqq_plat_page_size() != 0) {
        return HQQ_ERR_MEMORY_ALIGN;
    }
    return HQQ_OK;
}

hqq_status hqq_named_open(const char *name, uint64_t count, uint64_t size, int create, unsigned perm,
                          const hqq_options *opts, hqq_link **out, hqq_open_info *info) {
    hqq_open_req req;
    hqq_named *n;
    hqq_status st;
    size_t total;
    int os_err = 0, attempt;
    size_t name_len;

    if (out == NULL || name == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    if (info != NULL) {
        info->os_error = 0;
        memset(&info->want, 0, sizeof info->want);
        memset(&info->got, 0, sizeof info->got);
    }
    memset(&req, 0, sizeof req);
    st = hqq_options_normalize(opts, &req, info);
    if (st != HQQ_OK) {
        return st;
    }
    total = hqq_standard_size(count, size);
    if (total == 0) {
        return HQQ_ERR_INVALID_SIZE;
    }

    name_len = strlen(name) + 1;
    n = (hqq_named *)hqq_alloc(opt_alloc(&req), sizeof *n);
    if (n == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    memset(n, 0, sizeof *n);
    n->name = (char *)hqq_alloc(opt_alloc(&req), name_len);
    if (n->name == NULL) {
        hqq_free(opt_alloc(&req), n, sizeof *n);
        return HQQ_ERR_NO_MEMORY;
    }
    n->name_size = name_len;
    memcpy(n->name, name, name_len);
    atomic_flag_clear(&n->lock.f);

    for (attempt = 0;; attempt++) {
        st = map_named(opt_alloc(&req), n, total, create, perm, &os_err);
        if (create && st == HQQ_ERR_EXISTS) {
            /* The name exists: look at the existing object instead (Creator rules, SPEC 11.3). */
            st = map_named(opt_alloc(&req), n, total, 0, 0, &os_err);
            if (st == HQQ_ERR_NOT_EXIST && attempt < 3) {
                continue; /* removed between the two opens: create it */
            }
        }
        break;
    }
    if (st != HQQ_OK) {
        if (info != NULL) {
            info->os_error = os_err;
        }
        (void)hqq_named_release(opt_alloc(&req), n);
        return st;
    }

    req.base = (unsigned char *)n->map;
    req.named = n;
    if (create) {
        req.role = HQQ_ROLE_CREATOR;
        if (hqq_new_token(&req.creator_token) != 0) {
            (void)hqq_named_release(opt_alloc(&req), n);
            return HQQ_ERR_SYSTEM;
        }
        n->instance = req.creator_token;
    } else {
        req.role = HQQ_ROLE_OPENER;
    }

    st = hqq_link_open_core(&req, count, size, out, info);
    if (st != HQQ_OK) {
        (void)hqq_named_release(opt_alloc(&req), n);
        return st;
    }
    /* The segment is claimed or joined now: it is no longer an object nobody initialized,
     * so a later Empty object under the same name is not ours (SPEC 11.4; named_link.go:309). */
    n->fresh = 0;
    if (create) {
        /* We initialized the segment under a new creatorToken: the name is ours to remove. */
        n->unlink_on_close = 1;
    } else {
        const hqq_superblock *sb = (const hqq_superblock *)n->map;
        n->instance = atomic_load_explicit(&sb->creator_token, memory_order_seq_cst);
    }
    return HQQ_OK;
}

hqq_status hqq_standard_create(const char *name, uint64_t buffer_count, uint64_t buffer_size, unsigned perm,
                               const hqq_options *opts, hqq_link **out, hqq_open_info *info) {
    return hqq_named_open(name, buffer_count, buffer_size, 1, perm, opts, out, info);
}

hqq_status hqq_standard_open(const char *name, uint64_t buffer_count, uint64_t buffer_size,
                             const hqq_options *opts, hqq_link **out, hqq_open_info *info) {
    return hqq_named_open(name, buffer_count, buffer_size, 0, 0, opts, out, info);
}

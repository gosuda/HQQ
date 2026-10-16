/*
 * hqq_adv_rx.c - the receive path of the Advanced layer: validation and
 * reassembly of OpConnCopy frames (SPEC 10.5, 1.1 section 3.2) and routing of
 * completed messages (SPEC 10.6, 10.7). Only the dispatcher thread calls
 * adv_handle_copy(); the routing functions take the link lock.
 */
#include <limits.h>

#include "hqq_adv_internal.h"

/* ---- Metadata block (1.1 section 3.2) ------------------------------------------------- */

#define META_HDR 4u
#define TAG_BUDGET 1u
#define TAG_CALLER 2u
#define TAG_TRACE 3u

static uint64_t rd_u16(const unsigned char *p) { return (uint64_t)p[0] | ((uint64_t)p[1] << 8); }

static uint64_t rd_u64(const unsigned char *p) {
    uint64_t v = 0;
    unsigned i;
    for (i = 0; i < 8; i++) {
        v |= (uint64_t)p[i] << (8 * i);
    }
    return v;
}

/* Parses and removes the block of a completed request. HQQ_ERR_PROTOCOL when malformed. */
hqq_status adv_meta_strip(hqq_msg *m) {
    const unsigned char *p = m->data;
    uint64_t total = m->len;
    uint64_t tlv_len, pos;

    if (total < META_HDR) {
        return HQQ_ERR_PROTOCOL;
    }
    tlv_len = rd_u16(p);
    if (META_HDR + tlv_len > total) {
        return HQQ_ERR_PROTOCOL;
    }
    m->has_budget = 0;
    m->has_caller = 0;
    m->has_trace = 0;
    pos = META_HDR;
    while (pos < META_HDR + tlv_len) {
        unsigned tag, len;
        const unsigned char *v;
        if (pos + 2 > META_HDR + tlv_len) {
            return HQQ_ERR_PROTOCOL;
        }
        tag = p[pos];
        len = p[pos + 1];
        v = p + pos + 2;
        if (pos + 2 + len > META_HDR + tlv_len) {
            return HQQ_ERR_PROTOCOL;
        }
        switch (tag) {
        case TAG_BUDGET:
            if (len != 8) {
                return HQQ_ERR_PROTOCOL;
            }
            m->has_budget = 1;
            m->budget_ns = rd_u64(v);
            if (m->budget_ns > (uint64_t)INT64_MAX) {
                m->budget_ns = (uint64_t)INT64_MAX;
            }
            break;
        case TAG_CALLER:
            if (len != 8) {
                return HQQ_ERR_PROTOCOL;
            }
            m->caller = rd_u64(v);
            m->has_caller = m->caller != 0;
            break;
        case TAG_TRACE:
            if (len != 16) {
                return HQQ_ERR_PROTOCOL;
            }
            memcpy(m->trace, v, 16);
            m->has_trace = 0;
            for (unsigned i = 0; i < 16; i++) {
                if (m->trace[i] != 0) {
                    m->has_trace = 1;
                }
            }
            break;
        default:
            break; /* unknown tags are skipped */
        }
        pos += 2 + len;
    }
    /* Remove the block: the payload follows it. */
    m->len = total - (META_HDR + tlv_len);
    if (m->len > 0) {
        memmove(m->data, p + META_HDR + tlv_len, (size_t)m->len);
    }
    return HQQ_OK;
}

/* ---- OpConnCopy validation and reassembly (SPEC 10.5) ---------------------------------- */

static void fail_asm_locked(hqq_adv *a, adv_asm *x) {
    if (x != NULL) {
        adv_drop_asm_locked(a, x);
    }
}

/* Validation failure: drops the partial message and the refusal memory of its key. */
static hqq_status invalid(hqq_adv *a, uint64_t conn, uint64_t msg_id, uint32_t kind, hqq_status st) {
    adv_asm *x;
    hqq_spin_lock(&a->lock);
    x = NULL;
    {
        adv_asm *c;
        for (c = a->asms; c != NULL; c = c->next) {
            if (c->conn == conn && c->msg_id == msg_id && c->kind == kind) {
                x = c;
                break;
            }
        }
    }
    fail_asm_locked(a, x);
    adv_forget_refused_locked(a, conn, msg_id, kind);
    hqq_spin_unlock(&a->lock);
    return st;
}

/* Finds the partial message of a key (dispatcher only; a->lock held). */
static adv_asm *asm_lookup(hqq_adv *a, uint64_t conn, uint64_t msg_id, uint32_t kind) {
    adv_asm *x;
    for (x = a->asms; x != NULL; x = x->next) {
        if (x->conn == conn && x->msg_id == msg_id && x->kind == kind) {
            return x;
        }
    }
    return NULL;
}

hqq_status adv_handle_copy(hqq_adv *a, const hqq_packet *pkt, const unsigned char *payload,
                           adv_copy_out *out) {
    uint64_t conn = hqq_packet_operand(pkt, 0);
    uint64_t msg_id = hqq_packet_operand(pkt, 1);
    uint64_t total = hqq_packet_operand(pkt, 2);
    uint64_t off = hqq_packet_operand(pkt, 3);
    uint64_t csz = hqq_packet_operand(pkt, 4);
    uint64_t flags = hqq_packet_operand(pkt, 5);
    uint64_t aux = hqq_packet_operand(pkt, 6);
    uint32_t kind;
    uint64_t features;
    adv_asm *x;

    memset(out, 0, sizeof *out);
    features = a->features;

    /* 1. Flags (1.0 section 10.5 step 1, 1.1 section 3.1). Every frame, abort included. */
    if ((flags & ~(uint64_t)ADV_KNOWN_FLAGS) != 0) {
        return HQQ_ERR_PROTOCOL;
    }
    if ((flags & ADV_META) != 0 && ((features & HQQ_FEATURE_METADATA) == 0 || (flags & ADV_REQUEST) == 0)) {
        return HQQ_ERR_PROTOCOL;
    }
    if ((flags & ADV_REQUEST) != 0 && (flags & ADV_RESPONSE) != 0) {
        return HQQ_ERR_PROTOCOL;
    }
    if ((flags & ADV_ERROR) != 0 && (flags & ADV_RESPONSE) == 0) {
        return HQQ_ERR_PROTOCOL;
    }
    kind = (uint32_t)(flags & ADV_KIND_MASK);

    /* 2. Abort frames are accepted whatever their other operands are. */
    if ((flags & ADV_ABORT) != 0) {
        adv_call *c;
        hqq_spin_lock(&a->lock);
        x = asm_lookup(a, conn, msg_id, kind);
        if (x != NULL) {
            adv_drop_asm_locked(a, x);
        }
        if (adv_is_refused_locked(a, conn, msg_id, kind)) {
            adv_forget_refused_locked(a, conn, msg_id, kind);
        } else if (kind == ADV_RESPONSE) {
            for (c = a->calls; c != NULL; c = c->next) {
                if (c->conn == conn && c->msg_id == msg_id && !c->done) {
                    c->done = 1;
                    c->status = HQQ_ERR_MESSAGE_ABORTED;
                    adv_wake_locked(a);
                    break;
                }
            }
        }
        hqq_spin_unlock(&a->lock);
        return HQQ_OK;
    }

    /* 3. Geometry checks. Any failure fails the receive path. */
    if (csz > a->buffer_size || off > total || csz > total - off) {
        return invalid(a, conn, msg_id, kind, HQQ_ERR_INVALID_SIZE);
    }
    if (total > a->max_message) {
        return invalid(a, conn, msg_id, kind, HQQ_ERR_BUFFER_OVERFLOW);
    }
    if (total == 0 && (off != 0 || csz != 0 || (flags & (ADV_BEGIN | ADV_END)) != (ADV_BEGIN | ADV_END))) {
        return invalid(a, conn, msg_id, kind, HQQ_ERR_INVALID_SIZE);
    }
    if (total > 0 && csz == 0) {
        return invalid(a, conn, msg_id, kind, HQQ_ERR_INVALID_SIZE);
    }
    if (off != 0 && aux != 0) {
        return invalid(a, conn, msg_id, kind, HQQ_ERR_INVALID_SIZE);
    }
    if (total > (uint64_t)INT_MAX) {
        return invalid(a, conn, msg_id, kind, HQQ_ERR_BUFFER_OVERFLOW);
    }

    /* 4. Begin: starts a message. */
    if ((flags & ADV_BEGIN) != 0) {
        uint64_t t0 = hqq_plat_mono_ns();
        hqq_spin_lock(&a->lock);
        x = asm_lookup(a, conn, msg_id, kind);
        if (off != 0 || x != NULL || adv_is_refused_locked(a, conn, msg_id, kind)) {
            hqq_spin_unlock(&a->lock);
            return invalid(a, conn, msg_id, kind, HQQ_ERR_INVALID_SIZE);
        }
        hqq_spin_unlock(&a->lock);

        if ((flags & ADV_END) != 0) {
            /* Single-chunk message: complete at once, not charged to the budget. */
            hqq_msg *m;
            if (csz != total) {
                return HQQ_ERR_INVALID_SIZE;
            }
            m = adv_msg_get(a->pool, total);
            if (m == NULL) {
                return HQQ_ERR_NO_MEMORY;
            }
            m->len = total;
            if (total != 0) {
                memcpy(m->data, payload, (size_t)csz);
            }
            m->conn = conn;
            m->msg_id = msg_id;
            m->flags = (uint32_t)flags;
            m->aux = aux;
            m->t0_mono = t0;
            if ((flags & ADV_META) != 0) {
                hqq_status st = adv_meta_strip(m);
                if (st != HQQ_OK) {
                    adv_msg_put(m);
                    return st;
                }
            }
            out->complete = m;
            return HQQ_OK;
        }

        uint64_t charge = total + ADV_REASSEMBLY_OVERHEAD;
        {
            int refuse;
            hqq_spin_lock(&a->lock);
            refuse = charge > a->max_reassembly || a->assembly_bytes > a->max_reassembly - charge;
            if (refuse) {
                adv_refuse_locked(a, conn, msg_id, kind);
                hqq_spin_unlock(&a->lock);
                out->refused_conn = conn;
                out->refused_msg = msg_id;
                if (kind == ADV_REQUEST) {
                    out->refused_request = 1;
                } else if (kind == ADV_RESPONSE) {
                    out->refused_response = 1;
                } else {
                    hqq_spin_lock(&a->lock);
                    a->st_dropped++;
                    hqq_spin_unlock(&a->lock);
                }
                return HQQ_OK;
            }
            hqq_spin_unlock(&a->lock);
        }

        /* Allocate outside the lock: a pool miss may call the allocator. */
        x = (adv_asm *)NULL;
        {
            hqq_msg *buf = adv_msg_get(a->pool, total);
            adv_asm *n;
            if (buf == NULL) {
                return HQQ_ERR_NO_MEMORY;
            }
            hqq_spin_lock(&a->lock);
            n = a->asm_free;
            if (n != NULL) {
                a->asm_free = n->next;
            }
            hqq_spin_unlock(&a->lock);
            if (n == NULL) {
                n = (adv_asm *)hqq_alloc(a->alloc_set ? &a->alloc : NULL, sizeof *n);
                if (n == NULL) {
                    adv_msg_put(buf);
                    return HQQ_ERR_NO_MEMORY;
                }
            }
            memset(n, 0, sizeof *n);
            buf->len = total;
            n->msg = buf;
            n->conn = conn;
            n->msg_id = msg_id;
            n->kind = kind;
            n->hdr_flags = (uint32_t)(flags & ~(uint64_t)(ADV_BEGIN | ADV_END));
            n->aux = aux;
            n->total = total;
            n->t0_mono = t0;
            n->charge = charge;
            n->next_off = 0;
            hqq_spin_lock(&a->lock);
            a->assembly_bytes += charge;
            n->next = a->asms;
            a->asms = n;
            hqq_spin_unlock(&a->lock);
            x = n;
        }
        memcpy(x->msg->data, payload, (size_t)csz);
        x->next_off = csz;
        return HQQ_OK;
    }

    /* 5. Continuation chunks. */
    hqq_spin_lock(&a->lock);
    x = asm_lookup(a, conn, msg_id, kind);
    if (x == NULL) {
        int drained = adv_is_refused_locked(a, conn, msg_id, kind) || a->refused_evicted;
        if (drained && (flags & ADV_END) != 0) {
            adv_forget_refused_locked(a, conn, msg_id, kind);
        }
        hqq_spin_unlock(&a->lock);
        if (!drained) {
            return HQQ_ERR_PROTOCOL; /* no message, no refusal: SPEC 10.5 frame validity */
        }
        return HQQ_OK; /* a refused message: drain the chunk */
    }
    if (off != x->next_off || x->total != total) {
        adv_drop_asm_locked(a, x);
        hqq_spin_unlock(&a->lock);
        return HQQ_ERR_INVALID_SIZE;
    }
    hqq_spin_unlock(&a->lock);
    memcpy(x->msg->data + off, payload, (size_t)csz);
    x->next_off = off + csz;
    if ((flags & ADV_END) == 0) {
        return HQQ_OK;
    }

    /* End: the message is complete. Its header is the Begin frame's. */
    hqq_spin_lock(&a->lock);
    if (x->next_off != total) {
        adv_drop_asm_locked(a, x);
        hqq_spin_unlock(&a->lock);
        return HQQ_ERR_INVALID_SIZE;
    }
    {
        hqq_msg *m = x->msg;
        uint32_t hdr = x->hdr_flags;
        uint64_t first_aux = x->aux;
        uint64_t t0 = x->t0_mono;
        adv_asm **pp = &a->asms;
        hqq_status st;

        a->assembly_bytes -= x->charge;
        while (*pp != NULL && *pp != x) {
            pp = &(*pp)->next;
        }
        if (*pp == x) {
            *pp = x->next;
        }
        x->msg = NULL;
        x->next = a->asm_free;
        a->asm_free = x;
        hqq_spin_unlock(&a->lock);

        m->conn = conn;
        m->msg_id = msg_id;
        m->flags = hdr | ADV_BEGIN | ADV_END;
        m->aux = first_aux;
        m->t0_mono = t0;
        if ((hdr & ADV_META) != 0) {
            st = adv_meta_strip(m);
            if (st != HQQ_OK) {
                adv_msg_put(m);
                return st;
            }
        }
        out->complete = m;
    }
    return HQQ_OK;
}

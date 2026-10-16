#!/usr/bin/env python3
"""Exhaustive check of the store-buffering (Dekker) shape of the HQQ wake handshake
on an x86-TSO operational model (per-thread FIFO store buffers, locked RMW and
MFENCE drain the buffer, xchg == store + drain).

This is NOT herd7 and NOT the C11 model. It demonstrates one concrete, hardware-
realizable machine on which the release/acquire compilation of the handshake loses
the wake-up, and that the seq_cst / fenced compilations do not. Run:

    python3 spec/litmus/tso_sb_check.py

Compilation of the C11 orders on x86-64 (GCC/Clang):
    store release, load acquire, load seq_cst  -> plain mov
    store seq_cst                              -> xchg   (store + full barrier)
    fetch_add (any order)                      -> lock xadd (full barrier)
    atomic_thread_fence(seq_cst)               -> mfence
"""
import sys

# op forms: ("st", loc, val) buffered store   ("xchg", loc, val) store with barrier
#           ("ld", loc, reg)                 ("add", loc, n, reg) locked RMW (barrier)
#           ("mfence",)
WAKER_ACQREL = [("st", "seq", 1), ("ld", "waiters", "r0")]            # release store ; acquire load
WAITER_ACQREL = [("add", "waiters", 1, "r2"), ("ld", "seq", "r1")]    # acq_rel RMW ; acquire load
WAKER_SC = [("xchg", "seq", 1), ("ld", "waiters", "r0")]              # seq_cst store ; seq_cst load
WAITER_SC = [("add", "waiters", 1, "r2"), ("ld", "seq", "r1")]
WAKER_FENCE = [("st", "seq", 1), ("mfence",), ("ld", "waiters", "r0")]
WAITER_FENCE = [("add", "waiters", 1, "r2"), ("mfence",), ("ld", "seq", "r1")]


def explore(threads):
    n = len(threads)
    init = (tuple([0] * n), tuple(() for _ in range(n)), (("seq", 0), ("waiters", 0)), tuple(() for _ in range(n)))
    seen, finals, stack = set(), set(), [init]
    while stack:
        state = stack.pop()
        if state in seen:
            continue
        seen.add(state)
        pcs, bufs, mem, regs = state
        memd = dict(mem)
        progressed = False
        for t in range(n):
            # flush the oldest buffered store of thread t
            if bufs[t]:
                (loc, val), rest = bufs[t][0], bufs[t][1:]
                m2 = dict(memd); m2[loc] = val
                nb = bufs[:t] + (rest,) + bufs[t + 1:]
                stack.append((pcs, nb, tuple(sorted(m2.items())), regs)); progressed = True
            if pcs[t] >= len(threads[t]):
                continue
            op = threads[t][pcs[t]]
            npcs = pcs[:t] + (pcs[t] + 1,) + pcs[t + 1:]
            if op[0] == "st":
                nb = bufs[:t] + (bufs[t] + ((op[1], op[2]),),) + bufs[t + 1:]
                stack.append((npcs, nb, mem, regs)); progressed = True
            elif op[0] == "ld":
                val = memd[op[1]]
                for loc, v in reversed(bufs[t]):          # store forwarding from own buffer
                    if loc == op[1]:
                        val = v; break
                nr = regs[:t] + (regs[t] + ((op[2], val),),) + regs[t + 1:]
                stack.append((npcs, bufs, mem, nr)); progressed = True
            elif op[0] in ("xchg", "add", "mfence"):
                if bufs[t]:
                    continue                                # barrier: buffer must be drained first
                m2 = dict(memd); nr = regs
                if op[0] == "xchg":
                    m2[op[1]] = op[2]
                elif op[0] == "add":
                    old = m2[op[1]]; m2[op[1]] = old + op[2]
                    nr = regs[:t] + (regs[t] + ((op[3], old),),) + regs[t + 1:]
                stack.append((npcs, bufs, tuple(sorted(m2.items())), nr)); progressed = True
        if not progressed or (all(p >= len(threads[i]) for i, p in enumerate(pcs)) and not any(bufs)):
            if all(p >= len(threads[i]) for i, p in enumerate(pcs)) and not any(bufs):
                finals.add((dict(regs[0]).get("r0"), dict(regs[1]).get("r1")))
    return finals


def main():
    cases = [
        ("release/acquire compilation (plain mov + lock xadd)", [WAKER_ACQREL, WAITER_ACQREL], True),
        ("seq_cst compilation (xchg store, lock xadd)", [WAKER_SC, WAITER_SC], False),
        ("release/acquire + seq_cst fences (mfence)", [WAKER_FENCE, WAITER_FENCE], False),
    ]
    ok = True
    for name, threads, lost_expected in cases:
        finals = explore(threads)
        lost = (0, 0) in finals            # waker read waiters == 0  AND  waiter read old seq == 0
        print(f"{name}: outcomes (r0=waiters seen by waker, r1=seq seen by waiter) = {sorted(finals)}")
        print(f"    lost wake-up outcome (r0=0, r1=0) reachable: {lost}   [expected {lost_expected}]")
        ok &= lost == lost_expected
    print("RESULT:", "as expected" if ok else "UNEXPECTED")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())

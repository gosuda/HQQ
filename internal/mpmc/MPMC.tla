-------------------------------- MODULE MPMC --------------------------------
(***************************************************************************)
(* Model of the bounded lock-free MPMC ring buffer in mpmc.go (Dmitry      *)
(* Vyukov's algorithm).                                                    *)
(*                                                                         *)
(*   Producer (Enqueue / ReserveProducer + Commit):                        *)
(*     pos := load(w)                                                      *)
(*     loop: diff := int64(seq[pos&mask] - pos)                            *)
(*           diff == 0 : if CAS(w, pos, pos+1) { write data;               *)
(*                                  CAS seq: pos -> pos+1 }                *)
(*           diff <  0 : queue full, retry with the same pos               *)
(*           diff >  0 : another producer got pos, pos := load(w)          *)
(*                                                                         *)
(*   Consumer (Dequeue / ReserveConsumer + Release):                       *)
(*     pos := load(r)                                                      *)
(*     loop: diff := int64(seq[pos&mask] - (pos+1))                        *)
(*           diff == 0 : if CAS(r, pos, pos+1) { read data;                *)
(*                         CAS seq: pos+1 -> pos+mask+1 }                  *)
(*           diff <  0 : queue empty, retry with the same pos              *)
(*           diff >  0 : another consumer got pos, pos := load(r)          *)
(*                                                                         *)
(* Abstractions (all deliberate, see comments at the actions):             *)
(*  - uint64 counters are modelled modulo MaxVal = 4 * BufferSize. All     *)
(*    subtraction goes through UnsignedSub/SignedSub, which reproduce      *)
(*    Go's `int64(a - b)` on uint64 operands exactly, with 2^64 replaced   *)
(*    by MaxVal. The small modulus makes the model *more* adversarial than *)
(*    Go: a preempted thread can fall behind by >= MaxVal/2 positions, so  *)
(*    sign wraparound and ABA on the cursors are actually explored.        *)
(*  - "load seq, compare, CAS cursor" is one atomic step. This is sound:   *)
(*    if the CAS succeeds the cursor still equals pos, so no other thread  *)
(*    claimed pos in between, so seq[pos&mask] could not have changed      *)
(*    since it was loaded (only the owner of pos, or the consumer of       *)
(*    pos-BufferSize when diff < 0, writes it). If the CAS fails, or diff  *)
(*    /= 0, the thread only retries, which the model also allows.          *)
(*  - Retrying "with the same pos" after diff < 0 leaves every variable    *)
(*    unchanged, i.e. it is a stuttering step. After a failed cursor CAS   *)
(*    the Go code reloads pos (pos = load(w) / load(r)); the model allows  *)
(*    that (P_Check/C_Check, "reload pos") and, as stuttering, also keeping *)
(*    the old pos, so implementations of either kind are covered.          *)
(*  - The publish and release CASes (seq: pos -> pos+1 and pos+1 ->        *)
(*    pos+mask+1) are modelled as a plain step that is enabled when the    *)
(*    CAS expected value matches. A slot's owner is unique (NoRace) and    *)
(*    nobody else writes the slot's seq while it is owned, so the CAS      *)
(*    cannot fail for the owner; the invariant CompletionCAS checks that   *)
(*    the expected value always matches in the Publish and Read states.    *)
(*  - Blocking/parking (futex wait words, Windows wait objects), context   *)
(*    cancellation before a claim and Try* variants that give up are not   *)
(*    modelled: before a claim they have no effect on shared ring state    *)
(*    (they are equivalent to spinning or to abandoning a local pos). Lost *)
(*    wake-ups in the parking layer are outside the scope of this model.   *)
(*  - A zero-copy callback that panics still publishes (producer) or       *)
(*    releases (consumer) its slot from a deferred call. Publishing a      *)
(*    zeroed element is the ordinary Enqueue step with another payload,    *)
(*    so the model covers it; payload values are not modelled.             *)
(*  - The writers/readers sets and the variable q are ghost state used     *)
(*    only for checking; they do not influence the algorithm.              *)
(*                                                                         *)
(* VERIFICATION STATUS                                                     *)
(*   No machine-checked (TLAPS) proof exists. The properties below are     *)
(*   model-checked with TLC (tla2tools v1.8.0, run via scripts/tlc.sh) for  *)
(*   finite instances only:                                                *)
(*     MPMC.cfg (CI):  BufferSize = 2, Producers = {p1,p2},                *)
(*                     Consumers = {c1,c2}  (MaxVal = 8)                   *)
(*     Manual run:     BufferSize = 4, Producers = {p1,p2},                *)
(*                     Consumers = {c1,c2}  (MaxVal = 16)                  *)
(*   Invariants: TypeOK, QueueSafety, NoRace, DataIntegrity, FIFOBound,    *)
(*               CompletionCAS.                                            *)
(*   Properties: FIFOClaimOrder, ProducerProgress, ConsumerProgress,       *)
(*               SystemProgress.                                           *)
(*   Deadlock checking is ON (no -deadlock flag): the processes loop       *)
(*   forever, so a state without successors would be a bug.                *)
(*                                                                         *)
(*   NOT modelled (outside the scope of this specification):               *)
(*     - parking, wake-ups and the waiter counters in the ring header      *)
(*       (futex/ulock/Parker), including the process-local waiter          *)
(*       aggregation layer (one leader per wait word parks, the others     *)
(*       wait on a Go channel), which only affects when a blocked thread   *)
(*       re-runs the loop above;                                           *)
(*     - Format (re-initialization while the ring is quiescent);           *)
(*     - process crashes: a thread that dies between claim and publish     *)
(*       (or release) holds its slot forever, which blocks the ring and is *)
(*       what the fairness assumption below excludes;                      *)
(*     - stale copies of a reservation: their publish/release CAS fails,   *)
(*       and a failed CAS is a no-op for the ring, i.e. a stuttering step. *)
(***************************************************************************)
EXTENDS Integers, Sequences, FiniteSets, TLC

CONSTANTS BufferSize,  \* Number of slots (power of 2, >= 2; e.g. 2)
          Producers,   \* Set of producer processes (e.g. {p1, p2})
          Consumers    \* Set of consumer processes (e.g. {c1, c2})

\* A single slot is not a valid ring: pos+1 (published) and pos+mask+1
\* (free for the next lap) coincide when mask = 0, so a second producer would
\* overwrite an unconsumed element. mpmc.Format rejects size 1.
ASSUME /\ BufferSize \in Nat \ {0, 1}
       /\ \E k \in 0..BufferSize : 2^k = BufferSize   \* power of two
       /\ Producers \cap Consumers = {}
       /\ Producers /= {} /\ Consumers /= {}

\* Modulus standing in for 2^64. It is a multiple of BufferSize (so that
\* pos % BufferSize is the same as Go's pos & mask) and larger than
\* 2 * BufferSize, so that the signed difference can distinguish
\* "full/empty" from "already claimed" for up-to-date threads.
MaxVal == BufferSize * 4

Slots     == 0..(BufferSize - 1)
Positions == 0..(MaxVal - 1)

\* Go: a - b on uint64 (wraps modulo 2^64), here modulo MaxVal.
UnsignedSub(a, b) == (a - b + MaxVal) % MaxVal

\* Go: int64(a - b) for uint64 a, b. Two's-complement reinterpretation:
\* values >= 2^63 become negative. Here 2^63 is MaxVal \div 2.
SignedSub(a, b) ==
    LET diff == UnsignedSub(a, b)
    IN IF diff < (MaxVal \div 2) THEN diff ELSE diff - MaxVal

\* Go: x + 1 and x + mask + 1 on uint64.
Inc(x)        == (x + 1) % MaxVal
IncBy(x, val) == (x + val) % MaxVal

\* "No payload yet" marker for data. Not a valid position.
NoData == -1

VARIABLES seq,        \* seq[i]: sequence number of slot i (in Positions)
          w,          \* write cursor (in Positions)
          r,          \* read cursor (in Positions)
          pc,         \* program counter of every process
          p_pos,      \* producer-local copy of the write position
          c_pos,      \* consumer-local copy of the read position
          data,       \* data[i]: payload of slot i; producers store their pos
          writers,    \* ghost: producers currently owning slot i
          readers,    \* ghost: consumers currently owning slot i
          q           \* ghost: abstract FIFO of positions whose enqueue has
                      \*        linearized (w CAS) but whose dequeue has not
                      \*        (r CAS), in linearization order

vars == <<seq, w, r, pc, p_pos, c_pos, data, writers, readers, q>>

ProducerPCs == {"Loop", "Check", "Write", "Publish"}
ConsumerPCs == {"Loop", "Check", "Read"}

TypeOK ==
    /\ seq \in [Slots -> Positions]
    /\ w \in Positions
    /\ r \in Positions
    /\ \A p \in Producers : pc[p] \in ProducerPCs
    /\ \A c \in Consumers : pc[c] \in ConsumerPCs
    /\ p_pos \in [Producers -> Positions]
    /\ c_pos \in [Consumers -> Positions]
    /\ data \in [Slots -> Positions \cup {NoData}]
    /\ writers \in [Slots -> SUBSET Producers]
    /\ readers \in [Slots -> SUBSET Consumers]
    /\ q \in Seq(Positions)

Init ==
    /\ seq = [i \in Slots |-> i]      \* Go Format: e._seq = i
    /\ w = 0
    /\ r = 0
    /\ pc = [x \in Producers \cup Consumers |-> "Loop"]
    /\ p_pos = [p \in Producers |-> 0]
    /\ c_pos = [c \in Consumers |-> 0]
    /\ data = [i \in Slots |-> NoData]
    /\ writers = [i \in Slots |-> {}]
    /\ readers = [i \in Slots |-> {}]
    /\ q = <<>>

-----------------------------------------------------------------------------
(* Producer actions *)

\* pos := atomic.LoadUint64(&w)
P_Load(p) ==
    /\ pc[p] = "Loop"
    /\ p_pos' = [p_pos EXCEPT ![p] = w]
    /\ pc' = [pc EXCEPT ![p] = "Check"]
    /\ UNCHANGED <<seq, w, r, c_pos, data, writers, readers, q>>

\* seq load + diff + CAS(w, pos, pos+1), see the atomicity argument above.
P_Check(p) ==
    /\ pc[p] = "Check"
    /\ LET pos  == p_pos[p]
           idx  == pos % BufferSize
           diff == SignedSub(seq[idx], pos)
       IN \/ \* diff == 0 and CAS succeeds: slot claimed (enqueue linearizes)
             /\ diff = 0
             /\ w = pos
             /\ w' = Inc(pos)
             /\ writers' = [writers EXCEPT ![idx] = @ \cup {p}]
             /\ q' = Append(q, pos)
             /\ pc' = [pc EXCEPT ![p] = "Write"]
             /\ UNCHANGED <<seq, r, p_pos, c_pos, data, readers>>
          \/ \* diff > 0 (pos taken), or diff == 0 with a failed CAS:
             \* reload pos. (Retrying with the same pos is stuttering.)
             /\ \/ diff > 0
                \/ diff = 0 /\ w /= pos
             /\ pc' = [pc EXCEPT ![p] = "Loop"]
             /\ UNCHANGED <<seq, w, r, p_pos, c_pos, data, writers, readers, q>>
          \* diff < 0 (queue full): retry with the same pos. Nothing changes,
          \* so this is a stuttering step and needs no explicit disjunct.

\* Store the payload into the owned slot (ProducerSlot.Value() / copy elem).
P_Write(p) ==
    /\ pc[p] = "Write"
    /\ data' = [data EXCEPT ![p_pos[p] % BufferSize] = p_pos[p]]
    /\ pc' = [pc EXCEPT ![p] = "Publish"]
    /\ UNCHANGED <<seq, w, r, p_pos, c_pos, writers, readers, q>>

\* CompareAndSwapUint64(&slot._seq, pos, pos+1)  (ProducerSlot.Commit), which
\* always succeeds for the owner (see CompletionCAS).
P_Publish(p) ==
    /\ pc[p] = "Publish"
    /\ LET pos == p_pos[p]
           idx == pos % BufferSize
       IN /\ seq[idx] = pos                             \* CAS expected value
          /\ seq' = [seq EXCEPT ![idx] = Inc(pos)]
          /\ writers' = [writers EXCEPT ![idx] = @ \ {p}]
    /\ pc' = [pc EXCEPT ![p] = "Loop"]
    /\ UNCHANGED <<w, r, p_pos, c_pos, data, readers, q>>

Producer(p) == P_Load(p) \/ P_Check(p) \/ P_Write(p) \/ P_Publish(p)

-----------------------------------------------------------------------------
(* Consumer actions *)

\* pos := atomic.LoadUint64(&r)
C_Load(c) ==
    /\ pc[c] = "Loop"
    /\ c_pos' = [c_pos EXCEPT ![c] = r]
    /\ pc' = [pc EXCEPT ![c] = "Check"]
    /\ UNCHANGED <<seq, w, r, p_pos, data, writers, readers, q>>

\* seq load + diff + CAS(r, pos, pos+1)
C_Check(c) ==
    /\ pc[c] = "Check"
    /\ LET pos  == c_pos[c]
           idx  == pos % BufferSize
           diff == SignedSub(seq[idx], Inc(pos))
       IN \/ \* diff == 0 and CAS succeeds: slot claimed (dequeue linearizes)
             /\ diff = 0
             /\ r = pos
             /\ r' = Inc(pos)
             /\ readers' = [readers EXCEPT ![idx] = @ \cup {c}]
             /\ q' = IF q = <<>> THEN q ELSE Tail(q)  \* <<>> => FIFOClaimOrder fails
             /\ pc' = [pc EXCEPT ![c] = "Read"]
             /\ UNCHANGED <<seq, w, p_pos, c_pos, data, writers>>
          \/ \* diff > 0 (pos taken), or failed CAS: reload pos
             /\ \/ diff > 0
                \/ diff = 0 /\ r /= pos
             /\ pc' = [pc EXCEPT ![c] = "Loop"]
             /\ UNCHANGED <<seq, w, r, p_pos, c_pos, data, writers, readers, q>>
          \* diff < 0 (queue empty): retry with the same pos (stuttering).

\* The payload is read while in "Read" (DataIntegrity is checked in every
\* state of that interval); then the release CAS
\* CompareAndSwapUint64(&slot._seq, pos+1, pos+mask+1)  (ConsumerSlot.Release)
\* which always succeeds for the owner (see CompletionCAS).
C_Release(c) ==
    /\ pc[c] = "Read"
    /\ LET pos == c_pos[c]
           idx == pos % BufferSize
       IN /\ seq[idx] = Inc(pos)                        \* CAS expected value
          /\ seq' = [seq EXCEPT ![idx] = IncBy(pos, BufferSize)]
          /\ readers' = [readers EXCEPT ![idx] = @ \ {c}]
    /\ pc' = [pc EXCEPT ![c] = "Loop"]
    /\ UNCHANGED <<w, r, p_pos, c_pos, data, writers, q>>

Consumer(c) == C_Load(c) \/ C_Check(c) \/ C_Release(c)

-----------------------------------------------------------------------------

Next ==
    \/ \E p \in Producers : Producer(p)
    \/ \E c \in Consumers : Consumer(c)

\* Weak fairness per process: a thread that can take a non-stuttering step
\* is eventually scheduled. In particular every thread that owns a slot
\* eventually commits/releases it (in Go this is an obligation on the caller
\* of ReserveProducer/ReserveConsumer; a slot that is never released blocks
\* the ring, which no fairness assumption can fix).
Fairness ==
    /\ \A p \in Producers : WF_vars(Producer(p))
    /\ \A c \in Consumers : WF_vars(Consumer(c))

Spec == Init /\ [][Next]_vars /\ Fairness

-----------------------------------------------------------------------------
(* Safety *)

\* The cursors never drift more than BufferSize apart: producers never run
\* ahead of consumers by more than the capacity, and r never passes w.
QueueSafety ==
    UnsignedSub(w, r) <= BufferSize

\* Exclusive slot ownership: at most one producer and at most one consumer
\* own a slot, and never a producer and a consumer at the same time.
NoRace ==
    \A idx \in Slots :
        /\ Cardinality(writers[idx]) <= 1
        /\ Cardinality(readers[idx]) <= 1
        /\ writers[idx] /= {} => readers[idx] = {}
        /\ readers[idx] /= {} => writers[idx] = {}

\* Payload integrity: while a consumer owns position pos, its slot contains
\* exactly the payload written by the producer that claimed pos: no stale
\* read (producer has not written yet / data of an earlier lap) and no
\* overwrite of unread data by a producer of a later lap. The same holds for
\* a producer between writing and publishing (no write-write clobbering).
DataIntegrity ==
    /\ \A c \in Consumers :
           pc[c] = "Read" => data[c_pos[c] % BufferSize] = c_pos[c]
    /\ \A p \in Producers :
           pc[p] = "Publish" => data[p_pos[p] % BufferSize] = p_pos[p]

\* The abstract queue never holds more than BufferSize items and agrees with
\* the cursors.
FIFOBound ==
    /\ Len(q) <= BufferSize
    /\ Len(q) = UnsignedSub(w, r)

\* The completing CAS of every slot owner finds its expected value: a producer
\* between claim and publish sees seq = pos, a consumer between claim and
\* release sees seq = pos+1. Stale copies of a reservation are not modelled:
\* their CAS fails and changes nothing.
CompletionCAS ==
    /\ \A p \in Producers :
           pc[p] \in {"Write", "Publish"} => seq[p_pos[p] % BufferSize] = p_pos[p]
    /\ \A c \in Consumers :
           pc[c] = "Read" => seq[c_pos[c] % BufferSize] = Inc(c_pos[c])

\* Linearizability w.r.t. a FIFO queue (history-based check): enqueues
\* linearize at the successful w CAS (appending pos to q), dequeues at the
\* successful r CAS. Every dequeue must take the oldest enqueued, not yet
\* dequeued position. Together with DataIntegrity (the consumer of pos reads
\* the payload written for pos) this means items are delivered exactly once
\* and in enqueue order. Note that with several consumers the *completion*
\* (Release) order may differ from the claim order; the claim is the
\* linearization point.
ConsumerClaims(c) == pc[c] = "Check" /\ pc'[c] = "Read"

FIFOClaimOrder ==
    [][\A c \in Consumers :
          ConsumerClaims(c) => (q /= <<>> /\ c_pos[c] = Head(q))]_vars

-----------------------------------------------------------------------------
(* Liveness (checked under Fairness) *)

\* Lock-freedom, producer side: whenever some producer is trying to claim a
\* slot, some producer (not necessarily the same one) eventually owns one.
\* Holds because consumers keep draining the ring.
ProducerProgress ==
    (\E p \in Producers : pc[p] = "Check") ~> (\E p \in Producers : pc[p] = "Write")

\* Lock-freedom, consumer side: whenever some consumer is trying to claim a
\* slot, some consumer eventually owns one. Holds because producers keep
\* filling the ring (an empty ring is eventually refilled).
ConsumerProgress ==
    (\E c \in Consumers : pc[c] = "Check") ~> (\E c \in Consumers : pc[c] = "Read")

\* Both cursors advance infinitely often.
SystemProgress ==
    /\ []<><<w' /= w>>_w
    /\ []<><<r' /= r>>_r

\* NOT a theorem -- do NOT add to MPMC.cfg. Per-thread progress (starvation
\* freedom) does not hold: the algorithm is lock-free, not wait-free. Under
\* weak fairness, TLC finds a lasso in which c1 repeatedly loads r and is
\* then beaten to the CAS by c2 (and symmetrically for producers), so c1
\* never claims a slot although the ring is non-empty infinitely often.
\* Strong fairness would not be a faithful assumption about CAS contention.
ConsumerStarvationFreedom ==
    \A c \in Consumers :
        (pc[c] = "Check" /\ UnsignedSub(w, r) > 0) ~> (pc[c] = "Read")

ProducerStarvationFreedom ==
    \A p \in Producers :
        (pc[p] = "Check" /\ UnsignedSub(w, r) < BufferSize) ~> (pc[p] = "Write")

=============================================================================

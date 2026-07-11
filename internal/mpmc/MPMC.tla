-------------------------------- MODULE MPMC --------------------------------
EXTENDS Naturals, Sequences, TLC, FiniteSets

CONSTANTS BufferSize,  \* Size of the ring buffer (must be power of 2, e.g., 2)
          Producers,   \* Set of Producer processes (e.g., {p1, p2})
          Consumers    \* Set of Consumer processes (e.g., {c1, c2})

\* MaxVal defines the finite modular bit-width size (e.g. 8 for BufferSize=2)
MaxVal == BufferSize * 4

\* Simulates Go's unsigned modular subtraction (a - b) within MaxVal bits
UnsignedSub(a, b) ==
    IF a >= b THEN a - b ELSE (a + MaxVal) - b

\* Simulates Go's casting of unsigned sub to signed: int64(seq - pos)
SignedSub(a, b) ==
    LET diff == UnsignedSub(a, b)
    IN IF diff < (MaxVal \div 2)
       THEN diff
       ELSE diff - MaxVal

VARIABLES seq,        \* Sequence numbers for each slot (modular 0..MaxVal-1)
          w,          \* Write position index (modular 0..MaxVal-1)
          r,          \* Read position index (modular 0..MaxVal-1)
          pc,         \* Program counter for model checking
          p_pos,      \* Local copy of write position for Producers
          c_pos,      \* Local copy of read position for Consumers
          writers,    \* Track active writers for each slot (for race detection)
          readers     \* Track active readers for each slot (for race detection)

vars == <<seq, w, r, pc, p_pos, c_pos, writers, readers>>

Init == 
    /\ seq = [i \in 0..(BufferSize-1) |-> i]
    /\ w = 0
    /\ r = 0
    /\ pc = [p \in Producers \cup Consumers |-> "Loop"]
    /\ p_pos = [p \in Producers |-> 0]
    /\ c_pos = [c \in Consumers |-> 0]
    /\ writers = [i \in 0..(BufferSize-1) |-> {}]
    /\ readers = [i \in 0..(BufferSize-1) |-> {}]

\* Helper functions to increment values modulo MaxVal
Inc(x) == (x + 1) % MaxVal
IncBy(x, val) == (x + val) % MaxVal

\* Producer Actions
P_Load(p) ==
    /\ pc[p] = "Loop"
    /\ p_pos' = [p_pos EXCEPT ![p] = w]
    /\ pc' = [pc EXCEPT ![p] = "Check"]
    /\ UNCHANGED <<seq, w, r, c_pos, writers, readers>>

P_Check(p) ==
    /\ pc[p] = "Check"
    /\ LET pos == p_pos[p]
           idx == pos % BufferSize
           s == seq[idx]
           \* Simulate: diff := int64(seq - p)
           diff == SignedSub(s, pos)
       IN
         IF diff = 0
         THEN 
           \* CAS simulation
           IF w = pos
           THEN
             /\ w' = Inc(pos)
             /\ writers' = [writers EXCEPT ![idx] = writers[idx] \cup {p}]
             /\ pc' = [pc EXCEPT ![p] = "Write"]
             /\ UNCHANGED <<seq, r, p_pos, c_pos, readers>>
           ELSE
             /\ pc' = [pc EXCEPT ![p] = "Loop"]
             /\ UNCHANGED <<seq, w, r, p_pos, c_pos, writers, readers>>
         ELSE IF diff < 0
         THEN
           \* Queue Full: Spin-wait with same pos
           /\ pc' = [pc EXCEPT ![p] = "Loop"]
           /\ UNCHANGED <<seq, w, r, p_pos, c_pos, writers, readers>>
         ELSE
           \* diff > 0: Another producer claimed this slot, reload pos
           /\ pc' = [pc EXCEPT ![p] = "Loop"]
           /\ UNCHANGED <<seq, w, r, p_pos, c_pos, writers, readers>>

P_Write(p) ==
    /\ pc[p] = "Write"
    /\ LET pos == p_pos[p]
           idx == pos % BufferSize
       IN
         /\ seq' = [seq EXCEPT ![idx] = Inc(pos)]
         /\ writers' = [writers EXCEPT ![idx] = writers[idx] \ {p}]
         /\ pc' = [pc EXCEPT ![p] = "Loop"]
         /\ UNCHANGED <<w, r, p_pos, c_pos, readers>>

\* Consumer Actions
C_Load(c) ==
    /\ pc[c] = "Loop"
    /\ c_pos' = [c_pos EXCEPT ![c] = r]
    /\ pc' = [pc EXCEPT ![c] = "Check"]
    /\ UNCHANGED <<seq, w, r, p_pos, writers, readers>>

C_Check(c) ==
    /\ pc[c] = "Check"
    /\ LET pos == c_pos[c]
           idx == pos % BufferSize
           s == seq[idx]
           \* Simulate: diff := int64(seq - (p + 1))
           diff == SignedSub(s, Inc(pos))
       IN
         IF diff = 0
         THEN
           \* CAS simulation
           IF r = pos
           THEN
             /\ r' = Inc(pos)
             /\ readers' = [readers EXCEPT ![idx] = readers[idx] \cup {c}]
             /\ pc' = [pc EXCEPT ![c] = "Read"]
             /\ UNCHANGED <<seq, w, p_pos, c_pos, writers>>
           ELSE
             /\ pc' = [pc EXCEPT ![c] = "Loop"]
             /\ UNCHANGED <<seq, w, r, p_pos, c_pos, writers, readers>>
         ELSE IF diff < 0
         THEN
           \* Queue Empty: Spin-wait with same pos
           /\ pc' = [pc EXCEPT ![c] = "Loop"]
           /\ UNCHANGED <<seq, w, r, p_pos, c_pos, writers, readers>>
         ELSE
           \* diff > 0: Another consumer claimed this slot, reload pos
           /\ pc' = [pc EXCEPT ![c] = "Loop"]
           /\ UNCHANGED <<seq, w, r, p_pos, c_pos, writers, readers>>

C_Read(c) ==
    /\ pc[c] = "Read"
    /\ LET pos == c_pos[c]
           idx == pos % BufferSize
       IN
         /\ seq' = [seq EXCEPT ![idx] = IncBy(pos, BufferSize)]
         /\ readers' = [readers EXCEPT ![idx] = readers[idx] \ {c}]
         /\ pc' = [pc EXCEPT ![c] = "Loop"]
         /\ UNCHANGED <<w, r, p_pos, c_pos, writers>>

Next ==
    \/ \E p \in Producers : P_Load(p) \/ P_Check(p) \/ P_Write(p)
    \/ \E c \in Consumers : C_Load(c) \/ C_Check(c) \/ C_Read(c)

Spec == 
    /\ Init 
    /\ [][Next]_vars 
    /\ \A p \in Producers : WF_vars(P_Load(p) \/ P_Check(p) \/ P_Write(p))
    /\ \A c \in Consumers : WF_vars(C_Load(c) \/ C_Check(c) \/ C_Read(c))

\* Safety Invariant 1: w and r stay within BufferSize distance in modular arithmetic
QueueSafety == 
    UnsignedSub(w, r) <= BufferSize

\* Safety Invariant 2: No Read-Write or Write-Write Data Race
NoRace ==
    \forall idx \in 0..(BufferSize-1) :
        /\ Cardinality(writers[idx]) <= 1
        /\ (writers[idx] /= {} => readers[idx] = {})

\* Liveness Property: Lock-free progress (system-wide progress)
Liveness ==
    (\E p \in Producers : pc[p] = "Check") ~> (\E p2 \in Producers : pc[p2] = "Write")

=============================================================================
\* Formal Proof System (TLAPS) proof structure for Safety Invariance
THEOREM Spec => []QueueSafety
<1>1. Init => QueueSafety
  BY DEF Init, QueueSafety, UnsignedSub
<1>2. QueueSafety /\ [Next]_vars => QueueSafety'
  <2>1. SUFFICIENT ASSUME QueueSafety, [Next]_vars PROVE QueueSafety'
    BY DEF QueueSafety, Next, vars, P_Load, P_Check, P_Write, C_Load, C_Check, C_Read, UnsignedSub, Inc, IncBy
  <2>2. QED
<1>3. QED
  BY <1>1, <1>2, PTL
=============================================================================

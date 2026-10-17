---
name: deslop
description: Detects and eliminates AI-generated slop across tests, production code, comments, and technical documentation. Enforces behavior-driven testing, architectural integrity, and controlled technical writing (ASD-STE100, S1000D, ATA iSpec 2200, and zero-slop typography without em-dashes or LLM clichés).
license: MIT
---

# Deslop: Behavioral Verification, Architectural Integrity, and Controlled Technical Writing

Eliminate AI-generated slop across test suites, production implementations, code comments, and technical documentation. Every test must prove an observable contract rather than freeze internal implementation details. Every production change must integrate at the owning architectural boundary without test-specific shortcuts. Every technical document must obey strict controlled-language mechanics, predictable modular structure, and zero-slop typography.

---

## 1. Scope and Routing

Activate this skill whenever:

- Auditing, writing, or refactoring tests that inspect private state, mock internal helpers, regex-match source files, or pass without falsifiable assertions.
- Reviewing or cleaning up AI-generated production code containing test-fixture branches, silent no-op stubs, string-splicing facades, copy-pasted private helpers, or syntax-echoing comments.
- Creating, rewriting, or reviewing `README.md`, architecture guides (`docs/architecture.md`, `AGENTS.md`), wire and protocol specifications (`PROTOCOL.md`, `spec/*.md`), API references (`doc.go`, public headers), runbooks, or `CHANGELOG.md`.
- Enforcing strict typographic and lexical discipline: zero em-dashes, zero AI-slop buzzwords, controlled sentence length (**ASD-STE100**), modular data separation (**S1000D**), standardized section taxonomy (**ATA iSpec 2200**), and pre-procedure safety alerts (**MIL-STD-38784**).

**Skill Boundaries**:
- Route general test layer selection and hermetic fixture design to `testing-practices`.
- Route local control-flow flattening and naming mechanics to `code-health`.
- Route public signature design and resource ownership contracts to `api-design`.
- Route diff sizing and review etiquette to `code-review`.
- Use `deslop` as the authoritative standard for identifying and eradicating AI-generated test, code, comment, and documentation slop.

---

## 2. Test Deslopping: Verify Behavior, Never Implementation

The hallmark of AI-generated test slop is **verifying how code is written rather than what the system does**. Implementation-coupled tests break during harmless refactoring while passing silently when real user-facing or protocol invariants fail.

Apply the **Four-Question Test Filter** before keeping or writing any test:
1. What user-visible behavior, protocol invariant, or security boundary fails if this contract breaks?
2. What is the lowest coherent owner of that contract?
3. Can a deterministic unit test at that owner prove the contract without fake lifecycle wiring or private state inspection?
4. If proving the contract requires full transport, listener, or multi-component lifecycle wiring, does the test belong in a black-box end-to-end (`e2e/`) or component suite instead?

### 2.1 Source-Text and Regex Pinning ("Source-Presence Pins")

- **The Slop Pattern**: Reading `.go`, `.rs`, `.js`, `.ts`, or `.py` source files as raw strings (`os.ReadFile`, `fs.readFileSync`) and asserting `.includes(...)`, regular expression matches, function declaration order by character offset, or slicing source strings by indentation into `new Function(...)` / `eval`.
- **Why It Fails**: Treats source text as a runtime contract. Deleting a dead helper, renaming a local variable, reformatting lines, or adding a trailing `//` comment breaks tests while proving zero runtime behavior.
- **How to Fix**: Delete all source-string inclusion and regex tests. Load and execute the compiled artifact or script bundle inside its real runtime boundary (e.g., booting the bundle in an isolated VM context, invoking the compiled WASM module, or calling the exported package API) and assert actual execution outputs and side effects.

### 2.2 Middle-Layer Harnesses and Half-Initialized Structs

- **The Slop Pattern**: Allocating a complex struct literal directly (`&Manager{items: map..., ch: make(...)}`), bypassing the public constructor (`NewManager()` / `Start()`), manually locking private mutexes (`m.mu.Lock()`), injecting unexported fields (`m.conn = fakeConn`, `m.snapshot = snap`), and invoking unexported lifecycle methods (`m.reconcile()`, `m.setInternalState()`).
- **Why It Fails**: Exercises a synthetic object graph that never exists in production. It freezes private field names and internal method decomposition while missing initialization bugs, deadline ownership, and concurrency hazards at the real entrypoints.
- **How to Fix (Enforce the Barbell Testing Model)**:
  - **Bottom of the Barbell (Owner Unit Tests)**: Extract pure decision rules, parsers, and state calculations into deterministic functions or small owner types. Test them directly through their exported API with zero fake lifecycle wiring.
  - **Top of the Barbell (Black-Box E2E / Component Tests)**: Test multi-step lifecycles, connection deadlines, TLS handshakes, and retries through public constructors over real loopback sockets, `net.Pipe()`, or shared memory. Delete the brittle middle-layer harnesses entirely.

### 2.3 Private State Inspection and Backing-Array Assertions

- **The Slop Pattern**: Calling a public operation and asserting success by reading unexported struct fields (`assert(syncer.snapshot == nil)`, `assert(server.routingTable != nil)`), checking internal mutex state, or asserting slice backing-array pointer identity (`&out[0] != &in[0]`) on internal helpers.
- **Why It Fails**: Couples tests to internal caching or memory representation rather than the contract observed by callers or network peers.
- **How to Fix**: Assert externally observable outcomes at the boundary:
  - Instead of checking `server.routingTable != nil` after construction, send an HTTP `GET /discovery` request and assert a `200 OK` response with a valid payload.
  - Instead of checking `syncer.snapshot == nil` after a transient `503` retry, inspect atomic request counters and received payloads on the test server to verify that failed status checks do not trigger uploads and subsequent retries deliver the intact body.
  - To prove defensive copying, mutate the returned slice or map in the test and assert that a subsequent public read from the owner remains unchanged.

### 2.4 Assertion-Free "Log-and-Pass" and Rationalized Outcome Tests

- **The Slop Pattern**:
  - Executing an operation, catching an error or unexpected result, and logging a rationalization instead of failing (`if err != nil { t.Logf("operation returned error (acceptable in this mode): %v", err) }`).
  - Calling `rows, err := db.Query(...)` and immediately closing `rows` without asserting `err` or inspecting the result set.
  - Writing conditional assertions that only execute when two computed scores happen to differ, logging `t.Logf("scores tied; skipping order check")` otherwise.
  - Leaving `fmt.Printf(".")` progress dots or unused destructured variables silenced with `_ = val` inside tests.
- **Why It Fails**: Manufactures line coverage while making zero falsifiable claims. Any regression passes silently.
- **How to Fix**: Every test must make an unconditional, falsifiable assertion on the exact return value, state transition, or error sentinel/code (`errors.Is`, exact `SQLSTATE`, HTTP status). Use deterministic inputs and seeds that guarantee distinct outcomes. Strip all `fmt.Print*` noise from unit tests.

### 2.5 Tautological Constant, Setter, and Mock-Echo Tests

- **The Slop Pattern**:
  - Copying literal constant values from `foo.go` into `foo_test.go` within the same package (`assert(MagicHeader == 0x53544C33)`).
  - Calling a trivial setter or option builder and asserting only `err == nil` or that a getter returns the literal value just set.
  - Configuring a mock to return `X`, invoking a thin wrapper, asserting it returned `X`, and verifying exact call counts on read-only queries.
- **Why It Fails**: Tests compiler constant propagation, memory assignment, or mock framework syntax rather than domain invariants.
- **How to Fix**:
  - **Cross-Artifact Conformance for Constants and Layouts**: Only test constants and struct layouts against an independent, language-neutral source of truth (such as `spec/constants.json`). Verify ABI size (`sizeof`), field offsets (`offsetof`), zero unrecorded struct padding (`prevEnd == field.Offset`), cache-line isolation between producer and consumer atomics (`offset / cacheLine`), and mathematical alignment formulas.
  - **Behavioral Option Verification**: Prove a configuration setter works by observing the behavioral boundary it controls (e.g., setting `MaxActiveStreams(2)` and verifying the third concurrent stream is rejected with `ErrStreamLimitExceeded`).
  - **Real Fakes and Subprocess Re-Execution**: Replace mocks with stateful in-memory fakes or self-re-executing test binaries (`TestMain` dispatching on an environment variable like `FAKE_WORKER_MODE=crash|hang|serve`) to test real OS process spawning, file locks, and signal escalation (`SIGTERM` to `SIGKILL`).

### 2.6 Log-Output Pinning and Ephemeral Task-ID Naming

- **The Slop Pattern**:
  - Swapping global loggers to capture stdout/stderr and asserting exact JSON log keys or human-readable error message substrings.
  - Naming test functions or comments after temporary planning phases, ticket numbers, or errata codes (`TestPhase3_Step2_B7_Fix`, `// C1: handle edge case`).
- **Why It Fails**: Turns diagnostic wording tweaks into test failures. Once planning scratchpads (`PHASE2_PLAN.md`) are deleted, ticket-coded test names become unreadable archaeological noise.
- **How to Fix**: Assert structured error types, error sentinels (`errors.Is`, `errors.As`), or wire error codes. Name every test function strictly after the timeless behavioral contract it protects (`TestStaticCacheDoesNotFollowRedirects`, `TestLeaseRequestRejectsTTLAboveMaximum`).

### 2.7 Wall-Clock Sleeps, Production Timeouts, and Runtime Heuristics

- **The Slop Pattern**:
  - Calling `time.Sleep(200 * time.Millisecond)` after `go server.Serve()` hoping the listener has bound before dialing.
  - Waiting out a hardcoded 20-second production timeout in a unit test.
  - Calling `runtime.GC()`, `time.Sleep(20ms)`, and comparing `runtime.NumGoroutine()` before and after with a `+1` slop tolerance to test for goroutine leaks.
- **Why It Fails**: Causes multi-second test suites and nondeterministic failures under CI scheduler contention.
- **How to Fix**:
  - Bind the `net.Listener` or `net.PacketConn` synchronously before starting the serve goroutine, or expose an explicit readiness channel.
  - Inject a virtual clock/ticker (`withClock`, `withTicker`) or pass a short configurable deadline (`5ms`) into the constructor.
  - Prove resource shutdown deterministically by canceling the context on a `net.Pipe()` pair and asserting that peer `Read()` returns `io.EOF` and the worker waitgroup completes within a diagnostic timeout bound.

### 2.8 Orphaned Tests for Dead Production Code ("Defense-in-Depth Illusion")

- **The Slop Pattern**: Keeping legacy packages and their passing unit test suites after production traffic has migrated to a new subsystem, or running cross-language parity tests between live code and dead code.
- **Why It Fails**: Uncalled production code provides zero defense; its passing tests create false confidence while masking missing coverage on the live execution path.
- **How to Fix**: Trace the live call graph from binary entrypoints (`cmd/`) and exported library roots. Delete unreachable production packages and their test files in the same commit.

---

## 3. Production Code and Comment Deslopping

### 3.1 Test-Specific Production Branches and Silent No-Op Stubs

- **The Slop Pattern**:
  - Adding conditional branches in production code that check for literal test fixture names (e.g., `if roleName == "r" && !store.Active() { return ErrUnsupported }`) so legacy unsupported-feature gate tests and new feature tests pass simultaneously.
  - Parsing security-critical or transactional commands (`GRANT`, `REVOKE`, `VACUUM`, ` VerifySignature`) and returning a silent `nil` error (no-op) so end-to-end test scripts finish without errors.
- **Remediation**: Never branch on test inputs or environment flags inside domain logic. Either implement the real catalog/state enforcement at the owning layer and update the gate tests, or return an explicit, honest `Unsupported` error (`SQLSTATE 0A000`, `ErrUnsupported`).

### 3.2 String-Splicing and Non-Transactional Sidecar Facades

- **The Slop Pattern**: Implementing a deep architectural feature (such as access control, query rewriting, or protocol framing) by regex-splicing input strings or storing metadata in non-transactional sidecar JSON files (`/tmp/meta.json`) outside the core parser, binder, optimizer, and transactional storage engine.
- **Remediation**: Enforce a strict **No Hotfixes to Rewrite Facades** rule. Delete string-splicing interceptors and integrate the feature natively into the domain model, binder, plan nodes (such as explicit security-barrier nodes), and transactional storage.

### 3.3 Copy-Pasted Private Helpers vs. Boundary-Conflating DRY

- **The Slop Pattern**:
  - **Under-Consolidation**: Copy-pasting identical recursive tree comparisons, error-unwrapping loops, or B-tree scan helpers across five internal packages; reinventing standard library functions (`cmp.Compare`, `slices.Contains`, `maps.Clone`); or adding single-use pass-through getter methods.
  - **Over-Consolidation (Boundary-Conflating DRY)**: Merging two security predicates that look syntactically similar (e.g., folding `.local` mDNS names and IPv6 zone IDs into `IsLocalhost`, which is used both for SSRF rejection and for local TLS trust exceptions), inadvertently widening a security trust boundary.
- **Remediation**:
  - Place canonical comparison and traversal methods on the owning domain type and use standard library primitives.
  - Split predicates strictly by the **security or domain invariant they protect** (e.g., `IsLoopbackHost` for local TLS trust exceptions vs. `IsLocalNetworkHost` for SSRF/self-registration rejection), even if parts of their IP-parsing logic overlap.

### 3.4 Code Comment Slop: Syntax Echoing, Decorative Banners, and Debugging Diaries

- **The Slop Pattern**:
  - Narrating what the next line of syntax does (`// Store the size of the ring`, `// Check for timeout`, `// Yield to other goroutines`, `// 1. Create server`).
  - Decorative ASCII section banners (`// ── Section ──`, `// ========`), star symbols (`★`), and em-dash chains.
  - Multi-paragraph historical war stories explaining what broken code used to do before a bug fix (`// Initially we used a regex stub, which failed on 2026-04-06...`).
- **Remediation**:
  - Delete all syntax-echoing comments, decorative banners, and historical diaries.
  - Reserve comments strictly for timeless, non-obvious **"Why"** constraints:
    - Hardware and architecture realities (e.g., why `cacheLine = 128` is required for Apple Silicon / arm64 coherence granules).
    - Concurrency and lock-free state transitions (e.g., exact Vyukov sequence states `seq == pos + 1` or why a `SIGKILL` waiter leak costs one extra `futex(WAKE)` syscall without risking memory safety or deadlock).
    - Toolchain and memory-model constraints (e.g., why an `mmap` base address is held as `uintptr` to satisfy `go vet` `unsafeptr` checks).

---

## 4. Strict Punctuation and Anti-Slop Lexicon

### 4.1 Absolute Ban on Em-Dashes and Prose Arrows

Never use the em-dash (U+2014, HTML `&mdash;`), spaced en-dash (U+2013 surrounded by spaces), double-hyphen (`--`), or arrows (`->`, U+2192) as prose punctuation in any documentation file, docstring, comment, list item, or table cell. Overuse of em-dashes is the primary syntactic signature of unedited LLM text and creates loose, imprecise clause boundaries.

Select the exact punctuation mark that defines the logical relationship between clauses:

| Logical Relationship | Required Punctuation | Example |
| :--- | :--- | :--- |
| **Elaboration, definition, or cause** | Colon (`:`) | `Generation counters identify a specific slot incarnation: numeric indices alone cannot distinguish recycled buffers.` |
| **Two closely linked independent clauses** | Semicolon (`;`) | `Listen initializes local shared-memory regions immediately; it does not block on peer attachment.` |
| **Subordinate metadata (sizes, codes, defaults)** | Parentheses (`()`) | `Frame headers encode four transport modes (Unreliable, Reliable, Ordered, and Stream).` |
| **Separate, standalone technical claim** | Period (`.`) | `Session keys live exclusively in pinned memory. The runtime wipes all key buffers on shutdown.` |
| **Continuous action or participial modifier** | Comma (`,`) | `The writer splits oversized payloads into 4096-byte segments, zero-padding the trailing cache line.` |

*Note*: Standard ASCII hyphens (`-`) remain mandatory in compound modifiers (`zero-allocation parser`, `shared-memory ring`, `64-byte header`), CLI flags (`-listen-addr`), and negative numbers.

### 4.2 Banned AI-Slop Vocabulary

Never use any of the following words or phrases in technical documentation, comments, or commit messages:

- **AI Hyperbole and Marketing Buzzwords**: `seamless`, `seamlessly`, `robust`, `battle-tested`, `cutting-edge`, `next-generation`, `state-of-the-art`, `powerhouse`, `unleash`, `empower`, `supercharge`, `elevate`, `delve`, `tapestry`, `testament`, `landscape`, `holistic`, `synergy`, `paradigm`, `first-class citizen`, `out of the box`, `under the hood`, `heavy lifting`, `breeze`, `effortlessly`, `meticulously`, `vital`, `crucial`, `pivotal`, `paramount`, `game-changer`, `harness`.
- **Throat-Clearing Prefaces**: `It is important to note that`, `It is worth mentioning that`, `In order to` (use `To`), `At its core`, `Fundamentally`, `Essentially`, `Basically`, `When it comes to`, `In the realm of`, `Let's explore`, `Dive into`, `As mentioned earlier`, `In today's fast-paced`.
- **Hollow Verb Phrases**:
  - Replace `is responsible for handling` with `handles`, `dispatches`, `parses`, or `validates`.
  - Replace `serves as a` or `acts as a` with `is`, `terminates`, or `exposes`.
  - Replace `facilitates the process of` with the exact operation (`schedules`, `serializes`, `retransmits`).
  - Replace `provides the ability to` with `lets callers`, `exposes`, or `supports`.
  - Replace `utilizes` or `leverages` with `uses`, `calls`, or `reads`.
  - Replace vague `ensures that` with the concrete mechanism (`rejects`, `bounds`, `wipes`, `verifies`).

### 4.3 Banned Rhetorical Structures

1. **"Not only X, but also Y"**: State both technical facts directly (`The handshake validates the protocol version and negotiates ring capacity.`).
2. **"Whether you are X or Y"**: State supported modes plainly (`The library exposes both a zero-copy reservation API and standard net.Conn wrappers.`).
3. **Negative-Positive Tautologies ("It is not just X; it is Y")**: Delete the theatrical contrast and state the engineering mechanism.
4. **Repetitive Participial Bullet Openings**: Never start consecutive bullet items with `-ing` participles (`Ensuring...`, `Providing...`, `Supporting...`). Start bullets with imperative verbs, concrete nouns, or bolded component identifiers.
5. **Unbounded Quantifiers**: Replace `fast`, `low-latency`, `small`, `large`, or `many` with exact metrics, byte sizes, timeouts, or asymptotic bounds (`O(1)` lookup, `64-byte` header, `256 KiB` ring, `10-minute` maximum TTL).

### 4.4 Korean Technical Prose Rules

When writing or editing Korean technical documentation, enforce these additional constraints:

- **Strict Clause Punctuation**: Never connect clauses loosely with dashes or spaced hyphens (` - `). Separate clauses explicitly using a colon (`:`), semicolon (`;`), parentheses (`()`), comma (`,`), or period (`.`).
- **No Translationese or Double Passives**: Eliminate double-passive and translated passive constructions such as `~되어집니다`, `~해집니다`, and `~에 의해 처리됩니다`. Use direct active verbs (`처리한다`, `검증한다`, `거부한다`).
- **No Low-Density Abstract Modifiers**: Remove vague modifiers such as `다양한`, `효과적으로`, `원활하게`, `강력한`, `체계적인`, `손쉽게`, and `완벽하게`. State exact numeric bounds or concrete operational conditions instead.
- **No Verbose Connector Padding**: Replace padded phrases such as `~를 통해`, `~할 수 있도록 지원합니다`, and `~하는 역할을 수행합니다` with direct predicates (`~로 ~한다`, `~를 제공한다`, `~한다`).

---

## 5. Controlled Technical English (ASD-STE100 Adaptation)

Aerospace maintenance and flight manuals enforce **ASD-STE100 (Simplified Technical English)** so engineers worldwide interpret every sentence in exactly one way without fatal ambiguity. Apply these five ASD-STE100 rules to all software specifications, runbooks, architecture guides, and API manuals:

### 5.1 One Word, One Meaning (Eliminate Polysemy)

Assign each domain concept and action verb a single, invariant meaning across the repository. Never rotate synonyms for stylistic variety:

| Ambiguous / Overloaded Term | Approved Controlled Replacement | Rule |
| :--- | :--- | :--- |
| `follow` (meaning obey a rule) | `obey`, `conforms to`, `satisfies` | Reserve `follow` strictly for spatial/pointer traversal (`follow the next-frame offset`). |
| `handle` / `process` / `deal with` | `parse`, `validate`, `dispatch`, `route`, `reject` | Name the exact state transition or check performed. |
| `manage` | `allocate`, `supervise`, `evict`, `schedule` | State the concrete lifecycle operation owned by the component. |
| `check` (as a vague verb) | `verify`, `assert`, `compare`, `inspect` | State what invariant is tested and what happens on mismatch. |
| `may` (meaning might fail at runtime) | `can` (capability/possibility) vs. `MAY` (RFC 2119 optional permission) | Never confuse runtime failure possibility (`can return ErrClosed`) with normative protocol permission (`MAY omit padding`). |

### 5.2 Part-of-Speech Locking

Use technical words in a single grammatical role. Do not convert struct names, noun concepts, or flags into improvised verbs:
- **Bad**: `The client leases the relay and TLSes the upstream socket before syncing.`
- **Good**: `The client acquires a lease from the relay, completes a TLS handshake on the upstream socket, and runs a cache synchronization pass.`

### 5.3 Strict Sentence and Paragraph Length Bounds

Long, multi-clause sentences hide edge cases and failure conditions. Obey these quantitative ceilings:

1. **Procedural Sentences (Setup, CLI Runbooks, Migration Steps, Build Instructions)**:
   - Maximum **20 words per sentence**.
   - Maximum **1 instruction per sentence** (unless two actions must occur simultaneously, such as `Hold the lock while updating the index.`).
2. **Descriptive Sentences (Architecture, Protocol Specs, API Contracts)**:
   - Maximum **25 words per sentence**.
   - Maximum **6 sentences per paragraph**.
   - If a paragraph exceeds 6 sentences, split it by sub-topic or convert repeated structure into a Markdown table.

### 5.4 Active Voice and Imperative Mood

1. **Descriptive Prose (Active Voice Only)**:
   - Always identify the actor (component, caller, kernel, peer) performing the action.
   - **Bad (Passive)**: `Unauthenticated registration challenges are bounded by an eviction queue when memory limits are exceeded.` (20 words, hides owner).
   - **Good (Active)**: `The challenge store evicts the oldest unauthenticated entry when pending challenges exceed 4096 slots.` (14 words, explicit actor and limit).
2. **Procedural Prose (Imperative Verb First)**:
   - Start every step with a bare imperative verb (`Run`, `Set`, `Verify`, `Close`, `Delete`).
   - **Bad**: `Prior to commencing the schema migration, it is necessary to ensure that all read replicas have been drained.`
   - **Good**:
     ```markdown
     1. Drain all read replicas.
     2. Run the schema migration command.
     ```

### 5.5 Compound Noun Cluster Limit (Maximum 3 Consecutive Nouns)

Never stack more than **3 consecutive nouns** in a single phrase. Unpack 4+ noun chains using prepositions (`for`, `in`, `of`, `on`) or hyphenate fixed two-word modifiers so ownership is unmistakable:

| Uncontrolled Noun Cluster (4+ Nouns) | Controlled ASD-STE100 Phrase (Max 3 Nouns) |
| :--- | :--- |
| `Relay discovery cache upload admission controller` (6 nouns) | `The admission controller for cache uploads in relay discovery` |
| `Shared memory ring buffer slot sequence counter` (7 nouns) | `The sequence counter for slots in the shared-memory ring` |
| `Client session token expiration validation policy` (6 nouns) | `The validation policy for expiration of client session tokens` |

### 5.6 Uncontrolled Prose vs. ASD-STE100 Controlled Prose

| Category | Uncontrolled Technical Prose (Slop) | ASD-STE100 Controlled Prose (Best Practice) |
| :--- | :--- | :--- |
| **Procedural Instruction** | `Ensure that the supervisor daemon is gracefully terminated prior to executing the shared-memory segment cleanup utility.` | `1. Stop the supervisor daemon.`<br>`2. Run the cleanup utility for shared-memory segments.` |
| **Hazard Warning** | `Concurrent modification of the payload buffer prior to commit completion may result in corrupted frame delivery.` | `Do not modify the payload buffer before CommitWrite returns. Early modification corrupts frames read by the peer.` |
| **State Transition** | `The backup relay path gets activated automatically whenever primary heartbeat packet loss exceeds the configured threshold.` | `The router switches to the backup relay when primary heartbeat loss exceeds 3 consecutive packets.` |

---

## 6. Modular Document Architecture (S1000D & ATA iSpec 2200)

### 6.1 S1000D Data Module Separation

Aerospace standard **S1000D** forbids mixing conceptual theory, operator steps, and fault tables in the same block. Structure every section as one distinct **Data Module** type:

1. **Descriptive Module**: Explains *what* a component is, its memory/wire layout, state machine, and invariants. Contains declarative active-voice statements, tables, and diagrams. Contains zero step-by-step operator instructions.
2. **Procedural Module**: Explains *how* an operator or caller performs an action. Contains pre-procedure safety alerts, prerequisites, numbered imperative steps (`<= 20` words each), copy-pasteable commands, and verification checks.
3. **Fault-Isolation / Reference Module**: Maps observable symptoms, error sentinels, or status codes directly to root causes and deterministic recovery actions in a tabular format.

### 6.2 Prose vs. Table vs. Diagram Selection Rules

Convert dense paragraphs into structured visual modules whenever these thresholds apply:

| Information Pattern | Required Format | Formatting Rules |
| :--- | :--- | :--- |
| **3+ items compared across 3+ shared attributes** (modes, CLI flags, config keys, error sentinels, API tiers) | **Markdown Table** | Left-align text columns (`:---`), right-align numeric columns (`---:`). Keep cells to concise fragments or single STE100 sentences. |
| **Byte, bit, or memory-mapped layouts** (frame headers, superblocks, cache-line structs) | **ASCII Byte/Bit Map + Field Table** | Show exact byte offsets (`0x00..0x07` or bit rulers) in a fenced `text` block, followed immediately by a table of offset, size, type, endianness, and invariant. |
| **Multi-step handshakes, RPC lifecycles, or actor flows** (3+ message hops) | **Mermaid `sequenceDiagram`** | Label arrows with exact frame or method names and key arguments. Include `alt` / `opt` blocks for timeout or rejection branches. |
| **Lifecycle transitions and protocol state machines** | **Mermaid `stateDiagram-v2` + Transition Table** | Pair the state graph with a table mapping `(Current State, Event/Guard)` to `(Next State, Side Effect)`. |
| **Subsystem dependencies, data pipelines, or routing trees** | **Mermaid `flowchart TD` / `LR`** | Quote all node labels containing parentheses, colons, or slashes (`id["Label (Detail)"]`). Keep edge labels to 1 to 3 words. |

### 6.3 ATA iSpec 2200 Standardized Document Blueprints

Just as **ATA iSpec 2200** guarantees a fixed chapter hierarchy across every aircraft manual, enforce a predictable section order across all repository documents.

**Universal Heading Rule**: Never place two headings back-to-back (`## Section` followed immediately by `### Subsection`). Every heading must open with a 2-to-4 sentence scope lead or summary table.

#### Blueprint A: `README.md` (Repository Entrypoint)
1. **Title and Identity Lead**: Define what the system is, what boundary it owns, and how callers interact with it in 2 to 4 sentences (`<= 25` words each).
2. **Architecture and Capability Overview**: A concise diagram or tier table showing how components fit together and which API tier fits each workload.
3. **Quickstart (Procedural Module)**: Minimal, compiled, copy-pasteable code or CLI example demonstrating initialization, error checking, and resource cleanup (`defer conn.Close()`).
4. **Core Mechanics and Invariants (Descriptive Module)**: Ownership rules, concurrency safety, delivery guarantees, and resource bounds.
5. **Documentation Index**: Table linking to deep-dive specs (`PROTOCOL.md`, `docs/*`) with one-line scope descriptions.
6. **Build, Verification, and Benchmarks (Procedural Module)**: Exact commands to build, run unit/race/fuzz suites, and reproduce benchmarks.

#### Blueprint B: `PROTOCOL.md` / `spec/*.md` (Wire and Memory Specifications)
Write specifications so an independent engineer can implement a conforming peer without reading your source code:
1. **Normative Conventions**: Define RFC 2119 keywords (`MUST`, `MUST NOT`, `REQUIRED`, `SHOULD`, `SHOULD NOT`, `MAY`).
2. **Constants and Magic Numbers**: Tabulate magic bytes, version numbers, size limits, alignment granules, and default timeouts in hex and decimal.
3. **Memory and Wire Layouts**: Specify byte offsets, endianness (`little-endian`, `network byte order`), alignment rules, and zero-padding invariants.
4. **Memory Ordering and Concurrency Semantics**: Specify atomic ordering (`Acquire`, `Release`, `SeqCst`) for every shared field read and write.
5. **State Machine and Fault Recovery**: Define legal transitions, crash detection (epoch/generation fencing), and exact error codes returned on malformed inputs.

#### Blueprint C: `docs/architecture.md` and `AGENTS.md` (Subsystem Guides)
1. **Layered Subsystem Map**: Define each package, its single responsibility, and strict import directionality (what it may import and what it must never import).
2. **Invariants and Resource Ownership**: Document who allocates, mutates, and releases buffers, descriptors, keys, or connections across boundaries.
3. **Execution and Concurrency Model**: Specify thread/goroutine lifecycles, queue capacities, backpressure behavior, and shutdown drain order.

#### Blueprint D: Package Docstrings and Public Headers (`doc.go`, `.h`)
1. **Ownership and Lifetime Contract**: State whether passed slices/buffers are borrowed for the call duration or retained asynchronously, and when returned views expire.
2. **Concurrency Contract**: State whether a type is safe for concurrent callers or requires external synchronization.
3. **Error Semantics**: Tabulate or list exact error sentinels returned under boundary conditions.

#### Blueprint E: `CHANGELOG.md` and Verification Reports
1. **Categorized Impact**: Group entries under `Breaking Changes`, `Features`, `Bug Fixes`, and `Performance`. Describe observable contract changes rather than commit subjects.
2. **Migration Snippets**: Provide `Before` and `After` code blocks for every breaking signature or config change.
3. **Mandatory `### Not verified` Disclosure**: Explicitly list platforms, OS primitives, or failure modes that were only cross-compiled or statically analyzed rather than executed natively on the test host.

### 6.4 Source-Verified Truthfulness and Anti-Diary Rules

- **Zero Hallucinated Symbols**: Every type name, function signature, CLI flag, file path (`catalog.db` vs. imaginary `manifest`), and error code in documentation must exist in the current source tree.
- **Zero Unmeasured Benchmarks**: Never include throughput or latency numbers unless a reproducible benchmark suite exists in the repository and the exact hardware/command is cited.
- **Zero Implementation Diaries or Local Paths**: Never commit chronological status logs (`Status at 11ce187 (2026-10-11)...`), celebratory status theater (`✅ CLOSED`, `100% Working`), or machine-local paths (`/tmp/regressdump`, `~/.claude/plans/...`) into repository documentation.

---

## 7. Safety Alert Hierarchy and Placement (MIL-STD-38784 & ASD-STE100)

In aerospace technical manuals (**MIL-STD-38784** and **ASD-STE100**), placing a safety alert *after* an instruction is a critical defect because the technician executes the hazardous step before reading the warning. Apply the same strict discipline to software documentation:

### 7.1 Mandatory Pre-Procedure Placement Rule

Always place `CAUTION`, `WARNING`, and `IMPORTANT` alerts **IMMEDIATELY BEFORE** the procedural step, CLI command, or code snippet they govern. Never place a hazard alert below a command block as a postscript. Do not stack two alerts consecutively.

### 7.2 Severity Hierarchy

| GitHub Alert Tag | Aerospace Equivalent | Triggering Software Condition |
| :--- | :--- | :--- |
| `> [!CAUTION]` | **WARNING** (Critical Hazard) | Action can cause **irreversible data loss**, **secret/key exposure**, **memory corruption / UB** (`unsafe.Pointer`, use-after-free, data race), or **security boundary bypass**. |
| `> [!WARNING]` | **CAUTION** (System Damage) | Action can cause **breaking wire/API incompatibility**, **active connection drops**, **deadlock**, **file descriptor leaks**, or **out-of-memory exhaustion**. |
| `> [!IMPORTANT]` | **MANDATORY PREREQUISITE** | Essential contract invariant or ordering requirement that callers must satisfy for normal operation. |
| `> [!NOTE]` | **NOTE** (Information Only) | Supplemental background explanation. **Must never contain a hazard condition or a procedural instruction.** |

### 7.3 Controlled 3-Sentence Alert Format

Write every `CAUTION` and `WARNING` block using this maximum 3-sentence structure:
1. **Sentence 1 (Imperative Command/Prohibition, `<= 20` words)**: State clearly what the caller must or must not do.
2. **Sentence 2 (Concrete Consequence, `<= 25` words)**: State the exact corruption, data loss, or failure mode that occurs if violated.
3. **Sentence 3 (Safe Remediation, Optional, `<= 20` words)**: State how to perform the operation safely.

---

## 8. Best Practices and Paired Examples (`BAD` vs. `GOOD`)

### Example 1: Source-Text Regex Pinning vs. Runtime Behavioral Execution

**Why**: Asserting substrings on raw source files freezes variable names and comments while proving nothing about runtime execution.

**BAD (Source-Text Slop)**
```javascript
test("rewriter strips integrity and nonce attributes", () => {
  const src = fs.readFileSync("web/prelude.js", "utf8");
  assert(src.includes("function stripNonceAndIntegrity("));
  assert(src.indexOf("removeAttribute('nonce')") < src.indexOf("removeAttribute('integrity')"));
});
```

**GOOD (Behavioral Execution)**
```javascript
test("prelude removes nonce and integrity attributes from inserted script elements", () => {
  const realm = bootPreludeInVM();
  const script = realm.document.createElement("script");
  script.setAttribute("nonce", "secret-123");
  script.setAttribute("integrity", "sha384-abc");

  realm.document.head.appendChild(script);

  assert.equal(script.hasAttribute("nonce"), false);
  assert.equal(script.hasAttribute("integrity"), false);
});
```

### Example 2: Middle-Layer Private State Inspection vs. Observable Protocol Contract

**Why**: Locking private mutexes and checking unexported fields couples the test to internal representation and skips the real HTTP transport contract.

**BAD (Middle-Layer Harness & Private Field Slop)**
```go
func TestCacheRetry(t *testing.T) {
	s := &CacheSyncer{client: stubClient{status: 503}}
	s.mu.Lock()
	s.snapshot = &Snapshot{Hash: "abc"}
	s.mu.Unlock()

	_ = s.syncOnce(context.Background())

	s.mu.Lock()
	if s.snapshot == nil {
		t.Fatal("expected private snapshot field to remain non-nil after 503")
	}
	s.mu.Unlock()
}
```

**GOOD (Black-Box Protocol Contract)**
```go
func TestCacheSyncRetriesTransientServerErrorsAndUploadsIntactBundle(t *testing.T) {
	var attempts atomic.Int32
	var uploadedBytes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upload body: %v", err)
		}
		uploadedBytes.Store(int64(len(body)))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	syncer := cache.NewSyncer(srv.URL, t.TempDir())
	if err := syncer.SyncNow(context.Background()); err == nil {
		t.Fatal("first SyncNow = nil, want 503 transient error")
	}
	if got := uploadedBytes.Load(); got != 0 {
		t.Fatalf("uploaded bytes after failed check = %d, want 0", got)
	}
	if err := syncer.SyncNow(context.Background()); err != nil {
		t.Fatalf("second SyncNow: %v", err)
	}
	if got := uploadedBytes.Load(); got <= 0 {
		t.Fatalf("uploaded bytes after retry = %d, want > 0", got)
	}
}
```

### Example 3: Tautological Constant Test vs. Spec-Driven ABI & Cache-Line Invariant Test

**Why**: Copying literals from the same package tests nothing. Verifying struct offsets against a cross-language spec prevents ABI drift and false sharing.

**BAD (Tautological Constant Slop)**
```go
func TestConstants(t *testing.T) {
	if Magic != 0x48515133 {
		t.Fatal("wrong magic")
	}
	if RingHeaderSize != 384 {
		t.Fatal("wrong ring header size")
	}
}
```

**GOOD (Cross-Artifact Layout & Cache-Line Isolation Test)**
```go
func TestRingHeaderMatchesSpecAndIsolatesCacheLines(t *testing.T) {
	spec := loadSpecConstants(t, "../spec/constants.json")
	if got := int(unsafe.Sizeof(RingHeader{})); got != spec.RingHeader.Size {
		t.Fatalf("sizeof(RingHeader) = %d, want %d", got, spec.RingHeader.Size)
	}

	writeLine := int(unsafe.Offsetof(RingHeader{}.WriteIdx)) / spec.CacheLineSize
	readLine := int(unsafe.Offsetof(RingHeader{}.ReadIdx)) / spec.CacheLineSize
	if writeLine == readLine {
		t.Fatalf("WriteIdx and ReadIdx share cache line %d; producer and consumer atomics must be isolated", writeLine)
	}
}
```

### Example 4: Production Test-Fixture Branching vs. Honest Domain Enforcement

**Why**: Branching on literal test inputs (`role == "r"`) or returning silent no-ops hides missing security features and corrupts production behavior.

**BAD (Test-Specific Production Hack & Silent No-Op)**
```go
func (e *Engine) execCreateRole(stmt *CreateRoleStmt) error {
	// Keep legacy unsupported gate test passing when role is named "r"
	if stmt.Name == "r" && len(stmt.Options) == 0 {
		return ErrFeatureNotSupported
	}
	return nil // No-op stub so integration scripts pass
}
```

**GOOD (Real Catalog Enforcement)**
```go
func (e *Engine) execCreateRole(ctx context.Context, stmt *CreateRoleStmt) error {
	if err := e.catalog.CreateRole(ctx, stmt.Name, stmt.Options); err != nil {
		return err
	}
	return nil
}
```

### Example 5: AI-Slop Prose with Em-Dashes vs. Controlled Technical Writing (ASD-STE100 + HQQ)

**Why**: Eliminating em-dashes, buzzwords, passive voice, and 4+ noun clusters makes architecture and failure boundaries immediately verifiable.

**BAD (AI Slop + Em-Dashes + Uncontrolled Noun Clusters)**
```markdown
At its core, the transport engine is a robust, next-generation IPC framework designed to seamlessly bridge the gap between shared memory and sockets -- empowering developers to effortlessly handle high-throughput workloads under the hood. When it comes to shared memory ring buffer slot sequence counter management, stale peers are automatically detected and cleaned up so that your application never hangs.
```

**GOOD (Controlled Technical Prose: Zero Em-Dashes, Active Voice, STE100 Bounds)**
```markdown
The transport engine implements inter-process communication over POSIX shared memory and OS wait primitives. Callers select either the zero-copy slot reservation API or standard `net.Conn` stream adapters.

The acceptor tracks peer liveness using an epoch counter in the 512-byte superblock. When a peer process exits without releasing its claim, the supervisor increments the epoch and reclaims orphaned slots within 50 milliseconds.
```

### Example 6: Post-Command Vague Note vs. Pre-Procedure MIL-STD-38784 Safety Alert & STE100 Steps

**Why**: Placing a hazard warning before the code block prevents callers from copying a memory-corrupting pattern. Numbered imperative steps under 20 words eliminate procedural ambiguity.

**BAD (Post-Procedure Note & Passive Prose)**
````markdown
### Zero-Copy Read Procedure

Ensure that `TryReserveRead` is invoked to obtain a view of the next frame payload, after which `CommitRead` should be called:

```go
view, ok := link.TryReserveRead()
if ok {
    link.CommitRead(view)
    processPayload(view.Bytes())
}
```

> [!NOTE]
> It is worth mentioning that `view.Bytes()` is backed by shared memory -- so reading it after `CommitRead` may result in data races if the producer overwrites the slot.
````

**GOOD (Pre-Procedure `CAUTION` + ASD-STE100 Imperative Steps)**
````markdown
### Zero-Copy Read Procedure

Read a frame from the shared-memory ring without heap allocation by reserving the slot, consuming the payload, and committing the read index.

> [!CAUTION]
> Do not read from `view.Bytes()` after calling `CommitRead`. Committing the slot publishes it for immediate overwrite by producer processes and causes concurrent memory corruption. Copy the payload before calling `CommitRead` if you retain the data after the call.

1. Call `TryReserveRead` to acquire a read view of the next committed slot.
2. Read or copy the bytes from `view.Bytes()` while the reservation remains active.
3. Call `CommitRead` to release the slot back to the ring buffer.

```go
view, ok := link.TryReserveRead()
if !ok {
    return ErrRingEmpty
}
payload := slices.Clone(view.Bytes())
link.CommitRead(view)
processPayload(payload)
```
````

### Example 7: Korean Technical Documentation (Translationese Slop vs. Controlled Korean Prose)

**Why**: Eliminating dash connectors, double passives (`~되어집니다`), and low-density modifiers (`다양한`, `효과적으로`, `손쉽게`) while placing safety alerts before procedures prevents misinterpretation in Korean documentation.

**BAD (AI Slop, Translationese, and Post-Procedure Caution)**
```markdown
본 라이브러리는 근본적으로 공유 메모리를 활용하여 다양한 프로세스 간의 원활한 통신을 효과적으로 수행하는 역할을 합니다 -- 이를 통해 개발자는 복잡한 동기화 문제를 손쉽게 해결할 수 있도록 지원받습니다. 슬롯이 반환되어진 이후에는 버퍼에 접근하지 않도록 주의가 요구되어집니다.
```

**GOOD (Controlled Korean Technical Prose)**
```markdown
이 라이브러리는 POSIX 공유 메모리(`mmap`)와 커널 대기 프리미티브 위에서 동작하는 프로세스 간 통신(IPC) 엔진이다. 호출자는 고정 크기 슬롯을 예약하는 제로카피 API와 표준 `net.Conn` 스트림 어댑터 중 워크로드에 맞는 계층을 선택한다.

> [!CAUTION]
> `CommitRead`를 호출한 뒤에는 반환된 슬롯의 바이트 슬라이스를 읽지 마십시오. 슬롯을 반환하면 생산자 프로세스가 해당 메모리를 즉시 덮어쓰므로 데이터 경쟁이 발생합니다.
```

---

## 9. Master Execution Protocol and Verification Checklist

When asked to write, review, or deslop tests, code, or documentation, execute the following protocol:

### 9.1 Prompt Specification for Controlled Technical Writing & Deslopping

Apply these exact formatting constraints to every generated or edited artifact:

1. **Test Contract Gate**:
   - Delete any test that reads source files as strings, mutates private fields, calls unexported lifecycle helpers on half-initialized structs, or logs errors without failing.
   - Replace `time.Sleep` and `runtime.NumGoroutine()` checks with virtual clocks, explicit readiness signals, or deterministic pipe closure (`io.EOF`).
   - Name every test function after the timeless behavioral contract it verifies.
2. **Production & Comment Gate**:
   - Remove all test-fixture conditionals and silent no-op stubs from production code.
   - Delete all comments that restate syntax, draw ASCII banners, or narrate commit/bug history. Keep only concise "Why" comments explaining hardware, concurrency, or toolchain invariants.
3. **Typography & Lexicon Gate**:
   - **Zero Em-Dashes**: Never output U+2014, spaced U+2013, `--`, or prose arrows (`->`). Use `:`, `;`, `()`, `.`, or `,`.
   - **Zero AI Slop Words**: Never output banned buzzwords (`seamless`, `robust`, `cutting-edge`, `under the hood`, `heavy lifting`, `leverage`, `utilize`, `delve`, `crucial`, `pivotal`, `paramount`) or throat-clearing prefaces (`At its core`, `It is important to note that`, `When it comes to`).
4. **ASD-STE100 Sentence & Grammar Gate**:
   - **Procedural sentences**: `<= 20` words, 1 instruction per sentence, starting with an imperative verb.
   - **Descriptive sentences**: `<= 25` words, `<= 6` sentences per paragraph, written in active voice.
   - **Noun clusters**: `<= 3` consecutive nouns.
   - **One Word, One Meaning**: Use exact mechanical verbs (`parse`, `validate`, `enqueue`, `evict`, `abort`) instead of polysemous placeholders (`handle`, `process`, `manage`, `follow`).
5. **S1000D, ATA iSpec 2200, & Safety Alert Gate**:
   - Separate Descriptive, Procedural, and Fault-Isolation modules; never mix theory and CLI steps in one paragraph.
   - Place `> [!CAUTION]` and `> [!WARNING]` blocks **immediately before** the procedure or code block they govern, formatted in `<= 3` controlled sentences.
   - Verify every symbol, signature, CLI flag, file path, and benchmark command against the live source tree. Include a `### Not verified` section in release or verification summaries when cross-compiled targets were not executed natively.

### 9.2 Automated Mechanical Audit Commands

Run these deterministic checks before completing any documentation, test, or code deslopping task:

```bash
# 1. Verify zero em-dashes, spaced en-dashes, or prose arrows in documentation (must return 0 matches)
rg -n "\x{2014}| \x{2013} |\x{2192}" README.md PROTOCOL.md CHANGELOG.md AGENTS.md docs/ spec/ 2>/dev/null

# 2. Verify zero banned AI-slop vocabulary in documentation and comments (must return 0 matches)
rg -ni "\b(seamless|seamlessly|robust|battle-tested|cutting-edge|next-generation|state-of-the-art|powerhouse|unleash|empower|supercharge|elevate|delve|tapestry|testament|holistic|synergy|paradigm|under the hood|heavy lifting|effortlessly|meticulously|crucial|pivotal|paramount|game-changer|utilize|utilizes|utilizing|leverage|leverages|leveraging)\b" README.md PROTOCOL.md CHANGELOG.md AGENTS.md docs/ spec/ 2>/dev/null

# 3. Verify zero throat-clearing prefaces or banned rhetorical formulas (must return 0 matches)
rg -ni "(it is important to note|it is worth mentioning|at its core|when it comes to|in the realm of|not only .* but also|whether you are .* or)" README.md PROTOCOL.md CHANGELOG.md AGENTS.md docs/ spec/ 2>/dev/null

# 4. Audit tests for sleep-based synchronization, runtime goroutine counting, or source-file string pinning
rg -n "(time\.Sleep|runtime\.NumGoroutine|readFileSync\(|os\.ReadFile\(.*\.(go|rs|js|ts|py))" . --glob "*test*" 2>/dev/null

# 5. Audit Go tests for observation-only t.Logf inside error branches or stdout pollution
rg -n "(fmt\.Print|t\.Logf\(.*err)" . --glob "*_test.go" 2>/dev/null
```

### 9.3 Final Completion Checklist

- [ ] Every test verifies an observable behavioral contract at either the lowest coherent owner unit or the black-box E2E boundary.
- [ ] Zero tests inspect raw source files, mutate private struct fields, or pass without falsifiable assertions.
- [ ] Zero production branches check test fixture identifiers or return silent no-ops for unimplemented commands.
- [ ] Zero em-dashes (U+2014), spaced en-dashes (U+2013), or banned AI-slop words exist in any modified file.
- [ ] Procedural steps obey ASD-STE100 bounds (`<= 20` words, imperative verb start, 1 action per sentence).
- [ ] Descriptive paragraphs obey ASD-STE100 bounds (`<= 25` words per sentence, `<= 6` sentences per paragraph, active voice, `<= 3` consecutive nouns).
- [ ] All `CAUTION` and `WARNING` safety alerts appear **before** the procedure or code block they govern.
- [ ] Every documented type, function signature, CLI flag, constant, and file path matches the current source tree.

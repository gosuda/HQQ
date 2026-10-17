---
name: technical-writing
description: Writes and maintains high-precision, zero-slop technical documentation, READMEs, architecture guides, ADRs, API references, and protocol specifications without em-dashes or LLM clichés. Use whenever creating, rewriting, reviewing, or synchronizing project documentation after code changes.
license: MIT
---

# Technical Writing & Documentation Engineering

Write like a principal engineer and staff technical editor. Good technical prose respects the reader's time, leads with concrete mechanics, and anchors every architectural claim in real code, data layouts, contracts, and invariants.

---

## 1. Scope and Routing

Use this skill whenever:

- Creating or rewriting `README.md`, architecture guides (`docs/architecture.md`, `AGENTS.md`), protocol or wire specifications (`PROTOCOL.md`, `spec/*.md`), API reference manuals, Architecture Decision Records (ADRs), or `CHANGELOG.md`.
- Synchronizing documentation after modifying public signatures, CLI flags, configuration keys, wire formats, state machines, or subsystem boundaries.
- Auditing or reviewing existing markdown and docstrings (`doc.go`, module headers) for AI-generated slop, punctuation tics, stale symbols, or low-density prose.

**Skill Boundaries**:
- Route inline code comments and Git commit message formatting to `zero-slop`.
- Route signature design, type modeling, and ownership semantics to `api-design`.
- Use `technical-writing` as the authoritative standard for all long-form documentation, specification engineering, and package-level reference prose.

---

## 2. Voice, Tone, and Editorial Philosophy

1. **Concrete Mechanics Over Marketing Abstractions**: State what a component does in terms of data structures, state transitions, memory layouts, concurrency primitives, network protocols, and failure modes. Never describe software with emotional adjectives or promotional promises.
2. **High Signal-to-Noise Ratio**: Assume the reader understands software engineering fundamentals. Do not explain what a mutex, ring buffer, B-tree, or HTTP status code is; explain how *this* codebase bounds contention, manages lifetimes, enforces idempotency, or frames payloads.
3. **Authoritative, Composed Cadence**: Vary sentence length deliberately. Pair a 20-word technical explanation with a 7-word declarative consequence. Rhythm comes from syntax and precision, not theatrical punctuation or exclamation marks.
4. **Source-Verified Accuracy**: Never invent or guess a function signature, type name, constant value, CLI flag, error sentinel, or file path. Every symbol, byte size, timeout, default value, and import path in documentation must match the current source tree.

---

## 3. Strict Punctuation & Typography Rules

### 🚫 Absolute Ban on Em-Dashes (`—`)

Never use the em-dash (`—`, U+2014), spaced en-dash (` – `, U+2013), or double-hyphen (`--`) as prose punctuation in any documentation file, docstring, list item, or table cell. Overuse of em-dashes is the most recognizable syntactic tic of LLM-generated text and produces loose, parenthetical sentences that weaken technical authority.

Choose the exact punctuation mark that expresses the logical relationship between clauses:

| Logical Relationship | Replacement | Example |
| :--- | :--- | :--- |
| **Elaboration, definition, or consequence** | Colon (`:`) | `Generation counters identify a specific slot incarnation: numeric indices alone cannot distinguish recycled buffers.` |
| **Two closely linked independent clauses** | Semicolon (`;`) | `Listen initializes local shared-memory regions immediately; it does not block on peer attachment.` |
| **Subordinate metadata (sizes, codes, defaults)** | Parentheses (`()`) | `Frame headers encode four transport modes (Unreliable, Reliable, Ordered, and Stream).` |
| **Separate, high-impact technical claim** | Period (`.`) | `Session keys live exclusively in pinned memory. The runtime wipes all key buffers on shutdown.` |
| **Continuous action or participial modifier** | Comma (`,`) | `The writer splits oversized payloads into 4096-byte segments, zero-padding the trailing cache line.` |

*Note*: Standard ASCII hyphens (`-`) in compound modifiers (`zero-allocation parser`, `shared-memory ring`, `64-byte header`), CLI flags (`-listen-addr`), and negative numbers remain mandatory.

---

## 4. Anti-Slop Lexicon & Structural Bans

### 🚫 Banned English Vocabulary

Never use the following words or phrases in any technical document:

- **AI Hyperbole & Buzzwords**: `seamless`, `seamlessly`, `robust`, `battle-tested`, `cutting-edge`, `next-generation`, `state-of-the-art`, `powerhouse`, `unleash`, `empower`, `supercharge`, `elevate`, `delve`, `tapestry`, `testament`, `landscape`, `holistic`, `synergy`, `paradigm`, `first-class citizen`, `out of the box`, `under the hood`, `heavy lifting`, `breeze`, `effortlessly`, `meticulously`, `vital`, `crucial`, `pivotal`, `paramount`, `game-changer`, `harness`.
- **Throat-Clearing Prefaces**: `It is important to note that`, `It is worth mentioning that`, `In order to`, `At its core`, `Fundamentally`, `Essentially`, `Basically`, `When it comes to`, `In the realm of`, `Let's explore`, `Dive into`, `As mentioned earlier`, `In today's fast-paced`.
- **Hollow Verb Phrases**:
  - Instead of `is responsible for handling` -> use `handles`, `dispatches`, `parses`, `validates`.
  - Instead of `serves as a` or `acts as a` -> state what it *is* or what it *does* (`is the public entrypoint`, `terminates inbound streams`).
  - Instead of `facilitates the process of` -> name the exact operation (`schedules`, `serializes`, `retransmits`).
  - Instead of `provides the ability to` -> use `lets callers`, `exposes`, `supports`.
  - Instead of `ensures that` (when used vaguely) -> state the mechanism (`rejects`, `bounds`, `wipes`, `verifies`).
  - Instead of `utilizes` or `leverages` -> use `uses`, `calls`, `reads`.

### 🚫 Banned Rhetorical Structures

1. **"Not only X, but also Y"**: State both facts directly (`The handshake validates the protocol version and negotiates ring capacity`).
2. **"Whether you are X or Y"**: State the supported modes plainly (`The library exposes both a low-level zero-copy API and standard net.Conn wrappers`).
3. **Negative-Positive Tautologies ("It's not just X; it's Y")**: Delete the theatrical contrast and state the engineering truth.
4. **Repetitive Participial Bullet Intros**: In bulleted lists, do not start consecutive lines with `-ing` participles (`Ensuring...`, `Providing...`, `Leveraging...`). Start with imperative verbs, concrete nouns, or bolded component names.
5. **Vague Quantifiers**: Replace `fast`, `low-latency`, `small`, `large`, or `many` with exact numbers, complexity bounds (`O(1)` lookup, `64-byte` header, `256 KiB` ring), or measurable conditions.

### 🚫 Korean Technical Prose Bans (한국어 문서 작성 시 추가 규칙)

When writing or editing technical documentation in Korean, eliminate translationese and LLM filler:

- **Em-dash (`—`) 금지**: 한국어 문장에서도 `—`나 ` - `를 절 구분자로 쓰지 말고, 콜론(`:`), 괄호(`()`), 쉼표(`,`), 또는 마침표(`.`)로 문장을 명확히 분리한다.
- **번역투 및 이중 피동 금지**: `~되어집니다`, `~해집니다`, `~에 의해 처리됩니다` 대신 능동형 동사(`처리한다`, `검증한다`, `거부한다`)를 사용한다.
- **저밀도 수식어 금지**: `다양한`, `효과적으로`, `원활하게`, `강력한`, `체계적인`, `손쉽게`, `완벽하게` 같은 추상적 부사·형용사를 삭제하고 구체적인 수치나 동작으로 대체한다.
- **군더더기 연결구 축약**: `~를 통해`(수단 남발), `~할 수 있도록 지원합니다`, `~하는 역할을 수행합니다`를 쓰지 말고 `~로 ~한다`, `~를 제공한다`, `~한다`로 직결한다.

---

## 5. Prose vs. Table vs. Diagram Selection Rules

Never write long paragraphs when structured visual formats convey the mechanics faster. Apply these conversion rules:

| Information Pattern | Required Format | Formatting Rules |
| :--- | :--- | :--- |
| **3+ items compared across 3+ shared attributes** (modes, CLI flags, config keys, error sentinels, API tiers) | **Markdown Table** | Left-align text columns (`:---`), right-align numeric columns (`---:`). Keep cell text to fragments or single sentences without em-dashes. |
| **Byte, bit, or memory-mapped layouts** (frame headers, superblocks, cache-line structs) | **ASCII Byte/Bit Map + Field Table** | Show exact byte offsets (`0x00..0x07` or bit rulers) in a fenced `text` block, followed immediately by a table defining each field's offset, size, type, endianness, and invariant. |
| **Multi-step handshakes, RPC lifecycles, or actor interactions** (3+ message hops) | **Mermaid `sequenceDiagram`** | Label arrows with exact frame/method names and key arguments. Include failure or timeout branches (`alt` / `opt`) where relevant. |
| **Lifecycle transitions and protocol state machines** | **Mermaid `stateDiagram-v2` + Transition Table** | Pair the visual state graph with a table mapping `(Current State, Event/Guard) -> (Next State, Side Effect)`. |
| **Subsystem dependencies, data pipelines, or routing trees** | **Mermaid `flowchart TD` / `LR`** | Quote all node labels containing parentheses, colons, or slashes (`id["Label (Detail)"]`). Keep edge labels to 1 to 3 words. |

---

## 6. Document Archetype Blueprints

### Opening Every Document and Section

1. **Document Lead**: The first paragraph immediately below `# Title` must define what the system or module is, what boundary it owns, and how callers or peers interact with it in 2 to 4 crisp sentences.
2. **No Stacked Headings**: Never place two headings back-to-back (`## Section` followed immediately by `### Subsection`) without an introductory sentence or summary table establishing the section's scope.

### Blueprint A: `README.md` (Project Entrypoint)

Structure top-level repository READMEs in this order:
1. **Title & Lead Paragraph**: Exact technical identity, execution model, and primary interfaces.
2. **Architecture / Capability Overview**: A concise diagram or tier table showing how components fit together and which API or mode to choose for a given workload.
3. **Quickstart**: Minimal, runnable, copy-pasteable code or CLI examples showing realistic initialization, error handling, and resource cleanup (`defer conn.Close()`).
4. **Core Mechanics & Guarantees**: Summary of memory ownership, concurrency safety, delivery guarantees, or performance bounds.
5. **Documentation Index**: Table linking to deep-dive specs (`PROTOCOL.md`, `docs/*`) with one-line scope descriptions.
6. **Build, Verification, & Benchmarks**: Exact commands to build, run unit/race/fuzz tests, and reproduce benchmarks.

### Blueprint B: `PROTOCOL.md` / `spec/*.md` (Protocol & Wire Specifications)

Write specifications so an independent engineer can implement a conforming peer without reading your source code:
1. **Normative Language**: Use uppercase RFC 2119 keywords (`MUST`, `MUST NOT`, `REQUIRED`, `SHOULD`, `SHOULD NOT`, `MAY`) deliberately to distinguish hard protocol invariants from implementation advice.
2. **Constants & Magic Numbers**: Tabulate all magic bytes, version numbers, size limits, alignment boundaries, and default timeouts with exact hex/decimal values.
3. **Memory & Wire Layouts**: Provide exact byte offsets, endianness (`little-endian`, `network byte order`), alignment rules, and padding requirements (`MUST be zero-filled`).
4. **Ordering & Concurrency Semantics**: For shared-memory or lock-free protocols, specify exact memory-ordering requirements (`Acquire`, `Release`, `SeqCst`) for every atomic field read and write.
5. **State Machines & Failure Recovery**: Define legal state transitions, teardown sequences, crash detection (e.g., dead-peer detection, epoch/generation fencing), and exact error codes returned on malformed inputs.

### Blueprint C: `docs/architecture.md` & `AGENTS.md` (Architecture & Subsystem Guides)

1. **Layered Hierarchy / Subsystem Map**: Define each package or layer, its single responsibility, and strict dependency directionality (what it may import and what it must never import).
2. **Invariants & Ownership**: Document who allocates, mutates, and releases buffers, descriptors, keys, or connections across boundaries. Explain the failure mode each invariant prevents (e.g., why generation counters prevent ABA slot reuse).
3. **Execution & Concurrency Model**: Specify goroutine/thread lifecycles, queue bounds, backpressure behavior when queues fill, and shutdown drain ordering.

### Blueprint D: API Reference & Package Docstrings (`doc.go`, Public Headers)

1. **Ownership & Lifetime Contract**: State whether passed slices/buffers are borrowed for the duration of the call or retained asynchronously, and when returned views become invalid.
2. **Concurrency Contract**: State explicitly whether a type is safe for concurrent use by multiple callers or requires external synchronization.
3. **Error Semantics**: List specific error sentinels or error types returned under boundary conditions (timeout, closed peer, buffer exhaustion, invalid state).

### Blueprint E: `CHANGELOG.md` & Migration Guides

1. **Categorized Entries**: Group changes under `Breaking Changes`, `Features`, `Bug Fixes`, and `Performance`.
2. **Concrete Impact**: State the observable behavior change, affected public symbols, or wire format revision rather than echoing commit titles.
3. **Migration Snippets**: Whenever a breaking change alters a public signature or configuration schema, include a brief `Before` / `After` code block showing callers how to update.

---

## 7. Paired Examples: AI Slop vs. Technical Precision

### Example 1: Systems & IPC Overview
- ❌ **BAD (Slop + Em-dash)**:
  > At its core, the transport engine is a robust, next-generation IPC framework designed to seamlessly bridge the gap between raw shared memory and standard network sockets — empowering developers to effortlessly build high-performance applications under the hood.
- ✅ **GOOD (Technical Precision)**:
  > The transport engine implements inter-process communication over POSIX shared memory and OS wait primitives. It exposes both a zero-copy reservation API for latency-critical callers and standard `net.Conn` and `net.PacketConn` adapters.

### Example 2: Distributed Backend & API Service
- ❌ **BAD (Slop + Em-dash)**:
  > The order processor plays a crucial role in handling all the heavy lifting of payment execution — ensuring that retries are meticulously managed and duplicate charges are effortlessly prevented across distributed nodes.
- ✅ **GOOD (Technical Precision)**:
  > `OrderProcessor` executes payment capture idempotently using a caller-supplied 128-bit `IdempotencyKey`. Concurrent requests with the same key block on a lease lock in PostgreSQL (`SELECT ... FOR UPDATE SKIP LOCKED`); retries after a committed capture return the stored `Receipt` without re-invoking the upstream payment gateway.

### Example 3: Wire & Memory Layout Specification
- ❌ **BAD (Slop + Em-dash)**:
  > When it comes to frame parsing, the library leverages a cutting-edge zero-allocation cursor approach — which means it avoids heap allocations in order to supercharge throughput.
- ✅ **GOOD (Technical Precision)**:
  > Frame parsers operate directly on memory-mapped ring slices. Parsing returns zero-allocation views over the shared buffer, and writers serialize 16-byte headers in place before publishing the updated write index with store-release semantics.

### Example 4: CLI & SDK Reference
- ❌ **BAD (Slop + Em-dash)**:
  > Not only does the CLI provide a flexible way to inspect cluster health, but it also serves as a powerful diagnostic powerhouse whether you are debugging locally or running in production.
- ✅ **GOOD (Technical Precision)**:
  > `clusterctl status` queries each node's `/healthz` endpoint concurrently with a 2-second deadline. It prints a tab-aligned status table to `stdout` and exits with code `0` when all nodes report `Ready`, `1` on degraded quorum, or `2` on flag validation errors.

### Example 5: Korean Technical Documentation (한국어 기술 문서)
- ❌ **BAD (Slop + 번역투 + Em-dash)**:
  > 본 라이브러리는 근본적으로 공유 메모리를 활용하여 다양한 프로세스 간의 원활한 통신을 효과적으로 수행하는 역할을 합니다 — 이를 통해 개발자는 복잡한 동기화 문제를 손쉽게 해결할 수 있도록 지원받습니다.
- ✅ **GOOD (Technical Precision)**:
  > 이 라이브러리는 POSIX 공유 메모리(`mmap`)와 futex/ulock 대기 프리미티브 위에서 동작하는 프로세스 간 통신(IPC) 엔진이다. 호출자는 고정 크기 슬롯을 직접 예약하는 제로카피(Zero-copy) API와 표준 `net.Conn` 호환 스트림 API 중 워크로드에 맞는 계층을 선택할 수 있다.

---

## 8. Post-Change Documentation Synchronization Workflow

Whenever a code modification alters public APIs, configuration options, CLI flags, protocol behavior, memory/wire layouts, subsystem boundaries, file paths, or operational defaults, synchronize the documentation before completing the task:

1. **Identify Impacted Surfaces**:
   - Public types, functions, methods, options, or error sentinels -> update `README.md`, package docstrings (`doc.go` or public header files), and API reference docs.
   - Memory layouts, frame headers, atomic ordering rules, state machines, or constants -> update `PROTOCOL.md` and `spec/`.
   - Subsystem boundaries, internal package layout, or concurrency invariants -> update `AGENTS.md` and `docs/`.
   - User-visible behavior, new capabilities, or breaking changes -> update `CHANGELOG.md`.
2. **Verify Against Source**:
   - Inspect the modified source files directly. Update affected struct/type definitions, function signatures, default constants, byte offsets, or sequence diagrams so zero drift remains between code and prose.
3. **Run Mechanical Editorial Checks**:
   Run these checks across all modified documentation files before finishing:
   ```bash
   # 1. Check for forbidden em-dashes and spaced en-dashes (must return 0 matches)
   rg -n "—| – " README.md PROTOCOL.md CHANGELOG.md AGENTS.md docs/ spec/ 2>/dev/null

   # 2. Check for banned AI-slop vocabulary (must return 0 matches)
   rg -ni "\b(seamless|seamlessly|robust|battle-tested|cutting-edge|next-generation|state-of-the-art|powerhouse|unleash|empower|supercharge|elevate|delve|tapestry|testament|holistic|synergy|paradigm|under the hood|heavy lifting|effortlessly|meticulously|crucial|pivotal|paramount)\b" README.md PROTOCOL.md CHANGELOG.md AGENTS.md docs/ spec/ 2>/dev/null
   ```
4. **Complete Verification Checklist**:
   - [ ] Zero em-dashes (`—`) or spaced en-dashes (` – `) exist in any modified documentation or docstring.
   - [ ] Zero banned AI-slop buzzwords, throat-clearing prefaces, or hollow verb phrases remain.
   - [ ] Every documented type, function signature, CLI flag, constant value, and byte offset matches the current source code.
   - [ ] Every `#` or `##` heading is followed by a lead paragraph or summary table before any subheading.
   - [ ] Multi-item comparisons (3+ items across 3+ attributes) use Markdown tables rather than repetitive prose.
   - [ ] Relative markdown links and file references resolve to existing paths.

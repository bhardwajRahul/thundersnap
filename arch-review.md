# Architecture / Code Review

Collected review notes, broken down module by module. This is a review pass only —
nothing is fixed here. Each module section is organized into five subsections:

1. **Edge cases for unit tests** — currently-untested cases worth covering.
2. **Duplicated code (within the module)** — copy-paste / near-duplicate logic.
3. **Overly complicated / redundant / dead code paths.**
4. **Unclear / unexplained code** — places that need an explanatory comment.
5. **Godoc accessibility** — exported items missing godoc, or that should be unexported.

A short "Highest-value items" list closes the big modules.

---

## frameid/  [DONE]

### 1. Edge cases for unit tests
- `MustNew` panic path is never exercised (acknowledged hard to trigger, but no test
  forces/verifies the panic).
- No test that two consecutive `New()` calls produce *distinct* IDs (uniqueness only
  asserted indirectly via time-ordering).
- `Parse` of a valid but non-v7 UUID (e.g. a v4 string) is untested — `Parse` accepts
  any UUID version, surprising given the package is documented as "UUIDv7". A test
  documenting that `Parse`/`New` version expectations differ would help.
- `Nil` round-trip: `Parse("000...0")` → value for which `IsZero` is true, untested.
- `TestTimeOrdering` (frameid_test.go:92-109) is brittle: relies on `>=` ordering with
  1ms sleeps; sub-millisecond ordering within the same ms is implementation-dependent.

### 2. Duplicated code
- None. File is ~50 lines, thin wrappers over `github.com/google/uuid`.

### 3. Overly complicated / redundant
- None.

### 4. Unclear / unexplained code
- `type ID = uuid.UUID` (line 15) is a deliberate **type alias** (not a defined type) so
  callers can use `uuid.UUID` interchangeably — but the choice is unexplained and has
  API-compatibility implications. A one-line comment on why alias vs named type would help.

### 5. Godoc accessibility
- In good shape. All exported items (`ID`, `New`, `MustNew`, `Parse`, `MustParse`,
  `IsZero`, `Nil`) have proper godoc starting with the identifier name.
- No exported items that should be unexported.

---

## refid/  [DONE]

### 1. Edge cases for unit tests
- **No unit test file at all** — only `refid.go`; all coverage is the single e2e test
  (`e2e/refid_test.go`, root+btrfs). Pure btrfs-free logic that could/should have unit tests:
  - `IDDir` and `Path` construction: `Path(frame, "")`, refName with slashes, and
    especially `Path("/f", "../escape")` — **refName is not sanitized** and would escape
    the `/id` dir. Untested and arguably unguarded.
  - `isSubvolume` returning false when the `btrfs` binary is absent (it swallows all errors
    → returns false; could mask real failures).
- Branch-coverage gaps (need btrfs, currently e2e-only): the "leftover plain dir replaced"
  paths in `Ensure` (77-81), `Move` (113-117), and `Remove`'s plain-dir fallback (130-135).

### 2. Duplicated code
- "create subvolume + chmod 0700" sequence duplicated between `ensureIDSubvol` (53-58) and
  `Ensure` (82-87). Candidate for a shared `createSubvol(path)` helper.
- "stat → if plain dir → RemoveAll" guard repeated 3×: `ensureIDSubvol` (47-51), `Ensure`
  (77-81), `Move` (113-117). Candidate for `removeIfPlainDir(path)`.

### 3. Overly complicated / redundant
- `ensureIDSubvol` (45-61) runs `isSubvolume` (→ `btrfs subvolume show`) up to twice in the
  common already-a-subvolume case (line 47 and 52). Minor inefficiency / convoluted flow.
- `Move` calls `Remove(dstFramePath, refName)` (110) which re-checks `isSubvolume(dst)` even
  though the caller already established it on 109 — redundant btrfs invocation.

### 4. Unclear / unexplained code
- `Move` 103-106: the `!isSubvolume(src)` → `return Ensure(...)` branch — *why* a "move"
  creates a brand-new empty subvolume (the ref had no prior identity state) is in the godoc
  but not at the code site. Worth an inline note.
- `isSubvolume` (27-29): silently treats *any* error (btrfs not installed, permission denied,
  not-exist) as "not a subvolume". This conflation is subtle and affects all callers'
  correctness; a clarifying comment would help.
- `Move` 119-123 comment about cross-parent rename keeping the nested subvolume intact is
  good — keep it.

### 5. Godoc accessibility
- In good shape: `IDDir`, `Path`, `Ensure`, `Move`, `Remove` all have proper godoc; package
  doc is thorough. No exported items should be unexported.

---

## snaphash/  [DONE]

### 1. Edge cases for unit tests
- **The prepended/padding bits are never validated on decode.** `Decode` ignores bit 0
  ("prepended zero", 122-125) and the trailing padding bit (257). Many distinct 43-char
  strings decode to the same hash, so `Decode(Encode(h))` is non-injective. No test asserts
  whether a crafted string with bit-0 = 1 is accepted or rejected (currently silently
  accepted). **Biggest gap** — pin this behavior down.
- No `Encode→Decode→Encode` canonicalization test documenting the non-injective behavior.
- `TestDecodeValidButCrafted` (277-293) decodes crafted strings but never checks the
  resulting hash nor that re-encoding differs.
- `UnmarshalText` direct call (not via JSON) on invalid input is untested.
- `ParseHash` error path untested (only happy path).
- `FromBytes` with `len(b)==0` and `len(b)==33` (boundary just above 32) untested; only
  len-3 is tested for the panic.

### 2. Duplicated code
- Bit-addressing math `byteIdx := hashBit/8; bitInByte := 7-(hashBit%8)` appears in both
  `Encode` (78-79) and `Decode` (134-135). Parallel logic where a bug in one and not the
  other would be easy to miss.

### 3. Overly complicated / redundant
- `Encode` (63-87) reimplements base64url by hand with a nested per-bit loop, even though
  the package already defines the standard base64url alphabet (line 50). Could likely use
  `encoding/base64` RawURLEncoding over a shifted 33-byte buffer. The hand-rolled bit loop
  is the most convoluted code in the small modules; strong refactor candidate.
  - RESOLUTION: the stdlib approach is NOT viable — the format packs exactly 258 bits
    (43*6), which is not a whole number of bytes. RawURLEncoding over 33 bytes (264 bits)
    yields 44 chars, changing the on-disk snap-ID wire format. Kept the bit loop but
    factored the shared bit-addressing (getHashBit/setHashBit/encBitToHashBit) and the
    char alphabet/decode into helpers so Encode and Decode can no longer drift.
- The inner three-way special-casing in `Encode` (`if bitPos==0 continue`, `if hashBit<256`,
  implicit ≥257-contributes-0) would be eliminated by the stdlib approach.
- `Decode` (128-138) likewise hand-rolls bit extraction.

### 4. Unclear / unexplained code
- The 1-bit shift rationale *is* well documented (package doc + inline). Good.
- **Not explained:** that `Decode` deliberately does **not** verify the prepended bit is 0
  nor the final padding bit is 0, and that this makes decode non-injective. A reader can't
  tell if this is intentional leniency or a bug. Prime candidate for a comment.
- Line 122 comment ("43 chars * 6 bits = 258 bits" vs "Bits 257 is padding") is grammatically
  off and easy to misread against the 257-bit value used in `Encode`. Clarify.

### 5. Godoc accessibility
- All exported identifiers have proper godoc.
- **API smell:** `ParseHash` (185) and `Decode` (93) are functionally identical (`ParseHash`
  just calls `Decode`). Two public names for the same op — flag for possible consolidation.

---

## frames/  [DONE]

### 1. Edge cases for unit tests
- `Create` with zero/nil UUID (`frames.go:102-104`) — the "cannot create frame with nil
  UUID" guard is never exercised.
- Malformed/invalid JSONC on disk (`Get`, 139-147) — `hujson.Standardize` / `json.Unmarshal`
  error paths untested.
- `List` robustness (217-242) — no test that directories are skipped (`e.IsDir()`),
  non-`.jsonc` files ignored, or a `.jsonc` whose stem is an unparseable UUID is skipped
  (236-238). Interaction with the frame's own `fs/<uuid>/` dir untested.
- `AddHistoryEntry` / `AddTaint` on nonexistent frame (propagate `ErrFrameNotFound`) untested.
- `AddTaint` sort ordering when `frame.Taints` is already unsorted — untested (it re-sorts
  the whole slice each call).
- `Update` does not preserve `CreatedAt` — a caller constructing a fresh `Frame` would zero
  it; no test documents this.
- **Concurrency / atomicity:** `write` (257-269) uses non-atomic `os.WriteFile` (no temp+
  rename); read-modify-write methods (`AddHistoryEntry`, `AddTaint`) are racy. No concurrent
  test. (Highest-value untested area; shared with refs.)
- `UnionTaints` with duplicate/empty-string entries within a single set untested.
- Permission-error on an unreadable meta file → should return the wrapped read error, not
  `ErrFrameNotFound`; untested.

### 2. Duplicated code
- `os.Stat` existence check re-implemented in `Create` (114), `Update` (156), `Exists` (247).
- `os.IsNotExist` → `ErrFrameNotFound` mapping repeated in `Get`, `Update`, `Delete`.

### 3. Overly complicated / redundant
- `List` magic number: `uuidStr := name[:len(name)-6]` (235) hardcodes `len(".jsonc")`;
  `strings.TrimSuffix` would be clearer. (Same magic 6 appears in refs.)
- `Update` (156-161) stats purely to map error to `ErrFrameNotFound`; TOCTOU vs the
  subsequent `write`. The stat is advisory.

### 4. Unclear / unexplained code
- `AddHistoryEntry` (191): `append([]HistoryEntry{entry}, frame.History...)` — the
  newest-first prepend idiom is non-obvious and allocates each call. Comment it.
- `Frame.Home` / `Frame.Work` (49,54): "zero hash means empty subvolume" is documented (good),
  but the interaction with `omitempty` (a zero hash may be omitted from JSON) is not.
- `List` skipping: the reason directories are skipped (the frame's own `fs/<uuid>/` subvolume
  sits next to `fs/<uuid>.jsonc`) is not stated.

### 5. Godoc accessibility
- All exported identifiers documented. No missing godoc, nothing obviously over-exported.

---

## refs/  [DONE]

### 1. Edge cases for unit tests
Coverage is fairly strong already. Gaps:
- **Trailing dot/dash/underscore**: `ValidateName` regex `^[a-zA-Z0-9][a-zA-Z0-9._-]*$`
  *allows* `"foo."`, but the package doc (refs.go:39) claims "no trailing dots". Doc-vs-code
  mismatch worth a test.
- Malformed JSONC in `Get` (156-164) — error paths untested (same as frames).
- `Move`/`SetAutorun` with an invalid name should return `ErrInvalidRefName`, not
  `ErrRefNotFound`; untested.
- `List` filtering (215-235) — skips dirs / ignores non-`.jsonc`; untested.
- `IDDirExists` / `EnsureIDDir` / `RemoveIDDir` with invalid names (248,265,273) untested;
  `RemoveIDDir` on a nonexistent dir (should be nil) untested.
- `Delete` does not remove the id dir → `id/<name>/` orphaned. Latent leak; no test documents
  whether intentional.
  - RESOLUTION: intentional. The daemon's handleRefDelete refuses to delete a ref with a
    non-empty id dir unless --force; the id dir is identity state owned by refid, not the
    ref-config store, so refs.Delete deliberately leaves it. Left as-is.
- Symlink edge case: if `id/<name>` is a symlink, `RemoveIDDir`'s `RemoveAll` vs
  `IDDirExists`'s `ReadDir` differ; untested.
- **Concurrency:** `TestConcurrentMoves` runs sequentially by design; same non-atomic `write`
  (284-296) race in `Move`/`SetAutorun` as frames. No true concurrency test.

### 2. Duplicated code
- `ValidateName` called identically at the top of `Create`, `Get`, `Delete`, `Exists`,
  `IDDirExists`, `EnsureIDDir`, `RemoveIDDir` (7 sites). Defensible guard pattern but could
  be centralized in the path helpers.
- **`regexp.MustCompile(`\.\.`)` is recompiled on every `ValidateName` call** (104) instead
  of being a package-level var like `validRefName`. Duplication-of-intent + perf smell.
- `os.IsNotExist` → not-found mapping repeated in `Get`, `Delete`, `IDDirExists`.

### 3. Overly complicated / redundant
- `ValidateName` recompiles the `..` regex per call (see above).
- `List` magic `6` (231): same `name[:len(name)-6]` brittleness as frames.
- **Dead code:** `fsDir` helper (88-90) computes `<state-dir>/fs/<uuid>` but is **never used**
  anywhere in the package. Remove, or it signals a missing feature.

### 4. Unclear / unexplained code
- `Move` (178): newest-first `Reflog` prepend idiom — comment it (same as frames).
- `ValidateName` consecutive-dot check (103-106): the comment is good, but it's not explained
  *why* the regex alone is insufficient (the regex permits `.`, so `..` would otherwise pass).
- Package doc (39) says "no trailing dots" but code does not block them — reconcile.
- `IDDirExists` returning false for an *empty* dir (260, `len(entries) > 0`) is an intentional
  "exists and is non-empty" semantic; the rationale could use a one-liner at the call site.

### 5. Godoc accessibility
- All exported identifiers documented.
- Minor inconsistency: `ReflogEntry`'s fields `UUID`/`Time` (44-45) lack per-field godoc,
  unlike `Ref`'s fields.
- `fsDir` (88) already unexported but is dead code (see §3).

---

## metrics/  [DONE]

RESOLUTION: Added metrics/metrics_test.go covering CountSnaps (missing/empty
dir, .tsc/.txt noise ignored, and the documented foo.tsm/ directory match),
CountFrames (missing/empty fsDir, bare dir without .jsonc, .jsonc whose <name>
is a file, loose non-dir at user level), CountRefs (nil store, empty, populated),
the collector via a registry with nil RunningSessions/RunningVMs closures
yielding 0 and with closures read on scrape, NewRegistry double-registration
error, and NewHandler. Added one-line comments to CountSnaps/CountFrames/CountRefs
documenting the deliberate error->0 swallowing (a scrape never fails).

### 1. Edge cases for unit tests
- **No unit tests at all** in `metrics/` (only e2e, root+btrfs), yet `CountSnaps`,
  `CountFrames`, `CountRefs` are pure filesystem logic trivially testable with `t.TempDir()`:
  - `CountSnaps` (23-35): non-existent dir, empty dir, non-`.tsm` files, and a directory
    entry named `foo.tsm/` (currently counted because it checks name suffix, not `IsDir()`).
  - `CountFrames` (40-64): empty/non-existent `fsDir`; `<name>.jsonc` exists but `<name>/` is
    a file not dir (skipped — assert it); bare dir without `.jsonc` not counted; unreadable
    user dir (51 `continue`).
  - `CountRefs` (67-76): `nil` store (68) and `List()` error (72) both collapse to 0 silently;
    untested.
  - `Collect`/`NewRegistry`/`NewHandler`: nil `RunningSessions`/`RunningVMs` closures →
    zero (142-148); double-registration error (160-168). Testable via `prometheus.Registry`
    + `testutil`.

### 2. Duplicated code
- Three `os.ReadDir(...) → if err return 0` preambles (24-27, 41-44, 50-52) — short, arguably
  clearer inline.
- Five `NewDesc` blocks (104-128) + five `MustNewConstMetric` lines (149-153) are repetitive
  but idiomatic for Prometheus collectors; not worth abstracting.

### 3. Overly complicated / redundant
- None significant. Silently returning `0` on every error (25,42,51,73) is a deliberate
  simplification — a permissions error is indistinguishable from "0". Design smell, not
  redundancy. No dead code.

### 4. Unclear / unexplained code
- Well-commented overall. The collector comment (89-90) explaining why a custom `Collector`
  (no shared mutable gauge state) is used is good.
- Worth a one-liner: *why* errors are swallowed to `0` everywhere ("a missing/unreadable dir
  is reported as zero so a scrape never fails").

### 5. Godoc accessibility
- All exported identifiers documented (`CountSnaps`, `CountFrames`, `CountRefs`, `Sources`,
  `NewRegistry`, `NewHandler`, package doc).
- `Sources` fields lack per-field godoc but the struct doc covers them collectively
  (acceptable). No exported item should be unexported.

---

## snapsubdir/  [DONE]

RESOLUTION:
- Added snapsubdir/snapsubdir_test.go with table-driven Validate tests covering
  "", "/", ".", "..", "../x", "a/../..", "/..", "keep", "/keep", "keep/",
  "a/b/c", "a/../b", "a//b", "./keep". These confirm the previously-suspected
  dead branch (clean == ".." || HasPrefix(clean, "../")) was unreachable:
  escaping inputs collapse against the anchored "/" to the root and hit the
  root-error branch. Removed that dead branch and documented why no traversal
  check is needed.
- §2 dedup: extracted btrfsCmd(args...) wrapping CombinedOutput + error (now
  used by snapshot, subvolume delete, and property set ro), and removeChildren
  to collapse the two near-identical ReadDir+recurse blocks in
  removePathRecursive.
- §4 comments: noted the srcSub->promote rename is a same-subvolume in-fs move,
  and that a stale promote dir can only exist if the source frame contained one.
- The btrfs-level Snapshot/removePathRecursive paths remain covered by
  e2e/snap_subdir_test.go (full e2e suite passes).

### 1. Edge cases for unit tests
- **`Validate` is pure string logic, needs no btrfs, and has zero unit tests** despite being
  exported. Table-driven cases: `""`, `"/"`, `"."`, `".."`, `"../x"`, `"a/../.."`, `"/keep"`,
  `"keep/"`, `"a/../b"` (→ `"b"`), `"a//b"`. (See §3 about a possibly-unreachable branch.)
- `Snapshot` error paths (btrfs-level): subdir not found (104-107); subdir is a file (108-110);
  subdir is itself a subvolume; subdir named `.ts-subdir-promote`; stale promote dir cleared
  (115); **nested subvolumes among pruned siblings** (the whole reason `removePathRecursive`
  exists — untested); `dstPath` already existing.
- `removePathRecursive` (44-85): non-existent path; plain file; empty dir; subvol-inside-dir;
  subvol-inside-subvol. None unit-tested.

### 2. Duplicated code
- `removePathRecursive` has two nearly identical `ReadDir` + range + recurse blocks: the
  subvolume branch (56-64) and the plain-dir branch (73-81), differing only in the final
  action (`btrfs subvolume delete` vs `os.Remove`). Clearest in-module duplication.
- `exec.Command("btrfs", ...).CombinedOutput()` + `fmt.Errorf("...: %w\n%s", err, out)` recurs
  at 65-68, 99-101, 153-155.

### 3. Overly complicated / redundant
- **Likely dead branch:** in `Validate`, after `filepath.Clean("/"+subdir)` + `TrimPrefix`,
  the result can never retain a leading `..` (Clean collapses `..` against the anchored `/`),
  so `clean == ".." || HasPrefix(clean, "../")` (30) is probably unreachable — escaping inputs
  resolve to `""` and hit the root-error branch (27-29) instead. Verify with a unit test; if
  confirmed, it's redundant defensive code.

### 4. Unclear / unexplained code
- The `filepath.Clean("/"+subdir)` anchor trick (25) is well commented. Good.
- `os.Rename(srcSub, promote)` (118) relies on source and dst being on the **same btrfs fs**
  (a snapshot of itself) — that's what makes the rename cheap/atomic. Worth a one-line note.
- The defensive `os.RemoveAll(promote)` (115) isn't explained — why would a fresh snapshot
  already contain `.ts-subdir-promote`? (Only if the source frame had such a dir.)

### 5. Godoc accessibility
- All exported (`Validate`, `Snapshot`, package doc) documented; unexported helpers documented
  too. No exported item should be unexported.

---

## tsm/  (largest, most important module)  [DONE]

RESOLUTION (committed across several jj changes):
- **Dead code deleted:** `checkURLExistsRaw` (peers.go), `stripPathPrefix` (indexer.go) —
  both verified unused (tree still compiles), removed along with their now-unused imports.
- **download.go honesty fixes:** the empty `CharDev/BlockDev` branch in
  `createNonFileEntries` is now documented as an intentional no-op (mknod needs CAP_MKNOD,
  downloads are not assumed root); the silent `os.Chmod`/`os.Lchown` error-swallowing
  `if err := ...{}` blocks became explicit `_ = os.Chmod(...)` / `_ = os.Lchown(...)`; the
  path-length sort assumption and the file-level (not package) comment were clarified.
- **Redundant exported field removed:** `IndexerOptions.Progress` is gone; progress is now
  gated solely on `ProgressWriter != nil` (callers in cmd/thundersnapd, cmd/tsm, and the
  progress tests updated).
- **Unexported internal chunking primitives:** `rollsum`+`reset`/`add`/`roll`/`digest` and
  `findSplitPoint` are now lowercase with godoc (only used in-package). The all-ones split
  test and the bounded `uint16(level)` cast are now explained.
- **Godoc / stale-comment fixes (types.go):** constant blocks converted to godoc form; stale
  `\x02` magic/version comments corrected to `\x03`; `EntryType` + `String()` documented; the
  reserved TSM/TSC flag bits annotated (kept, since they are on-disk-format placeholders) with
  the type/flags nibble-packing explained; `BlobSHA256`/`ZeroBlockSHA` git-blob framing noted.
- **New tests:** `tsm_corrupt_test.go` — corrupt/truncated `ParseTSM`/`ParseTSC` cases (short
  buffer, bad magic, checksum mismatch, count mismatch, truncated entries) plus
  `TestChunkDataReaderParity` asserting `ChunkData` and `ChunkReader` produce identical chunk
  boundaries/hashes across sizes spanning buffer boundaries.

DELIBERATELY NOT CHANGED (with rationale):
- **`ChunkData` vs `ChunkReader` NOT collapsed.** The parity test proves they agree today; both
  feed the on-disk chunk hashes, so merging them risks a silent wire-format change for no
  functional gain. Kept as two impls guarded by the parity test against drift.
- **Exported reserved flag constants kept** (annotated, not deleted): they pin bit positions in
  the on-disk format even though the current writer never sets them.
- **`uint16` path-length / `uint32` chunk-count casts:** documented as practically bounded
  rather than adding guards/errors for inputs (>64KB paths, >4G chunks/file) that cannot arise
  from a real filesystem snapshot; revisit only if a concrete overflow path appears.
- `isConnRefused` string-match, the hardlink-map key truncation, and the two-pass `Index`
  re-stat are left as-is (pre-existing behavior, no observed bug); noted here for future work.

### 1. Edge cases for unit tests
**Corrupt / truncated parsing (highest value — pure functions, easy to test, and this parses
untrusted downloaded data):**
- `ParseTSM` (tsm.go:309) / `ParseTSC` (tsc.go:182): no corrupt-input tests. Untested branches:
  bad magic (315/188), file shorter than header+footer (310/183), **TSM checksum mismatch**
  (388), **TSC checksum mismatch** (200), `invalid entry length <58` (343), `entry extends
  beyond file` (346), `chunk ref table size not multiple of 4` (361), chunk refs out of range
  (399-401), `chunk count mismatch` (tsc 218), truncated symlink/hardlink/device data
  (478-497), and a corrupt huge `FileCount` (loop at 337).

**Filename / content edge cases:**
- Unicode / spaces / newline-containing paths (paths are length-prefixed, so should survive —
  round-trip test).
- **Path length > 65535:** `encodeEntry` (tsm.go:108) casts pathLen to `uint16`; a >64KB path
  silently truncates and corrupts the file. Same for symlink target length (160) and total
  entry length (104). No test and **no guard**.
- Empty-path collision / `LookupPath("")` returning root (lightly tested).

**Symlinks / special files:**
- Symlink whose target changed (never reused, always re-read; untested).
- Real unix socket (`EntryTypeSocket`) from a live FS — only the type byte is round-tripped.
- Block-device detection from a live FS (indexer.go:251) untested.

**Permission bits / metadata:**
- setuid/setgid/sticky bits survive index→TSM→download? (download.go:282 masks `Mode&0xFFF`.)
  Security-relevant, untested.
- Permission-denied during walk (silently skipped: indexer.go:91,134,142) untested.

**Incremental reuse correctness (subtle, under-tested):**
- Same path+size+mtime but content changed → stale chunks reused (indexer.go:326-330). MEMORY
  claims nanosecond mtime makes this safe; no test guards the boundary.
- Parent index out-of-range bail-out (334-336) untested.
- Parent entry type changed (file↔symlink↔hardlink) — `parent.Type != EntryTypeFile` guard
  (326) untested.

**Hardlinks:**
- Hardlink recorded in the `hardlinks` map (219) before the entry is confirmed added; if the
  "first" file is then skipped (permission error), `LinkIndex` could point at the wrong entry.
- Hardlink key `Dev<<32 | Ino` (211) truncates `Dev`/`Ino` > 32 bits → possible collision.

**Chunk boundaries / large files:**
- Multi-GB file: `ChunkStart`/`ChunkCount` are `uint32` (types.go:115-116); chunk-count cast
  in processEntry (236) is unguarded.
- **`ChunkData` vs `ChunkReader` parity:** no test asserts identical chunk boundaries between
  the two independent implementations — they can diverge at buffer boundaries. Real gap.
- Zero-block flag: `TSCEntryFlagZeroBlock` set on a BLOB_MAX run of zeros and reconstructed
  as a hole (indexer 287, download 366/416) untested.

**Concurrency:**
- `CheckPeersForSnapshot` (peers.go:29) writes a shared slice from goroutines (distinct
  indices, so safe) — no `-race` test documents the invariant.

### 2. Duplicated code
- **`ChunkData` (chunking.go:180) vs `ChunkReader` (chunking.go:85):** near-duplicate
  content-defined-chunking algorithm; `ChunkData` is essentially the in-memory version of the
  streaming loop and can subtly diverge. Strong candidate to have `ChunkData` delegate to
  `ChunkReader(bytes.NewReader(data), ...)`.
- **TSM header/body encoding written 3×:** `TSMWriter.Write` (224-260), `ParseTSM` (327-385,
  which re-encodes entries identically to recompute the checksum). A shared
  `writeHashableBody(w)` would remove the copy and the drift risk. Same symmetry for TSC
  (`TSCWriter.Write` 132-136 vs `ParseTSC` 227-230).
- **passwd/shadow/group scanning in stripuids.go:** `EnsureUserInPasswd` (51-61),
  `ensureUserInShadow` (298-309), `ensureUserInGroup` (139-157), `LookupUser` (265-278) each
  independently build a `bufio.Scanner` with the same buffer sizes, split on `:`, check
  `fields[0]`. The "insert after the root/UID-0 line, else append" logic is duplicated between
  `EnsureUserInPasswd` (83-100) and `ensureUserInShadow` (324-341). Candidate `insertAfterRoot`.
- `checkURLExists` (peers.go:57) vs `checkURLExistsRaw` (peers.go:92) — two impls of the same
  "URL returns 200" check (the Raw one is dead, see §3).

### 3. Overly complicated / redundant / dead code
- **Dead: `checkURLExistsRaw` (peers.go:90-156)** — 65-line raw-TCP HTTP client never called;
  duplicates `checkURLExists`; only reads one 1024-byte chunk (could miss the status line).
  Delete candidate.
- **Dead: `stripPathPrefix` (indexer.go:411-421)** — defined, never used (indexer uses
  `filepath.Rel` inline at 124).
- **Dead/incomplete: device branch in `createNonFileEntries` (download.go:482-485)** —
  `CharDev/BlockDev` case is an empty body with a comment claiming the mknod is "in a helper"
  that doesn't exist here; device nodes are silently dropped on download. Latent bug or dead
  intent — confusing.
- **Silent error swallowing (download.go:282-289):** `os.Chmod`/`os.Lchown` errors captured
  into `if err := ...; err != nil { // Non-fatal }` with empty bodies (vet-flaggable). Should
  be `_ = os.Chmod(...)`.
- `ChunkReader` leftover handling (chunking.go:90-166) is convoluted: `if ofs==0 &&
  chunkSize==0 { break }` (150) + per-iteration `leftover = make(...)` realloc + the
  `remaining`/`io.EOF`/`remaining>=BLOB_MAX` interplay (120-131). Needs the parity test.
- `Indexer.Index` two-pass (indexer.go:88-152): `filepath.Walk` already provides `info` and
  visits in lexical order, but the code discards `info`, collects paths, `sort.Strings`, then
  re-`Lstat`s every path (132). The re-stat is redundant work; the sort is only partly so
  (Walk is per-directory lexical, not global). Comment or simplify.
- `level` cast: computed as `int` (chunking.go:116) then cast to `uint16` (138); `Level` is
  `uint16` (types.go:163). Practically bounded; the cast is unexplained.
- **Redundant flag bits:** `TSMFlagHasExtendedAttrs = 1<<0` and `TSMFlagHasXattrs = 1<<2`
  (types.go:98,100) are the same concept with two bits; `TSMEntryFlagHasXattr = 1<<4` (132) is
  a third. Looks like copy-paste; confusing.

### 4. Unclear / unexplained code
- `types.go:14-45` chunking constants: `ROLLSUM_CHAR_OFFSET=31`, `FANOUT_BITS=4`, and
  `BUP_WINDOWSIZE = 1 << (BUP_WINDOWBITS-1)` (note the `-1` → 64 not 128, easy to misread)
  deserve a note on why the `-1`.
- `FindSplitPoint` (chunking.go:49): `(r.s2 & (BUP_BLOBSIZE-1)) == ((^uint32(0)) &
  (BUP_BLOBSIZE-1))` reads as line noise — *why* all-ones is the target, and the level-
  counting loop (54-57), are the heart of format determinism and the least commented.
- `init()` / `BlobSHA256` (types.go:181-187): the git-blob framing (`"blob %d\x00"`) is
  mentioned but not *why* (bup/git object-store compatibility?).
- `typeFlags := uint16(entry.Type) | entry.Flags` (tsm.go:116) and the decode masks
  (`Type = typeFlags & 0x0F`, `Flags = typeFlags & 0xFFF0`, 435-436): packing a 4-bit type
  into the low nibble of a 16-bit flags field is undocumented (and silently caps EntryType at
  15, which is why `TSMEntryFlagHasXattr` starts at `1<<4`).
- Chunk-ref-table build in `Write` (tsm.go:194-208): the `if indexMap != nil && int(origIdx) <
  len(indexMap)` fallback to the un-remapped `origIdx` (201-205) is unexplained — when would
  `origIdx >= len(indexMap)`? Possibly writes a wrong index silently.
- `download.go:455-460` sorts entries by `len(Path)` to "create parents before children" —
  path-length is not a correct topological order in general; it works only because a parent is
  always shorter than its *own* children. The comment undersells the assumption.
- `isConnRefused` (peers.go:80-88) matches on the error-string substring — fragile/locale-
  dependent; why not `errors.Is(err, syscall.ECONNREFUSED)`?

### 5. Godoc accessibility
- Missing godoc: `EntryType.String()` (types.go:61); the eight `EntryType` constants (51-58,
  no per-const or block godoc); `Rollsum` methods `Init`/`Roll`/`Digest` (chunking.go:16,30,36)
  — exported, no godoc; `IndexerStats` fields; `TSMHeader`/`TSCHeader` `Magic`-comment is
  **stale** (says `"TSM\x02"` but the const is `\x03`, types.go:22 — also on version comments).
- Doc-form violations (comment exists but doesn't start with the identifier name, so the
  godoc/`go doc` association is weak): `TSMVersion`/`TSCVersion` (15,18), `TSMMagic`/`TSCMagic`
  (21,24), the header-size constants (27-34), the BUP_* constants (36-44), and the
  `TSMFlags`/`TSMEntryFlags`/`TSCFlags`/`TSCEntryFlags` group comments.
- Should probably be **unexported**: `Rollsum` + `Init`/`Roll`/`Digest`, `FindSplitPoint`
  (internal chunking primitives, only used in-package); `stripPathPrefix` and
  `checkURLExistsRaw` (unexported already but **unused** — delete).
- `IndexerOptions.Progress` is redundant with `ProgressWriter != nil` (both gate every
  progress call at 348/360) — not a godoc issue but a redundant exported field.
- `download.go:1-2` has a second package-level comment block describing the *file*, not the
  package — malformed as a package doc.

### Highest-value items (tsm)
1. Corrupt/truncated `ParseTSM`/`ParseTSC` tests (large untested surface; parses untrusted data).
2. `ChunkData` vs `ChunkReader` parity test, then collapse the duplication.
3. Delete dead code: `checkURLExistsRaw`, `stripPathPrefix`; resolve the empty device-node
   branch in `createNonFileEntries`.
4. Guard the `uint16` path-length / `uint32` chunk-count casts, or document the limits.
5. Fix stale `\x02` magic comments; convert constant block comments to godoc form.

---

## thundersnap/  (container.go, vm.go)  [DONE]

RESOLUTION (committed):
- **First unit tests for the package** (`vm_test.go`, `container_test.go`):
  - `monitorEvents`: guest-panic closes `panicked` and returns; clean EOF returns without
    closing it; malformed JSON returns without panicking.
  - `vsockResponseWriter.finish` (against `net.Pipe`): default status 0→200, custom
    status+headers, multi-`Write` body accumulation, Content-Length, and empty body.
  - `Wait()` returns nil once `done` closes.
  - `buildNsenterCmd` arg-building (incl. tsBinary default + override); `ReleaseContainerNs`
    on an unknown rootFS is a no-op; refcount lifecycle (release twice → shutdown+delete),
    using a real `cat` child that exits on stdin EOF like the real init.
- **Dedup:** `RunInContainerNs`/`StartInContainerNs` now share one `buildNsenterCmd` helper
  (single source of truth for the `nsenter ... ts drop-caps-and-run` invocation). The five
  copy-pasted VM teardown ladders in `StartVM` collapsed into one `cleanup()` closure, and the
  two identical socket-wait loops into `waitForSocket(path, attempts, delay)`.
- **`finish` no longer issues a zero-length body write** (a no-op on the wire that also
  dead-locked an unbuffered pipe with no body reader).
- **`handleVsockConnection`** rewritten from a `for { ... return }` (dead loop) to a clear
  single-request handler with a comment that there is intentionally no keep-alive.
- **Docs:** PID-reuse caveat on the `Kill(pid,0)` liveness probe; `sessionID` format intent;
  `eventMonitorFd`/`ExtraFiles[0]` coupling; guest-CID-3-vs-host-CID-2 note; `Wait()` always-nil
  return; `monitorEvents` single-decode-error-stops-monitoring robustness note.

DELIBERATELY NOT CHANGED (with rationale):
- **`RunInContainerNs`/`StartInContainerNs` kept (NOT deleted).** The review called them dead,
  but `e2e/container_test.go` (`TestContainerSharedPIDNamespace`) drives both through the real
  production path. They are not the daemon's session path, but they are a live, tested API; the
  `--skip-mount-setup` divergence is not a bug for their single-shot/long-running use (they do
  not stack per-session devpts the way concurrent SSH sessions did). Left as-is, deduped.
- `SetControlHandler` kept — it has a real consumer (cmd/thundersnapd/main.go).
- PID-reuse hardening (start-time/cmdline verification) and SIGTERM/orphan handling for
  virtiofsd/passt/cloud-hypervisor are documented as known gaps but not implemented (no observed
  failure; out of scope for a cleanup pass).

### 1. Edge cases for unit tests
- **Refcount lifecycle / concurrency** (`ContainerNsManager`): concurrent get/release races,
  double-release, release of unknown `rootFS` (container.go:159) — all untested. Can be tested
  with a fake long-lived init process (e.g. `sleep`).
- **PID-reuse hazard:** `GetOrCreateContainerNs` (55) uses `syscall.Kill(initPid, 0)` as a
  liveness probe. If the original init died and the PID was recycled, the probe succeeds and a
  foreign PID is "reused". Untested; arguably a real bug (no start-time/cmdline verification).
- `refCount` underflow when release is called more than get (167) — unguarded, untested.
- READY-handshake failures (110-139): read error, non-"READY" prefix, 10s timeout — none
  tested.
- Partial-init cleanup: `StdinPipe`/`StdoutPipe`/`Start` failure paths (88-105) — verify no
  fd/process leak.
- **`monitorEvents` (vm.go:314) is pure and trivially unit-testable** with an `io.Reader`:
  panic-event JSON closes the channel; EOF returns cleanly; malformed JSON logs+returns. Best
  unit-test target in the file. Note a robustness bug: one decode error permanently stops
  panic monitoring (322-324).
- **`vsockResponseWriter`/`finish` (vm.go:427-482) is pure and unit-testable** against
  `net.Pipe()`: default status 0→200 (449), header serialization, content-length, multiple
  `Write` calls accumulating, `WriteHeader` never called, empty body.
- `handleVsockConnection` (390): single-shot loop (always returns after one request) and the
  EOF-vs-error branch (398-401) untestable-claim worth pinning.
- `VMSession.Close` (335) double-close / close-after-self-exit: derefs `chvCmd.Process.Pid`
  with no nil guard; untested.
- Signal handling: no SIGTERM/SIGINT handling for spawned `virtiofsd`/`passt`/
  `cloud-hypervisor`; orphan behavior unspecified/untested.

### 2. Duplicated code
- **`RunInContainerNs` (container.go:185-225) vs `StartInContainerNs` (232-274):** near
  identical — both `GetOrCreateContainerNs`, compute `absRootFS`, build the identical
  `ts drop-caps-and-run --chroot=... --` slice + identical `nsenter -t <pid> -p -m -u --`
  slice + `exec.Command("nsenter", ...)`, `cmd.Dir="/"`. Only differ by `CombinedOutput()` vs
  `Start()`. Shared arg-building → one `buildNsenterCmd` helper.
- **VM teardown ladders in `StartVM` copy-pasted 5×** (vm.go:92-97, 115-120, 129-137, 180-189,
  230-241), growing incrementally — `passt.Kill/Wait`, `virtiofsd.Kill/Wait`,
  `os.Remove(sock)`. Prime source of latent process/socket leaks; one `cleanup()` closure (or
  deferred-with-success-flag) would eliminate it. Inconsistency: the `pty.Start` failure path
  (232) re-does teardown manually whereas the vsock-listen failure path (277) calls
  `session.Close()`.
- Socket-wait polling loops duplicated verbatim: virtiofsd (86-97) and passt (123-137) — same
  `for i:=0;i<50` / `Stat` / 100ms sleep. Candidate `waitForSocket(path, attempts, delay)`.

### 3. Overly complicated / redundant / dead code
- **`RunInContainerNs`/`StartInContainerNs` appear dead AND subtly wrong.** The daemon's real
  container path (`cmd/thundersnapd/main.go` `runContainerSession`) re-implements the same
  `nsenter ... ts drop-caps-and-run` spawn inline rather than calling these. So the spawn logic
  lives in **two places** that can drift — and critically, these package helpers do **not**
  pass `--skip-mount-setup` (the fix for bug #11), so they would re-run `setupDev()` and stack
  a fresh devpts per call. Either the daemon should use them, or they should be removed.
- `handleVsockConnection` loop (vm.go:395-424) is `for { ... return }` — every branch returns
  after one request (HTTP/1.0 style); the `for` wrapper is dead control flow.
- `Wait()` always returns `nil` (vm.go:290-293) — the `error` return is vestigial; `Done()`
  already exposes the channel. Redundant API surface.

### 4. Unclear / unexplained code
- `sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())` (vm.go:68) — PID
  concatenated with nanos, no separator (in principle collidable). Explain the intent.
- `const eventMonitorFd = 3` + `ExtraFiles[0]` (vm.go:194-196): the coupling between
  `eventMonitorFd=3` and `ExtraFiles` having exactly one element is implicit/fragile.
- vsock CID mix: `cid=3` here (210-212) vs comment saying "CID 2 (host)" (268) — guest-CID vs
  host-CID is confusing without a note.
- pipe-end ownership (vm.go:232-233 error path, 245, 251) is split across three places; one
  ownership note would help.
- `GetOrCreateContainerNs` `Kill(pid,0)` (container.go:55) deserves a comment (signal 0 =
  existence check) AND the PID-reuse caveat.
- The giant `cmdline` format string (vm.go:177) nests three levels of shell quoting; the *why*
  (sh as init) is commented but not the quoting structure.

### 5. Godoc accessibility
- Exported API is well-documented (`ContainerNsManager`, `NewContainerNsManager`,
  `GetOrCreateContainerNs`, `ReleaseContainerNs`, `RunInContainerNs`, `StartInContainerNs`,
  `VMConfig`+fields, `VsockPort`, `VshPort`, `VMSession`, `SetControlHandler`, `StartVM`,
  `Wait`, `Done`, `Panicked`, `Close`, `VshSocketPath`).
- Should likely be **unexported** (no external callers / dead): `RunInContainerNs`,
  `StartInContainerNs` (see §3); verify `SetControlHandler` has a consumer.

### Highest-value items (thundersnap)
1. Zero unit tests despite `monitorEvents`, `vsockResponseWriter.finish`, and the refcount
   logic being readily testable.
2. `RunInContainerNs`/`StartInContainerNs` are effectively dead and **divergent** from the
   daemon's corrected spawn path (missing `--skip-mount-setup`).
3. VM teardown ladders copy-pasted 5× — consolidate into one cleanup helper.
4. PID-reuse hazard in the `Kill(pid,0)` liveness check is real and undocumented.

---

## cmd/ts/  (in-container client + daemon-invoked helper modes)

### 1. Edge cases for unit tests
The existing tests (`snap_test.go`) only round-trip JSON structs and **re-implement** the
colon/frame-spec parsing inline — they never call the production code, so they can't catch a
regression. Untested pure, root-free functions:
- **`resolveSnapSubdir` (main.go:327)** — most testable pure function, entirely untested:
  relative→container-abs (strip leading `/`), `filepath.Clean` cases, root-rejection branch
  (347-349, input cleaning to `/`), non-existent path stat error, path-is-a-file (342),
  symlink-to-dir (it uses `os.Stat`, so follows links — pin this down).
- **`findExecutable` (main.go:2285)** — PATH lookup: name with `/`, empty PATH → hardcoded
  default (2296-2297), file present but not executable (2303) skipped, first-match-wins, not
  found.
- The `su` → `/bin/sh` fallback (1625-1648) — three branches (`su - root`→`/bin/sh -l`,
  `su root -c CMD`→`/bin/sh -c CMD`, unsupported-form error); currently inline + `os.Exit`, so
  untestable until extracted.
- Hand-rolled flag parsers in `cmdDropCapsAndRun` (1453-1491) and `cmdContainerInit`
  (1972-1989): `--flag value` vs `--flag=value`, `--` terminator, `--hostname` with no value
  (silently ignored), unknown flags.
- `applyWinsize` parse (1835): malformed `"WIDTH HEIGHT\n"`, negative/zero, extra whitespace.
- Subcommand dispatch (`main` 90, `cmdRef` 2471): unknown-command default — untestable while
  dispatch lives in `os.Exit`-calling functions; a dispatch map would fix that.
- `base == "sh" || base == "-sh"` shell detection (73) — extract to a bool helper to pin the
  `-sh` login-shell case.

### 2. Duplicated code
- **HTTP client construction duplicated ~19×** — the exact `&http.Client{Transport:
  &http.Transport{DialContext: ... dialThunder(ctx, sockPath)}}` block in `doPing`, `doSnap`,
  `doDeleteSnap`, `doListSnaps`, `doCreate`, `doDeleteFrame`, `doListFrames`, `doWhoHas`,
  `doTaint`, `doDownloadDocker`, `doDownloadSnap`, `doRefCreate`, `doRefMove`, `doRefDelete`,
  `doListRefs`, `doReflog`, `doLog`, `doAutorunSet`, `doAutorunStop`. One `newThunderClient`.
- **POST-JSON-and-decode** pattern (marshal → `client.Post` → decode → check `Status != "ok"`)
  repeated ~8× (`doDeleteSnap`, `doDeleteFrame`, `doTaint`, `doRefCreate`, `doRefMove`,
  `doRefDelete`, `doAutorunSet`, `doAutorunStop`); only the error-field name and types vary.
- **NDJSON streaming-progress loop duplicated 4× with dangerous divergence** (`doSnap`
  421-471, `doCreate` 723-784, `doDownloadDocker` 1166-1219, `doDownloadSnap` 1352-1422):
  - TTY truncation differs (`termWidth` vs `termWidth-5`+ellipsis).
  - Non-TTY printing differs (each line vs only the last).
  - **Result detection differs:** only `doCreate` handles `Type=="" && Status!=""`
    (non-streaming error); the other three would report "no result received from server" for
    the same server response. Real inconsistency / latent bug.
- TTY-detection + `termWidth` preamble copy-pasted in all four streaming functions.
- Colon/frame-spec split loop in `cmdWhoHas` (900-906) and `cmdDownloadSnap` (1241-1247)
  (+ twice in tests).
- getopt boilerplate (`opts.Parse(append([]string{"ts X"}, args...))` with the same comment)
  repeated ~10×.

### 3. Overly complicated / redundant / dead code
- **`cmdDropCapsAndRun` (1445-1664) is ~220 lines** mixing arg-parse, mount setup, chroot,
  devpts, PTY handshake, cap-dropping, PATH defaulting, the `su` fallback, and three exec
  strategies. Decompose.
- **`cmdContainerInit` (1969) and `cmdDropCapsAndRun` (1445) duplicate the entire namespace
  setup**: hand-rolled flag loop, `unix.Mount("","/",MS_REC|MS_PRIVATE)`, chroot+chdir,
  `MkdirAll`+mount proc/sys, `setupDev()`, sethostname/setdomainname. `--skip-mount-setup`
  exists precisely because the two are two halves of the same setup that drifted. A shared
  `setupContainerNamespace(opts)` would unify them.
- **Three exec strategies (1650-1663):** `runWithExternalPty`, `runWithPty`, bare
  `syscall.Exec`. The two PTY paths set up a controlling terminal but share no code; the
  relationship is non-obvious. Consolidate or add a dispatch comment.
- `doDownloadSnap` dead/contorted branch (1402-1407): an empty-bodied `if ... && !isTTY`
  followed by `else if` — just an obfuscated `if lastProgressMsg != "" && isTTY`.
- `runShell` arg parsing (2344-2362): only the **last** positional wins (others dropped); no
  `$0/$@`; flags `-i -l -e -x -v` accepted-and-ignored (so `sh -e` lies about errexit). The
  interactive REPL (2420) is line-at-a-time so multi-line constructs break — comment that it's
  a deliberately minimal REPL.
- `cmdContainerInit` stdin-drain goroutine (2061-2071) reads one byte at a time just to detect
  EOF — inefficient and unexplained.
- `inVM()` (165) stats `/dev/vsock` on every dial across ~19 call sites though the answer is
  process-static.

### 4. Unclear / unexplained code
- `runWithExternalPty` setsid+TIOCSCTTY+dup2 (1784-1801): explains *what* but not *why setsid
  must precede TIOCSCTTY* (needs session-leader with no controlling tty). `if pts.Fd() > 2`
  (1799) guards against closing a just-dup'd 0/1/2 — non-obvious.
- `setupDev` devpts `newinstance` + `/dev/ptmx` symlink (1928-1931): crux of the concurrent-
  PTS bug fix (#11), but only says "for the newinstance mount" — explain that `newinstance`
  gives each container its own pts numbering and why `ptmx` is a symlink not a device node.
- vsock bind-mount dance (1862-1878, 1941-1953): good comment exists, but add *why mknod can't
  recreate a misc device* (kernel-dynamic).
- `Ctty: 0` in `runWithPty` ForkExec (1719): explain why `Ctty` is the index into `Files` (0),
  not the fd number — common footgun.
- `cmdCheckIsolation` mountinfo parsing (2241-2276): field-index assumptions + `!foundRoot →
  private` fallback rely on the `/proc/self/mountinfo` format; note it's e2e-only diagnostic.
- `MS_STRICTATIME` + `size=65536k` for `/dev` (1884): the 64MB cap / STRICTATIME choice
  (Docker parity) unexplained.
- **Possible latent bug:** `os.MkdirAll("/dev/shm", 1777)` (1934) — decimal `1777` is mode
  `03561` octal, not sticky-`0777`. Masked because the next line mounts tmpfs with `mode=1777`
  (a correct *string*), but the literal should be `0o1777`.

### 5. Godoc accessibility
- `package main`, so exported types are not importable. The request/response structs are
  consistently documented. No exported `type` is missing a type-level godoc; package doc exists.
- **Stylistic flag:** ~all exported DTO types (`ControlRequest`, `SnapResponse`,
  `SnapStreamEvent`, `CreateRequest/Response`, `RefRequest/Response`, …) could be unexported —
  they're capitalized only so JSON reads naturally; tests share the package so unexporting
  wouldn't break them.
- `WhoHasResponse`/`WhoHasPeerInfo` (943,950) coexist with the imported `tsm.PeerResult`; the
  "for compatibility with existing code" comment (985) doesn't say what consumes which —
  clarify or unexport.

### Highest-value items (cmd/ts)
1. Extract a shared HTTP client + POST/decode helper (removes ~19 + ~8 copies).
2. Unify the 4 NDJSON streaming loops — they've already diverged (TTY truncation, non-TTY
   printing, and `doSnap` silently dropping the `Type=="" && Status!=""` error case).
3. Unify `cmdContainerInit` + `cmdDropCapsAndRun` namespace setup behind one helper.
4. Add unit tests for `resolveSnapSubdir`, `findExecutable`, the `su` fallback, the flag
   parsers — after extracting the `os.Exit`-laden logic into pure helpers.
5. Investigate `os.MkdirAll("/dev/shm", 1777)` (1934) — decimal vs octal.

---

## cmd/thundersnapd/  (the daemon — core of the review)

### 1. Edge cases for unit tests (pure-logic helpers)
- `parseFrameSpec` / `hasBlankRootfs` / `isFrameSpec` (main.go:3979-4026): `parseFrameSpec("")`;
  `>3` colon parts (4th silently dropped); `hasBlankRootfs` interaction with literal `"nil"`;
  `"::"` (empty rootfs, drives the error branch at 4123/4406).
- **`sanitizeForPath` (1413):** security-adjacent, untested — `"a/b"`→`"a_b"`, `".."`→`"_"`,
  leading-dot stripping, empty→`"_"`, `".."`-then-collapse ordering, null bytes.
- `stripDomain` (1430): `"user@host"`→`"user"`, no-`@`, multiple `@`.
- **`parseRangeHeader` (5829):** `"bytes=0-99"`, suffix `"-500"`, open-ended `"100-"`, `start
  >= fileSize`, `start > end`, `end >= fileSize` clamp, multi-range rejection, missing prefix,
  suffix larger than file.
- **Data-dir → fsDir/snapsDir derivation (504-507)** is inline in `main()` → untestable;
  extract a `deriveDataDirs(dataDir)` helper. The "both under the same data-dir" invariant is
  load-bearing for the same-filesystem btrfs check (#15).
- `framePathForUUID` (refs_handlers.go:18): the nil/empty-`flagFsDir` guard isn't directly
  tested (assert it returns `""`, no panic).
- Ref handlers: `handleRefMove` when `oldUUID == uuid` (move to same frame, refid.Move skipped,
  153); `handleRefDelete` Force=true vs conflict (182-191); `handleReflog` empty-reflog;
  `handleAutorun` clear-vs-set (349).
- `selectTargetUser` (1517): the `targetUser != ""` early-return branch needs no FS — fast
  unit test.

### 2. Duplicated code (the headline)
- **2a. The 4 session-command forms in `runContainerSession` (1622-1735)** — four nearly
  identical `[]string` builders for `ts drop-caps-and-run ... su - <user> [-c cmd]`, differing
  only by PTY-vs-non-PTY (`--pty-handshake-fd=3`) and command-vs-interactive (`-c rawCmd`):
  non-PTY+cmd (1629-1634), non-PTY+interactive (1637-1642), PTY+cmd (1702-1708),
  PTY+interactive (1710-1716). The two `nsenter` prefixes (1654-1658, 1720-1724) are
  byte-for-byte identical; the two `cmd.Env` blocks (1663-1667, 1729-1734) differ only by
  `TERM=`. Collapse to one `buildSessionCommand(initPid, tsBinary, absRootFS, runAsUser,
  rawCmd, pty, term)`. Removes ~60 lines and the class of bug already hit twice (#10, #11).
- **2b. VMX vsock protocol writing** duplicated in `runVMXSession` (2302-2305) and
  `runVMXOuterShell` (2346-2350); only the `VMX\x00framePath\x00` prefix differs → one
  `writeVshdRequest(conn, framePath, targetUser, args)`.
- **2c. VMX session setup boilerplate** duplicated in `runVMXSession` (2258-2289) and
  `runVMXOuterShell` (2313-2335): sanitize → `userFsDir` → `.vmx-`+iso → `prepareVMXRootFS` →
  `makeVMXControlHandler` → `getOrCreateVMX` → `connectToVshd` → `defer releaseVMX`. ~90% same.
- **2d. JSON error responses — two parallel idioms:** `refs_handlers.go`/`frames_handlers.go`
  use clean `jsonError`/`jsonResponse` (refs_handlers.go:358-368), but main.go inlines the
  4-line `Header().Set("Content-Type") + WriteHeader + Encode({Status:"error", Message:...})`
  ~30×. Every typed response is just `{Status, Message, ...}`; consolidate on a generic
  `writeJSON(w, code, v)`.
- **2e. "method not allowed" preamble** (`if r.Method != POST {...}`) repeated ~15×; a
  `requirePost(w,r) bool` or a mux middleware would remove all of them.
- **2f. "extract tailscale user from rootFS path"** identical block in `handleCreate`
  (4078-4091) and `handleDeleteFrame` (3726-3738) → one `tailscaleUserFromRootFS(rootFS)`.
- **2g. btrfs `subvolume snapshot/create/delete` invocations** repeated ≥8× with the same
  `CombinedOutput()` + `fmt.Errorf("...: %w\noutput: %s")` wrap (ensureRootFS 2535, ensureFrameFS
  2594/2602/2630/2638/2665/2673/2740, handleDeleteFrame 3774/3781/3788/3795,
  createSnapshotWithTaintsSubdir 5219, cleanup 5211). Thin `btrfsSnapshot/CreateSubvol/
  DeleteSubvol` wrappers would centralize error formatting + make them mockable.
- **2h. Three progress-writer types** (`snapProgressWriter` 3366-3401, `createProgressWriter`
  4445-4497, `downloadProgressWriter` 4817) implement the same `Write` → trim → NDJSON →
  flush, differing only in the event struct. Share a generic streaming writer.
- **2i. Frame-rootfs post-setup** ("ensure user in passwd → EnsureSudoers → ensureResolvConf →
  ensureTmpDir") repeated in `ensureRootFS` (2549-2566), `ensureFrameFS` (2711-2727),
  `createFrame` (4568-4584) → one `finalizeFrameRootfs(rootFS)`.

### 3. Overly complicated / redundant code paths (session-entry focus)
- **3a. Three session-routing cases that converge to two.** `switch cap.Isolation`
  (693-723) has `vmx`, `none`, default(`container`) — but `none` and `container` both call
  `runContainerSession` (713, 719); the only difference is the greeting string. Fold `none`
  into default with a greeting chosen from a small map. The SFTP subsystem handler (745-775)
  re-implements its own `@` parsing + rootFS setup instead of reusing `parseSSHUser`/
  `prepareContainerRootFS`.
- **3b. `runContainerSession` is ~290 lines with a duplicated PTY/non-PTY split.** It builds
  `cmd` for the non-PTY case (1661-1667) then **throws it away and rebuilds it** inside the
  `if isPty` branch (1727-1734) — the initial `exec.Command` is dead work when `isPty`. Build
  args + `cmd` once via the unified builder, then branch only on I/O plumbing. Roughly halves
  the function.
  - **Unified shape for the whole session layer:** a `sessionSpec{kind, rootFS, runAsUser,
    rawCmd, pty, term, win}` + `enterSession(s, spec)`. Container kinds → one nsenter+drop-caps
    builder + one `runWithIO(cmd, s, pty)` (PTY-handshake vs pipes). VMX kinds → one
    `connectVMX` + `writeVshdRequest` + the already-shared `proxyVMSession`. **Note: empty vs
    non-empty frames are already unified** below the session layer (ensureRootFS→ensureFrameFS);
    the redundancy is purely in command-construction + I/O-plumbing.
- **3c. Snapshot pipeline is a chain of thin forwarders:** `createSnapshot` (5036) →
  `createSnapshotSubdir` → `createSnapshotWithTaints` (5186, one-line) →
  `createSnapshotWithTaintsSubdir` (real impl); plus `createSnapshotWithFidx` (5159) one-lines
  into `createSnapshotWithTaints` with `taints=nil`. Three trivial wrappers default one param.
  `createSnapshot` appears to have **no remaining callers** — likely dead code.
- **3d. `makeSnapHandler` non-streaming branch (3422-3442)** duplicates the error/success JSON
  writing; the client always passes `stream=1` (per MEMORY), so it's effectively unused — keep
  with a comment or remove.
- **3e. `handleCreate*` cluster doubles everything (4036-4443):** legacy (`frame_name`+
  `snapshot_id`) vs new (`snapshot_spec`→UUID), each split into streaming/non-streaming —
  `handleCreate`, `handleCreateWithUUID` (4171), `handleCreateStreamingWithUUID` (4295),
  `handleCreateStreaming` (4385). They duplicate blank-rootfs validation, snapshot-existence
  `os.Stat`, `createFrame`, metadata storage, ref creation. `handleCreateWithUUID` and its
  streaming twin are ~80% identical. Reduce to one create core + a "reporter" interface
  (buffered JSON vs streaming `createProgressWriter`).
- **3f. `proxyVMSession` drain loop (2400-2409)** uses `goto done` out of a `for`/`select` to
  wait ≤2 goroutines with a 100ms timeout; a `sync.WaitGroup` (or just not draining, since the
  goroutines exit when `conn.Close()` unblocks `io.Copy`) is clearer.

### 4. Unclear / unexplained code
- **`controlServer` vsock handshake (handleConn, 3132-3159):** *why* an HTTP server on a Unix
  socket first speaks a `CONNECT <port>` / `OK <port>` text handshake — because the same client
  talks to both a real cloud-hypervisor vsock (VM) and this Unix socket (container), so the
  socket emulates the vsock CONNECT protocol. In MEMORY but not in a comment here.
- **Port constants:** `thunderPort = 5223` (3045) vs `VshPort` (2162) vs `meshPort = 7575`
  ("TSTS in leetspeak", 5336 — only this one is explained). Note *why 5223* and that it must
  match the in-container `ts` client's hardcoded CONNECT port.
- `createSnapshotWithTaintsSubdir` (5198-5232): the writable-snapshot-then-prune dance and why
  a subdir snap can't reuse the parent manifest (5250-5258) is subtle; one more line would help.
- `btrfsMagic = 0x9123683E` (5779): cite `BTRFS_SUPER_MAGIC` from `<linux/magic.h>`.
- `su - user` vs `su user` (1625-1633, 1700-1707): *is* commented (rsync/HOME, #10) — keep it
  on all unified forms after refactor.
- **`--skip-mount-setup` (1632/1639/1705/1714) has no comment** explaining it prevents stacking
  a fresh `devpts newinstance` per session (#11). Given it was a real bug, comment the builder.
- `openContainerPTY`/`ptsnameFromMaster`/`unlockPTY` (5906-5950): `TIOCGPTN`/`TIOCSPTLCK`
  re-implement glibc `ptsname`/`unlockpt`; note "Go equivalent of unlockpt(3)/ptsname(3),
  needed because we open the *container's* ptmx via /proc/<pid>/root".
- `prepareVMXRootFS` `.vmx-` prefix + `/bin/sh -> ts` symlink (2246-2250): the fact that `ts`
  *is* the shell (argv0 `sh` triggers shell mode) is non-obvious; comment it.
- **`sftpHandler.toHostPath` (main.go:1931) has a Chinese-language comment** (`防止目录遍历攻击`
  = "prevent directory traversal attack") — presumably unintentional; make it English.

### 5. Godoc accessibility
- `main` package, so exported is misleading. Many response DTOs are exported only by
  convention and could be unexported: `RefRequest/Response`, `RefListEntry/Response`,
  `ReflogEntryResponse`, `ReflogResponse`, `AutorunRequest/Response` (refs_handlers.go);
  `LogEntry/Response` (frames_handlers.go); `ControlRequest/Response`, `SnapResponse`,
  `SnapStreamEvent`, `TaintRequest/Response`, `Delete*Request/Response`, `CreateRequest/
  Response`, `CreateStreamEvent`, `ListSnapsResponse`, `SnapInfo`, `MeshPing` (main.go);
  `SnapMeta`, `SnapSource`, `FrameMeta` (metadata.go).
- `UnionTaints`/`IntersectTaints` (metadata.go) exported + documented but only used in-package.
- **Concrete godoc bug:** `ReflogEntryResponse` (refs_handlers.go:257) has a doc comment that
  starts `// ReflogEntry is ...` — comment/identifier mismatch (go-lint flags it). Every doc
  should begin with the actual identifier name.

### Highest-value items (daemon)
1. **Unify the 4 session command forms** in `runContainerSession` (§2a/§3b) — biggest win,
   directly addresses the suspected redundancy, kills a recurring bug class (#10, #11).
2. Collapse the VMX pair `runVMXSession`/`runVMXOuterShell` + the vshd protocol writer
   (§2b/§2c).
3. Consolidate HTTP error/response writing onto `jsonError`/`jsonResponse` + a `requirePost`
   guard; extract `tailscaleUserFromRootFS` and the btrfs-exec wrappers (§2d–g).
4. Collapse the 4-way `handleCreate*` cluster behind a reporter interface (§3e); remove the
   dead `createSnapshot` wrapper (§3c).
5. Add unit tests for `sanitizeForPath`, `parseRangeHeader`, `parseFrameSpec`/`hasBlankRootfs`
   edge cases, and an extracted `deriveDataDirs`.
6. Add comments on the vsock CONNECT emulation, `--skip-mount-setup`, the PTY ioctls; fix the
   `ReflogEntryResponse` godoc and the Chinese comment at main.go:1931.

---

## cmd/vshd/  (VM/container shell daemon — largest of the small binaries)

### 1. Edge cases for unit tests
The only test (`TestSuLoginChangesWorkingDirectory`) tests `su`, not vshd's logic, and
`t.Skip`s in 3 places. Untested pure functions:
- `lookupUserHome` (49-66) and `lookupContainerUserHome` (357-374): `/etc/passwd` parsing —
  malformed lines (<6 fields), comments, blanks, substring usernames, duplicates, 6-vs-7
  fields, trailing whitespace. (Hardcoded `/etc/passwd` path hurts testability — flag it.)
- `selectTargetUser`/`selectContainerUser` (26-45, 335-354): the `targetUser != ""` early
  return is trivially testable, untested.
- **Shell-quoting** in `runCommand` (436-440) and `runContainerCommand` (287-291) — `'` +
  `ReplaceAll(arg, "'", "'\\''")` + `'` — pure transform, duplicated and untested (args with
  quotes, spaces, empty, embedded `$`).
- Wire-protocol parsing in `handleConnection`/`handleVMXConnection`: `argCount` negative /
  non-numeric / larger than actual args (would block on `ReadString`/EOF) — unhandled, untested.

### 2. Duplicated code
- `lookupUserHome` (49-66) vs `lookupContainerUserHome` (357-374): identical `/etc/passwd`
  scanner, only the path differs.
- `selectTargetUser` (26-45) vs `selectContainerUser` (335-354): same ubuntu→user→root
  algorithm, differing only by the `containerRootFS` prefix.
- Shell-quoting + `su - <user> -c` construction duplicated verbatim in `runCommand` (436-441)
  and `runContainerCommand` (287-292).
- The two PTY copy loops in `runInteractiveShell` (399-418) and `runContainerShell` (252-271)
  are byte-for-byte identical (`done` channel, two `io.Copy`, SIGHUP, Wait); likewise the
  non-PTY loops `runCommand` (462-474) vs `runContainerCommand` (318-330).
- Null-delimited field reading appears in both `handleConnection` (134-153) and
  `handleVMXConnection` (188-208).

### 3. Overly complicated / redundant
- VM vs container entry is parallel and could be unified: `runInteractiveShell`/`runCommand`
  (direct on VM) vs `runContainerShell`/`runContainerCommand` (wrapped in `ts drop-caps-and-run
  --chroot`). The only real differences are the chroot+tsBinary prefix and which
  `select*User`. Four functions for essentially 2 behaviors × 2 contexts.
- `firstField = firstField[:len(firstField)-1]` (123) and similar slice-off-the-delimiter ops
  repeated ~8× — fragile/unclear (safe only because `ReadString` errors without the delimiter).

### 4. Unclear / unexplained code
- `cmd.Process.Signal(syscall.SIGHUP)` then `Wait()` (269) — why SIGHUP specifically, and why
  after only ONE of the two copy goroutines fires (`<-done`, 267) implies session end. Comment.
- `Cloneflags: CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWUTS` (239, 297) — why these three
  namespaces (and the relationship to `ts drop-caps-and-run` doing the chroot) is implicit.
- VMX detected by the literal first field `"VMX"` (protocol comment 111-115 helps) — but a real
  username `"VMX"` would collide; unhandled and unexplained.
- `initTsBinaryPath` silent fallback to `/bin/ts` on stat failure could surprise.

### 5. Godoc accessibility
- **No package-level doc comment** — vshd is the largest/most protocol-heavy binary yet lacks
  one (tsm/tsvm/dist/trivial-httpd all have one). Add a package comment describing the vsock
  protocol + purpose. All funcs unexported (appropriate for `main`).

---

## cmd/vsh/

### 1. Edge cases for unit tests
- Essentially all I/O glue; only the `"OK"` prefix check (43) is extractable. Low value.

### 2. Duplicated code — none.

### 3. Overly complicated / redundant
- **Dead/placeholder:** the SIGWINCH handler (57-63) registers a signal + goroutine but only
  has `// Could send window size to guest here` — consumes the signal, never acts. Resize or
  remove.

### 4. Unclear / unexplained code
- `buf := make([]byte, 256)` + single `Read` (37-38) assumes the whole `OK <port>\n` arrives in
  one read and that nothing after it is consumed prematurely — comment the risk.
- The stdin→vsock goroutine (69-71) is never awaited, so on exit buffered stdin may be lost
  (intentional but unexplained).

### 5. Godoc accessibility
- **No package doc comment** (user-facing client; a one-line purpose statement would help).
  All identifiers `main`-local.

---

## cmd/tsm/

### 1. Edge cases for unit tests
- `printFile` extension dispatch (101-111): unknown-extension error and case-sensitivity
  (`.TSM`) untested.
- Default-output-basename logic (73-77, `TrimRight(inputDir, "/")`): input `/` → empty basename
  edge.

### 2. Duplicated code — minor (`printTSM`/`printTSC` share a header pattern; fine).

### 3. Overly complicated / redundant
- `outBase = strings.TrimRight(inputDir, "/")` after `filepath.Clean` (59) is largely
  redundant — Clean already strips trailing slashes (except root).

### 4. Unclear / unexplained code
- `Progress: true, IsTTY: true` hardcoded (82-84) regardless of whether stderr is a TTY — a
  non-TTY invocation still gets `\r` progress. Comment or add an `isatty` check.

### 5. Godoc accessibility
- Good package doc; all funcs unexported. `print_` trailing-underscore var (24) avoids the
  `print` builtin — fine.

---

## cmd/tsvm/

- Pure glue (flag parse + `StartVM` + signal select). Nothing meaningfully unit-testable; no
  duplication; clean and minimal; self-explanatory `select`. Has a package doc. No issues.

---

## cmd/dist/

### 3. Overly complicated / redundant
- `out := filepath.Join(wd, "dist")` computed then overwritten if `*outPath != ""` (86-89); the
  flag help says "default: ./dist" but the code uses an absolute `wd/dist` — help-vs-behavior
  mismatch (relative vs absolute). Minor doc bug.

Otherwise: minor justified reuse of `tspkgs.Targets()`/`FilterTargets()`; has a package doc; all
unexported. No significant issues.

---

## cmd/trivial-httpd/

### 1. Edge cases for unit tests
- **`parseRangeHeader` (179-246)** — highest-value untested function in this set: missing
  `bytes=` prefix, multiple (comma) ranges, malformed `-`, suffix `-500` > file (clamp to 0),
  `-0`, open-ended `0-`, `start >= fileSize`, `end >= fileSize` clamp, `start > end`, empty
  `-`, non-numeric, `fileSize == 0`. None tested.
- `ServeHTTP` path-traversal guard — testable via `httptest`.

### 2. Duplicated code
- Range-response logic duplicated between the symlink branch (108-121) and the regular-file
  branch (152-175): both `parseRangeHeader` + set `Content-Range`/`Content-Length`/
  `Accept-Ranges` + write `206`. Unify.

### 3. Overly complicated / redundant
- **Weak traversal guard:** `strings.HasPrefix(fullPath, fs.root)` (64) is prefix-string-based,
  so a sibling `/srv/root-evil` passes when root is `/srv/root`. Combined with `filepath.Clean`
  + `HasPrefix(cleanPath, "..")` (56) it's mostly OK, but the prefix check should use a
  separator boundary.
- `mode.IsRegular() && mode&os.ModeSymlink == 0` (83) — a symlink is never regular, so the
  `&& symlink==0` is redundant (harmless).

### 4. Unclear / unexplained code
- `HasPrefix(cleanPath, "..")` (56) only catches a *leading* `..`; the deeper rationale (that
  `filepath.Join` with an absolute-cleaned URL path can't escape) is implicit.
- `O_NONBLOCK` on a regular file (125) has no effect for normal files; why NONBLOCK
  specifically (vs only NOFOLLOW) is unclear given `IsRegular` already excludes FIFOs/devices.

### 5. Godoc accessibility
- Good package doc; `fileServer` unexported; `ServeHTTP` is the conventional interface-method
  exception. No required godoc missing.

---

## Cross-cutting observations (within-module scope only)

- **Atomic writes:** both `frames` and `refs` use non-atomic `os.WriteFile` for read-modify-
  write metadata; if the daemon ever writes concurrently, this is the top risk in both.
- **Session-entry redundancy (the user's specific question):** the redundancy is concentrated
  in two places — `cmd/thundersnapd`'s `runContainerSession` (4 command forms + duplicated
  PTY/non-PTY build), and `cmd/vshd`'s 4 parallel container/VM shell functions. In both, the
  empty-vs-nonempty and user-vs-root distinctions are *already* unified below the entry layer;
  what remains duplicated is command-construction and I/O plumbing. `thundersnap`'s
  `RunInContainerNs`/`StartInContainerNs` are a *third* copy of the spawn logic that the daemon
  bypasses (and which is missing `--skip-mount-setup`).
- **Package doc gaps:** `cmd/vshd` and `cmd/vsh` are the only binaries lacking a package-level
  doc comment.
- **Godoc-form / unexport stylistic items** recur across the `main` packages (`cmd/ts`,
  `cmd/thundersnapd`): many response DTOs are exported only for JSON-readability and could be
  unexported; one concrete doc/identifier mismatch is `ReflogEntryResponse`.

---

# Cross-module architecture review

This section steps back from individual modules and looks at file sizes, package boundaries,
and the overall dependency shape. Grounding facts:

- Module path: `github.com/tailscale/thundersnap`.
- Internal package import frequency (non-test): `frameid` ×11, `tsm` ×9, `refs` ×6, `snaphash`
  ×4, `thundersnap` ×4, `frames` ×3, `snapsubdir` ×2, `refid` ×2, `metrics` ×2.
- The pure-data/library packages (`frameid`, `snaphash`, `tsm`, `frames`, `refs`, `refid`,
  `snapsubdir`, `metrics`) are leaf-ish and low-dependency — the layering at the *library*
  level is healthy. The architectural debt is almost entirely in the two `main` packages.

## 1. Oversized source files

| File | Lines | Assessment |
|------|------:|------------|
| `cmd/thundersnapd/main.go` | **5960** | Far too large; ~15 distinct responsibilities in one file. |
| `cmd/ts/main.go` | **2984** | Too large; client + 3 privileged helper modes in one file. |
| `tsm/tsm_test.go` | 1271 | Large but it's a test file; acceptable, could split by concern. |
| `tsm/download.go` / `tsm.go` | 551 / 530 | Reasonable for their scope. |
| `cmd/vshd/main.go` | 581 | Borderline; see §2. |

### `cmd/thundersnapd/main.go` (5960 lines) — the dominant problem
A scan of top-level declarations shows this single file owns at least these *unrelated*
concerns, most of which have no reason to share a file:

- **Process/daemon lifecycle:** `main`, `runActivate`, `runStatus`, `runForceReauth`,
  `writeStatus*`, `fatalWithStatus`, admin control socket (`startAdminControlSocket`,
  `handleAdminConnection`), host-key handling, tailscale whois.
- **cgroups / resource control:** `getSystemMemoryBytes`, `initParentCgroup`,
  `setProcessOOMScore`, `setupContainerCgroup`, `configureContainerResources` (~lines 1249-1410).
- **Session entry:** `parseSSHUser`, `selectTargetUser`, `runContainerSession` (~300 lines),
  `runSFTPSession` + the entire `sftpHandler`/`listerat`/`linkInfo` SFTP implementation
  (~1848-2120).
- **VMX/VM session glue:** `connectToVshd`, `prepareVMXRootFS`, `runVMXSession`,
  `runVMXOuterShell`, `makeVMXControlHandler`, `proxyVMSession`.
- **Rootfs construction:** `prepareContainerRootFS`, `ensureRootFS`, `ensureFrameFS`,
  `setupMinimalRootfs`, `copyTsBinary`/`copyVshdBinary`/`copyBinaryToRootFS`,
  `ensureResolvConf`, `ensureTmpDir`, btrfs helpers (`isSubvolume`, `isDirEmpty`).
- **Control server / vsock emulation:** `controlServer`, `controlResponseWriter`,
  `startControlServer`, the CONNECT/OK handshake.
- **HTTP API handlers:** ping, snap (+ progress writer + streaming), taint, delete-snap,
  delete-frame, create (the 4-way cluster), list-snaps — plus their request/response DTOs.
- **PTY plumbing:** `openContainerPTY`/`ptsnameFromMaster`/`unlockPTY` and the btrfs-magic /
  range-header helpers near the end.

A reader cannot hold this file in their head, and unrelated changes constantly collide here.
The good news: the package *already* demonstrates the target pattern — `refs_handlers.go`,
`frames_handlers.go`, `metadata.go`, `policy.go`, `portmap.go`, `docker.go`, `nfs.go` are
clean single-concern files. `main.go` should be decomposed the same way (see §3).

### `cmd/ts/main.go` (2984 lines)
Bundles three very different programs behind argv0/subcommand dispatch:
1. the **user-facing client** (`snap`, `snaps`, `frame(s)`, `ref(s)`, `log`, `taint`, …, each a
   `cmdX`/`doX` pair that all build the same HTTP-over-unix client);
2. the **privileged in-container helpers** (`container-init`, `drop-caps-and-run`, `drop-caps`)
   that do namespace/chroot/devpts/capability work;
3. the **minimal shell** (`runShell` + REPL) used when argv0 is `sh`.

These share almost no code and have very different risk profiles (the client is ordinary HTTP
glue; the helpers manipulate kernel namespaces and drop capabilities). Splitting them into
separate files within the package — `client.go`, `container_setup.go`, `shell.go`,
`httpclient.go` — would isolate the dangerous privileged code from the mundane client code and
make the privileged paths far easier to audit and test.

## 2. Opportunities to break functionality into new packages (with fewer deps)

The recurring theme: several self-contained, well-bounded chunks currently live inside `main`
packages where they cannot be imported, reused, or unit-tested in isolation. Extracting them
into small leaf packages would shrink the giant files *and* widen unit-test coverage (most of
these need no root/btrfs once isolated behind an interface).

Highest-value extractions, roughly in priority order:

1. **A `btrfsutil` (or `btrfs`) package.** `exec.Command("btrfs", "subvolume", …)` with the
   identical `CombinedOutput()`+error-wrap is open-coded in `cmd/thundersnapd/main.go` (≥8×),
   `snapsubdir`, `refid`, and `tsm` paths. A single package with `Snapshot/CreateSubvol/
   DeleteSubvol/IsSubvolume(path)` + one error-formatting convention would (a) remove the
   biggest cross-module duplication, (b) give one place to mock for tests, and (c) let
   `snapsubdir`/`refid` drop their private `isSubvolume` copies. `isSubvolume` literally exists
   3× (snapsubdir, refid, thundersnapd) today.

2. **A `session` package for the container/VM entry layer.** This directly addresses the
   user's session-redundancy question at the architectural level: pull `runContainerSession`,
   the 4 command-form builders, the nsenter/drop-caps arg construction, the PTY-vs-pipe I/O
   plumbing, and the VMX `connectToVshd`/`proxyVMSession`/`writeVshdRequest` logic out of
   `main.go` into a package with a single `enterSession(spec sessionSpec)` entry point. Crucially
   this would also absorb `thundersnap.RunInContainerNs`/`StartInContainerNs` (the dead, divergent
   third copy of the spawn logic) so there is exactly **one** implementation of "spawn into the
   container ns", shared by the daemon and any other caller. Most of the arg-building becomes
   pure and unit-testable once it's not entangled with `ssh.Session` I/O.

3. **An `sftp` (or `sftpserver`) package.** The entire `sftpHandler` + `listerat` + `linkInfo`
   implementation (~270 lines in `main.go`) is a self-contained `sftp.Handlers` implementation
   with no daemon-specific state beyond a root path. It belongs in its own package and is
   independently testable.

4. **A `rootfs` package.** `prepareContainerRootFS`, `ensureRootFS`, `ensureFrameFS`,
   `setupMinimalRootfs`, `copy*Binary`, `ensureResolvConf`, `ensureTmpDir`, and the
   `finalizeFrameRootfs` helper proposed in the per-module review form a coherent "build/prepare
   a frame's root filesystem" unit. Extracting it untangles rootfs construction from HTTP
   handling and gives the snapshot/create paths a clean dependency.

5. **A `cgroup` package.** The cgroup/OOM/memory block (~lines 1249-1410) is pure Linux
   resource-control plumbing with zero coupling to the rest of the daemon. Easy, clean lift.

6. **A `controlvsock` package.** The control-server + CONNECT/OK vsock-emulation handshake
   (`controlServer`, `controlResponseWriter`, the `CONNECT <port>`/`OK <port>` protocol) is a
   protocol unit shared in spirit with the VM-side vsock code in `thundersnap/vm.go` and the
   `cmd/vsh` client. Co-locating the wire-protocol constants (`thunderPort`, the CONNECT format)
   in one importable package would stop `cmd/vsh`, `cmd/vshd`, the daemon, and `thundersnap/vm.go`
   from each hardcoding their own copy of the same handshake.

7. **A shared `thunderclient` package for the HTTP-over-unix client.** `cmd/ts` builds the
   `dialThunder`-backed `http.Client` ~19× and re-implements POST+decode ~8×; the daemon-side
   and any future tooling would benefit from one client package (it could live under `tsm`-style
   leaf, importable by `cmd/ts` and tests).

## 3. Suggested target layout (illustrative, not prescriptive)

```
cmd/thundersnapd/   main.go (wiring/flags only, ~few hundred lines) + the existing
                    *_handlers.go files, now joined by session.go-thin-wiring
session/            enterSession, command builders, PTY/pipe plumbing, VMX glue
rootfs/             frame rootfs construction + binary copy + resolv/tmp
btrfsutil/          subvolume create/delete/snapshot/is-subvolume (1 impl, used everywhere)
cgroup/             memory/OOM/cgroup setup
controlvsock/       control server + CONNECT/OK handshake + shared port constants
sftpserver/         sftpHandler + listerat + linkInfo
thunderclient/      HTTP-over-unix client + POST/decode helpers (used by cmd/ts)
cmd/ts/             client.go + container_setup.go + shell.go + httpclient-thin-wiring
```

This keeps `frameid`/`snaphash`/`tsm`/`frames`/`refs`/`refid`/`snapsubdir`/`metrics` exactly as
they are (they're already good leaves) and concentrates the refactor on draining the two
`main.go` files into focused, importable, testable packages.

## 4. Other cross-module architecture issues

- **Three copies of `isSubvolume`** (snapsubdir, refid, cmd/thundersnapd) and the open-coded
  btrfs invocations are the clearest cross-module duplication; the `btrfsutil` package above is
  the single highest-leverage structural fix.

- **Spawn logic lives in two packages that drift.** `thundersnap.RunInContainerNs`/
  `StartInContainerNs` vs the daemon's inline `runContainerSession` is not just per-module dead
  code — it's a cross-module hazard: the canonical behavior (with the `--skip-mount-setup` fix)
  exists only in the daemon copy, while the *exported, importable* copy in the `thundersnap`
  package is subtly wrong. Any new consumer that does the "right" thing (import the library)
  gets the buggy path. Consolidating into the `session` package removes the trap.

- **Wire-protocol constants are scattered.** `thunderPort = 5223`, `VshPort`, `VsockPort`,
  `meshPort = 7575`, and the `CONNECT <port>`/`OK <port>` and null-delimited vshd protocols are
  defined independently in `cmd/thundersnapd`, `cmd/vshd`, `cmd/vsh`, and `thundersnap/vm.go`.
  These four components must agree, but nothing enforces it at compile time. A shared protocol
  package (or at least shared constants) would make the contract explicit and prevent silent
  skew.

- **`thundersnap` package naming collision.** There is a package `thundersnap/` *and* a binary
  `cmd/thundersnapd/` *and* the module is `thundersnap`. The `thundersnap` library package is
  imported by only two binaries (`cmd/thundersnapd`, `cmd/tsvm`) and mixes two unrelated
  concerns (container-ns management in `container.go`, VM lifecycle in `vm.go`). Splitting it
  into `containerns/` and `vm/` (note: there is already an empty `vm/` dir) would give each a
  single responsibility and clearer names. (There is also a stray empty `vm/` directory and a
  `bin/` dir in the tree worth tidying.)

- **DTO duplication across the client/daemon boundary.** `cmd/ts` and `cmd/thundersnapd` each
  define their own copies of the request/response structs (`SnapResponse`, `CreateRequest`,
  `RefRequest`, etc.) that must stay byte-compatible over JSON. A shared `apitypes` (or
  `protocol`) package importable by both would remove a whole class of "client and server
  structs drifted" bugs and is a natural companion to the `thunderclient` extraction.

- **Test-tier coupling lives in `e2e/` but unit-test seams are missing.** Because so much logic
  is trapped in `main` packages behind `os.Exit`/`ssh.Session`/root requirements, the project
  leans heavily on slow root+btrfs e2e tests for things that *could* be fast unit tests (arg
  parsing, path resolution, command construction, range parsing, manifest parsing of corrupt
  input). The package extractions above are the prerequisite that would let the test pyramid
  rebalance toward cheap unit tests — this is the strongest long-term argument for the refactor,
  beyond readability.

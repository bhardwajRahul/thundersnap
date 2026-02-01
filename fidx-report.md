# FIDX v2: A Unified Filesystem Index Format

## Executive Summary

This document proposes a replacement for the current `.mfidx` format that addresses
several limitations while adding new capabilities for efficient filesystem
synchronization, incremental downloads, and chunk retrieval from slab files.

**Key design decision**: Use **two files** rather than one:

1. **Manifest file** (`.tsm` - ThunderSnap Manifest): Contains file metadata
   (paths, sizes, permissions, timestamps) and per-file chunk list references.

2. **Chunk index file** (`.tsc` - ThunderSnap Chunks): A sorted, deduplicated
   list of all unique chunks with their SHA and size, plus optional slab location
   data.

This split provides the best trade-offs for all required operations.

**Hash algorithm**: SHA-256 (32 bytes). Git is transitioning from SHA-1 to
SHA-256, with SHA-256 becoming the default in Git 3.0 (targeted end of 2026).
Using SHA-256 now future-proofs the format and provides stronger collision
resistance. The 12-byte increase per hash is acceptable given the security
and compatibility benefits.

---

## Requirements Analysis

### Must Support

1. **Full filesystem metadata**: uid, gid, mode, mtime, atime, ctime, size,
   symlink targets, device nodes, etc.

2. **Fast diffing**: Compare two manifests to find changed files, or compare
   a manifest against a live filesystem.

3. **Slab-based chunk retrieval**: Download missing chunks from slab files
   hosted on R2 or similar, even when the original source files don't exist.

4. **Incremental manifest download**: Fetch only the changed parts of a
   manifest using the fidx-of-fidx trick or similar.

5. **Bounded RAM usage**: Stream processing for large filesystems (millions
   of files) without loading everything into memory.

6. **Efficient disk usage**: Don't waste space on redundant data.

### Current mfidx Limitations

| Issue | Impact |
|-------|--------|
| Limited metadata (only filename, size, mtime) | Can't restore permissions, ownership |
| No chunk deduplication in index | Same chunk listed N times if in N files |
| Sequential scan required for lookups | O(n) to find a file or chunk |
| All-or-nothing download | Must fetch entire mfidx even for small changes |
| No slab location data | Can't map chunks to external storage |
| Variable-length records | Hard to mmap efficiently in Go |

---

## Proposed Design: Two-File Architecture

### Why Two Files?

A single file optimized for one operation is suboptimal for others:

| Operation | Best Structure |
|-----------|----------------|
| List files with metadata | Sorted by path, variable-length records |
| Find chunk by SHA | Sorted by SHA, fixed-length records |
| Diff two manifests | Both sorted by path |
| Download incrementally | Content-defined chunks of the index itself |
| Map chunk to slab location | Sorted by SHA with location data |

Combining these into one file creates conflicts. Two files with clear
responsibilities handle all cases well.

---

## File Format: Manifest (`.tsm`)

The manifest describes the filesystem tree and references chunks by index into
the chunk file.

### Header (64 bytes, fixed)

```
Offset  Size  Field
0       4     Magic: "TSM\x02" (ThunderSnap Manifest v2)
4       4     Flags (uint32, big-endian)
              Bit 0: has_extended_attrs
              Bit 1: has_acls
              Bit 2: has_xattrs
              Bits 3-31: reserved
8       8     File count (uint64)
16      8     Total size of all files (uint64)
24      8     Chunk file SHA reference (first 8 bytes of .tsc SHA)
32      8     Creation timestamp (Unix nanos)
40      8     Source filesystem UUID (for btrfs)
48      16    Reserved (zero)
```

### File Entries (variable length, sorted by path)

Each file entry:

```
Offset  Size  Field
0       2     Entry length (uint16, includes this field)
2       2     Path length (uint16)
4       N     Path (UTF-8, no null terminator)
4+N     2     Entry type + flags (uint16)
              Bits 0-3: type (0=file, 1=dir, 2=symlink, 3=hardlink,
                              4=blockdev, 5=chardev, 6=fifo, 7=socket)
              Bit 4: has_xattr
              Bit 5: is_sparse
              Bits 6-15: reserved
6+N     4     Mode (uint32, full st_mode including type bits)
10+N    4     UID (uint32)
14+N    4     GID (uint32)
18+N    8     Size (uint64)
26+N    8     Mtime (int64, Unix nanos)
34+N    8     Ctime (int64, Unix nanos)
42+N    8     Atime (int64, Unix nanos) [optional based on flags]
50+N    4     Chunk start index (uint32, index into .tsc file)
54+N    4     Chunk count (uint32)
58+N    ...   Type-specific data (symlink target, device numbers, etc.)
```

For **symlinks**: 2-byte target length + target string follows chunk count.

For **hardlinks**: 4-byte link target index (file entry number of target).

For **devices**: 4-byte major + 4-byte minor device numbers.

### Footer (64 bytes)

```
Offset  Size  Field
0       32    SHA-256 of all preceding bytes
32      32    SHA-256 of the corresponding .tsc file (for integrity check)
```

### Design Notes

- **Sorted by path**: Enables binary search for file lookup, merge-based diffing.
- **Chunk indices, not SHAs**: Files reference chunks by index into the chunk
  file, saving 28 bytes per reference (4 vs 32). A file with 1000 chunks saves
  28KB.
- **Fixed metadata fields**: Most fields are fixed-width for easy parsing.
  Variable parts (path, symlink target) have explicit lengths.
- **Directories included**: Empty directories are preserved; they have
  chunk_count=0.

---

## File Format: Chunk Index (`.tsc`)

The chunk index is a sorted, deduplicated list of all chunks needed to
reconstruct the filesystem, plus optional slab location data.

### Header (64 bytes, fixed)

```
Offset  Size  Field
0       4     Magic: "TSC\x02" (ThunderSnap Chunks v2)
4       4     Flags (uint32)
              Bit 0: has_slab_locations
              Bit 1: has_compression_hints
              Bits 2-31: reserved
8       8     Chunk count (uint64)
16      8     Total chunk data size (uint64)
24      4     Slab count (uint32, 0 if no slab locations)
28      4     Slab table offset (uint32, offset to slab name table)
32      32    Reserved (zero)
```

### Chunk Entries (40 or 48 bytes each, sorted by SHA)

Without slab locations (40 bytes):
```
Offset  Size  Field
0       32    SHA-256
32      4     Size (uint32, allows chunks > 64KB if needed)
36      2     Level (uint16, bupsplit level for hierarchical grouping)
38      2     Flags (uint16)
              Bit 0: is_zero_block (don't store/fetch, reconstruct as zeros)
              Bit 1: is_literal (stored uncompressed in slab)
              Bits 2-15: reserved
```

With slab locations (48 bytes):
```
Offset  Size  Field
0       32    SHA-256
32      4     Size (uint32)
36      2     Level (uint16)
38      2     Slab index (uint16, index into slab name table)
40      8     Offset in slab (uint64)
```

### Slab Name Table (variable, at end before footer)

If `has_slab_locations` flag is set:

```
Offset  Size  Field
0       2     Number of slabs (uint16)
2       ...   For each slab:
              2     Name length (uint16)
              N     Slab name/URL (UTF-8)
```

### Footer (32 bytes)

```
Offset  Size  Field
0       32    SHA-256 of all preceding bytes (header + entries + slab table)
```

### Design Notes

- **Sorted by SHA**: Enables binary search for chunk lookup, O(log n).
- **Deduplicated**: Each unique chunk appears exactly once.
- **Fixed-size entries**: Easy to mmap and index in Go.
- **Optional slab locations**: When present, the index becomes a complete
  map from chunk SHA to storage location.
- **Slab index**: 16-bit index allows 65535 slabs. For very large deployments,
  could extend to 32-bit in a future version.

---

## Operations

### Creating a Manifest

```
Input: Filesystem path
Output: .tsm + .tsc files

1. Walk filesystem, collecting file metadata
2. For each regular file:
   a. Content-defined chunking (bupsplit)
   b. Add chunks to chunk set (deduplicate by SHA)
   c. Record chunk indices for this file
3. Sort files by path
4. Sort chunks by SHA, assign final indices
5. Update file entries with final chunk indices
6. Write .tsc file (sorted chunks)
7. Write .tsm file (sorted files with chunk references)
8. Compute and append checksums
```

### Diffing Two Manifests

```
Input: old.tsm, new.tsm
Output: List of (added, removed, modified, unchanged) files

1. Open both .tsm files
2. Merge-join on sorted paths:
   - Path in old only: removed
   - Path in new only: added
   - Path in both: compare metadata + chunk indices
     - All same: unchanged
     - Different: modified
3. For modified files, optionally compute chunk-level diff
```

RAM usage: O(1) - streaming merge of two sorted files.

### Diffing Manifest vs Filesystem

```
Input: .tsm file, filesystem path
Output: List of changes

1. Walk filesystem in sorted path order
2. Stream .tsm entries in sorted order
3. Merge-join:
   - In .tsm only: deleted
   - In FS only: added
   - In both: compare stat() vs metadata
     - Different mtime/size: modified (content check needed)
     - Same metadata: unchanged (trust mtime)
```

### Downloading Missing Chunks from Slab

```
Input: .tsc file with slab locations, list of needed chunk SHAs
Output: Chunk data

1. Binary search .tsc for each needed SHA
2. Group chunks by slab
3. For each slab:
   a. Fetch slab from R2 (or use local cache)
   b. For each chunk in this slab:
      - Seek to offset
      - Read size bytes
      - Verify SHA
      - Return chunk data
```

### Incremental Manifest Download

The `.tsm` and `.tsc` files themselves can be incrementally downloaded using
the existing fidx-of-fidx technique:

```
Server hosts:
  snapshot.tsm       (the manifest)
  snapshot.tsc       (the chunk index)
  snapshot.tsm.fidx  (fidx of the manifest file itself)
  snapshot.tsc.fidx  (fidx of the chunk index itself)

Client:
1. Download snapshot.tsm.fidx (small)
2. Compare against local old.tsm.fidx
3. Fetch only changed chunks of snapshot.tsm
4. Same for .tsc file
```

Since both files are sorted, changes tend to cluster:
- New files appear at their sorted position (localized change)
- New chunks appear at their sorted SHA position (localized change)

This gives good incremental behavior without any special format changes.

---

## RAM Budget Analysis

For a filesystem with 1 million files and 10 million unique chunks:

### Current mfidx (all in RAM)

- File entries: ~1M * (50 bytes path + 24 bytes metadata) = ~74 MB
- Chunk entries inline: ~10M * 24 bytes = 240 MB
- **Total: ~314 MB minimum**

### Proposed Format (streaming)

- One file entry in flight: ~100 bytes
- One chunk entry in flight: 48 bytes
- Binary search buffer (for lookups): ~4KB
- **Total: < 1 MB for streaming operations**

For operations that need random access (e.g., "find file X"):
- Mmap the .tsm file
- Binary search on path
- **RAM: proportional to OS page cache, not file size**

---

## Disk Space Analysis

For 1 million files, 10 million chunks:

### Current mfidx

```
Header:              8 bytes
Per file:           ~74 bytes avg (separator + metadata + inline chunks)
                    But chunks are duplicated across files
Estimated:          74M (files) + 240M (chunks with duplication) = ~314 MB
```

### Proposed Format

```
.tsm file:
  Header:           64 bytes
  Per file:         ~58 bytes avg (without inline chunk SHAs)
  Footer:           64 bytes
  Total:            ~58 MB

.tsc file:
  Header:           64 bytes
  Per chunk:        48 bytes (with slab locations)
  Footer:           32 bytes
  Total:            ~480 MB

Combined:           ~538 MB
```

Wait, that's larger! The issue is we're storing more metadata, slab locations,
and using SHA-256 (32 bytes vs SHA-1's 20 bytes).

**Without slab locations** (pure manifest, no external chunk storage):

```
.tsc per chunk:     40 bytes
Total .tsc:         ~400 MB
Combined:           ~458 MB
```

Still larger due to richer metadata and SHA-256. But:
- We gain uid/gid/ctime/atime that mfidx lacks
- We gain chunk deduplication (important for diffing)
- We gain O(log n) lookups vs O(n)
- We gain SHA-256 security (future-proof, Git 3.0 default)

**Space optimization**: If files are sorted and we use delta encoding for paths:

```
Per file:           ~40 bytes avg (with path prefix compression)
.tsm total:         ~40 MB
```

This brings combined size to ~440 MB. The ~40% increase over mfidx is the cost
of SHA-256 + richer metadata, but we gain significant functionality.

---

## Alternative Considered: Single File

A single-file design was considered where file entries contain inline chunk
SHAs:

**Pros**:
- One file to manage
- Atomic updates

**Cons**:
- Can't sort by both path AND SHA
- Chunk deduplication requires building an index anyway
- Larger file size (chunks duplicated)
- Can't add slab locations without duplicating data
- Incremental download less effective (changes scattered)

The two-file design is more complex but significantly more capable.

---

## Alternative Considered: SQLite

Using SQLite for the index:

**Pros**:
- Rich query support
- Built-in indexing
- Transactions

**Cons**:
- Larger file size (B-tree overhead)
- Slower sequential scan
- Can't easily diff two databases
- Harder to incrementally download
- More complex to mmap
- External dependency

For this use case, custom binary formats provide better performance and
simpler incremental sync.

---

## Implementation Phases

### Phase 1: Core Format

1. Define Go structs for .tsm and .tsc
2. Implement writer (filesystem walk → files)
3. Implement reader (parse, iterate, lookup)
4. Implement streaming diff (two .tsm files)

### Phase 2: Slab Integration

1. Extend .tsc with slab location data
2. Implement chunk fetcher (SHA → slab → data)
3. Integrate with bupdate for reconstruction

### Phase 3: Incremental Download

1. Generate .fidx for .tsm and .tsc files
2. Implement chunk-level sync of manifest files
3. Test with R2 hosting

### Phase 4: Filesystem Diff

1. Implement manifest vs live filesystem comparison
2. Optimize for common case (few changes)
3. Add "quick" mode using only mtime

---

## Appendix: Wire Format Summary

### .tsm (Manifest)

```
+------------------+
| Header (64B)     |
+------------------+
| File Entry 1     |
| File Entry 2     |
| ...              |
| File Entry N     |
+------------------+
| Footer (64B)     |
+------------------+
```

### .tsc (Chunks)

```
+------------------+
| Header (64B)     |
+------------------+
| Chunk Entry 1    |  (40B or 48B each)
| Chunk Entry 2    |
| ...              |
| Chunk Entry M    |
+------------------+
| Slab Table       |  (optional)
+------------------+
| Footer (32B)     |
+------------------+
```

---

## Appendix: Comparison with Existing Formats

| Feature | mfidx | bupindex | TSM+TSC |
|---------|-------|----------|---------|
| Full POSIX metadata | No | Yes | Yes |
| Chunk deduplication | No | N/A | Yes |
| O(log n) file lookup | No | Tree | Binary search |
| O(log n) chunk lookup | No | Via midx | Binary search |
| Slab locations | No | Pack offsets | Yes |
| Incremental download | Via fidx | No | Via fidx |
| Mmap-friendly | Partial | Yes | Yes |
| Streaming write | Yes | No (tree) | Yes |
| Streaming diff | No | No | Yes |

---

## Appendix: Example Usage

```go
// Create manifest from filesystem
manifest, chunks, err := tsm.Create("/path/to/snapshot")
manifest.WriteTo("snap.tsm")
chunks.WriteTo("snap.tsc")

// Diff two manifests
old := tsm.Open("old.tsm")
new := tsm.Open("new.tsm")
for diff := range tsm.Diff(old, new) {
    fmt.Printf("%s: %s\n", diff.Type, diff.Path)
}

// Find missing chunks and download from slab
chunks := tsc.Open("snap.tsc")
for _, sha := range neededChunks {
    loc := chunks.Lookup(sha)
    data := fetchFromSlab(loc.SlabURL, loc.Offset, loc.Size)
    // use data...
}
```

---

## Conclusion

The two-file TSM+TSC design provides:

1. **Rich metadata** for precise filesystem reconstruction
2. **Efficient diffing** via sorted, streaming merge
3. **Slab integration** for CDN-hosted chunk retrieval
4. **Incremental download** via existing fidx-of-fidx technique
5. **Bounded RAM** through streaming and mmap
6. **Reasonable disk usage** with chunk deduplication

The added complexity of two files is justified by the significant capability
gains over the current mfidx format.

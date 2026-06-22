# fidx to tsm/tsc Transition Analysis

## Overview

The codebase currently has two parallel chunk indexing formats:

| Format | Package | Hash | Purpose |
|--------|---------|------|---------|
| `.fidx` / `.mfidx` | `bupdate` | SHA-1 (20 bytes) | Mesh replication, chunk transfer |
| `.tsm` / `.tsc` | `tsm` | SHA-256 (32 bytes) | Snapshot identity, full metadata |

This document analyzes the current usage and outlines a migration path to consolidate on tsm/tsc.

## Current State

### Format Comparison

| Aspect | fidx | tsm/tsc |
|--------|------|---------|
| Hash algorithm | SHA-1 (20 bytes) | SHA-256 (32 bytes) |
| Chunk entry size | 24 bytes | 40 bytes |
| File metadata | Minimal (name, size, mtime) | Full (mode, uid, gid, times, symlinks, devices) |
| Deduplication | Per-mfidx | Global (TSC is sorted unique chunks) |
| Lookup | Linear scan | Binary search |

### What thundersnapd Does Today

During `ts snap`, thundersnapd creates **both** formats:

```go
// cmd/thundersnapd/main.go:3625-3645

// Creates .fidx (mfidx format)
bupdate.CreateFidx(tmpPath, tmpFidxPath, fidxOpts)

// Creates .tsm + .tsc
tsm.Create(tmpPath, tmpPath, tsmOpts)

// Creates .fidx.fidx (for bootstrapping fidx download)
bupdate.CreateSingleFidx(finalFidxPath, finalFidxFidxPath, fidxFidxOpts)
```

The snapshot ID is derived from the TSM SHA-256:
```go
// cmd/thundersnapd/main.go:3648-3653
tsmReader, err := tsm.ReadTSM(tmpTSMPath)
snapshotID := hex.EncodeToString(tsmReader.SHA256[:])
```

### Mesh Replication (fidx-based)

The entire mesh download path uses fidx exclusively:

**`bupdate/download.go:DownloadSnapshot()`:**
1. Downloads `.stamp` file (parent reference)
2. Downloads `.fidx.fidx` (chunk index of the fidx file itself)
3. Downloads `.fidx` using range requests guided by fidx.fidx
4. Parses `.fidx` to get file list
5. Downloads each file using fidx chunk info

**Local deduplication (`loadLocalMappings`):**
- Scans `snapshotsDir` for `.fidx` and `.mfidx` files
- Builds SHA-1 -> (file, offset, size) mapping
- Used to avoid re-downloading chunks that exist locally

### Commands Using fidx

| Command | Usage |
|---------|-------|
| `ts fidx <path>` | `bupdate.CreateFidx()` - generates fidx for a file/directory |
| `ts bupdate` | Full fidx-based reconstruction from HTTP |
| `ts download-snap` | `bupdate.DownloadSnapshot()` |
| `ts who-has` | `bupdate.CheckPeersForSnapshot()` |
| `fidx` (standalone) | `bupdate.CreateMFIDX()`, `bupdate.CreateSingleFidx()` |
| `bupdate` (standalone) | Fidx-based file reconstruction |

### Commands Using tsm/tsc

| Command | Usage |
|---------|-------|
| `tsm <directory>` | `tsm.Create()` - generates tsm+tsc |
| `tsm -p <file>` | `tsm.ReadTSM()`, `tsm.ReadTSC()` - prints contents |
| `ts snap` (internal) | Uses TSM SHA-256 as snapshot ID |

## Why fidx Still Exists

**fidx is the mesh transfer protocol.** The tsm/tsc format is used for:
- Snapshot identification (TSM SHA-256 becomes the snapshot ID)
- Rich metadata preservation

But actual data transfer over the mesh relies entirely on fidx because:
1. The download code predates tsm/tsc
2. fidx.fidx bootstrapping allows downloading the index itself efficiently
3. All the HTTP range request logic is fidx-aware

## Migration Plan

### Phase 1: Add TSC-based Local Deduplication

Currently `loadLocalMappings()` scans `.fidx` files. Add parallel support for `.tsc`:

```go
// New function in tsm package
func LoadLocalChunkMap(snapshotsDir string) (map[[32]byte]ChunkLocation, error)
```

This would scan `.tsc` files and build a SHA-256 -> location map.

### Phase 2: TSM/TSC Download Path

Create new download functions that use tsm/tsc:

1. Download `.tsm` and `.tsc` files (small, can fetch whole)
2. Parse TSM to get file list with chunk references
3. Parse TSC to get chunk SHA-256 -> size mapping
4. For each file, use chunk refs to fetch via HTTP range requests
5. Deduplicate against local TSC chunk map

Key changes needed:
- `tsm/download.go` - new download logic
- HTTP serving of `.tsm` and `.tsc` files
- Chunk location tracking (TSC stores SHA+size, need offset calculation)

### Phase 3: Deprecate fidx Generation

Once mesh download works with tsm/tsc:

1. Stop generating `.fidx` and `.fidx.fidx` in thundersnapd
2. Keep bupdate package for legacy compatibility (reading old fidx files)
3. Remove `ts fidx` command or have it generate tsm/tsc instead

### Phase 4: Remove fidx Code

After a transition period:

1. Remove `cmd/fidx`
2. Remove fidx generation from `bupdate/indexer.go`
3. Keep fidx parsing for legacy file support (optional)
4. Update README to remove fidx references

## Files to Modify

### High Priority (Core Migration)

| File | Changes |
|------|---------|
| `tsm/download.go` | New file: TSM/TSC-based download |
| `tsm/chunklookup.go` | New file: Local chunk deduplication from TSC |
| `cmd/thundersnapd/main.go` | Switch to tsm download, stop generating fidx |
| `bupdate/httpserver.go` | Serve .tsm and .tsc files |

### Medium Priority (Command Updates)

| File | Changes |
|------|---------|
| `cmd/ts/main.go` | Update `download-snap`, remove/update `fidx` and `bupdate` commands |
| `cmd/tsm/main.go` | Already complete |

### Low Priority (Cleanup)

| File | Changes |
|------|---------|
| `cmd/fidx/main.go` | Delete |
| `cmd/bupdate/main.go` | Delete or repurpose |
| `bupdate/indexer.go` | Remove CreateFidx, CreateMFIDX, CreateSingleFidx |
| `bupdate/download.go` | Delete |
| `README.md` | Update tool descriptions |

## Benefits of Migration

1. **Security**: SHA-256 vs SHA-1 (collision resistance)
2. **Efficiency**: Binary search in sorted TSC vs linear scan
3. **Metadata**: Full filesystem attributes preserved
4. **Simplicity**: One format instead of two
5. **Deduplication**: Global chunk index enables better dedup

## Risks

1. **Breaking change**: Old snapshots with only fidx won't be downloadable by new code
2. **Testing**: Mesh replication is complex, needs thorough testing
3. **Performance**: TSC entries are larger (40 vs 24 bytes), but lookup is O(log n) vs O(n)

## Recommendation

The migration is worthwhile but not urgent. Current state works, just has redundancy.

Suggested approach:
1. Implement TSM/TSC download path alongside fidx (Phase 1-2)
2. Test thoroughly with real mesh transfers
3. Switch default to TSM/TSC, keep fidx as fallback
4. After confidence period, remove fidx generation (Phase 3)
5. Eventually remove fidx code entirely (Phase 4)

# Hosted Thundersnap: Infrastructure & Storage Architecture

## Context

Building a hosted version of thundersnap where customers get powerful machines with "infinite" storage that never fills up. Key principles:

- **Run free**: Big machines, lots of RAM, fast NVMe - feel like your own hardware
- **Never permanently lose data**: Thundersnap replication to cold storage handles durability
- **Never fill up**: Tiered storage with local NVMe cache + remote HDD backing
- **Some downtime OK**: Hardware fails sometimes; restore from snapshot, not HA
- **Predictable billing**: Flat tiers, no overages

---

## Architecture: bcache + NBD Tiered Storage

### The Core Insight

Use bcache in writeback mode with local NVMe as cache and remote HDD (via NBD) as backing store. This provides:

1. **"Infinite" capacity** - Backing store can be huge and cheap (HDD)
2. **Fast hot data** - Working set cached on local NVMe
3. **Automatic tiering** - Cold data (old cargo cruft, nix history, logs) migrates to slow tier
4. **Off-site durability** - All data eventually reaches the remote backing store

```
Customer Machine (Bare Metal or NUC)
├── Local NVMe (2TB) - bcache cache device (writeback mode)
│   ├── Hot working data
│   ├── Recent writes (fast!)
│   └── Frequently accessed files
│
└── NBD client ──────────────────────────► NBD Server (Cloud)
    │                                       │
    │ Encrypted tunnel (WireGuard)          │
    │                                       ▼
    │                              Shared Storage Server
    │                              ├── RAID10 HDD (88TB+)
    │                              ├── Per-customer sparse files
    │                              └── Durable backing store
    │
    └── bcache backing device
        ├── Cold data (old cruft)
        └── Source of truth for durability
```

### How bcache Writeback Works

| Operation | What Happens | Speed |
|-----------|--------------|-------|
| Write | Goes to NVMe cache only, returns immediately | **NVMe-fast** |
| Read (cached) | Served from NVMe | **NVMe-fast** |
| Read (cache miss) | Fetched from backing, cached | Backing speed |
| Background writeback | Dirty data async-flushed to backing | Background |

**Key behavior**: bcache automatically evicts cold data from NVMe to make room for hot data. Old `.cargo/registry`, `/nix/store` history, unpruned logs naturally migrate to the slow tier without user intervention.

---

## Capacity Model

**Important**: bcache is a cache, not additive storage. The backing device must be >= total data size.

| Component | Size | Role |
|-----------|------|------|
| Local NVMe | 2TB | Cache (hot data, all writes) |
| Remote HDD backing | 2.2TB | Source of truth (all data lives here eventually) |
| **User sees** | **2.2TB** | With 2TB of it cached/fast |

The NVMe accelerates access but doesn't add capacity. Hot data exists on both; cold data only on backing.

---

## Provider: Hetzner

### Customer Machines (Bare Metal)

| Model | CPU | RAM | NVMe | Price |
|-------|-----|-----|------|-------|
| AX42 | Ryzen 7 PRO 8700GE (8-core) | 64GB DDR5 | 1TB | ~€57/mo |
| AX52 | Ryzen 7 7700 (8-core) | 64GB DDR5 | 2TB | ~€64/mo |
| AX102 | Ryzen 9 7950X3D (16-core) | 128GB DDR5 | 4TB | ~€104/mo |

### Shared Storage Server (HDD Backing)

| Model | CPU | RAM | NVMe | HDD | Price |
|-------|-----|-----|------|-----|-------|
| SX65 | Ryzen 7 3700X | 64GB | 2TB | 4×22TB = 88TB | €104/mo |
| SX135 | Ryzen 9 3900 | 128GB | 4TB | 8×22TB = 176TB | ~€200/mo |

**Use RAID10** for the HDDs (not RAID5):
- RAID5 is dangerous with 22TB drives (rebuild time too long)
- RAID10 rebuilds fast, safer
- SX65 with RAID10: 44TB usable
- SX135 with RAID10: 88TB usable

### Cost Per Customer

| Component | Cost |
|-----------|------|
| AX52 bare metal | €64/mo |
| 2.2TB HDD backing (shared SX65, 40 customers) | €2.60/mo |
| **Total** | **~€67/mo** |

Customer price: $149/mo (healthy margin)

---

## Data Safety & Recovery

### bcache Error Handling (Graceful Degradation)

When NVMe starts failing:
1. bcache detects errors (configurable threshold)
2. Disables writeback, flushes all dirty data to backing
3. Switches to passthrough mode (direct to HDD)
4. System continues running, just slower

**This is the graceful path** - NVMe degrades → data flushes → cache disabled → backing takes over.

### Data Loss Scenarios

| Scenario | Outcome |
|----------|---------|
| NVMe errors detected | Graceful flush → passthrough mode → **No data loss** |
| Clean shutdown | Flush dirty → **No data loss** |
| Power loss (PLP NVMe) | NVMe consistent → reboot → flush → **No data loss** |
| Kernel panic + NVMe survives | Flush dirty on reboot → **No data loss** |
| **Kernel panic + NVMe dead** | **Corruption → restore from thundersnap snapshot** |

**With datacenter NVMe (Power Loss Protection), the only risk is kernel panic followed by total NVMe failure.** Very narrow failure window.

### Consistent Snapshot Strategy

To guarantee a recovery point on the backing store:

```bash
# 1. Pause writeback (new writes still go to NVMe, just not flushed)
echo 0 > /sys/block/bcache0/bcache/writeback_running

# 2. Sync filesystem
btrfs filesystem sync /mnt

# 3. Drain all dirty data to backing
echo 0 > /sys/block/bcache0/bcache/writeback_percent
# Wait for dirty_data = 0

# 4. Take snapshot (backing now has everything up to this point)
btrfs subvolume snapshot -r /mnt /mnt/.snapshots/checkpoint-$(date +%s)

# 5. Resume writeback
echo 1 > /sys/block/bcache0/bcache/writeback_running
echo 40 > /sys/block/bcache0/bcache/writeback_percent
```

**Result**: Snapshot is guaranteed consistent on backing store. Even if NVMe explodes 1 second later, that snapshot is recoverable.

**COW safety**: Post-snapshot writes go to new blocks (btrfs COW). They cannot corrupt the snapshot's data - it's refcounted and protected.

---

## Performance Tuning

### bcache Settings for Remote Backing

```bash
# Disable congestion detection (remote is "slow" by design)
echo 0 > /sys/block/bcache0/bcache/congested_read_threshold_us
echo 0 > /sys/block/bcache0/bcache/congested_write_threshold_us

# Cache everything (don't bypass sequential - it's a slow link)
echo 0 > /sys/block/bcache0/bcache/sequential_cutoff

# Enable readahead on cache miss (fetch more data per round-trip)
echo 1M > /sys/block/bcache0/bcache/cache/cache0/cache_readahead

# Allow more dirty data (buffer for slow writeback)
echo 70 > /sys/block/bcache0/bcache/writeback_percent
```

### NBD Optimizations for High Latency

```bash
# Multiple connections for pipelining
nbd-client -N backing -C 4 server 10809 /dev/nbd0

# Large readahead
echo 8192 > /sys/block/nbd0/queue/read_ahead_kb
echo 8192 > /sys/block/bcache0/queue/read_ahead_kb
```

### Expected Performance

| Access Pattern | Performance |
|----------------|-------------|
| Write (any) | NVMe-fast (async writeback) |
| Read (cached) | NVMe-fast |
| Sequential read (cold) | Bandwidth-limited (acceptable with readahead) |
| Random read (cold) | Latency-bound (slow, but rare) |

---

## Deployment Scenarios

### Scenario A: Hetzner Datacenter (Same DC)

- Customer VM + Storage Server in same Hetzner DC
- ~0.5-1ms RTT between machines
- 1Gbps network (10Gbps available for +€43/mo)
- Cache misses are fast (~100 MB/s)

**Best performance. Recommended for hosted offering.**

### Scenario B: Home NUC + Cloud Backing

- Intel NUC at home with local NVMe
- Storage server in Hetzner cloud
- ~20-100ms RTT over internet
- Encrypted tunnel (WireGuard)

**Works if**:
- Hot working set fits in NVMe (90%+ cache hit rate)
- Rarely access cold data
- Upload bandwidth keeps up with data generation
- Accept slow cold reads

**Benefits**:
- "Infinite" overflow for cruft accumulation
- Off-site backup built-in (all data reaches cloud)
- NUC dies → restore from cloud backing

---

## Metadata Considerations

Filesystem metadata (inodes, directories, extent trees) is problematic over high-latency links:
- Tiny blocks (4KB)
- Random access pattern
- Touched constantly (`ls`, `stat`, `find`)

### btrfs + bcache Behavior

- Metadata is **not** pinned to SSD (btrfs doesn't support this yet)
- bcache will cache metadata naturally (small, frequently accessed)
- After warm-up, expect 99%+ metadata cache hits
- Cold boot / first access to old directories = slow

**Mitigation**: Warm cache after boot:
```bash
find /mnt -type f > /dev/null 2>&1 &
```

### bcachefs Alternative (If Stable)

bcachefs has explicit metadata targeting:
```bash
bcachefs format \
  --metadata_target=ssd \
  --foreground_target=ssd \
  --background_target=hdd
```

Metadata **always** stays on SSD, never touches slow tier. But bcachefs was removed from Linux kernel (Sept 2025), now DKMS-only. Not recommended for production hosting yet.

---

## Comparison: Hetzner vs AWS

| Component | Hetzner | AWS |
|-----------|---------|-----|
| NVMe $/TB (instance storage) | ~€27/TB | ~$132/TB |
| Compute (8 vCPU, 64GB) | ~€70/mo | ~$300-500/mo |
| Attachable SSD | €52/TB (3x replicated) | $80/TB (gp3) |
| Internal bandwidth | Free | Free (same AZ) |

**Hetzner is 4-5x cheaper** for this architecture.

---

## Implementation Phases

### Phase 1: Manual MVP
1. Provision AX52 bare metal + SX65 storage server
2. Set up RAID10 on SX65 HDDs
3. Configure NBD server on SX65
4. Set up bcache on AX52 (NVMe cache + NBD backing)
5. Install btrfs, thundersnap
6. Manual snapshot archival testing

### Phase 2: Automation
1. Automated bcache + NBD setup via cloud-init
2. Consistent snapshot workflow in thundersnapd
3. Per-customer sparse file provisioning on storage server
4. Monitoring (cache hit rates, dirty data, backing health)

### Phase 3: Multi-Customer
1. NBD server handling multiple customers
2. Stripe billing integration
3. Customer dashboard
4. Auto-provisioning workflow

---

## Pricing Model

| Tier | Machine | NVMe Cache | HDD Backing | Your Cost | Customer Price |
|------|---------|------------|-------------|-----------|----------------|
| Standard | AX42 | 1TB | 1TB | ~€60/mo | $99/mo |
| Pro | AX52 | 2TB | 2.5TB | ~€67/mo | $149/mo |
| Power | AX102 | 4TB | 5TB | ~€110/mo | $249/mo |

**Included**:
- Bare metal machine (dedicated, no neighbors)
- "Infinite" feeling storage (bcache tiering)
- Off-site durability (all data on RAID10 backing)
- Thundersnap pre-configured

**No overages, ever.**

---

## Open Questions

1. **Snapshot frequency** - How often to take consistent checkpoints? (Hourly? Every 15 min?)

2. **Cold storage archival** - Also archive old snapshots to Storage Box (€3.20/TB) for long-term retention?

3. **Cache warm-up** - Pre-warm common paths on boot? Or let it warm naturally?

4. **Customer-initiated snapshots** - Let users trigger consistent checkpoints on demand?

---

## Sources

- [bcache Kernel Documentation](https://docs.kernel.org/admin-guide/bcache.html)
- [Hetzner AX Servers](https://www.hetzner.com/dedicated-rootserver/matrix-ax/)
- [Hetzner SX Storage Servers](https://www.hetzner.com/dedicated-rootserver/matrix-sx/)
- [btrfs Hardware Considerations](https://btrfs.readthedocs.io/en/latest/Hardware.html)
- [bcachefs Caching/Tiering](https://bcachefs.org/Caching/)
- [NBD Protocol](https://nbd.sourceforge.io/)

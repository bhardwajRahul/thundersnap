# Claude Code Project Notes

## Development Workflow

**Always run tests after making changes:**

```bash
make test    # Runs all tests with proper CGO settings
```

**Building binaries:**

```bash
make binaries   # Build all binaries to ./bin/
make ts         # Build just the ts binary
```

## CGO Requirements

The `cmd/ts` and `cmd/vshd` binaries require `CGO_ENABLED=0` because they run
inside containers/VMs where dynamically linked binaries may not work. The
Makefile handles this automatically.

If you see a CGO error when running tests directly (`go test ./...`), use
`make test` instead.

## Project Structure

- `cmd/ts` - Client CLI that runs inside containers/VMs (requires CGO_ENABLED=0)
- `cmd/thundersnapd` - Host daemon that manages snapshots and containers
- `cmd/vsh` / `cmd/vshd` - vsock shell client/daemon for VM access
- `bupdate` - Binary update/sync library
- `thundersnap` - VM management library
- `tsm` - Thundersnap manager library

// Package infiniblock implements an NBD (Network Block Device) server
// backed by a sparse file. It advertises a large virtual size (15 TiB by
// default; see DefaultExportSize) but only allocates disk space for blocks
// that are actually written.
// TRIM commands punch holes in the backing file to reclaim space.
package infiniblock

import (
	"encoding/binary"
	"io"
)

// NBD protocol magic numbers
const (
	nbdMagic         = 0x4e42444d41474943 // "NBDMAGIC"
	ihaveoptMagic    = 0x49484156454f5054 // "IHAVEOPT"
	optReplyMagic    = 0x3e889045565a9
	requestMagic     = 0x25609513
	simpleReplyMagic = 0x67446698
)

// NBD handshake flags (server -> client)
const (
	nbdFlagFixedNewstyle = 1 << 0 // Server supports fixed newstyle
	nbdFlagNoZeroes      = 1 << 1 // Server won't send 124 zero bytes
)

// NBD client flags (client -> server)
const (
	nbdFlagCFixedNewstyle = 1 << 0 // Client supports fixed newstyle
	nbdFlagCNoZeroes      = 1 << 1 // Client won't expect 124 zero bytes
)

// NBD transmission flags (describe export capabilities)
const (
	nbdFlagHasFlags        = 1 << 0  // Export has flags
	nbdFlagReadOnly        = 1 << 1  // Export is read-only
	nbdFlagSendFlush       = 1 << 2  // Server supports FLUSH
	nbdFlagSendFUA         = 1 << 3  // Server supports FUA (force unit access)
	nbdFlagRotational      = 1 << 4  // Export is rotational (HDD-like)
	nbdFlagSendTrim        = 1 << 5  // Server supports TRIM
	nbdFlagSendWriteZeroes = 1 << 6  // Server supports WRITE_ZEROES
	nbdFlagSendDF          = 1 << 7  // Server supports DF (don't fragment)
	nbdFlagCanMultiConn    = 1 << 8  // Multiple connections OK
	nbdFlagSendResize      = 1 << 9  // Server supports RESIZE (experimental)
	nbdFlagSendCache       = 1 << 10 // Server supports CACHE
	nbdFlagSendFastZero    = 1 << 11 // WRITE_ZEROES is fast
)

// NBD option codes (client -> server during handshake)
const (
	nbdOptExportName      = 1  // Select export (no reply, enters transmission)
	nbdOptAbort           = 2  // Abort negotiation
	nbdOptList            = 3  // List available exports
	nbdOptStartTLS        = 5  // Start TLS
	nbdOptInfo            = 6  // Get info about export
	nbdOptGo              = 7  // Select export (with reply, enters transmission)
	nbdOptStructuredReply = 8  // Enable structured replies
	nbdOptListMetaContext = 9  // List meta contexts
	nbdOptSetMetaContext  = 10 // Set meta context
)

// NBD option reply types (server -> client during handshake)
const (
	nbdRepAck          = 1         // Success
	nbdRepServer       = 2         // Server (export) info
	nbdRepInfo         = 3         // NBD_OPT_INFO/GO reply
	nbdRepMetaContext  = 4         // Meta context reply
	nbdRepErrUnsup     = 1<<31 | 1 // Unsupported option
	nbdRepErrPolicy    = 1<<31 | 2 // Policy forbids
	nbdRepErrInvalid   = 1<<31 | 3 // Invalid option
	nbdRepErrPlatform  = 1<<31 | 4 // Platform error
	nbdRepErrTLSReqd   = 1<<31 | 5 // TLS required
	nbdRepErrUnknown   = 1<<31 | 6 // Unknown export
	nbdRepErrShutdown  = 1<<31 | 7 // Server shutting down
	nbdRepErrBlockSize = 1<<31 | 8 // Block size error
	nbdRepErrTooBig    = 1<<31 | 9 // Option data too big
)

// NBD info types for NBD_OPT_INFO/NBD_OPT_GO replies
const (
	nbdInfoExport      = 0 // Export size and flags
	nbdInfoName        = 1 // Export name
	nbdInfoDescription = 2 // Export description
	nbdInfoBlockSize   = 3 // Block size constraints
)

// NBD command types (transmission phase)
const (
	nbdCmdRead        = 0
	nbdCmdWrite       = 1
	nbdCmdDisc        = 2 // Disconnect
	nbdCmdFlush       = 3
	nbdCmdTrim        = 4
	nbdCmdCache       = 5
	nbdCmdWriteZeroes = 6
	nbdCmdBlockStatus = 7
	nbdCmdResize      = 8
)

// NBD command flags
const (
	nbdCmdFlagFUA      = 1 << 0 // Force unit access
	nbdCmdFlagNoHole   = 1 << 1 // Don't punch hole (for WRITE_ZEROES)
	nbdCmdFlagDF       = 1 << 2 // Don't fragment
	nbdCmdFlagReqOne   = 1 << 3 // Request one (for BLOCK_STATUS)
	nbdCmdFlagFastZero = 1 << 4 // Fail fast if slow (for WRITE_ZEROES)
)

// NBD error codes
const (
	nbdEOK       = 0
	nbdEPerm     = 1
	nbdEIO       = 5
	nbdENomem    = 12
	nbdEInval    = 22
	nbdENospc    = 28
	nbdEOverflow = 75
	nbdENotSup   = 95
	nbdEShutdown = 108
)

// DefaultExportSize is 15 TiB - a large sparse virtual size.
//
// This limit exists for busybox nbd-client compatibility. Busybox uses the
// NBD_SET_SIZE_BLOCKS ioctl with an unsigned long argument. When busybox is
// compiled as 32-bit (common even on 64-bit kernels), unsigned long is 32 bits,
// limiting block count to 2^32. With 4096-byte blocks, that's ~16 TiB max.
// Larger sizes overflow to 0, causing "No space left on device" errors.
//
// The official nbd-client (64-bit) can handle much larger sizes. If busybox
// support is dropped, this could be raised to 1 EiB (1 << 60) or higher.
//
// btrfs can be created with -b to use only part of this.
const DefaultExportSize = 15 << 40 // 15 TiB

// Request is an NBD request header from the client.
type Request struct {
	Magic  uint32
	Flags  uint16
	Type   uint16
	Handle uint64
	Offset uint64
	Length uint32
}

// RequestSize is the size of an NBD request header.
const RequestSize = 28

// ReadRequest reads an NBD request from r.
func ReadRequest(r io.Reader) (Request, error) {
	var req Request
	if err := binary.Read(r, binary.BigEndian, &req); err != nil {
		return req, err
	}
	return req, nil
}

// SimpleReply is an NBD simple reply header.
type SimpleReply struct {
	Magic  uint32
	Error  uint32
	Handle uint64
}

// WriteSimpleReply writes an NBD simple reply to w.
func WriteSimpleReply(w io.Writer, handle uint64, errCode uint32) error {
	reply := SimpleReply{
		Magic:  simpleReplyMagic,
		Error:  errCode,
		Handle: handle,
	}
	return binary.Write(w, binary.BigEndian, &reply)
}

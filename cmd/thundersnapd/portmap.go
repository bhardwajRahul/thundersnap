// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Portmapper (rpcbind) implementation for thundersnapd.
// Provides a minimal portmapper that returns fixed ports for NFS and MOUNT services.
// Supports both TCP (with record marking) and UDP (without record marking).
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
)

// RPC program numbers
const (
	portmapProg = 100000
	mountProg   = 100005
	nfsProg     = 100003
	nlmProg     = 100021 // NFS Lock Manager (lockd)
	nfsAclProg  = 100227 // NFS ACL program
)

// Portmapper v2 procedures
const (
	pmapProcNull    = 0
	pmapProcSet     = 1
	pmapProcUnset   = 2
	pmapProcGetport = 3
	pmapProcDump    = 4
)

// rpcbind v3/v4 procedures (superset of v2)
const (
	rpcbProcGetaddr = 3 // Same proc number, different args/response format
)

// portmapServer handles portmapper requests.
type portmapServer struct {
	nfsPort   uint32
	mountPort uint32
}

// ServeTCP handles TCP connections on the portmapper listener.
func (p *portmapServer) ServeTCP(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go p.handleTCPConn(conn)
	}
}

// ServeUDP handles UDP packets on the portmapper connection.
func (p *portmapServer) ServeUDP(conn net.PacketConn) error {
	buf := make([]byte, 65536)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		go p.handleUDPPacket(conn, addr, buf[:n])
	}
}

func (p *portmapServer) handleTCPConn(conn net.Conn) {
	defer conn.Close()

	for {
		// Read RPC record marker (4 bytes, high bit = last fragment)
		var fragHeader [4]byte
		if _, err := io.ReadFull(conn, fragHeader[:]); err != nil {
			return
		}
		fragLen := binary.BigEndian.Uint32(fragHeader[:]) & 0x7FFFFFFF

		// Read the RPC message
		msg := make([]byte, fragLen)
		if _, err := io.ReadFull(conn, msg); err != nil {
			return
		}

		resp := p.handleRPCMessage(msg, nil, nil)
		if resp != nil {
			p.sendTCPFragment(conn, resp)
		}
	}
}

func (p *portmapServer) handleUDPPacket(conn net.PacketConn, addr net.Addr, msg []byte) {
	// UDP has no record marking - the message is the raw RPC
	resp := p.handleRPCMessage(msg, conn, addr)
	if resp != nil {
		conn.WriteTo(resp, addr)
	}
}

// handleRPCMessage processes an RPC message and returns the response (without record marking).
// conn and addr are provided for UDP packets so we can send callbacks.
func (p *portmapServer) handleRPCMessage(msg []byte, conn net.PacketConn, addr net.Addr) []byte {
	// Parse RPC header
	if len(msg) < 24 {
		log.Printf("portmap: message too short (%d bytes)", len(msg))
		return nil
	}
	xid := binary.BigEndian.Uint32(msg[0:4])
	msgType := binary.BigEndian.Uint32(msg[4:8])
	if msgType != 0 { // 0 = CALL
		log.Printf("portmap: ignoring non-CALL message type %d", msgType)
		return nil
	}

	rpcvers := binary.BigEndian.Uint32(msg[8:12])
	prog := binary.BigEndian.Uint32(msg[12:16])
	vers := binary.BigEndian.Uint32(msg[16:20])
	proc := binary.BigEndian.Uint32(msg[20:24])

	log.Printf("portmap: RPC call xid=%d rpcvers=%d prog=%d vers=%d proc=%d", xid, rpcvers, prog, vers, proc)

	if prog == nlmProg {
		// NFS Lock Manager - fake it by saying yes to everything
		return p.handleNLM(xid, vers, proc, msg[24:], conn, addr)
	}

	if prog != portmapProg {
		log.Printf("portmap: rejecting unknown program %d", prog)
		return p.makeReject(xid)
	}

	switch proc {
	case pmapProcNull:
		return p.makeNull(xid)
	case pmapProcGetport: // Also rpcbProcGetaddr (same proc number)
		if vers >= 3 {
			// rpcbind v3/v4 uses GETADDR with different arg format
			return p.makeGetaddrResponse(xid, msg[24:])
		}
		return p.makeGetportResponse(xid, msg[24:])
	default:
		log.Printf("portmap: rejecting unknown procedure %d", proc)
		return p.makeReject(xid)
	}
}

func (p *portmapServer) makeNull(xid uint32) []byte {
	// RPC reply: accepted, success, no data
	resp := make([]byte, 24)
	binary.BigEndian.PutUint32(resp[0:4], xid)
	binary.BigEndian.PutUint32(resp[4:8], 1)   // REPLY
	binary.BigEndian.PutUint32(resp[8:12], 0)  // MSG_ACCEPTED
	binary.BigEndian.PutUint32(resp[12:16], 0) // AUTH_NULL
	binary.BigEndian.PutUint32(resp[16:20], 0) // length 0
	binary.BigEndian.PutUint32(resp[20:24], 0) // SUCCESS
	return resp
}

// makeGetaddrResponse handles rpcbind v3/v4 GETADDR requests.
// Args: r_prog (uint32), r_vers (uint32), r_netid (string), r_addr (string), r_owner (string)
// Returns: universal address (string) or empty string if not found
func (p *portmapServer) makeGetaddrResponse(xid uint32, body []byte) []byte {
	if len(body) < 8 {
		return p.makeReject(xid)
	}

	// Skip credentials
	credLen := binary.BigEndian.Uint32(body[4:8])
	offset := uint32(8) + credLen
	if credLen%4 != 0 {
		offset += 4 - (credLen % 4)
	}

	if uint32(len(body)) < offset+8 {
		return p.makeReject(xid)
	}

	// Skip verifier
	verfLen := binary.BigEndian.Uint32(body[offset+4 : offset+8])
	offset += 8 + verfLen
	if verfLen%4 != 0 {
		offset += 4 - (verfLen % 4)
	}

	if uint32(len(body)) < offset+8 {
		return p.makeReject(xid)
	}

	// Read GETADDR args: r_prog, r_vers, r_netid (string), r_addr (string), r_owner (string)
	reqProg := binary.BigEndian.Uint32(body[offset : offset+4])
	reqVers := binary.BigEndian.Uint32(body[offset+4 : offset+8])
	offset += 8

	// Read netid string (XDR string: length + data + padding)
	if uint32(len(body)) < offset+4 {
		return p.makeReject(xid)
	}
	netidLen := binary.BigEndian.Uint32(body[offset : offset+4])
	offset += 4
	if uint32(len(body)) < offset+netidLen {
		return p.makeReject(xid)
	}
	netid := string(body[offset : offset+netidLen])
	offset += netidLen
	if netidLen%4 != 0 {
		offset += 4 - (netidLen % 4)
	}

	// Determine if this is TCP
	isTCP := netid == "tcp" || netid == "tcp6"
	isUDP := netid == "udp" || netid == "udp6"

	var uaddr string
	switch reqProg {
	case nfsProg:
		if isTCP {
			// Universal address format for TCP: "0.0.0.0.p1.p2" where port = p1*256 + p2
			p1 := p.nfsPort / 256
			p2 := p.nfsPort % 256
			uaddr = fmt.Sprintf("0.0.0.0.%d.%d", p1, p2)
		}
		// UDP not supported - uaddr stays empty, client will try TCP
		log.Printf("portmap: GETADDR for NFS vers=%d netid=%s -> %q", reqVers, netid, uaddr)
	case mountProg:
		if isTCP {
			p1 := p.mountPort / 256
			p2 := p.mountPort % 256
			uaddr = fmt.Sprintf("0.0.0.0.%d.%d", p1, p2)
		}
		// UDP not supported - uaddr stays empty, client will try TCP
		log.Printf("portmap: GETADDR for MOUNT vers=%d netid=%s -> %q", reqVers, netid, uaddr)
	case nlmProg:
		// NFS Lock Manager - we handle it on port 111
		if isTCP || isUDP {
			p1 := uint32(111) / 256
			p2 := uint32(111) % 256
			uaddr = fmt.Sprintf("0.0.0.0.%d.%d", p1, p2)
		}
		log.Printf("portmap: GETADDR for NLM (lockd) vers=%d netid=%s -> %q", reqVers, netid, uaddr)
	case nfsAclProg:
		// NFS ACL program - not supported, return empty address
		log.Printf("portmap: GETADDR for NFS_ACL vers=%d netid=%s -> \"\" (not supported)", reqVers, netid)
	default:
		log.Printf("portmap: GETADDR for unknown program %d vers=%d netid=%s -> \"\"", reqProg, reqVers, netid)
	}

	// Build response: XDR string (length + data + padding)
	uaddrBytes := []byte(uaddr)
	padding := (4 - (len(uaddrBytes) % 4)) % 4
	respLen := 24 + 4 + len(uaddrBytes) + padding

	resp := make([]byte, respLen)
	binary.BigEndian.PutUint32(resp[0:4], xid)
	binary.BigEndian.PutUint32(resp[4:8], 1)   // REPLY
	binary.BigEndian.PutUint32(resp[8:12], 0)  // MSG_ACCEPTED
	binary.BigEndian.PutUint32(resp[12:16], 0) // AUTH_NULL
	binary.BigEndian.PutUint32(resp[16:20], 0) // length 0
	binary.BigEndian.PutUint32(resp[20:24], 0) // SUCCESS
	binary.BigEndian.PutUint32(resp[24:28], uint32(len(uaddrBytes)))
	copy(resp[28:], uaddrBytes)

	return resp
}

func (p *portmapServer) makeGetportResponse(xid uint32, body []byte) []byte {
	// Skip auth credentials in body
	// Format: cred flavor (4) + cred len (4) + cred data (len) + verf flavor (4) + verf len (4) + verf data (len)
	// Then: prog (4) + vers (4) + proto (4) + port (4)

	if len(body) < 8 {
		return p.makeReject(xid)
	}

	// Skip credentials
	credLen := binary.BigEndian.Uint32(body[4:8])
	offset := uint32(8) + credLen

	// Align to 4 bytes
	if credLen%4 != 0 {
		offset += 4 - (credLen % 4)
	}

	if uint32(len(body)) < offset+8 {
		return p.makeReject(xid)
	}

	// Skip verifier
	verfLen := binary.BigEndian.Uint32(body[offset+4 : offset+8])
	offset += 8 + verfLen
	if verfLen%4 != 0 {
		offset += 4 - (verfLen % 4)
	}

	if uint32(len(body)) < offset+16 {
		return p.makeReject(xid)
	}

	// Read GETPORT args: prog, vers, proto, port
	reqProg := binary.BigEndian.Uint32(body[offset : offset+4])
	reqVers := binary.BigEndian.Uint32(body[offset+4 : offset+8])
	reqProto := binary.BigEndian.Uint32(body[offset+8 : offset+12])

	// Protocol numbers: 6 = TCP, 17 = UDP
	// Some clients may use other values, treat anything that's not UDP as TCP-capable
	isTCP := reqProto == 6 || (reqProto != 17)

	var port uint32
	switch reqProg {
	case nfsProg:
		// Return port for TCP requests (go-nfs doesn't support UDP)
		if isTCP {
			port = p.nfsPort
		} else {
			port = 0 // UDP not supported
		}
		log.Printf("portmap: GETPORT for NFS vers=%d proto=%d -> %d", reqVers, reqProto, port)
	case mountProg:
		if isTCP {
			port = p.mountPort
		} else {
			port = 0
		}
		log.Printf("portmap: GETPORT for MOUNT vers=%d proto=%d -> %d", reqVers, reqProto, port)
	case nlmProg:
		// NFS Lock Manager - we handle it on port 111 (same as portmap)
		port = 111
		log.Printf("portmap: GETPORT for NLM (lockd) vers=%d proto=%d -> %d", reqVers, reqProto, port)
	case nfsAclProg:
		// NFS ACL program - not supported
		port = 0
		log.Printf("portmap: GETPORT for NFS_ACL vers=%d proto=%d -> 0 (not supported)", reqVers, reqProto)
	default:
		port = 0
		log.Printf("portmap: GETPORT for unknown program %d vers=%d proto=%d -> 0", reqProg, reqVers, reqProto)
	}

	// Return port 0 for UDP requests - client will try TCP next
	// (Returning PROG_UNAVAIL causes unnecessary delays)

	// RPC reply with port
	resp := make([]byte, 28)
	binary.BigEndian.PutUint32(resp[0:4], xid)
	binary.BigEndian.PutUint32(resp[4:8], 1)   // REPLY
	binary.BigEndian.PutUint32(resp[8:12], 0)  // MSG_ACCEPTED
	binary.BigEndian.PutUint32(resp[12:16], 0) // AUTH_NULL
	binary.BigEndian.PutUint32(resp[16:20], 0) // length 0
	binary.BigEndian.PutUint32(resp[20:24], 0) // SUCCESS
	binary.BigEndian.PutUint32(resp[24:28], port)

	return resp
}

// NLM4 procedure numbers
const (
	nlmNull       = 0
	nlmTest       = 1
	nlmLock       = 2
	nlmCancel     = 3
	nlmUnlock     = 4
	nlmGranted    = 5
	nlmTestMsg    = 6
	nlmLockMsg    = 7
	nlmCancelMsg  = 8
	nlmUnlockMsg  = 9
	nlmGrantedMsg = 10
	nlmShare      = 20
	nlmUnshare    = 21
	nlmNmLock     = 22
	nlmFreeAll    = 23
)

// NLM4 status codes
const (
	nlm4Granted = 0
)

// handleNLM handles NFS Lock Manager requests by faking success for everything.
func (p *portmapServer) handleNLM(xid, vers, proc uint32, body []byte, conn net.PacketConn, addr net.Addr) []byte {
	log.Printf("portmap: NLM request vers=%d proc=%d", vers, proc)

	switch proc {
	case nlmNull:
		return p.makeNull(xid)

	case nlmTest, nlmLock, nlmCancel, nlmUnlock, nlmGranted, nlmShare, nlmUnshare, nlmNmLock:
		// These return nlm4_res: cookie + stat
		// We need to echo back the cookie from the request, then return NLM4_GRANTED
		return p.makeNLMRes(xid, body)

	case nlmTestMsg, nlmLockMsg, nlmCancelMsg, nlmUnlockMsg, nlmGrantedMsg:
		// Async message variants - send a void RPC reply, then send a callback
		// RPC CALL to the client with the corresponding _RES procedure
		resProc := proc + 5 // TEST_MSG(6)->TEST_RES(11), LOCK_MSG(7)->LOCK_RES(12), etc.
		cookie := p.extractCookie(body)
		log.Printf("portmap: NLM async proc=%d -> sending callback proc=%d to %v", proc, resProc, addr)

		if conn != nil && addr != nil {
			// Send callback to the client's NLM service (source port).
			callback := p.makeNLMCallback(vers, resProc, cookie)
			conn.WriteTo(callback, addr)
		}
		// Return a void RPC reply
		return p.makeNull(xid)

	case nlmFreeAll:
		// Returns void
		return p.makeNull(xid)

	default:
		log.Printf("portmap: NLM unknown proc=%d", proc)
		return p.makeNull(xid)
	}
}

// extractCookie extracts the cookie from an NLM request body.
func (p *portmapServer) extractCookie(body []byte) []byte {
	// Skip credentials and verifier to get to the NLM args
	if len(body) < 8 {
		return nil
	}

	credLen := binary.BigEndian.Uint32(body[4:8])
	offset := uint32(8) + credLen
	if credLen%4 != 0 {
		offset += 4 - (credLen % 4)
	}

	if uint32(len(body)) < offset+8 {
		return nil
	}

	verfLen := binary.BigEndian.Uint32(body[offset+4 : offset+8])
	offset += 8 + verfLen
	if verfLen%4 != 0 {
		offset += 4 - (verfLen % 4)
	}

	// Now we're at the NLM args. First field is the cookie (opaque<>).
	if uint32(len(body)) < offset+4 {
		return nil
	}

	cookieLen := binary.BigEndian.Uint32(body[offset : offset+4])
	offset += 4
	if uint32(len(body)) < offset+cookieLen {
		return nil
	}
	return body[offset : offset+cookieLen]
}

// makeNLMCallback creates an RPC CALL for an NLM callback (e.g., NLM_LOCK_RES).
// This is sent to the client to complete an async _MSG request.
func (p *portmapServer) makeNLMCallback(vers, proc uint32, cookie []byte) []byte {
	// RPC CALL format:
	// xid (4) + msg_type=0 (4) + rpc_vers=2 (4) + prog=NLM (4) + vers (4) + proc (4)
	// + cred_flavor=0 (4) + cred_len=0 (4) + verf_flavor=0 (4) + verf_len=0 (4)
	// + cookie (opaque<>) + stat=NLM4_GRANTED (4)

	cookiePadding := (4 - (len(cookie) % 4)) % 4
	callLen := 40 + 4 + len(cookie) + cookiePadding + 4

	call := make([]byte, callLen)

	// Use a random-ish XID for the callback
	xid := uint32(0x12345678) // Could use time or random, but doesn't matter much
	binary.BigEndian.PutUint32(call[0:4], xid)
	binary.BigEndian.PutUint32(call[4:8], 0)         // CALL
	binary.BigEndian.PutUint32(call[8:12], 2)        // RPC version 2
	binary.BigEndian.PutUint32(call[12:16], nlmProg) // NLM program
	binary.BigEndian.PutUint32(call[16:20], vers)    // NLM version
	binary.BigEndian.PutUint32(call[20:24], proc)    // Procedure (e.g., NLM_LOCK_RES)
	binary.BigEndian.PutUint32(call[24:28], 0)       // AUTH_NULL
	binary.BigEndian.PutUint32(call[28:32], 0)       // cred length 0
	binary.BigEndian.PutUint32(call[32:36], 0)       // AUTH_NULL verifier
	binary.BigEndian.PutUint32(call[36:40], 0)       // verf length 0

	// Cookie
	binary.BigEndian.PutUint32(call[40:44], uint32(len(cookie)))
	copy(call[44:], cookie)

	// Status: NLM4_GRANTED = 0
	statOffset := 44 + len(cookie) + cookiePadding
	binary.BigEndian.PutUint32(call[statOffset:statOffset+4], nlm4Granted)

	log.Printf("portmap: NLM callback: proc=%d cookie_len=%d total_len=%d", proc, len(cookie), callLen)
	return call
}

// makeNLMRes creates an NLM response with NLM4_GRANTED status.
// The response format is: cookie (opaque) + stat (enum)
func (p *portmapServer) makeNLMRes(xid uint32, body []byte) []byte {
	// Skip credentials and verifier to get to the NLM args
	if len(body) < 8 {
		return p.makeNull(xid)
	}

	credLen := binary.BigEndian.Uint32(body[4:8])
	offset := uint32(8) + credLen
	if credLen%4 != 0 {
		offset += 4 - (credLen % 4)
	}

	if uint32(len(body)) < offset+8 {
		return p.makeNull(xid)
	}

	verfLen := binary.BigEndian.Uint32(body[offset+4 : offset+8])
	offset += 8 + verfLen
	if verfLen%4 != 0 {
		offset += 4 - (verfLen % 4)
	}

	// Now we're at the NLM args. First field is the cookie (opaque<>).
	if uint32(len(body)) < offset+4 {
		return p.makeNull(xid)
	}

	cookieLen := binary.BigEndian.Uint32(body[offset : offset+4])
	offset += 4
	if uint32(len(body)) < offset+cookieLen {
		return p.makeNull(xid)
	}
	cookie := body[offset : offset+cookieLen]

	// Build response: RPC header + cookie + stat (NLM4_GRANTED = 0)
	cookiePadding := (4 - (len(cookie) % 4)) % 4
	respLen := 24 + 4 + len(cookie) + cookiePadding + 4

	resp := make([]byte, respLen)
	binary.BigEndian.PutUint32(resp[0:4], xid)
	binary.BigEndian.PutUint32(resp[4:8], 1)   // REPLY
	binary.BigEndian.PutUint32(resp[8:12], 0)  // MSG_ACCEPTED
	binary.BigEndian.PutUint32(resp[12:16], 0) // AUTH_NULL
	binary.BigEndian.PutUint32(resp[16:20], 0) // verf length 0
	binary.BigEndian.PutUint32(resp[20:24], 0) // SUCCESS

	// Cookie (opaque<>)
	binary.BigEndian.PutUint32(resp[24:28], uint32(len(cookie)))
	copy(resp[28:], cookie)

	// Status: NLM4_GRANTED = 0
	statOffset := 28 + len(cookie) + cookiePadding
	binary.BigEndian.PutUint32(resp[statOffset:statOffset+4], nlm4Granted)

	log.Printf("portmap: NLM response -> NLM4_GRANTED (cookie len=%d)", len(cookie))
	return resp
}

func (p *portmapServer) makeReject(xid uint32) []byte {
	// RPC reply: denied, auth error
	resp := make([]byte, 20)
	binary.BigEndian.PutUint32(resp[0:4], xid)
	binary.BigEndian.PutUint32(resp[4:8], 1)   // REPLY
	binary.BigEndian.PutUint32(resp[8:12], 1)  // MSG_DENIED
	binary.BigEndian.PutUint32(resp[12:16], 1) // AUTH_ERROR
	binary.BigEndian.PutUint32(resp[16:20], 1) // AUTH_BADCRED
	return resp
}

func (p *portmapServer) makeProgUnavail(xid uint32) []byte {
	// RPC reply: accepted but program unavailable
	// accept_stat = PROG_UNAVAIL (1)
	resp := make([]byte, 24)
	binary.BigEndian.PutUint32(resp[0:4], xid)
	binary.BigEndian.PutUint32(resp[4:8], 1)   // REPLY
	binary.BigEndian.PutUint32(resp[8:12], 0)  // MSG_ACCEPTED
	binary.BigEndian.PutUint32(resp[12:16], 0) // AUTH_NULL
	binary.BigEndian.PutUint32(resp[16:20], 0) // verf length 0
	binary.BigEndian.PutUint32(resp[20:24], 1) // PROG_UNAVAIL
	return resp
}

func (p *portmapServer) sendTCPFragment(conn net.Conn, data []byte) {
	// Write fragment header (length with high bit set for last fragment)
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data))|0x80000000)
	conn.Write(header[:])
	conn.Write(data)
}

// startPortmapper starts the portmapper server on port 111 (both TCP and UDP).
func startPortmapper(tcpLn net.Listener, udpConn net.PacketConn, nfsPort int) *portmapServer {
	pm := &portmapServer{
		nfsPort:   uint32(nfsPort),
		mountPort: uint32(nfsPort), // go-nfs serves MOUNT on the same port as NFS
	}

	go func() {
		log.Printf("Portmapper TCP listening (NFS port=%d, MOUNT port=%d)", nfsPort, nfsPort)
		if err := pm.ServeTCP(tcpLn); err != nil {
			log.Printf("Portmapper TCP error: %v", err)
		}
	}()

	go func() {
		log.Printf("Portmapper UDP listening")
		if err := pm.ServeUDP(udpConn); err != nil {
			log.Printf("Portmapper UDP error: %v", err)
		}
	}()

	return pm
}

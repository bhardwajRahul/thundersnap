package infiniblock

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
)

// Server is an NBD server backed by a sparse file.
type Server struct {
	backend    Backend
	exportSize uint64
	exportName string
	listener   net.Listener

	mu       sync.Mutex
	shutdown bool
	conns    map[net.Conn]struct{}
}

// ServerConfig configures an NBD server.
type ServerConfig struct {
	// Backend is the storage backend. Required.
	Backend Backend
	// ExportSize is the advertised size. Defaults to 1 EiB.
	ExportSize uint64
	// ExportName is the export name. Defaults to "default".
	ExportName string
}

// NewServer creates a new NBD server with the given config.
func NewServer(cfg ServerConfig) *Server {
	if cfg.ExportSize == 0 {
		cfg.ExportSize = DefaultExportSize
	}
	if cfg.ExportName == "" {
		cfg.ExportName = "default"
	}
	return &Server{
		backend:    cfg.Backend,
		exportSize: cfg.ExportSize,
		exportName: cfg.ExportName,
		conns:      make(map[net.Conn]struct{}),
	}
}

// ListenAndServe listens on addr and serves NBD connections.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = ln
	return s.Serve(ln)
}

// Serve accepts connections from ln and handles them.
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			shutdown := s.shutdown
			s.mu.Unlock()
			if shutdown {
				return nil
			}
			return err
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go s.handleConn(conn)
	}
}

// Close shuts down the server.
func (s *Server) Close() error {
	s.mu.Lock()
	s.shutdown = true
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// Addr returns the listener address, or empty if not listening.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) handleConn(conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()

	if err := s.handshake(conn); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			log.Printf("nbd handshake error: %v", err)
		}
		return
	}

	if err := s.transmission(conn); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			log.Printf("nbd transmission error: %v", err)
		}
	}
}

// handshake performs fixed newstyle negotiation.
func (s *Server) handshake(conn net.Conn) error {
	bw := bufio.NewWriter(conn)
	br := bufio.NewReader(conn)

	// Send server greeting
	// S: 64 bits, NBDMAGIC
	// S: 64 bits, IHAVEOPT (indicates newstyle)
	// S: 16 bits, handshake flags
	if err := binary.Write(bw, binary.BigEndian, uint64(nbdMagic)); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, uint64(ihaveoptMagic)); err != nil {
		return err
	}
	flags := uint16(nbdFlagFixedNewstyle | nbdFlagNoZeroes)
	if err := binary.Write(bw, binary.BigEndian, flags); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}

	// Read client flags
	var clientFlags uint32
	if err := binary.Read(br, binary.BigEndian, &clientFlags); err != nil {
		return err
	}
	// Track whether client supports fixed newstyle (for error handling).
	// We accept clients that don't set this flag (like busybox nbd-client)
	// but they can only use NBD_OPT_EXPORT_NAME.
	fixedNewstyle := clientFlags&nbdFlagCFixedNewstyle != 0

	// Option haggling loop
	for {
		// C: 64 bits, IHAVEOPT
		// C: 32 bits, option
		// C: 32 bits, length of option data
		// C: length bytes, option data
		var optMagic uint64
		if err := binary.Read(br, binary.BigEndian, &optMagic); err != nil {
			return err
		}
		if optMagic != ihaveoptMagic {
			return fmt.Errorf("bad option magic: %x", optMagic)
		}

		var optCode uint32
		if err := binary.Read(br, binary.BigEndian, &optCode); err != nil {
			return err
		}
		var optLen uint32
		if err := binary.Read(br, binary.BigEndian, &optLen); err != nil {
			return err
		}

		// Read option data
		optData := make([]byte, optLen)
		if optLen > 0 {
			if _, err := io.ReadFull(br, optData); err != nil {
				return err
			}
		}

		switch optCode {
		case nbdOptExportName:
			// NBD_OPT_EXPORT_NAME: no reply, just enter transmission
			// Send export info directly
			// S: 64 bits, size
			// S: 16 bits, transmission flags
			// S: 124 bytes zeroes (unless NBD_FLAG_C_NO_ZEROES)
			if err := binary.Write(bw, binary.BigEndian, s.exportSize); err != nil {
				return err
			}
			txFlags := uint16(nbdFlagHasFlags | nbdFlagSendFlush | nbdFlagSendTrim)
			if err := binary.Write(bw, binary.BigEndian, txFlags); err != nil {
				return err
			}
			if clientFlags&nbdFlagCNoZeroes == 0 {
				zeros := make([]byte, 124)
				if _, err := bw.Write(zeros); err != nil {
					return err
				}
			}
			if err := bw.Flush(); err != nil {
				return err
			}
			return nil // Enter transmission phase

		case nbdOptAbort:
			// Client wants to disconnect
			s.sendOptReply(bw, optCode, nbdRepAck, nil)
			bw.Flush()
			return io.EOF

		case nbdOptGo, nbdOptInfo:
			// NBD_OPT_GO / NBD_OPT_INFO: structured reply with export info
			// For simplicity, we ignore the requested export name and info requests
			// and just return our default export info

			// Send NBD_INFO_EXPORT
			info := make([]byte, 12)
			binary.BigEndian.PutUint16(info[0:2], nbdInfoExport)
			binary.BigEndian.PutUint64(info[2:10], s.exportSize)
			txFlags := uint16(nbdFlagHasFlags | nbdFlagSendFlush | nbdFlagSendTrim)
			binary.BigEndian.PutUint16(info[10:12], txFlags)
			if err := s.sendOptReply(bw, optCode, nbdRepInfo, info); err != nil {
				return err
			}

			// Send NBD_REP_ACK to complete
			if err := s.sendOptReply(bw, optCode, nbdRepAck, nil); err != nil {
				return err
			}
			if err := bw.Flush(); err != nil {
				return err
			}

			if optCode == nbdOptGo {
				return nil // Enter transmission phase
			}
			// NBD_OPT_INFO: continue option loop

		case nbdOptList:
			// List available exports - send our one export
			nameBytes := []byte(s.exportName)
			data := make([]byte, 4+len(nameBytes))
			binary.BigEndian.PutUint32(data[0:4], uint32(len(nameBytes)))
			copy(data[4:], nameBytes)
			if err := s.sendOptReply(bw, optCode, nbdRepServer, data); err != nil {
				return err
			}
			if err := s.sendOptReply(bw, optCode, nbdRepAck, nil); err != nil {
				return err
			}
			if err := bw.Flush(); err != nil {
				return err
			}

		default:
			// Unknown option
			if !fixedNewstyle {
				// Original newstyle: must disconnect on unknown option
				return fmt.Errorf("unknown option %d from non-fixed-newstyle client", optCode)
			}
			// Fixed newstyle: send NBD_REP_ERR_UNSUP
			if err := s.sendOptReply(bw, optCode, nbdRepErrUnsup, nil); err != nil {
				return err
			}
			if err := bw.Flush(); err != nil {
				return err
			}
		}
	}
}

// sendOptReply sends an option reply.
func (s *Server) sendOptReply(w io.Writer, opt uint32, replyType uint32, data []byte) error {
	// S: 64 bits, NBD_OPT_REPLY_MAGIC
	// S: 32 bits, option code
	// S: 32 bits, reply type
	// S: 32 bits, length
	// S: length bytes, data
	if err := binary.Write(w, binary.BigEndian, uint64(optReplyMagic)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, opt); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, replyType); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// transmission handles the transmission phase (actual I/O).
func (s *Server) transmission(conn net.Conn) error {
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)

	for {
		req, err := ReadRequest(br)
		if err != nil {
			return err
		}

		if req.Magic != requestMagic {
			return fmt.Errorf("bad request magic: %x", req.Magic)
		}

		switch req.Type {
		case nbdCmdRead:
			if err := s.handleRead(bw, req); err != nil {
				return err
			}

		case nbdCmdWrite:
			// Read the data from client
			data := make([]byte, req.Length)
			if _, err := io.ReadFull(br, data); err != nil {
				return err
			}
			if err := s.handleWrite(bw, req, data); err != nil {
				return err
			}

		case nbdCmdDisc:
			// Disconnect - no reply needed
			return nil

		case nbdCmdFlush:
			if err := s.handleFlush(bw, req); err != nil {
				return err
			}

		case nbdCmdTrim:
			if err := s.handleTrim(bw, req); err != nil {
				return err
			}

		default:
			// Unknown command - send error
			if err := WriteSimpleReply(bw, req.Handle, nbdENotSup); err != nil {
				return err
			}
			if err := bw.Flush(); err != nil {
				return err
			}
		}
	}
}

func (s *Server) handleRead(w *bufio.Writer, req Request) error {
	data := make([]byte, req.Length)
	_, err := s.backend.ReadAt(data, int64(req.Offset))
	if err != nil && !errors.Is(err, io.EOF) {
		// Send error reply
		if err := WriteSimpleReply(w, req.Handle, nbdEIO); err != nil {
			return err
		}
		return w.Flush()
	}

	// Send success reply + data
	if err := WriteSimpleReply(w, req.Handle, nbdEOK); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return w.Flush()
}

func (s *Server) handleWrite(w *bufio.Writer, req Request, data []byte) error {
	_, err := s.backend.WriteAt(data, int64(req.Offset))
	if err != nil {
		if err := WriteSimpleReply(w, req.Handle, nbdEIO); err != nil {
			return err
		}
		return w.Flush()
	}

	// Handle FUA flag
	if req.Flags&nbdCmdFlagFUA != 0 {
		if err := s.backend.Sync(); err != nil {
			if err := WriteSimpleReply(w, req.Handle, nbdEIO); err != nil {
				return err
			}
			return w.Flush()
		}
	}

	if err := WriteSimpleReply(w, req.Handle, nbdEOK); err != nil {
		return err
	}
	return w.Flush()
}

func (s *Server) handleFlush(w *bufio.Writer, req Request) error {
	err := s.backend.Sync()
	if err != nil {
		if err := WriteSimpleReply(w, req.Handle, nbdEIO); err != nil {
			return err
		}
		return w.Flush()
	}
	if err := WriteSimpleReply(w, req.Handle, nbdEOK); err != nil {
		return err
	}
	return w.Flush()
}

func (s *Server) handleTrim(w *bufio.Writer, req Request) error {
	err := s.backend.Trim(int64(req.Offset), int64(req.Length))
	if err != nil {
		if err := WriteSimpleReply(w, req.Handle, nbdEIO); err != nil {
			return err
		}
		return w.Flush()
	}
	if err := WriteSimpleReply(w, req.Handle, nbdEOK); err != nil {
		return err
	}
	return w.Flush()
}

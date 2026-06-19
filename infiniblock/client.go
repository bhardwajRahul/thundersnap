package infiniblock

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Client is a minimal NBD client for testing.
// It implements the fixed newstyle protocol.
type Client struct {
	conn   net.Conn
	br     *bufio.Reader
	bw     *bufio.Writer
	size   uint64
	flags  uint16
	handle atomic.Uint64

	mu sync.Mutex // protects writes
}

// Dial connects to an NBD server and performs handshake.
func Dial(addr string) (*Client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	c := &Client{
		conn: conn,
		br:   bufio.NewReader(conn),
		bw:   bufio.NewWriter(conn),
	}

	if err := c.handshake(); err != nil {
		conn.Close()
		return nil, err
	}

	return c, nil
}

func (c *Client) handshake() error {
	// Read server greeting
	var magic uint64
	if err := binary.Read(c.br, binary.BigEndian, &magic); err != nil {
		return err
	}
	if magic != nbdMagic {
		return fmt.Errorf("bad magic: %x", magic)
	}

	var optMagic uint64
	if err := binary.Read(c.br, binary.BigEndian, &optMagic); err != nil {
		return err
	}
	if optMagic != ihaveoptMagic {
		return fmt.Errorf("server does not support newstyle: %x", optMagic)
	}

	var serverFlags uint16
	if err := binary.Read(c.br, binary.BigEndian, &serverFlags); err != nil {
		return err
	}
	if serverFlags&nbdFlagFixedNewstyle == 0 {
		return fmt.Errorf("server does not support fixed newstyle")
	}

	// Send client flags
	clientFlags := uint32(nbdFlagCFixedNewstyle | nbdFlagCNoZeroes)
	if err := binary.Write(c.bw, binary.BigEndian, clientFlags); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}

	// Send NBD_OPT_GO to select export and enter transmission
	if err := c.sendOption(nbdOptGo, nil); err != nil {
		return err
	}

	// Read option replies until NBD_REP_ACK
	for {
		opt, replyType, data, err := c.readOptReply()
		if err != nil {
			return err
		}
		if opt != nbdOptGo {
			return fmt.Errorf("unexpected option in reply: %d", opt)
		}

		if replyType&(1<<31) != 0 {
			// Error reply
			return fmt.Errorf("server error: %d", replyType)
		}

		if replyType == nbdRepAck {
			// Done with option negotiation
			break
		}

		if replyType == nbdRepInfo && len(data) >= 12 {
			infoType := binary.BigEndian.Uint16(data[0:2])
			if infoType == nbdInfoExport {
				c.size = binary.BigEndian.Uint64(data[2:10])
				c.flags = binary.BigEndian.Uint16(data[10:12])
			}
		}
	}

	return nil
}

func (c *Client) sendOption(opt uint32, data []byte) error {
	// C: 64 bits, IHAVEOPT
	// C: 32 bits, option
	// C: 32 bits, length
	// C: length bytes, data
	if err := binary.Write(c.bw, binary.BigEndian, uint64(ihaveoptMagic)); err != nil {
		return err
	}
	if err := binary.Write(c.bw, binary.BigEndian, opt); err != nil {
		return err
	}
	if err := binary.Write(c.bw, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := c.bw.Write(data); err != nil {
			return err
		}
	}
	return c.bw.Flush()
}

func (c *Client) readOptReply() (opt uint32, replyType uint32, data []byte, err error) {
	var magic uint64
	if err = binary.Read(c.br, binary.BigEndian, &magic); err != nil {
		return
	}
	if magic != optReplyMagic {
		err = fmt.Errorf("bad opt reply magic: %x", magic)
		return
	}
	if err = binary.Read(c.br, binary.BigEndian, &opt); err != nil {
		return
	}
	if err = binary.Read(c.br, binary.BigEndian, &replyType); err != nil {
		return
	}
	var length uint32
	if err = binary.Read(c.br, binary.BigEndian, &length); err != nil {
		return
	}
	if length > 0 {
		data = make([]byte, length)
		if _, err = io.ReadFull(c.br, data); err != nil {
			return
		}
	}
	return
}

// Size returns the export size.
func (c *Client) Size() uint64 {
	return c.size
}

// Flags returns the transmission flags.
func (c *Client) Flags() uint16 {
	return c.flags
}

// SupportsFlush returns true if server supports FLUSH.
func (c *Client) SupportsFlush() bool {
	return c.flags&nbdFlagSendFlush != 0
}

// SupportsTrim returns true if server supports TRIM.
func (c *Client) SupportsTrim() bool {
	return c.flags&nbdFlagSendTrim != 0
}

// Read reads len(p) bytes starting at offset.
func (c *Client) Read(p []byte, offset uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	handle := c.handle.Add(1)
	req := Request{
		Magic:  requestMagic,
		Flags:  0,
		Type:   nbdCmdRead,
		Handle: handle,
		Offset: offset,
		Length: uint32(len(p)),
	}

	if err := binary.Write(c.bw, binary.BigEndian, &req); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}

	reply, err := c.readReply()
	if err != nil {
		return err
	}
	if reply.Handle != handle {
		return fmt.Errorf("handle mismatch: got %d, want %d", reply.Handle, handle)
	}
	if reply.Error != nbdEOK {
		return fmt.Errorf("server error: %d", reply.Error)
	}

	// Read data
	if _, err := io.ReadFull(c.br, p); err != nil {
		return err
	}
	return nil
}

// Write writes p starting at offset.
func (c *Client) Write(p []byte, offset uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	handle := c.handle.Add(1)
	req := Request{
		Magic:  requestMagic,
		Flags:  0,
		Type:   nbdCmdWrite,
		Handle: handle,
		Offset: offset,
		Length: uint32(len(p)),
	}

	if err := binary.Write(c.bw, binary.BigEndian, &req); err != nil {
		return err
	}
	if _, err := c.bw.Write(p); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}

	reply, err := c.readReply()
	if err != nil {
		return err
	}
	if reply.Handle != handle {
		return fmt.Errorf("handle mismatch: got %d, want %d", reply.Handle, handle)
	}
	if reply.Error != nbdEOK {
		return fmt.Errorf("server error: %d", reply.Error)
	}
	return nil
}

// Trim discards data in the range [offset, offset+length).
func (c *Client) Trim(offset, length uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	handle := c.handle.Add(1)
	req := Request{
		Magic:  requestMagic,
		Flags:  0,
		Type:   nbdCmdTrim,
		Handle: handle,
		Offset: offset,
		Length: uint32(length),
	}

	if err := binary.Write(c.bw, binary.BigEndian, &req); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}

	reply, err := c.readReply()
	if err != nil {
		return err
	}
	if reply.Handle != handle {
		return fmt.Errorf("handle mismatch: got %d, want %d", reply.Handle, handle)
	}
	if reply.Error != nbdEOK {
		return fmt.Errorf("server error: %d", reply.Error)
	}
	return nil
}

// Flush flushes data to stable storage.
func (c *Client) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	handle := c.handle.Add(1)
	req := Request{
		Magic:  requestMagic,
		Flags:  0,
		Type:   nbdCmdFlush,
		Handle: handle,
		Offset: 0,
		Length: 0,
	}

	if err := binary.Write(c.bw, binary.BigEndian, &req); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}

	reply, err := c.readReply()
	if err != nil {
		return err
	}
	if reply.Handle != handle {
		return fmt.Errorf("handle mismatch: got %d, want %d", reply.Handle, handle)
	}
	if reply.Error != nbdEOK {
		return fmt.Errorf("server error: %d", reply.Error)
	}
	return nil
}

// Disconnect gracefully disconnects from the server.
func (c *Client) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	handle := c.handle.Add(1)
	req := Request{
		Magic:  requestMagic,
		Flags:  0,
		Type:   nbdCmdDisc,
		Handle: handle,
		Offset: 0,
		Length: 0,
	}

	if err := binary.Write(c.bw, binary.BigEndian, &req); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}
	// No reply expected for disconnect
	return nil
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) readReply() (SimpleReply, error) {
	var reply SimpleReply
	if err := binary.Read(c.br, binary.BigEndian, &reply); err != nil {
		return reply, err
	}
	if reply.Magic != simpleReplyMagic {
		return reply, fmt.Errorf("bad reply magic: %x", reply.Magic)
	}
	return reply, nil
}

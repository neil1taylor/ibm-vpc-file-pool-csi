// Package nbd serves a read-only disk to a client over the network block
// device protocol (fixed newstyle), on a local socket. The browse helper uses it
// to show a backed-up VM disk to the libguestfs appliance, which runs QEMU and
// parses guest file systems that someone else wrote. So the client is not
// trusted: the server answers reads only, bounds every request, and never
// allocates more than a request may ask for. Protocol:
// https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md
package nbd

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Export is a disk that can be read. Size is fixed for the life of a connection.
type Export interface {
	io.ReaderAt
	Size() int64
}

// Options set the name of the export and the limits of the server.
type Options struct {
	// Name is the export name. A client that asks for "" gets it too.
	Name string
	// MaxConns is how many clients may be connected at once; zero means 4.
	MaxConns int
	// MaxRead is the largest read in one request; zero means 8 MiB. It is
	// sent to a client that asks for the block sizes.
	MaxRead int
	// HandshakeTimeout is how long the option phase may take; zero means a minute.
	HandshakeTimeout time.Duration
}

func (o Options) maxRead() int {
	if o.MaxRead > 0 {
		return o.MaxRead
	}
	return 8 << 20
}

func (o Options) maxConns() int {
	if o.MaxConns > 0 {
		return o.MaxConns
	}
	return 4
}

func (o Options) handshakeTimeout() time.Duration {
	if o.HandshakeTimeout > 0 {
		return o.HandshakeTimeout
	}
	return time.Minute
}

const (
	initMagic     = "NBDMAGIC"
	optMagic      = "IHAVEOPT"
	replyMagic    = 0x3e889045565a9
	requestMagic  = 0x25609513
	simpleReply   = 0x67446698
	maxOptionData = 4096

	handshakeFixedNewstyle = 1 << 0
	handshakeNoZeroes      = 1 << 1

	optExportName = 1
	optAbort      = 2
	optList       = 3
	optInfo       = 6
	optGo         = 7

	repAck        = 1
	repServer     = 2
	repInfo       = 3
	repErrUnsup   = 1<<31 | 1
	repErrInvalid = 1<<31 | 3
	repErrUnknown = 1<<31 | 6

	infoExport    = 0
	infoBlockSize = 3

	flagHasFlags = 1 << 0
	flagReadOnly = 1 << 1

	cmdRead       = 0
	cmdWrite      = 1
	cmdDisc       = 2
	cmdFlush      = 3
	cmdTrim       = 4
	cmdWriteZeros = 6

	errPerm  = 1
	errIO    = 5
	errInval = 22
)

// Listen makes the socket at path, for the owner only. A socket left from an
// earlier run is removed first.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve accepts clients on ln until ctx ends, and answers each in its own
// goroutine, up to MaxConns at a time. It closes ln.
func Serve(ctx context.Context, ln net.Listener, exp Export, opts Options) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns = map[net.Conn]bool{}
		slots = make(chan struct{}, opts.maxConns())
	)
	stop := context.AfterFunc(ctx, func() {
		_ = ln.Close()
		mu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	defer stop()
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = c.Close() // too many clients
			continue
		}
		mu.Lock()
		conns[c] = true
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			_ = Handle(ctx, c, exp, opts)
			mu.Lock()
			delete(conns, c)
			mu.Unlock()
		}()
	}
}

// Handle answers one client until it disconnects, breaks the protocol, or ctx
// ends. It closes conn.
func Handle(ctx context.Context, conn net.Conn, exp Export, opts Options) error {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(opts.handshakeTimeout()))
	return handle(ctx, conn, exp, opts, func() { _ = conn.SetDeadline(time.Time{}) })
}

func handleRW(ctx context.Context, rw io.ReadWriter, exp Export, opts Options) error {
	return handle(ctx, rw, exp, opts, func() {})
}

func handle(ctx context.Context, rw io.ReadWriter, exp Export, opts Options, transmission func()) error {
	flags, err := greet(rw)
	if err != nil {
		return err
	}
	size := exp.Size()
	if size < 0 {
		return errors.New("negative export size")
	}
	if err = negotiate(rw, opts, size, flags); err != nil {
		return err
	}
	transmission()
	return transmit(ctx, rw, exp, opts)
}

func greet(rw io.ReadWriter) (clientFlags uint32, err error) {
	hello := append([]byte(initMagic+optMagic), 0, 0)
	binary.BigEndian.PutUint16(hello[16:], handshakeFixedNewstyle|handshakeNoZeroes)
	if _, err = rw.Write(hello); err != nil {
		return 0, err
	}
	var b [4]byte
	if _, err = io.ReadFull(rw, b[:]); err != nil {
		return 0, err
	}
	clientFlags = binary.BigEndian.Uint32(b[:])
	if clientFlags&handshakeFixedNewstyle == 0 || clientFlags&^(handshakeFixedNewstyle|handshakeNoZeroes) != 0 {
		return 0, errors.New("the client does not use fixed newstyle")
	}
	return clientFlags, nil
}

func writeOptReply(w io.Writer, opt, typ uint32, data []byte) error {
	b := make([]byte, 20, 20+len(data))
	binary.BigEndian.PutUint64(b[0:], replyMagic)
	binary.BigEndian.PutUint32(b[8:], opt)
	binary.BigEndian.PutUint32(b[12:], typ)
	binary.BigEndian.PutUint32(b[16:], uint32(len(data))) // #nosec G115 -- options replies are tiny
	_, err := w.Write(append(b, data...))
	return err
}

func (o Options) knows(name string) bool { return name == "" || name == o.Name }

func exportInfo(size int64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], infoExport)
	binary.BigEndian.PutUint64(b[2:], uint64(size)) // #nosec G115 -- checked not negative
	binary.BigEndian.PutUint16(b[10:], flagHasFlags|flagReadOnly)
	return b
}

// negotiate runs the option phase. It returns when the client has chosen an
// export (by GO or EXPORT_NAME), and with an error if it aborts or misbehaves.
func negotiate(rw io.ReadWriter, opts Options, size int64, clientFlags uint32) error {
	for {
		var h [16]byte
		if _, err := io.ReadFull(rw, h[:]); err != nil {
			return err
		}
		if string(h[:8]) != optMagic {
			return errors.New("bad option magic")
		}
		opt, n := binary.BigEndian.Uint32(h[8:]), binary.BigEndian.Uint32(h[12:])
		if n > maxOptionData {
			return errors.New("option data too long")
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(rw, data); err != nil {
			return err
		}
		switch opt {
		case optExportName:
			if !opts.knows(string(data)) {
				return errors.New("unknown export")
			}
			reply := make([]byte, 10, 134)
			binary.BigEndian.PutUint64(reply[0:], uint64(size)) // #nosec G115 -- checked not negative
			binary.BigEndian.PutUint16(reply[8:], flagHasFlags|flagReadOnly)
			if clientFlags&handshakeNoZeroes == 0 {
				reply = append(reply, make([]byte, 124)...)
			}
			_, err := rw.Write(reply)
			return err
		case optAbort:
			_ = writeOptReply(rw, opt, repAck, nil)
			return errors.New("the client aborted")
		case optList:
			name := append(binary.BigEndian.AppendUint32(nil, uint32(len(opts.Name))), opts.Name...) // #nosec G115 -- a short name
			if err := writeOptReply(rw, opt, repServer, name); err != nil {
				return err
			}
			if err := writeOptReply(rw, opt, repAck, nil); err != nil {
				return err
			}
		case optInfo, optGo:
			if len(data) < 6 {
				if err := writeOptReply(rw, opt, repErrInvalid, nil); err != nil {
					return err
				}
				continue
			}
			nameLen := binary.BigEndian.Uint32(data)
			if uint64(nameLen)+6 > uint64(len(data)) {
				if err := writeOptReply(rw, opt, repErrInvalid, nil); err != nil {
					return err
				}
				continue
			}
			rest := data[4+nameLen:]
			nInfo := int(binary.BigEndian.Uint16(rest))
			if len(rest) != 2+2*nInfo {
				if err := writeOptReply(rw, opt, repErrInvalid, nil); err != nil {
					return err
				}
				continue
			}
			if !opts.knows(string(data[4 : 4+nameLen])) {
				if err := writeOptReply(rw, opt, repErrUnknown, nil); err != nil {
					return err
				}
				continue
			}
			if err := writeOptReply(rw, opt, repInfo, exportInfo(size)); err != nil {
				return err
			}
			for i := 0; i < nInfo; i++ {
				if binary.BigEndian.Uint16(rest[2+2*i:]) == infoBlockSize {
					sizes := make([]byte, 14)
					binary.BigEndian.PutUint16(sizes[0:], infoBlockSize)
					binary.BigEndian.PutUint32(sizes[2:], 1)
					binary.BigEndian.PutUint32(sizes[6:], 4096)
					binary.BigEndian.PutUint32(sizes[10:], uint32(opts.maxRead())) // #nosec G115 -- a small limit
					if err := writeOptReply(rw, opt, repInfo, sizes); err != nil {
						return err
					}
					break
				}
			}
			if err := writeOptReply(rw, opt, repAck, nil); err != nil {
				return err
			}
			if opt == optGo {
				return nil
			}
		default:
			// Structured replies, meta contexts, TLS: not offered. QEMU asks and
			// carries on when the answer is no.
			if err := writeOptReply(rw, opt, repErrUnsup, nil); err != nil {
				return err
			}
		}
	}
}

func writeReply(w io.Writer, code uint32, handle []byte, data []byte) error {
	b := make([]byte, 16, 16+len(data))
	binary.BigEndian.PutUint32(b[0:], simpleReply)
	binary.BigEndian.PutUint32(b[4:], code)
	copy(b[8:], handle)
	_, err := w.Write(append(b, data...))
	return err
}

// transmit answers requests until the client disconnects. It serves reads. A
// write, a trim and a write of zeros are refused with EPERM: the export is
// marked read-only, and a client that sends them anyway changes nothing.
func transmit(ctx context.Context, rw io.ReadWriter, exp Export, opts Options) error {
	size := exp.Size()
	usize := uint64(size) //nolint:gosec // the caller checked that size is not negative
	maxRead := int64(opts.maxRead())
	var buf []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var h [28]byte
		if _, err := io.ReadFull(rw, h[:]); err != nil {
			return err
		}
		if binary.BigEndian.Uint32(h[0:]) != requestMagic {
			return errors.New("bad request magic")
		}
		typ := binary.BigEndian.Uint16(h[6:])
		handle := h[8:16]
		off, rawLen := binary.BigEndian.Uint64(h[16:]), binary.BigEndian.Uint32(h[24:])
		length := int64(rawLen)
		switch typ {
		case cmdDisc:
			return nil
		case cmdFlush:
			if err := writeReply(rw, 0, handle, nil); err != nil {
				return err
			}
		case cmdWrite:
			// The payload follows the request. Read it so the stream stays in step.
			if length > maxRead {
				return errors.New("a write is too long")
			}
			if _, err := io.CopyN(io.Discard, rw, length); err != nil {
				return err
			}
			if err := writeReply(rw, errPerm, handle, nil); err != nil {
				return err
			}
		case cmdTrim, cmdWriteZeros:
			if err := writeReply(rw, errPerm, handle, nil); err != nil {
				return err
			}
		case cmdRead:
			if length == 0 || length > maxRead || off > usize || uint64(rawLen) > usize-off {
				if err := writeReply(rw, errInval, handle, nil); err != nil {
					return err
				}
				continue
			}
			if int64(cap(buf)) < 16+length {
				buf = make([]byte, 16+length)
			}
			out := buf[:16+length]
			binary.BigEndian.PutUint32(out[0:], simpleReply)
			binary.BigEndian.PutUint32(out[4:], 0)
			copy(out[8:], handle)
			n, err := exp.ReadAt(out[16:], int64(off)) // #nosec G115 -- checked against the size
			if err != nil && (!errors.Is(err, io.EOF) || int64(n) != length) {
				if werr := writeReply(rw, errIO, handle, nil); werr != nil {
					return werr
				}
				continue
			}
			if _, err = rw.Write(out); err != nil {
				return err
			}
		default:
			if err := writeReply(rw, errInval, handle, nil); err != nil {
				return err
			}
		}
	}
}

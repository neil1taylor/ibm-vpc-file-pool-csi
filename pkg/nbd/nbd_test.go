package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// memExport is a disk in memory.
type memExport struct{ b []byte }

func (m *memExport) Size() int64 { return int64(len(m.b)) }
func (m *memExport) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

// client is a small NBD client for the tests.
type client struct {
	t *testing.T
	c net.Conn
}

func (c *client) read(n int) []byte {
	c.t.Helper()
	b := make([]byte, n)
	_ = c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c.c, b); err != nil {
		c.t.Fatalf("read %d bytes: %v", n, err)
	}
	return b
}

func (c *client) write(b ...[]byte) {
	c.t.Helper()
	if _, err := c.c.Write(bytes.Join(b, nil)); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }

// start serves exp on one end of a pipe and returns the other end after the
// server greeting and the client flags.
func start(t *testing.T, exp Export, clientFlags uint32) *client {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	go func() { _ = Handle(context.Background(), a, exp, Options{Name: "disk"}) }()
	c := &client{t: t, c: b}
	if got := c.read(8); string(got) != "NBDMAGIC" {
		t.Fatalf("magic %q", got)
	}
	if got := c.read(8); string(got) != "IHAVEOPT" {
		t.Fatalf("opt magic %q", got)
	}
	if flags := binary.BigEndian.Uint16(c.read(2)); flags != 3 {
		t.Fatalf("handshake flags %d, want fixed newstyle and no zeroes (3)", flags)
	}
	c.write(be32(clientFlags))
	return c
}

func (c *client) option(opt uint32, data []byte) {
	c.write([]byte("IHAVEOPT"), be32(opt), be32(uint32(len(data))), data)
}

// reply reads one option reply and returns its type and data.
func (c *client) reply(opt uint32) (uint32, []byte) {
	c.t.Helper()
	h := c.read(20)
	if binary.BigEndian.Uint64(h[0:]) != 0x3e889045565a9 {
		c.t.Fatalf("option reply magic %x", h[0:8])
	}
	if got := binary.BigEndian.Uint32(h[8:]); got != opt {
		c.t.Fatalf("reply to option %d, want %d", got, opt)
	}
	n := binary.BigEndian.Uint32(h[16:])
	return binary.BigEndian.Uint32(h[12:]), c.read(int(n))
}

const (
	optName   = optExportName
	optStruct = 8
	flagRO    = flagHasFlags | flagReadOnly
)

func goData(name string) []byte {
	return bytes.Join([][]byte{be32(uint32(len(name))), []byte(name), be16(0)}, nil)
}

func (c *client) goExport(name string) (size uint64, flags uint16) {
	c.t.Helper()
	c.option(optGo, goData(name))
	typ, data := c.reply(optGo)
	if typ != repInfo || binary.BigEndian.Uint16(data[0:]) != 0 {
		c.t.Fatalf("expected the export info, got type %x", typ)
	}
	size, flags = binary.BigEndian.Uint64(data[2:]), binary.BigEndian.Uint16(data[10:])
	if typ, _ = c.reply(optGo); typ != repAck {
		c.t.Fatalf("expected ack, got %x", typ)
	}
	return size, flags
}

func (c *client) request(typ uint16, handle uint64, off uint64, length uint32, payload []byte) {
	c.write(be32(0x25609513), be16(0), be16(typ), be64(handle), be64(off), be32(length), payload)
}

// response reads a simple reply and returns the error code and, for a read
// that succeeded, the data.
func (c *client) response(handle uint64, dataLen int) (uint32, []byte) {
	c.t.Helper()
	h := c.read(16)
	if binary.BigEndian.Uint32(h[0:]) != 0x67446698 {
		c.t.Fatalf("reply magic %x", h[0:4])
	}
	if got := binary.BigEndian.Uint64(h[8:]); got != handle {
		c.t.Fatalf("handle %d, want %d", got, handle)
	}
	code := binary.BigEndian.Uint32(h[4:])
	if code != 0 || dataLen == 0 {
		return code, nil
	}
	return 0, c.read(dataLen)
}

func TestHandshake_GoThenRead(t *testing.T) {
	exp := &memExport{b: pattern(1 << 20)}
	c := start(t, exp, 3)
	size, flags := c.goExport("disk")
	if size != 1<<20 {
		t.Errorf("size %d", size)
	}
	if flags != flagRO {
		t.Errorf("transmission flags %b, want has-flags and read-only", flags)
	}
	c.request(0, 11, 4096, 5000, nil)
	code, data := c.response(11, 5000)
	if code != 0 || !bytes.Equal(data, exp.b[4096:4096+5000]) {
		t.Fatalf("read: code %d, data differs", code)
	}
}

func TestHandshake_BlockSizesAreSentWhenAsked(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	name := []byte("disk")
	c.option(optGo, bytes.Join([][]byte{be32(uint32(len(name))), name, be16(2), be16(0), be16(3)}, nil))
	if typ, _ := c.reply(optGo); typ != repInfo {
		t.Fatalf("got %x", typ)
	}
	typ, data := c.reply(optGo)
	if typ != repInfo || binary.BigEndian.Uint16(data) != 3 || binary.BigEndian.Uint32(data[2:]) != 1 ||
		binary.BigEndian.Uint32(data[10:]) != 8<<20 {
		t.Fatalf("block sizes: %x %x", typ, data)
	}
	if typ, _ = c.reply(optGo); typ != repAck {
		t.Fatalf("got %x", typ)
	}
}

func TestHandshake_EmptyNameIsTheDefaultExport(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	if size, _ := c.goExport(""); size != 4096 {
		t.Errorf("size %d", size)
	}
}

func TestHandshake_UnknownNameIsRefused(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	c.option(optGo, goData("other"))
	if typ, _ := c.reply(optGo); typ != repErrUnknown {
		t.Fatalf("got %x, want unknown export", typ)
	}
}

func TestHandshake_ExportNameOption(t *testing.T) {
	exp := &memExport{b: pattern(8192)}
	c := start(t, exp, 3)
	c.option(optName, []byte("disk"))
	h := c.read(8 + 2) // no zeroes were negotiated
	if binary.BigEndian.Uint64(h) != 8192 {
		t.Errorf("size %d", binary.BigEndian.Uint64(h))
	}
	if binary.BigEndian.Uint16(h[8:]) != flagRO {
		t.Errorf("flags %b", binary.BigEndian.Uint16(h[8:]))
	}
	c.request(0, 1, 0, 100, nil)
	if code, data := c.response(1, 100); code != 0 || !bytes.Equal(data, exp.b[:100]) {
		t.Fatal("read after EXPORT_NAME failed")
	}
}

func TestHandshake_ExportNameWithPaddingWhenZeroesAreAsked(t *testing.T) {
	c := start(t, &memExport{b: pattern(8192)}, 1) // fixed newstyle, but zeroes wanted
	c.option(optName, []byte("disk"))
	if h := c.read(8 + 2 + 124); binary.BigEndian.Uint64(h) != 8192 {
		t.Errorf("size %d", binary.BigEndian.Uint64(h))
	}
}

func TestHandshake_InfoThenGo(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	c.option(optInfo, goData("disk"))
	if typ, _ := c.reply(optInfo); typ != repInfo {
		t.Fatalf("got %x", typ)
	}
	if typ, _ := c.reply(optInfo); typ != repAck {
		t.Fatalf("got %x", typ)
	}
	c.goExport("disk")
}

func TestHandshake_UnsupportedOptionsAreAnswered(t *testing.T) {
	// QEMU asks for structured replies and meta contexts; the server says no
	// and goes on.
	for _, opt := range []uint32{optStruct, 5, 9, 10, 999} {
		c := start(t, &memExport{b: pattern(4096)}, 3)
		c.option(opt, []byte("junk"))
		if typ, _ := c.reply(opt); typ != repErrUnsup {
			t.Errorf("option %d: got %x, want unsupported", opt, typ)
		}
		c.goExport("disk")
	}
}

func TestHandshake_List(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	c.option(optList, nil)
	typ, data := c.reply(optList)
	if typ != 2 || binary.BigEndian.Uint32(data) != 4 || string(data[4:]) != "disk" {
		t.Fatalf("list: %x %q", typ, data)
	}
	if typ, _ = c.reply(optList); typ != repAck {
		t.Fatalf("got %x", typ)
	}
}

func TestHandshake_AbortEndsTheConnection(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	c.option(optAbort, nil)
	if typ, _ := c.reply(optAbort); typ != repAck {
		t.Fatalf("got %x", typ)
	}
	_ = c.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the server must close")
	}
}

func TestHandshake_ClientMustAcceptFixedNewstyle(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 0)
	_ = c.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.c.Read(make([]byte, 1)); err == nil {
		t.Fatal("a client that is not fixed newstyle must be dropped")
	}
}

func TestHandshake_BadOptionMagicDropsTheClient(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	c.write([]byte("NOTMAGIC"), be32(7), be32(0))
	_ = c.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.c.Read(make([]byte, 1)); err == nil {
		t.Fatal("must close")
	}
}

func TestHandshake_HugeOptionIsRefused(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	c.write([]byte("IHAVEOPT"), be32(999), be32(1<<30))
	_ = c.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(c.c); err != nil && !errors.Is(err, io.EOF) {
		// The server closed or reset: fine.
		return
	}
}

const (
	eperm  = 1
	eio    = 5
	einval = 22
)

func TestTransmission_WritesAreRefused(t *testing.T) {
	exp := &memExport{b: pattern(1 << 16)}
	orig := append([]byte(nil), exp.b...)
	c := start(t, exp, 3)
	c.goExport("disk")
	c.request(1, 5, 0, 512, bytes.Repeat([]byte{0xff}, 512))
	if code, _ := c.response(5, 0); code != eperm {
		t.Fatalf("write: code %d, want EPERM", code)
	}
	c.request(4, 6, 0, 512, nil) // trim
	if code, _ := c.response(6, 0); code != eperm {
		t.Fatalf("trim: code %d, want EPERM", code)
	}
	c.request(6, 7, 0, 512, nil) // write zeroes
	if code, _ := c.response(7, 0); code != eperm {
		t.Fatalf("write zeroes: code %d, want EPERM", code)
	}
	// The stream is still in step after the refused write.
	c.request(0, 8, 0, 16, nil)
	if code, data := c.response(8, 16); code != 0 || !bytes.Equal(data, orig[:16]) {
		t.Fatal("read after refused write failed")
	}
	if !bytes.Equal(exp.b, orig) {
		t.Fatal("the export changed")
	}
}

func TestTransmission_BadRangesAreRefused(t *testing.T) {
	c := start(t, &memExport{b: pattern(1 << 16)}, 3)
	c.goExport("disk")
	cases := []struct {
		name   string
		off    uint64
		length uint32
	}{
		{"past the end", 1<<16 - 10, 20},
		{"starts at the end", 1 << 16, 1},
		{"offset overflow", 1<<64 - 1, 16},
		{"zero length", 0, 0},
		{"huge length", 0, 9 << 20},
	}
	for i, tc := range cases {
		h := uint64(100 + i)
		c.request(0, h, tc.off, tc.length, nil)
		if code, _ := c.response(h, 0); code != einval {
			t.Errorf("%s: code %d, want EINVAL", tc.name, code)
		}
	}
	c.request(0, 200, 0, 4, nil)
	if code, _ := c.response(200, 4); code != 0 {
		t.Fatal("the connection must still work")
	}
}

func TestTransmission_FlushAndDisconnect(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	c.goExport("disk")
	c.request(3, 1, 0, 0, nil)
	if code, _ := c.response(1, 0); code != 0 {
		t.Fatalf("flush: code %d", code)
	}
	c.request(2, 2, 0, 0, nil)
	_ = c.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.c.Read(make([]byte, 1)); err == nil {
		t.Fatal("disconnect must close")
	}
}

func TestTransmission_UnknownCommandIsInvalid(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	c.goExport("disk")
	c.request(77, 1, 0, 0, nil)
	if code, _ := c.response(1, 0); code != einval {
		t.Fatalf("code %d, want EINVAL", code)
	}
}

func TestTransmission_BadRequestMagicDropsTheClient(t *testing.T) {
	c := start(t, &memExport{b: pattern(4096)}, 3)
	c.goExport("disk")
	c.write(be32(0xdeadbeef), make([]byte, 24))
	_ = c.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.c.Read(make([]byte, 1)); err == nil {
		t.Fatal("must close")
	}
}

type failingExport struct{ memExport }

func (failingExport) ReadAt(p []byte, off int64) (int, error) { return 0, errors.New("disk gone") }

func TestTransmission_ReadErrorIsEIO(t *testing.T) {
	c := start(t, &failingExport{memExport{b: pattern(4096)}}, 3)
	c.goExport("disk")
	c.request(0, 1, 0, 16, nil)
	if code, _ := c.response(1, 0); code != eio {
		t.Fatalf("code %d, want EIO", code)
	}
}

func TestServe_UnixSocketWithManyClients(t *testing.T) {
	exp := &memExport{b: pattern(1 << 18)}
	sock := shortSock(t)
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, exp, Options{Name: "disk", MaxConns: 2}) }()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("unix", sock)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			c := &client{t: t, c: conn}
			c.read(18)
			c.write(be32(3))
			c.goExport("disk")
			c.request(0, 1, 100, 1000, nil)
			if code, data := c.response(1, 1000); code != 0 || !bytes.Equal(data, exp.b[100:1100]) {
				t.Error("read failed")
			}
		}()
	}
	wg.Wait()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestListen_SocketIsPrivate(t *testing.T) {
	sock := shortSock(t)
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	fi, err := statMode(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi&0o077 != 0 {
		t.Errorf("socket mode %o must not give access to others", fi)
	}
}

func FuzzHandle(f *testing.F) {
	f.Add([]byte("\x00\x00\x00\x03IHAVEOPT\x00\x00\x00\x07\x00\x00\x00\x0a\x00\x00\x00\x04disk\x00\x00"))
	f.Add([]byte("\x00\x00\x00\x01IHAVEOPT\x00\x00\x00\x01\x00\x00\x00\x04disk\x25\x60\x95\x13\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x10"))
	f.Fuzz(func(t *testing.T, in []byte) {
		var out bytes.Buffer
		rw := struct {
			io.Reader
			io.Writer
		}{bytes.NewReader(in), &out}
		_ = handleRW(context.Background(), rw, &memExport{b: pattern(4096)}, Options{Name: "disk"})
	})
}

func statMode(path string) (os.FileMode, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Mode().Perm(), nil
}

// shortSock returns a socket path short enough for macOS (104 bytes).
func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

package filerestore

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// A file restore that reads a VM disk runs as two containers of one pod. The
// reader parses the guest's file systems, which are data that a stranger
// controlled, so it has no write access to the target and no capability. The
// writer holds the target and the capabilities, and does only what this protocol
// allows: it never sees the disk. The reader sends each call of the Dest
// interface over a private socket; the writer applies it to a DirDest, treating
// every message as hostile (see ServeWriter).

// Frame types sent by the reader.
const (
	opMkdir      byte = 'M'
	opFinishDir  byte = 'D'
	opCreate     byte = 'C'
	opData       byte = 'W' // file content; no reply
	opFileFinish byte = 'F'
	opFileClose  byte = 'X'
	opSymlink    byte = 'L'
	opFree       byte = 'R'
	opAbort      byte = 'A'
	opCommit     byte = 'K'
)

// Frame types sent by the writer.
const (
	replyOK  byte = 'o'
	replyErr byte = 'e'
)

const (
	// maxFrame is the most that one frame may hold. A file is sent in chunks of
	// copyBuffer.
	maxFrame = 4 << 20
	// dialTimeout is how long the reader waits for the writer to listen.
	dialTimeout = 2 * time.Minute
	// acceptTimeout is how long the writer waits for the reader. The reader dials
	// first thing, so a reader that has not come by then is not coming.
	acceptTimeout = 5 * time.Minute
)

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return errors.New("frame too large")
	}
	var head [5]byte
	head[0] = typ
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload))) //nolint:gosec // bounded above
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > maxFrame {
		return 0, nil, errors.New("frame too large")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return head[0], payload, nil
}

// message is the payload of a control frame.
type message struct {
	Path   [][]byte `json:"path,omitempty"`
	Meta   *Meta    `json:"meta,omitempty"`
	Target []byte   `json:"target,omitempty"`
	// Moment is the time of the backup that was read (RFC 3339), for the name of
	// the folder that Commit makes.
	Moment string `json:"moment,omitempty"`
	// Name, Free: replies.
	Name string `json:"name,omitempty"`
	Free int64  `json:"free,omitempty"`
	Err  string `json:"err,omitempty"`
}

// RemoteDest is a Dest whose calls are done by a writer on the other end of a
// connection.
type RemoteDest struct {
	mu   sync.Mutex
	conn io.Closer
	r    *bufio.Reader
	w    *bufio.Writer
}

// DialWriter connects to the writer's socket, retrying until it listens, and
// waits for the writer to say that it has opened the target. The error is the
// writer's own reason when it could not (for example a claim that holds a VM
// disk).
func DialWriter(ctx context.Context, socket string) (*RemoteDest, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", socket)
		if err == nil {
			return newRemoteDest(conn)
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("the writer of the restore did not start")
		case <-time.After(time.Second):
		}
	}
}

func newRemoteDest(conn io.ReadWriteCloser) (*RemoteDest, error) {
	d := &RemoteDest{conn: conn, r: bufio.NewReaderSize(conn, 64<<10), w: bufio.NewWriterSize(conn, 64<<10)}
	// The writer answers once it has opened the target.
	if _, err := d.readReply(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return d, nil
}

func (d *RemoteDest) readReply() (message, error) {
	typ, payload, err := readFrame(d.r)
	if err != nil {
		return message{}, errors.New("the writer of the restore stopped")
	}
	var m message
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &m); err != nil {
			return message{}, errors.New("the writer of the restore sent a reply that cannot be read")
		}
	}
	if typ == replyErr {
		return m, errors.New(m.Err)
	}
	return m, nil
}

func (d *RemoteDest) send(typ byte, m message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := writeFrame(d.w, typ, payload); err != nil {
		return errors.New("the writer of the restore stopped")
	}
	return nil
}

// call sends one frame and waits for the reply.
func (d *RemoteDest) call(typ byte, m message) (message, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.send(typ, m); err != nil {
		return message{}, err
	}
	if err := d.w.Flush(); err != nil {
		return message{}, errors.New("the writer of the restore stopped")
	}
	return d.readReply()
}

func (d *RemoteDest) Mkdir(path [][]byte) error {
	_, err := d.call(opMkdir, message{Path: path})
	return err
}

func (d *RemoteDest) FinishDir(path [][]byte, m Meta) error {
	_, err := d.call(opFinishDir, message{Path: path, Meta: &m})
	return err
}

func (d *RemoteDest) Symlink(path [][]byte, target []byte, m Meta) error {
	_, err := d.call(opSymlink, message{Path: path, Target: target, Meta: &m})
	return err
}

func (d *RemoteDest) Free() (int64, error) {
	m, err := d.call(opFree, message{})
	return m.Free, err
}

// Abort asks the writer to remove what it wrote.
func (d *RemoteDest) Abort() error {
	_, err := d.call(opAbort, message{})
	return err
}

// CommitMoment asks the writer to move the staging folder to its final name. The
// writer makes the name from its own policy (the argument is not sent) and the
// moment; it returns it.
func (d *RemoteDest) CommitMoment(_ string, moment time.Time) (string, error) {
	m := message{}
	if !moment.IsZero() {
		m.Moment = moment.UTC().Format(time.RFC3339)
	}
	r, err := d.call(opCommit, m)
	return r.Name, err
}

// Close ends the session. A writer that has not been told to commit removes
// what it wrote.
func (d *RemoteDest) Close() error { return d.conn.Close() }

func (d *RemoteDest) Create(path [][]byte) (DestFile, error) {
	if _, err := d.call(opCreate, message{Path: path}); err != nil {
		return nil, err
	}
	return &remoteFile{d: d}, nil
}

type remoteFile struct{ d *RemoteDest }

// Write sends content without waiting: the writer's answer comes with Finish.
func (f *remoteFile) Write(p []byte) (int, error) {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), copyBuffer)
		if err := writeFrame(f.d.w, opData, p[:n]); err != nil {
			return written, errors.New("the writer of the restore stopped")
		}
		p, written = p[n:], written+n
	}
	return written, nil
}

func (f *remoteFile) Finish(m Meta) error {
	_, err := f.d.call(opFileFinish, message{Meta: &m})
	return err
}

func (f *remoteFile) Close() error {
	_, err := f.d.call(opFileClose, message{})
	return err
}

// WriterPolicy is what the writer holds against the reader. It comes from the
// writer's own command line, never from the stream.
type WriterPolicy struct {
	Mode   string
	Policy string
	// Owner replaces the owner the reader asks for. Both fields are required.
	Owner Owner
	// RootMode is the mode of the root folder (at most 0777); zero means 0755.
	RootMode uint32
	// MaxBytes limits the content the writer accepts; zero means the default.
	MaxBytes int64
}

// WriterDest is what the writer applies calls to: a DirDest.
type WriterDest interface {
	Dest
	Commit(prefix string) (string, error)
	Close() error
}

// ServeWriter applies the reader's calls on conn to dest until the reader ends
// the session. It treats every message as hostile: paths are checked again, the
// bytes are counted here, setuid and setgid are removed here, the owner of the
// policy replaces the reader's, and the name of a committed folder is made here.
// A session that ends without a commit leaves nothing: dest is aborted. It
// returns the name of the committed folder, or "".
func ServeWriter(conn io.ReadWriter, dest WriterDest, pol WriterPolicy) (string, error) {
	s := &writerSession{dest: dest, pol: pol, w: bufio.NewWriter(conn)}
	r := bufio.NewReaderSize(conn, 64<<10)
	if err := s.open(); err != nil {
		_ = dest.Abort()
		_ = s.reply(message{}, err)
		return "", err
	}
	if err := s.reply(message{}, nil); err != nil {
		_ = dest.Abort()
		return "", err
	}
	for {
		typ, payload, err := readFrame(r)
		if err != nil {
			// The reader went away without committing.
			s.closeFile()
			if !s.committed {
				_ = dest.Abort()
			}
			if errors.Is(err, io.EOF) {
				return s.name, nil
			}
			return s.name, err
		}
		if typ == opData {
			s.data(payload)
			continue
		}
		var m message
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &m); err != nil {
				_ = dest.Abort()
				return "", errors.New("a message of the reader cannot be read")
			}
		}
		if rerr := s.reply(s.handle(typ, m)); rerr != nil {
			if !s.committed {
				_ = dest.Abort()
			}
			return s.name, rerr
		}
	}
}

type writerSession struct {
	dest      WriterDest
	pol       WriterPolicy
	w         *bufio.Writer
	limit     int64
	written   int64
	file      DestFile
	fileErr   error // first error of a Write, reported when the file ends
	committed bool
	name      string
}

// open fixes the limits of the session before any content is accepted: the
// owner must be set, and the content may not exceed the smaller of the limit
// and the space the target has left.
func (s *writerSession) open() error {
	if s.pol.Owner.UID == nil || s.pol.Owner.GID == nil {
		return errors.New("the writer has no owner to give the restored files")
	}
	if s.pol.RootMode&^0o777 != 0 {
		return errors.New("the mode of the root folder may not have setuid, setgid or sticky bits")
	}
	s.limit = s.pol.MaxBytes
	if s.limit <= 0 {
		s.limit = DefaultMaxBytes
	}
	free, err := s.dest.Free()
	if err != nil {
		return fmt.Errorf("read the free space: %w", err)
	}
	if free < s.limit {
		s.limit = free
	}
	return nil
}

func (s *writerSession) reply(m message, err error) error {
	typ := replyOK
	if err != nil {
		typ, m = replyErr, message{Err: err.Error()}
	}
	payload, merr := json.Marshal(m)
	if merr != nil {
		return merr
	}
	if err := writeFrame(s.w, typ, payload); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *writerSession) closeFile() {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

func (s *writerSession) data(p []byte) {
	if s.file == nil || s.fileErr != nil {
		if s.fileErr == nil {
			s.fileErr = errors.New("content without a file")
		}
		return
	}
	if s.written+int64(len(p)) > s.limit {
		s.fileErr = &NoSpaceError{Need: s.written + int64(len(p)), Have: s.limit}
		return
	}
	n, err := s.file.Write(p)
	s.written += int64(n)
	if err != nil {
		s.fileErr = err
	}
}

// meta sets what the writer insists on: the owner is the policy's, never the
// reader's, and the mode keeps only the permission bits (no setuid, setgid or
// sticky bit).
func (s *writerSession) meta(m *Meta) Meta {
	var out Meta
	if m != nil {
		out = *m
	}
	out.Mode &= 0o777
	out.UID, out.GID = s.pol.Owner.UID, s.pol.Owner.GID
	return out
}

// rootMeta is the meta of the root folder: the mode is the writer's own.
func (s *writerSession) rootMeta(m *Meta) Meta {
	out := s.meta(m)
	out.Mode = s.pol.RootMode
	if out.Mode == 0 {
		out.Mode = defaultRootMode
	}
	return out
}

func checkMessagePath(p [][]byte, allowEmpty bool) error {
	if len(p) == 0 && !allowEmpty {
		return errors.New("an empty path")
	}
	return browse.ValidatePath(p)
}

func (s *writerSession) handle(typ byte, m message) (message, error) {
	if s.file != nil && typ != opFileFinish && typ != opFileClose {
		return message{}, errors.New("a call came while a file was open")
	}
	switch typ {
	case opMkdir:
		if err := checkMessagePath(m.Path, false); err != nil {
			return message{}, err
		}
		return message{}, s.dest.Mkdir(m.Path)
	case opFinishDir:
		if err := checkMessagePath(m.Path, true); err != nil {
			return message{}, err
		}
		if len(m.Path) == 0 {
			return message{}, s.dest.FinishDir(nil, s.rootMeta(m.Meta))
		}
		return message{}, s.dest.FinishDir(m.Path, s.meta(m.Meta))
	case opCreate:
		if err := checkMessagePath(m.Path, false); err != nil {
			return message{}, err
		}
		f, err := s.dest.Create(m.Path)
		if err != nil {
			return message{}, err
		}
		s.file, s.fileErr = f, nil
		return message{}, nil
	case opFileFinish, opFileClose:
		f, ferr := s.file, s.fileErr
		if f == nil {
			return message{}, errors.New("no file is open")
		}
		s.file, s.fileErr = nil, nil
		if ferr != nil {
			_ = f.Close()
			return message{}, ferr
		}
		if typ == opFileClose {
			return message{}, f.Close()
		}
		return message{}, f.Finish(s.meta(m.Meta))
	case opSymlink:
		if err := checkMessagePath(m.Path, false); err != nil {
			return message{}, err
		}
		return message{}, s.dest.Symlink(m.Path, m.Target, s.meta(m.Meta))
	case opFree:
		free, err := s.dest.Free()
		return message{Free: free}, err
	case opAbort:
		return message{}, s.dest.Abort()
	case opCommit:
		if s.pol.Mode != ModeFolder {
			return message{}, errors.New("only a restore into a new folder commits")
		}
		var moment time.Time
		if m.Moment != "" {
			t, err := time.Parse(time.RFC3339, m.Moment)
			if err != nil {
				return message{}, errors.New("the moment is not a time")
			}
			moment = t
		}
		name, err := s.dest.Commit(FolderName(s.pol.Policy, moment))
		if err != nil {
			_ = s.dest.Abort()
			return message{}, err
		}
		s.committed, s.name = true, name
		return message{Name: name}, nil
	}
	return message{}, fmt.Errorf("unknown call %q", typ)
}

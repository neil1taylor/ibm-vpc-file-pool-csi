package filerestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// Options sets how a restore writes.
type Options struct {
	// Owner is given to everything restored. Nil fields keep the backup's owner.
	Owner Owner
	// RootMode is the mode of the destination root. Zero means 0755.
	RootMode uint32
	// MaxBytes is the most to write. Zero means DefaultMaxBytes.
	MaxBytes int64
}

const (
	setuidSetgid    = 0o6000
	defaultRootMode = 0o755
	listPageSize    = 1000
	maxRestorePaths = 100000
)

// normalize checks the chosen paths and returns them in an order where a
// folder comes before everything in it, without duplicates.
func normalize(chosen [][][]byte) ([][][]byte, error) {
	if len(chosen) == 0 {
		return nil, ErrNothingToRestore
	}
	if len(chosen) > maxRestorePaths {
		return nil, fmt.Errorf("more than %d paths", maxRestorePaths)
	}
	out := make([][][]byte, 0, len(chosen))
	seen := map[string]bool{}
	for _, c := range chosen {
		if err := browse.ValidatePath(c); err != nil {
			return nil, err
		}
		if k := joinKey(c); !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) < len(out[j]) })
	return out, nil
}

// joinKey makes a map key from a path. A name holds no NUL byte (ValidatePath),
// so the NUL separator cannot be confused with a name.
func joinKey(path [][]byte) string {
	var b []byte
	for _, n := range path {
		b = append(b, n...)
		b = append(b, 0)
	}
	return string(b)
}

func hasPrefix(path, prefix [][]byte) bool {
	if len(prefix) > len(path) {
		return false
	}
	for i := range prefix {
		if string(path[i]) != string(prefix[i]) {
			return false
		}
	}
	return true
}

// eachChild calls fn for every entry of the folder at path.
func eachChild(src browse.Tree, path [][]byte, fn func(browse.Entry) error) error {
	if w, ok := src.(browse.Walker); ok {
		return w.Walk(path, fn)
	}
	var cursor []byte
	for {
		page, err := src.List(path, cursor, listPageSize)
		if err != nil {
			return err
		}
		for _, e := range page.Entries {
			if err := fn(e); err != nil {
				return err
			}
		}
		if len(page.Next) == 0 {
			return nil
		}
		cursor = page.Next
	}
}

func browseCode(err error) string {
	var be *browse.Error
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

func childPath(path [][]byte, name []byte) [][]byte {
	return append(append(make([][]byte, 0, len(path)+1), path...), append([]byte(nil), name...))
}

// Measure counts what Restore would write. It reads the tree and writes nothing.
func Measure(ctx context.Context, src browse.Tree, chosen [][][]byte) (Result, error) {
	norm, err := normalize(chosen)
	if err != nil {
		return Result{}, err
	}
	var res Result
	var trees [][][]byte
	for _, path := range norm {
		if coveredBy(path, trees) {
			continue
		}
		e, err := src.Stat(path)
		if err != nil {
			return res, err
		}
		if e.Type == browse.TypeDir {
			trees = append(trees, path)
		}
		if err := measureEntry(ctx, src, path, e, true, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

func coveredBy(path [][]byte, trees [][][]byte) bool {
	for _, t := range trees {
		if hasPrefix(path, t) {
			return true
		}
	}
	return false
}

func measureEntry(ctx context.Context, src browse.Tree, path [][]byte, e browse.Entry, top bool, res *Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch e.Type {
	case browse.TypeSymlink:
		res.Links++
	case browse.TypeFile:
		if !e.Readable {
			if top {
				return &browse.Error{Code: browse.CodeNotReadable, Msg: "the backup reader may not read this"}
			}
			res.Unreadable++
			return nil
		}
		res.Files++
		res.Bytes += e.Size
	case browse.TypeDir:
		res.Dirs++
		err := eachChild(src, path, func(c browse.Entry) error {
			return measureEntry(ctx, src, childPath(path, c.Name), c, false, res)
		})
		if browseCode(err) == browse.CodeNotReadable && !top {
			res.Unreadable++
			return nil
		}
		return err
	default:
		if top {
			return &browse.Error{Code: browse.CodeSpecial, Msg: "this is not a regular file"}
		}
		res.Skipped++
	}
	return nil
}

type restorer struct {
	ctx   context.Context
	src   browse.Tree
	dst   Dest
	opt   Options
	limit int64
	res   Result
	// made are the folders above chosen paths that were made, in order of making.
	made    []madeDir
	madeSet map[string]bool
}

type madeDir struct {
	path  [][]byte
	entry browse.Entry
}

// Restore copies the chosen paths, with the folders above them, into dst. It
// checks that the paths fit before it writes anything. On any error it calls
// dst.Abort, so a restore leaves all of its files or none.
func Restore(ctx context.Context, src browse.Tree, dst Dest, chosen [][][]byte, opt Options) (Result, error) {
	r := &restorer{ctx: ctx, src: src, dst: dst, opt: opt, madeSet: map[string]bool{}}
	if err := r.run(chosen); err != nil {
		_ = dst.Abort()
		return r.res, err
	}
	return r.res, nil
}

func (r *restorer) run(chosen [][][]byte) error {
	norm, err := normalize(chosen)
	if err != nil {
		return err
	}
	need, err := Measure(r.ctx, r.src, norm)
	if err != nil {
		return err
	}
	r.limit = r.opt.MaxBytes
	if r.limit <= 0 {
		r.limit = DefaultMaxBytes
	}
	free, err := r.dst.Free()
	if err != nil {
		return fmt.Errorf("read the free space: %w", err)
	}
	if free < r.limit {
		r.limit = free
	}
	if need.Bytes > r.limit {
		return &NoSpaceError{Need: need.Bytes, Have: r.limit}
	}

	var trees [][][]byte
	for _, path := range norm {
		if coveredBy(path, trees) {
			continue
		}
		e, err := r.src.Stat(path)
		if err != nil {
			return err
		}
		if e.Type == browse.TypeDir {
			trees = append(trees, path)
		}
		if err := r.ensureAbove(path); err != nil {
			return err
		}
		if err := r.copy(path, e, true); err != nil {
			return err
		}
	}
	// The folders above the chosen paths, innermost first.
	for i := len(r.made) - 1; i >= 0; i-- {
		if err := r.dst.FinishDir(r.made[i].path, r.meta(r.made[i].entry)); err != nil {
			return fmt.Errorf("finish a folder: %w", err)
		}
	}
	rootMode := r.opt.RootMode
	if rootMode == 0 {
		rootMode = defaultRootMode
	}
	root := Meta{Mode: rootMode &^ setuidSetgid, UID: r.opt.Owner.UID, GID: r.opt.Owner.GID}
	if err := r.dst.FinishDir(nil, root); err != nil {
		return fmt.Errorf("finish the root folder: %w", err)
	}
	return nil
}

// ensureAbove makes the folders above path, from the backup's own folders.
func (r *restorer) ensureAbove(path [][]byte) error {
	for i := 1; i < len(path); i++ {
		sub := path[:i]
		k := joinKey(sub)
		if r.madeSet[k] {
			continue
		}
		e, err := r.src.Stat(sub)
		if err != nil {
			return err
		}
		if e.Type != browse.TypeDir {
			return &browse.Error{Code: browse.CodeNotDir, Msg: "a name in the path is not a folder"}
		}
		if err := r.dst.Mkdir(sub); err != nil {
			return fmt.Errorf("make a folder: %w", err)
		}
		r.madeSet[k] = true
		r.made = append(r.made, madeDir{path: append([][]byte(nil), sub...), entry: e})
		r.res.Dirs++
	}
	return nil
}

// meta works out what is set on an entry. setuid and setgid are always removed.
func (r *restorer) meta(e browse.Entry) Meta {
	m := Meta{Mode: e.Mode &^ setuidSetgid, MTime: e.MTime, UID: r.opt.Owner.UID, GID: r.opt.Owner.GID}
	if m.UID == nil {
		u := int64(e.UID)
		m.UID = &u
	}
	if m.GID == nil {
		g := int64(e.GID)
		m.GID = &g
	}
	return m
}

func (r *restorer) copy(path [][]byte, e browse.Entry, top bool) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	switch e.Type {
	case browse.TypeSymlink:
		if err := r.dst.Symlink(path, e.Link, r.meta(e)); err != nil {
			return fmt.Errorf("make a link: %w", err)
		}
		r.res.Links++
		return nil
	case browse.TypeFile:
		return r.copyFile(path, e, top)
	case browse.TypeDir:
		return r.copyDir(path, e, top)
	}
	if top {
		return &browse.Error{Code: browse.CodeSpecial, Msg: "this is not a regular file"}
	}
	r.res.Skipped++
	return nil
}

func (r *restorer) copyDir(path [][]byte, e browse.Entry, top bool) error {
	// The root of the backup is the destination root: it is made already, and
	// run finishes it last.
	isRoot := len(path) == 0
	if !isRoot {
		if err := r.dst.Mkdir(path); err != nil {
			return fmt.Errorf("make a folder: %w", err)
		}
		r.res.Dirs++
	}
	err := eachChild(r.src, path, func(c browse.Entry) error {
		return r.copy(childPath(path, c.Name), c, false)
	})
	if browseCode(err) == browse.CodeNotReadable && !top {
		r.res.Unreadable++
		err = nil
	}
	if err != nil {
		return err
	}
	if isRoot {
		return nil
	}
	if err := r.dst.FinishDir(path, r.meta(e)); err != nil {
		return fmt.Errorf("finish a folder: %w", err)
	}
	return nil
}

func (r *restorer) copyFile(path [][]byte, e browse.Entry, top bool) error {
	f, err := r.src.Open(path)
	if err != nil {
		switch browseCode(err) {
		case browse.CodeNotReadable:
			if top {
				return err
			}
			r.res.Unreadable++
			return nil
		case browse.CodeNotFound, browse.CodeSpecial, browse.CodeLink:
			if top {
				return err
			}
			// It changed or was replaced while we walked: leave it out.
			r.res.Skipped++
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()
	out, err := r.dst.Create(path)
	if err != nil {
		return fmt.Errorf("make a file: %w", err)
	}
	size := f.Size()
	if r.res.Bytes+size > r.limit {
		_ = out.Close()
		return &NoSpaceError{Need: r.res.Bytes + size, Have: r.limit}
	}
	n, err := io.CopyBuffer(out, io.NewSectionReader(f, 0, size), make([]byte, copyBuffer))
	if err != nil {
		_ = out.Close()
		return fmt.Errorf("copy a file: %w", err)
	}
	if n != size {
		_ = out.Close()
		return errors.New("a file got shorter while it was read")
	}
	if err := out.Finish(r.meta(e)); err != nil {
		_ = out.Close()
		return fmt.Errorf("finish a file: %w", err)
	}
	r.res.Files++
	r.res.Bytes += size
	return nil
}

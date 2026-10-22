package browse

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
)

// writeTar writes the file or folder at path to w. A folder is written with
// its content; names in the tar start at the last name of path (or at the root
// for the empty path). Links are written as links and never followed. A file
// this session may not read, and a device, pipe or socket, is left out and
// counted in the report.
func writeTar(t Tree, w io.Writer, path [][]byte) (TarReport, error) {
	if err := ValidatePath(path); err != nil {
		return TarReport{}, err
	}
	top, err := t.Stat(path)
	if err != nil {
		return TarReport{}, err
	}
	if top.Type == TypeSymlink {
		return TarReport{}, &Error{Code: CodeLink, Msg: "this is a symbolic link; links are not downloaded"}
	}
	tw := tar.NewWriter(w)
	var rep TarReport
	prefix := ""
	if len(path) > 0 {
		prefix = string(path[len(path)-1])
	}
	if err := tarEntry(t, tw, &rep, path, prefix, top, true); err != nil {
		return rep, err
	}
	return rep, tw.Close()
}

func tarEntry(t Tree, tw *tar.Writer, rep *TarReport, path [][]byte, name string, e Entry, top bool) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    int64(e.Mode),
		Uid:     int(e.UID),
		Gid:     int(e.GID),
		ModTime: e.MTime,
		Format:  tar.FormatPAX,
	}
	switch e.Type {
	case TypeDir:
		return tarDir(t, tw, rep, path, name, hdr, top)
	case TypeSymlink:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = string(e.Link)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		rep.Links++
		return nil
	case TypeFile:
		return tarRegular(t, tw, rep, path, hdr)
	}
	rep.Skipped++
	return nil
}

func tarDir(t Tree, tw *tar.Writer, rep *TarReport, path [][]byte, name string, hdr *tar.Header, top bool) error {
	if len(path) > MaxPathDepth {
		return &Error{Code: CodeBadPath, Msg: "folders are nested too deep"}
	}
	if name != "" {
		hdr.Typeflag = tar.TypeDir
		hdr.Name = name + "/"
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
	}
	var cursor []byte
	for {
		page, err := t.List(path, cursor, 1000)
		if err != nil {
			var ae *Error
			if errors.As(err, &ae) && ae.Code == CodeNotReadable && !top {
				rep.Unreadable++
				return nil
			}
			return err
		}
		for _, c := range page.Entries {
			childPath := append(append([][]byte{}, path...), c.Name)
			childName := string(c.Name)
			if name != "" {
				childName = name + "/" + childName
			}
			if err := tarEntry(t, tw, rep, childPath, childName, c, false); err != nil {
				return err
			}
		}
		if len(page.Next) == 0 {
			return nil
		}
		cursor = page.Next
	}
}

func tarRegular(t Tree, tw *tar.Writer, rep *TarReport, path [][]byte, hdr *tar.Header) error {
	f, err := t.Open(path)
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) {
			switch ae.Code {
			case CodeNotReadable:
				rep.Unreadable++
				return nil
			case CodeNotFound, CodeSpecial, CodeLink:
				// It changed or was replaced while we walked: leave it out.
				rep.Skipped++
				return nil
			}
		}
		return err
	}
	defer func() { _ = f.Close() }()
	hdr.Typeflag = tar.TypeReg
	hdr.Size = f.Size()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.Copy(tw, io.NewSectionReader(f, 0, hdr.Size))
	if err != nil {
		return err
	}
	if n != hdr.Size {
		return fmt.Errorf("a file got shorter while it was read")
	}
	rep.Files++
	return nil
}

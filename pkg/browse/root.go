package browse

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
)

// ChooseRoot says which folder to read. Without --at it is the latest copy.
// With --at it is the newest moment folder at or before the time; with no
// moment folders yet, the only copy is the latest one. The info says which
// moment the session reads.
func ChooseRoot(latest, momentsDir string, at time.Time) (string, Info, error) {
	if momentsDir == "" {
		return latest, Info{Latest: true}, nil
	}
	names, err := MomentNames(momentsDir)
	if err != nil {
		return "", Info{}, err
	}
	pick, newest, err := PickMoment(names, at)
	if err != nil {
		return "", Info{}, err
	}
	if pick == "" {
		return latest, Info{Latest: true}, nil
	}
	if at.IsZero() {
		return latest, Info{Moment: pick, Latest: true}, nil
	}
	return filepath.Join(momentsDir, pick), Info{Moment: pick, Latest: newest}, nil
}

// MomentNames lists the moments folder through the safe reader, so a link
// named like a moment is not taken for a folder.
func MomentNames(dir string) ([]string, error) {
	tree, err := NewTree(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // no moments yet
		}
		return nil, fmt.Errorf("open the moments folder: %w", err)
	}
	defer func() { _ = tree.Close() }()
	var names []string
	var cursor []byte
	for {
		page, err := tree.List(nil, cursor, 1000)
		if err != nil {
			return nil, fmt.Errorf("list the moments folder: %w", err)
		}
		for _, e := range page.Entries {
			if e.Type == TypeDir {
				names = append(names, string(e.Name))
			}
		}
		if len(page.Next) == 0 {
			return names, nil
		}
		cursor = page.Next
	}
}

package pool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// fileListName is the file next to disk.img that holds the file list of the
	// disk. A Kopia snapshot of the backup folder carries it to COS.
	fileListName = ".filelist.json.gz"

	// fileListDeadline stops a build that does not end. The disk is held while it
	// runs, and a hold is promised for 2 hours at most.
	fileListDeadline = 30 * time.Minute

	fileListOutVol   = "filelist-out"
	fileListOutDir   = "/out"
	fileListOutFile  = fileListOutDir + "/filelist.json.gz"
	publishContainer = "publish"

	maxFileListNotes   = 8
	maxFileListNoteLen = 300
)

// FileListOutcome is what a build of a file list reports.
type FileListOutcome struct {
	// Moment is the moment of the disk that the list describes.
	Moment time.Time
	// Entries and Bytes are the number of names listed and the size of the list.
	Entries int
	Bytes   int64
	// Notes are sentences for the person: a file system that could not be listed.
	Notes []string
}

// publishScript copies the finished list next to the disk, or removes the list
// of an earlier moment when the build failed, so that a list is never taken for
// the wrong moment. It only copies bytes: it never reads the list.
const publishScript = `set -eu
while [ ! -e ` + fileListOutFile + ` ] && [ ! -e ` + fileListOutFile + `.failed ]; do sleep 1; done
if [ -e ` + fileListOutFile + ` ]; then
  cp ` + fileListOutFile + ` /dest/` + fileListName + `.tmp
  mv /dest/` + fileListName + `.tmp /dest/` + fileListName + `
else
  rm -f /dest/` + fileListName + ` /dest/` + fileListName + `.tmp
fi
`

// BuildFileList runs a Job that reads the file systems of the VM disk in
// backupPath once and saves the file list next to the disk. The reader runs
// the guest image on the backup mounted read-only, with no Secrets and no API
// token, like a browse session on the disk. A second container, which never
// reads the disk, copies the finished file to the backup folder.
func (r *ShareJobRunner) BuildFileList(ctx context.Context, key string, share ShareMount, backupPath, consistency string) (bool, FileListOutcome, error) {
	if r.guestImage == "" {
		return false, FileListOutcome{}, errNoBrowseGuest
	}
	if err := util.ValidateBackupPath(backupPath); err != nil {
		return false, FileListOutcome{}, err
	}
	if consistency != "" && consistency != "FileSystemConsistent" && consistency != "CrashConsistent" {
		return false, FileListOutcome{}, errors.New("the consistency of the copy is not one that the helper knows")
	}
	args := []string{"/usr/local/bin/browse", "--mode=build-list", "--root=/backup",
		"--guest-socket=/tmp/disk.sock", "--guest-appliance=/opt/guestfs-appliance", "--list-out=" + fileListOutFile}
	if consistency != "" {
		args = append(args, "--guest-consistency="+consistency)
	}
	sub := strings.TrimPrefix(backupPath, "/")
	outSize := resource.MustParse("256Mi")
	outMount := corev1.VolumeMount{Name: fileListOutVol, MountPath: fileListOutDir}
	shape := jobShape{
		image:     r.guestImage,
		command:   args,
		runAsUser: 0,
		folders:   []folderMount{{share: 0, subPath: sub, at: "/backup", readOnly: true}},
		volumes: []corev1.Volume{{Name: fileListOutVol, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &outSize}}}},
		mounts: []corev1.VolumeMount{outMount},
		sidecars: []sideContainer{{
			name:    publishContainer,
			command: []string{"/bin/sh", "-c", publishScript},
			folders: []folderMount{{share: 0, subPath: sub, at: "/dest"}},
			mounts:  []corev1.VolumeMount{outMount},
		}},
		noToken:        true,
		hardened:       true,
		pullSecrets:    r.browsePullSecrets,
		activeDeadline: int64(fileListDeadline.Seconds()),
	}
	r.shapeGuest(&shape)
	done, msgs, err := r.runMessages(ctx, key, []ShareMount{share}, shape)
	if err != nil || !done {
		return false, FileListOutcome{}, err
	}
	out, ok := parseFileListOutcome(msgs.of(mainContainerName))
	if !ok {
		return true, FileListOutcome{}, errors.New("the file list Job finished but did not report what it listed")
	}
	return true, out, nil
}

// parseFileListOutcome reads the termination message of the reader. The reader
// parses a hostile disk, so its words are cut to a safe size and form.
func parseFileListOutcome(msg string) (FileListOutcome, bool) {
	var m struct {
		Moment  string   `json:"moment"`
		Entries int      `json:"entries"`
		Bytes   int64    `json:"bytes"`
		Notes   []string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(msg), &m); err != nil || m.Entries < 0 || m.Bytes < 0 {
		return FileListOutcome{}, false
	}
	t, err := time.Parse(time.RFC3339, m.Moment)
	if err != nil {
		return FileListOutcome{}, false
	}
	out := FileListOutcome{Moment: t, Entries: m.Entries, Bytes: m.Bytes}
	for _, n := range m.Notes {
		if len(out.Notes) == maxFileListNotes {
			break
		}
		n = strings.Map(func(r rune) rune {
			if unicode.IsPrint(r) {
				return r
			}
			return -1
		}, n)
		if len(n) > maxFileListNoteLen {
			n = strings.ToValidUTF8(n[:maxFileListNoteLen], "")
		}
		if n != "" {
			out.Notes = append(out.Notes, n)
		}
	}
	return out, true
}

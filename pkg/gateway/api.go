// Package gateway is the one door to browse sessions. Users talk to it with
// their own Kubernetes token. It checks the token and the user's rights on
// every call, makes and finds the user's BackupBrowse sessions, and relays
// calls to the helper pod over a TLS connection that trusts only that session's
// certificate. See docs/specs/2026-10-22-backup-file-restore.md.
package gateway

import (
	"time"
)

// Rights-only name. No object of this kind is ever stored: it exists so that
// RBAC can say "may read the backups of this namespace".
const (
	RightsGroup    = "storage.ibmcloud.io"
	RightsResource = "backupfiles"
	RightsVerb     = "get"
)

// User is who made a call, as the Kubernetes API says.
type User struct {
	Name   string
	UID    string
	Groups []string
	Extra  map[string][]string
}

// Volume is one backed-up volume of a namespace.
type Volume struct {
	Policy           string     `json:"policy"`
	Namespace        string     `json:"namespace"`
	PVC              string     `json:"pvc"`
	VM               string     `json:"vm,omitempty"`
	LastBackupTime   *time.Time `json:"lastBackupTime,omitempty"`
	OldestBackupTime *time.Time `json:"oldestBackupTime,omitempty"`
	RestorePoints    int32      `json:"restorePoints"`
	Consistency      string     `json:"consistency,omitempty"`
	Retained         bool       `json:"retained,omitempty"`
}

// SessionRequest asks for a session on one backup of one volume. A nil Moment
// is the latest copy.
type SessionRequest struct {
	Policy    string     `json:"policy"`
	Namespace string     `json:"namespace"`
	PVC       string     `json:"pvc"`
	Moment    *time.Time `json:"moment,omitempty"`
}

// Session is what a user sees of a BackupBrowse.
type Session struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
}

// Access is one right to check as the user.
type Access struct {
	Namespace string
	Group     string
	Resource  string
	Verb      string
}

// RestoreRequest asks to copy chosen files out of one backup into a claim.
// Paths are in the escaped form that `kubectl backup ls` prints.
type RestoreRequest struct {
	Policy           string     `json:"policy"`
	Namespace        string     `json:"namespace"`
	PVC              string     `json:"pvc"`
	Moment           *time.Time `json:"moment,omitempty"`
	Paths            []string   `json:"paths"`
	TargetNamespace  string     `json:"targetNamespace"`
	TargetName       string     `json:"targetName"`
	Existing         bool       `json:"existing,omitempty"`
	SizeGB           int64      `json:"sizeGB,omitempty"`
	StorageClassName string     `json:"storageClassName,omitempty"`
}

// Restore is what a user sees of a BackupFileRestore.
type Restore struct {
	Name            string     `json:"name"`
	Phase           string     `json:"phase"`
	Message         string     `json:"message,omitempty"`
	TargetNamespace string     `json:"targetNamespace"`
	TargetName      string     `json:"targetName"`
	Existing        bool       `json:"existing,omitempty"`
	Folder          string     `json:"folder,omitempty"`
	SizeGB          int64      `json:"sizeGB,omitempty"`
	RestoredMoment  *time.Time `json:"restoredMoment,omitempty"`
	Latest          bool       `json:"latest,omitempty"`
	Files           int        `json:"files"`
	Dirs            int        `json:"dirs"`
	Links           int        `json:"links"`
	Skipped         int        `json:"skipped"`
	Unreadable      int        `json:"unreadable"`
	Bytes           int64      `json:"bytes"`
}

// ErrorBody is the body of every error.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

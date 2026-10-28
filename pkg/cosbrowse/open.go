package cosbrowse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/keyprotect"
)

// OpenOptions lets tests replace the machine. A zero value is the real thing.
type OpenOptions struct {
	// Kopia is the Kopia program; empty means "kopia".
	Kopia string
	// StateDir is Kopia's private folder; empty means DefaultStateDir.
	StateDir string
	// ReadFile reads a mounted Secret file; nil means os.ReadFile.
	ReadFile func(name string) ([]byte, error)
	// Unwrapper replaces the Key Protect client. Nil makes the real one.
	Unwrapper func(cfg keyprotect.Config) (keyprotect.Unwrapper, error)
	// Log gets the notes about the key (for example, that the emergency key is
	// used). Nil means stderr. It never gets a key.
	Log io.Writer
	// Repo, if set, replaces the Kopia program (tests).
	Repo Repo
	// backend replaces the S3 words (tests).
	backend []string
	// Timeout limits the start: the key, the connection and the first read.
	Timeout time.Duration
}

// Open reads the keys, connects to the repository in COS read-only and opens the
// backup of the volume. The backup key is unwrapped here, in the memory of this
// process, and given to the Kopia child in its environment; it is not written to
// disk. at is the time to open; the zero time opens the latest copy.
//
// Every error is a sentence for a person: a wrong key, an unreachable service
// or a missing backup is said in words, with no key in it.
func Open(ctx context.Context, src Source, at time.Time, opts OpenOptions) (*Opened, error) {
	if err := src.Validate(); err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	repo := opts.Repo
	if repo == nil {
		k, err := newKopiaFor(ctx, src, opts)
		if err != nil {
			return nil, err
		}
		repo = k
	}
	return OpenRepo(ctx, repo, Params{Policy: src.Policy, Volume: src.Volume, At: at})
}

func newKopiaFor(ctx context.Context, src Source, opts OpenOptions) (*KopiaRepo, error) {
	read := opts.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	log := opts.Log
	if log == nil {
		log = os.Stderr
	}
	file := func(name, what string) (string, error) {
		b, err := read(name)
		if err != nil {
			return "", fmt.Errorf("cannot read the %s: the Secret is not mounted or is missing its key", what)
		}
		return strings.TrimSpace(string(b)), nil
	}
	access, err := file(AccessKeyFile, "COS access key")
	if err != nil {
		return nil, err
	}
	secret, err := file(SecretKeyFile, "COS secret key")
	if err != nil {
		return nil, err
	}
	key, err := backupKey(ctx, src, opts, file, log)
	if err != nil {
		return nil, err
	}
	k := NewKopia(KopiaConfig{
		Binary: opts.Kopia, StateDir: opts.StateDir, Source: src,
		AccessKey: access, SecretKey: secret, Password: key, backend: opts.backend,
	})
	if err := k.Connect(ctx); err != nil {
		return nil, err
	}
	return k, nil
}

// backupKey returns the readable backup key. Key Protect unwraps the wrapped
// key. The emergency key is used only when Key Protect cannot be reached, or
// when the source says to use nothing else.
func backupKey(ctx context.Context, src Source, opts OpenOptions, file func(name, what string) (string, error), log io.Writer) (string, error) {
	emergency := ""
	if src.EmergencyKey {
		var err error
		if emergency, err = file(EmergencyKeyFile, "emergency backup key"); err != nil {
			return "", err
		}
	}
	if src.EmergencyOnly {
		if emergency == "" {
			return "", errors.New("the emergency backup key is empty")
		}
		return emergency, nil
	}
	wrapped, err := file(WrappedKeyFile, "wrapped backup key")
	if err != nil {
		return "", err
	}
	apiKey, err := file(APIKeyFile, "Key Protect API key")
	if err != nil {
		return "", err
	}
	cfg := keyprotect.Config{Region: src.KeyProtect.Region, InstanceID: src.KeyProtect.InstanceID, RootKeyID: src.KeyProtect.RootKeyID, APIKey: apiKey}
	newClient := opts.Unwrapper
	if newClient == nil {
		newClient = func(c keyprotect.Config) (keyprotect.Unwrapper, error) { return keyprotect.New(c) }
	}
	u, err := newClient(cfg)
	if err != nil {
		return "", fmt.Errorf("cannot use Key Protect: %w", err)
	}
	key, err := keyprotect.Resolve(ctx, u, wrapped, emergency, log)
	if err != nil {
		return "", fmt.Errorf("the backup key could not be unwrapped: %w", err)
	}
	return key, nil
}

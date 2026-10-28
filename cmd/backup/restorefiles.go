package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/gateway"
)

// repeated returns every value of a flag that may be given more than once, as
// --path A --path B or --path=A.
func repeated(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		flag := strings.TrimLeft(a, "-")
		if k, v, ok := strings.Cut(flag, "="); ok {
			if k == name {
				out = append(out, v)
			}
			continue
		}
		if flag == name && i+1 < len(args) {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

// readPathsFile reads paths, one per line in escaped form, blank lines ignored.
func readPathsFile(name string) ([]string, error) {
	f, err := os.Open(name) //nolint:gosec // the user chose the file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

// restoreRequestOf reads the flags of restore-files into a request.
func restoreRequestOf(args []string, flags map[string]string) (gateway.RestoreRequest, error) {
	var req gateway.RestoreRequest
	t, err := parseTarget(flags)
	if err != nil {
		return req, err
	}
	if t.session != "" {
		return req, errors.New("restore-files takes --policy and --pvc, not --session")
	}
	req.Policy, req.Namespace, req.PVC, req.Moment = t.policy, t.namespace, t.pvc, t.moment
	req.Source, req.FromCOS = t.source, t.bucket
	to := flags["to"]
	ns, name, ok := strings.Cut(to, "/")
	if !ok || ns == "" || name == "" {
		return req, errors.New("--to NAMESPACE/CLAIM is required")
	}
	req.TargetNamespace, req.TargetName = ns, name
	if flags["existing"] == "true" && flags["new-claim"] == "true" {
		return req, errors.New("give --new-claim or --existing, not both")
	}
	req.Existing = flags["existing"] == "true"
	if v := flags["size-gb"]; v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n < 1 {
			return req, errors.New("--size-gb must be a whole number of GiB")
		}
		req.SizeGB = n
	}
	req.StorageClassName = flags["storage-class"]
	if req.FromCOS != nil && !req.Existing && req.StorageClassName == "" {
		return req, errors.New("--storage-class is required for a new claim when the bucket is named: COS does not keep the class of the backed-up claim")
	}
	if req.Existing && (req.SizeGB != 0 || req.StorageClassName != "") {
		return req, errors.New("--size-gb and --storage-class are for a new claim; an existing claim keeps its size and class")
	}
	paths := repeated(args, "path")
	if f := flags["paths-file"]; f != "" {
		more, rerr := readPathsFile(f)
		if rerr != nil {
			return req, fmt.Errorf("--paths-file: %w", rerr)
		}
		paths = append(paths, more...)
	}
	if len(paths) == 0 {
		return req, errors.New("give at least one --path (or --paths-file)")
	}
	// The same check the gateway and the webhook make; the answer names the number of the path, not its text.
	parsed, cerr := filerestore.CheckPaths(paths)
	if cerr != nil {
		return req, errors.New(strings.ReplaceAll(cerr.Error(), "spec.paths", "--path"))
	}
	for _, p := range parsed {
		// --fs puts the file system of a VM disk in front of every path.
		withFSPath, ferr := withFS(flags, p)
		if ferr != nil {
			return req, ferr
		}
		req.Paths = append(req.Paths, browse.JoinPath(withFSPath))
	}
	return req, nil
}

func runRestoreFiles(ctx context.Context, gw *gwClient, out io.Writer, args []string) error {
	flags := parseBrowseArgs(args)
	timeout := 30 * time.Minute
	if v := flags["timeout"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("--timeout: %w", err)
		}
		timeout = d
	}
	if name := flags["status"]; name != "" && name != "true" {
		var r gateway.Restore
		if err := gw.doJSON(ctx, http.MethodGet, "/v1/restores/"+url.PathEscape(name), nil, &r); err != nil {
			return err
		}
		printRestore(out, r)
		return nil
	}
	req, err := restoreRequestOf(args, flags)
	if err != nil {
		return err
	}
	var r gateway.Restore
	if err := gw.doJSON(ctx, http.MethodPost, "/v1/restores", req, &r); err != nil {
		return err
	}
	where := "a new claim " + req.TargetNamespace + "/" + req.TargetName
	if req.Existing {
		where = "a new folder in the claim " + req.TargetNamespace + "/" + req.TargetName
	}
	when := "the latest copy"
	if req.Moment != nil {
		when = "the backup at or before " + req.Moment.UTC().Format(time.RFC3339)
	}
	from := req.Namespace + "/" + req.PVC
	switch {
	case req.FromCOS != nil:
		from = "volume " + req.FromCOS.Volume + " in the COS bucket " + req.FromCOS.Bucket
	case r.Source == v1alpha1.BackupSourceCOS:
		from += " in COS"
	}
	_, _ = fmt.Fprintf(out, "Restore %s asked: %d paths from %s (policy %s, %s) into %s\n",
		r.Name, len(req.Paths), from, req.Policy, when, where)
	if flags["wait"] != "true" {
		_, _ = fmt.Fprintf(out, "Follow it with: kubectl backup restore-files --status %s\n", r.Name)
		return nil
	}
	return waitRestoreFiles(ctx, gw, out, r.Name, timeout)
}

func waitRestoreFiles(ctx context.Context, gw *gwClient, out io.Writer, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		var r gateway.Restore
		if err := gw.doJSON(ctx, http.MethodGet, "/v1/restores/"+url.PathEscape(name), nil, &r); err != nil {
			return err
		}
		if r.Phase != last {
			last = r.Phase
			_, _ = fmt.Fprintf(out, "  %s %s\n", r.Phase, r.Message)
		}
		switch r.Phase {
		case "Done":
			printRestore(out, r)
			return nil
		case "Failed":
			return fmt.Errorf("the restore failed: %s", r.Message)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s; the restore goes on: kubectl backup restore-files --status %s", timeout, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

func printRestore(out io.Writer, r gateway.Restore) {
	_, _ = fmt.Fprintf(out, "Restore %s: %s", r.Name, r.Phase)
	if r.Message != "" {
		_, _ = fmt.Fprintf(out, " - %s", r.Message)
	}
	_, _ = fmt.Fprintln(out)
	if r.Phase != "Done" {
		return
	}
	if r.Existing {
		_, _ = fmt.Fprintf(out, "The files are in the folder %s of the claim %s/%s.\n", r.Folder, r.TargetNamespace, r.TargetName)
	} else {
		_, _ = fmt.Fprintf(out, "The new claim %s/%s holds the files.\n", r.TargetNamespace, r.TargetName)
	}
	_, _ = fmt.Fprintf(out, "%d files, %d folders, %d links, %d bytes", r.Files, r.Dirs, r.Links, r.Bytes)
	if r.Unreadable > 0 {
		_, _ = fmt.Fprintf(out, "; %d could not be read", r.Unreadable)
	}
	if r.Skipped > 0 {
		_, _ = fmt.Fprintf(out, "; %d devices, pipes or sockets were left out", r.Skipped)
	}
	_, _ = fmt.Fprintln(out)
}

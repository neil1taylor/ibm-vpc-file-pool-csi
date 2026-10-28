package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/gateway"
)

// gwClient talks to the backup gateway. Its http client adds the user's own
// Kubernetes credentials.
type gwClient struct {
	base string
	hc   *http.Client
}

// booleanFlags never take a value.
var booleanFlags = map[string]bool{"wait": true, "yes": true, "dry-run": true, "existing-bucket": true, "from-cos": true,
	"skip-marker-cleanup": true, "moments": true, "existing": true, "new-claim": true}

// positional returns the arguments that are not flags and not flag values.
func positional(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			out = append(out, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") || booleanFlags[name] {
			continue
		}
		if i+1 < len(args) && (!strings.HasPrefix(args[i+1], "-") || args[i+1] == "-") {
			i++
		}
	}
	return out
}

// parseBrowseArgs reads flags where a lone "-" counts as a value (for --out -).
func parseBrowseArgs(args []string) map[string]string {
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if k, v, ok := strings.Cut(name, "="); ok {
			flags[k] = v
			continue
		}
		if booleanFlags[name] {
			flags[name] = "true"
			continue
		}
		if i+1 < len(args) && (!strings.HasPrefix(args[i+1], "-") || args[i+1] == "-") {
			flags[name] = args[i+1]
			i++
		} else {
			flags[name] = "true"
		}
	}
	return flags
}

// apiError turns an error answer of the gateway into an error.
func apiError(resp *http.Response) error {
	var e gateway.ErrorBody
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return fmt.Errorf("%s (%s)", e.Message, e.Code)
	}
	return fmt.Errorf("the gateway answered %s", resp.Status)
}

func (g *gwClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the backup gateway: %w", err)
	}
	return resp, nil
}

// doJSON makes a call and decodes a JSON answer.
func (g *gwClient) doJSON(ctx context.Context, method, path string, body, out any) error {
	resp, err := g.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type target struct {
	policy, namespace, pvc string
	moment                 *time.Time
	session                string
}

func parseTarget(flags map[string]string) (target, error) {
	t := target{session: flags["session"]}
	if t.session != "" {
		return t, nil
	}
	t.policy = flags["policy"]
	ns, name, ok := strings.Cut(flags["pvc"], "/")
	if t.policy == "" || !ok || ns == "" || name == "" {
		return t, errors.New("--policy and --pvc NAMESPACE/NAME are required (or --session NAME)")
	}
	t.namespace, t.pvc = ns, name
	if m := flags["moment"]; m != "" {
		v, err := time.Parse(time.RFC3339, m)
		if err != nil {
			return t, fmt.Errorf("--moment must be a time like 2026-10-21T09:30:00Z: %w", err)
		}
		t.moment = &v
	}
	return t, nil
}

// pollEvery is how often to ask whether a new session is ready. The request to
// open a session does not wait: a proxy in front of the gateway would cut a long
// request (the OpenShift router does after 30 seconds).
var pollEvery = 2 * time.Second

// openSession returns the name of a ready session on the backup, opening or
// reusing one.
func (g *gwClient) openSession(ctx context.Context, t target, timeout time.Duration) (string, error) {
	if t.session != "" {
		return t.session, nil
	}
	var s gateway.Session
	req := gateway.SessionRequest{Policy: t.policy, Namespace: t.namespace, PVC: t.pvc, Moment: t.moment}
	if err := g.doJSON(ctx, http.MethodPost, "/v1/sessions", req, &s); err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for s.Phase != "Ready" {
		switch s.Phase {
		case "Failed", "Closed":
			return "", fmt.Errorf("the session %s: %s %s", s.Name, strings.ToLower(s.Phase), s.Message)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the session %s is still %s after %s: %s", s.Name, s.Phase, timeout, s.Message)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollEvery):
		}
		if err := g.doJSON(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(s.Name), nil, &s); err != nil {
			return "", err
		}
	}
	return s.Name, nil
}

func sessionTimeout(flags map[string]string) (time.Duration, error) {
	if v := flags["timeout"]; v != "" {
		return time.ParseDuration(v)
	}
	return 5 * time.Minute, nil
}

// pathOf reads the one optional path argument.
func pathOf(args []string) ([][]byte, error) {
	pos := positional(args)
	if len(pos) > 1 {
		return nil, errors.New("give one path")
	}
	p := "/"
	if len(pos) == 1 {
		p = pos[0]
	}
	path, err := browse.SplitPath(p)
	if err != nil {
		return nil, err
	}
	if err := browse.ValidatePath(path); err != nil {
		return nil, err
	}
	return path, nil
}

func modeString(e browse.Entry) string {
	var b [10]byte
	switch e.Type {
	case browse.TypeDir:
		b[0] = 'd'
	case browse.TypeSymlink:
		b[0] = 'l'
	case browse.TypeOther:
		b[0] = '?'
	default:
		b[0] = '-'
	}
	const rwx = "rwxrwxrwx"
	for i := range 9 {
		if e.Mode&(1<<(8-i)) != 0 {
			b[i+1] = rwx[i]
		} else {
			b[i+1] = '-'
		}
	}
	return string(b[:])
}

func runLs(ctx context.Context, gw *gwClient, out io.Writer, args []string) error {
	flags := parseBrowseArgs(args)
	t, err := parseTarget(flags)
	if err != nil {
		return err
	}
	timeout, err := sessionTimeout(flags)
	if err != nil {
		return err
	}
	path, err := pathOf(args)
	if err != nil {
		return err
	}
	name, err := gw.openSession(ctx, t, timeout)
	if err != nil {
		return err
	}
	base := "/v1/sessions/" + url.PathEscape(name)
	if flags["moments"] == "true" {
		var got struct {
			Moments []string `json:"moments"`
		}
		if err := gw.doJSON(ctx, http.MethodPost, base+"/moments", struct{}{}, &got); err != nil {
			return err
		}
		for _, m := range got.Moments {
			if v, err := time.Parse(browse.MomentLayout, m); err == nil {
				_, _ = fmt.Fprintln(out, v.UTC().Format(time.RFC3339))
			}
		}
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	var cursor []byte
	for {
		var page browse.ListPage
		req := browse.ListRequest{PathRequest: browse.PathRequest{Path: path}, Cursor: cursor}
		if err := gw.doJSON(ctx, http.MethodPost, base+"/list", req, &page); err != nil {
			return err
		}
		for _, e := range page.Entries {
			name := browse.Escape(e.Name)
			switch e.Type {
			case browse.TypeDir:
				name += "/"
			case browse.TypeSymlink:
				name += " -> " + browse.Escape(e.Link)
			}
			if !e.Readable {
				name += " (not readable)"
			}
			_, _ = fmt.Fprintf(w, "%s\t%d:%d\t%d\t%s\t%s\n", modeString(e), e.UID, e.GID, e.Size, e.MTime.UTC().Format("2006-01-02 15:04"), name)
		}
		if len(page.Next) == 0 {
			break
		}
		cursor = page.Next
	}
	return w.Flush()
}

func runBrowse(ctx context.Context, gw *gwClient, out io.Writer, args []string) error {
	flags := parseBrowseArgs(args)
	if name := flags["close"]; name != "" && name != "true" {
		if err := gw.doJSON(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(name), nil, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "closed session %s\n", name)
		return nil
	}
	t, err := parseTarget(flags)
	if err != nil {
		return err
	}
	timeout, err := sessionTimeout(flags)
	if err != nil {
		return err
	}
	name, err := gw.openSession(ctx, t, timeout)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "session %s is ready. Use it with --session %s; it ends after 15 minutes without use.\n", name, name)
	return nil
}

func runGet(ctx context.Context, gw *gwClient, out io.Writer, args []string) error {
	flags := parseBrowseArgs(args)
	t, err := parseTarget(flags)
	if err != nil {
		return err
	}
	timeout, err := sessionTimeout(flags)
	if err != nil {
		return err
	}
	if len(positional(args)) != 1 {
		return errors.New("give the path of the file or folder to get")
	}
	path, err := pathOf(args)
	if err != nil {
		return err
	}
	session, err := gw.openSession(ctx, t, timeout)
	if err != nil {
		return err
	}
	base := "/v1/sessions/" + url.PathEscape(session)
	var entry browse.Entry
	if err := gw.doJSON(ctx, http.MethodPost, base+"/stat", browse.PathRequest{Path: path}, &entry); err != nil {
		return err
	}
	name := "backup"
	if len(path) > 0 {
		name = string(path[len(path)-1])
	}
	switch entry.Type {
	case browse.TypeSymlink:
		return fmt.Errorf("%s is a symbolic link to %s; links are not downloaded", browse.JoinPath(path), browse.Escape(entry.Link))
	case browse.TypeFile:
		return getFile(ctx, gw, out, base, path, name, flags["out"])
	case browse.TypeDir:
		if dest := flags["extract"]; dest != "" {
			return getFolder(ctx, gw, out, base, path, func(r io.Reader) error { return extractTar(r, dest, out) })
		}
		return getFolder(ctx, gw, out, base, path, func(r io.Reader) error { return saveTo(r, out, flags["out"], name+".tar") })
	}
	return fmt.Errorf("%s is not a file or a folder", browse.JoinPath(path))
}

// saveTo writes r to the place --out says: "-" is standard output, an existing
// folder gets a file with the default name, anything else is the file name. An
// existing file is never replaced.
func saveTo(r io.Reader, stdout io.Writer, outFlag, defaultName string) error {
	if outFlag == "-" {
		_, err := io.Copy(stdout, r)
		return err
	}
	dest := defaultName
	if outFlag != "" {
		dest = outFlag
		if fi, err := os.Stat(outFlag); err == nil && fi.IsDir() {
			dest = filepath.Join(outFlag, defaultName)
		}
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the user chose the destination
	if err != nil {
		return err
	}
	if _, cerr := io.Copy(f, r); cerr != nil { //nolint:gosec // G703/G110: the user chose the destination; the size is the server's, limited by the gateway
		_ = f.Close()
		_ = os.Remove(dest) //nolint:gosec // G703: removes the file this command just made
		return cerr
	}
	return f.Close()
}

func getFile(ctx context.Context, gw *gwClient, out io.Writer, base string, path [][]byte, name, outFlag string) error {
	req := browse.FileRequest{PathRequest: browse.PathRequest{Path: path}}
	neg := int64(-1)
	req.Length = &neg
	resp, err := gw.do(ctx, http.MethodPost, base+"/file", req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return saveTo(resp.Body, out, outFlag, name)
}

func getFolder(ctx context.Context, gw *gwClient, _ io.Writer, base string, path [][]byte, sink func(io.Reader) error) error {
	resp, err := gw.do(ctx, http.MethodPost, base+"/tar", browse.PathRequest{Path: path})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return sink(resp.Body)
}

// extractTar unpacks a tar into dest. Every write goes through an os.Root, so
// no name can leave dest. Only folders and regular files are made; links,
// devices and anything else are skipped and reported. Special permission bits
// are not applied, and a file that exists is not replaced.
func extractTar(r io.Reader, dest string, out io.Writer) error {
	if err := os.MkdirAll(dest, 0o755); err != nil { //nolint:gosec // the user chose the destination
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	skipped := 0
	skip := func(why, name string) {
		skipped++
		_, _ = fmt.Fprintf(out, "skipped (%s): %s\n", why, browse.Escape([]byte(name)))
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the tar: %w", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				skip("outside the folder or not possible", hdr.Name)
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				skip("outside the folder or not possible", hdr.Name)
				continue
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(uint32(hdr.Mode&0o777))) //nolint:gosec // G115: masked to the nine permission bits
			if err != nil {
				skip("outside the folder, or it exists", hdr.Name)
				continue
			}
			_, cerr := io.CopyN(f, tr, hdr.Size)
			if clerr := f.Close(); cerr == nil {
				cerr = clerr
			}
			if cerr != nil {
				return fmt.Errorf("writing %s: %w", browse.Escape([]byte(name)), cerr)
			}
		case tar.TypeSymlink, tar.TypeLink:
			skip("link", hdr.Name)
		default:
			skip("not a file or folder", hdr.Name)
		}
	}
	if skipped > 0 {
		_, _ = fmt.Fprintf(out, "%d entries were skipped\n", skipped)
	}
	return nil
}

// gatewayInfoName is the ConfigMap, in the driver namespace, that says where
// the gateway is. The chart lets every signed-in user read it, and only it.
const gatewayInfoName = "backup-gateway-info"

// gatewayInfo reads the gateway's address (and its CA, if it has one) from the
// ConfigMap the chart makes.
func gatewayInfo(ctx context.Context, c client.Client, namespace string) (url, ca string, err error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gatewayInfoName}, cm); err != nil {
		return "", "", fmt.Errorf("cannot find the gateway address (ConfigMap %s/%s: %v); give it with --gateway https://HOST", namespace, gatewayInfoName, err)
	}
	if cm.Data["url"] == "" {
		return "", "", fmt.Errorf("ConfigMap %s/%s has no url; give the address with --gateway https://HOST", namespace, gatewayInfoName)
	}
	return cm.Data["url"], cm.Data["ca.crt"], nil
}

// newGatewayClient makes a client for the gateway that sends the user's own
// credentials from the kubeconfig (a token, or what an exec plugin gives). It
// trusts the CA of the gateway, not the cluster's.
func newGatewayClient(ctx context.Context, flags map[string]string) (*gwClient, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = flags["kubeconfig"]
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}
	base, ca := flags["gateway"], ""
	if base == "" {
		base = os.Getenv("KUBECTL_BACKUP_GATEWAY")
	}
	if base == "" {
		c, cerr := client.New(cfg, client.Options{Scheme: newScheme()})
		if cerr != nil {
			return nil, cerr
		}
		if base, ca, cerr = gatewayInfo(ctx, c, driverNamespace(flags)); cerr != nil {
			return nil, cerr
		}
	}
	if f := flags["gateway-ca-file"]; f != "" {
		b, rerr := os.ReadFile(f) //nolint:gosec // the user chose the file
		if rerr != nil {
			return nil, rerr
		}
		ca = string(b)
	}
	gcfg := rest.CopyConfig(cfg)
	gcfg.Host = base
	gcfg.TLSClientConfig = rest.TLSClientConfig{CAData: []byte(ca)}
	// A client certificate is for the Kubernetes API; the gateway needs a token.
	hc, err := rest.HTTPClientFor(gcfg)
	if err != nil {
		return nil, err
	}
	return &gwClient{base: strings.TrimSuffix(base, "/"), hc: hc}, nil
}

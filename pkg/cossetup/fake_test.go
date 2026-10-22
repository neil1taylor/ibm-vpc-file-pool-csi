package cossetup

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// fakeCloud is an in-memory stand-in for the IBM Cloud REST APIs that cos-setup
// calls. It records every call that is not a read.
type fakeCloud struct {
	mu        sync.Mutex
	writes    []string
	instances map[string]map[string]any
	buckets   map[string]*fakeBucket
	roles     map[string]map[string]any
	serviceID map[string]map[string]any
	policies  []map[string]any
	keys      map[string]map[string]any
	zones     map[string]map[string]any
	rules     []map[string]any
	kpKeys    map[string]bool
	nextID    int
	requests  int // every request, reads included
	// broadPolicyOnKey makes a new resource key add a policy on the whole
	// instance to the service ID, as a service key might.
	broadPolicyOnKey bool
	// hideKeySecret makes a read of an existing resource key leave out the
	// credentials, as IBM Cloud does not show an HMAC secret a second time.
	hideKeySecret bool
}

type fakeBucket struct {
	versioning, lock bool
	kpCRN            string
	objects          map[string]string
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{
		instances: map[string]map[string]any{}, buckets: map[string]*fakeBucket{},
		roles: map[string]map[string]any{}, serviceID: map[string]map[string]any{},
		keys: map[string]map[string]any{}, zones: map[string]map[string]any{},
		kpKeys: map[string]bool{"rootkey1": true},
	}
}

func (f *fakeCloud) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeCloud) writeCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.writes) }

func (f *fakeCloud) hasWrite(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.writes {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeCloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	}
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/rm/"):
		jsonOut(w, 200, map[string]any{"resources": []any{map[string]any{"id": "rg-1", "name": r.URL.Query().Get("name")}}})
	case strings.HasPrefix(p, "/rc/"):
		f.resourceController(w, r, in)
	case strings.HasPrefix(p, "/iam/"):
		f.iam(w, r, in)
	case strings.HasPrefix(p, "/cbr/"):
		f.cbr(w, r, in)
	case strings.HasPrefix(p, "/cos/"):
		f.cos(w, r, body)
	case strings.HasPrefix(p, "/kp/"):
		f.kp(w, r, in)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCloud) resourceController(w http.ResponseWriter, r *http.Request, in map[string]any) {
	switch {
	case r.URL.Path == "/rc/v2/resource_instances" && r.Method == http.MethodGet:
		var out []any
		if inst, ok := f.instances[r.URL.Query().Get("name")]; ok {
			out = append(out, inst)
		}
		jsonOut(w, 200, map[string]any{"resources": out})
	case r.URL.Path == "/rc/v2/resource_instances" && r.Method == http.MethodPost:
		guid := f.id("guid-cos")
		crn := "crn:v1:bluemix:public:cloud-object-storage:global:a/acct1:" + guid + "::"
		inst := map[string]any{"id": crn, "guid": guid, "crn": crn, "name": in["name"], "resource_group_id": in["resource_group"]}
		f.instances[nameOf(in)] = inst
		jsonOut(w, 201, inst)
	case r.URL.Path == "/rc/v2/resource_keys" && r.Method == http.MethodGet:
		var out []any
		if k, ok := f.keys[r.URL.Query().Get("name")]; ok {
			if f.hideKeySecret {
				k = map[string]any{"id": k["id"], "name": k["name"]}
			}
			out = append(out, k)
		}
		jsonOut(w, 200, map[string]any{"resources": out})
	case r.URL.Path == "/rc/v2/resource_keys" && r.Method == http.MethodPost:
		params, _ := in["parameters"].(map[string]any)
		k := map[string]any{"id": f.id("key"), "name": in["name"], "credentials": map[string]any{
			"cos_hmac_keys": map[string]any{"access_key_id": "AKIDFAKE", "secret_access_key": "SECRETHMACFAKE"}}}
		k["serviceid_crn"] = params["serviceid_crn"]
		f.keys[nameOf(in)] = k
		if f.broadPolicyOnKey {
			f.policies = append(f.policies, map[string]any{"id": f.id("pol"), "type": "access",
				"subjects": []any{map[string]any{"attributes": []any{map[string]any{"name": "iam_id", "value": "iam-ServiceId-1"}}}},
				"roles":    []any{map[string]any{"role_id": "crn:v1:bluemix:public:iam::::serviceRole:Writer"}},
				"resources": []any{map[string]any{"attributes": []any{
					map[string]any{"name": "serviceName", "value": "cloud-object-storage"},
					map[string]any{"name": "serviceInstance", "value": in["source"]}}}}})
		}
		jsonOut(w, 201, k)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCloud) iam(w http.ResponseWriter, r *http.Request, in map[string]any) {
	switch {
	case r.URL.Path == "/iam/v2/roles" && r.Method == http.MethodGet:
		var custom []any
		for _, role := range f.roles {
			custom = append(custom, role)
		}
		jsonOut(w, 200, map[string]any{"custom_roles": custom})
	case r.URL.Path == "/iam/v2/roles" && r.Method == http.MethodPost:
		in["id"] = f.id("role")
		in["crn"] = "crn:v1:bluemix:public:iam::a/acct1::customRole:" + nameOf(in)
		f.roles[nameOf(in)] = in
		jsonOut(w, 201, in)
	case r.URL.Path == "/iam/v1/serviceids" && r.Method == http.MethodGet:
		var out []any
		if s, ok := f.serviceID[r.URL.Query().Get("name")]; ok {
			out = append(out, s)
		}
		jsonOut(w, 200, map[string]any{"serviceids": out})
	case r.URL.Path == "/iam/v1/serviceids" && r.Method == http.MethodPost:
		in["id"] = "ServiceId-1"
		in["iam_id"] = "iam-ServiceId-1"
		in["crn"] = "crn:v1:bluemix:public:iam-identity::a/acct1::serviceid:ServiceId-1"
		f.serviceID[nameOf(in)] = in
		jsonOut(w, 201, in)
	case r.URL.Path == "/iam/v1/policies" && r.Method == http.MethodGet:
		out := []any{}
		for _, pol := range f.policies {
			if pol["type"] != r.URL.Query().Get("type") {
				continue
			}
			if iam := r.URL.Query().Get("iam_id"); iam != "" && !strings.Contains(fmt.Sprint(pol["subjects"]), iam) {
				continue
			}
			out = append(out, pol)
		}
		jsonOut(w, 200, map[string]any{"policies": out})
	case r.URL.Path == "/iam/v1/policies" && r.Method == http.MethodPost:
		in["id"] = f.id("pol")
		f.policies = append(f.policies, in)
		jsonOut(w, 201, in)
	case strings.HasPrefix(r.URL.Path, "/iam/v1/policies/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/iam/v1/policies/")
		for i, pol := range f.policies {
			if pol["id"] == id {
				f.policies = append(f.policies[:i], f.policies[i+1:]...)
				w.WriteHeader(204)
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCloud) cbr(w http.ResponseWriter, r *http.Request, in map[string]any) {
	switch {
	case r.URL.Path == "/cbr/v1/zones" && r.Method == http.MethodGet:
		out := []any{}
		for _, z := range f.zones {
			out = append(out, z)
		}
		jsonOut(w, 200, map[string]any{"zones": out})
	case r.URL.Path == "/cbr/v1/zones" && r.Method == http.MethodPost:
		in["id"] = f.id("zone")
		id, _ := in["id"].(string)
		f.zones[id] = in
		jsonOut(w, 201, in)
	case strings.HasPrefix(r.URL.Path, "/cbr/v1/zones/") && r.Method == http.MethodGet:
		if z, ok := f.zones[strings.TrimPrefix(r.URL.Path, "/cbr/v1/zones/")]; ok {
			jsonOut(w, 200, z)
			return
		}
		http.NotFound(w, r)
	case r.URL.Path == "/cbr/v1/rules" && r.Method == http.MethodGet:
		out := []any{}
		for _, ru := range f.rules {
			out = append(out, ru)
		}
		jsonOut(w, 200, map[string]any{"rules": out})
	case r.URL.Path == "/cbr/v1/rules" && r.Method == http.MethodPost:
		in["id"] = f.id("rule")
		f.rules = append(f.rules, in)
		jsonOut(w, 201, in)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCloud) cos(w http.ResponseWriter, r *http.Request, body []byte) {
	rest := strings.TrimPrefix(r.URL.Path, "/cos/")
	name, key, _ := strings.Cut(rest, "/")
	b := f.buckets[name]
	q := r.URL.Query()
	switch {
	case key == "" && r.Method == http.MethodHead:
		if b == nil {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
	case key == "" && r.Method == http.MethodPut && len(q) == 0:
		if b != nil {
			w.WriteHeader(409)
			return
		}
		f.buckets[name] = &fakeBucket{objects: map[string]string{}, kpCRN: r.Header.Get("ibm-sse-kp-customer-root-key-crn")}
		w.WriteHeader(200)
	case b == nil:
		w.WriteHeader(404)
	case key == "" && q.Has("versioning") && r.Method == http.MethodPut:
		b.versioning = strings.Contains(string(body), "<Status>Enabled</Status>")
	case key == "" && q.Has("versioning"):
		st := ""
		if b.versioning {
			st = "<Status>Enabled</Status>"
		}
		_, _ = fmt.Fprintf(w, "<VersioningConfiguration>%s</VersioningConfiguration>", st)
	case key == "" && q.Has("object-lock") && r.Method == http.MethodPut:
		b.lock = strings.Contains(string(body), "<ObjectLockEnabled>Enabled</ObjectLockEnabled>")
	case key == "" && q.Has("object-lock"):
		if !b.lock {
			w.WriteHeader(404)
			return
		}
		_, _ = fmt.Fprint(w, "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>")
	case r.Method == http.MethodPut:
		b.objects[key] = string(body)
	case r.Method == http.MethodGet:
		if v, ok := b.objects[key]; ok {
			_, _ = fmt.Fprint(w, v)
			return
		}
		w.WriteHeader(404)
	default:
		w.WriteHeader(405)
	}
}

func (f *fakeCloud) kp(w http.ResponseWriter, r *http.Request, in map[string]any) {
	id := strings.TrimPrefix(r.URL.Path, "/kp/api/v2/keys/")
	if !f.kpKeys[id] {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		jsonOut(w, 200, map[string]any{"resources": []any{map[string]any{"id": id}}})
		return
	}
	plain, _ := in["plaintext"].(string)
	pt, _ := base64.StdEncoding.DecodeString(plain)
	rev := make([]byte, len(pt))
	for i := range pt {
		rev[len(pt)-1-i] = pt[i] ^ 0x5a
	}
	jsonOut(w, 200, map[string]any{"ciphertext": "kp:" + hex.EncodeToString(rev)})
}

func nameOf(in map[string]any) string { s, _ := in["name"].(string); return s }

func startFake(f *fakeCloud) *httptest.Server { return httptest.NewServer(f) }

package cossetup

import (
	"bufio"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 is a transfer check that the S3 API asks for, not security
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

type attribute struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type attrSet struct {
	Attributes []attribute `json:"attributes"`
}

func (a attrSet) get(name string) string {
	for _, at := range a.Attributes {
		if at.Name == name {
			return at.Value
		}
	}
	return ""
}

func (a attrSet) has(want map[string]string) bool {
	for k, v := range want {
		if a.get(k) != v {
			return false
		}
	}
	return true
}

func attrs(kv ...string) attrSet {
	var s attrSet
	for i := 0; i+1 < len(kv); i += 2 {
		s.Attributes = append(s.Attributes, attribute{Name: kv[i], Value: kv[i+1]})
	}
	return s
}

type policy struct {
	ID       string    `json:"id,omitempty"`
	Type     string    `json:"type"`
	Subjects []attrSet `json:"subjects"`
	Roles    []struct {
		RoleID string `json:"role_id"`
	} `json:"roles"`
	Resources []attrSet `json:"resources"`
}

func (p policy) hasRole(id string) bool {
	for _, r := range p.Roles {
		if r.RoleID == id {
			return true
		}
	}
	return false
}

func (p policy) anySubject(want map[string]string) bool {
	for _, s := range p.Subjects {
		if s.has(want) {
			return true
		}
	}
	return false
}

func (p policy) anyResource(want map[string]string) bool {
	for _, s := range p.Resources {
		if s.has(want) {
			return true
		}
	}
	return false
}

func policyBody(typ string, subject attrSet, roleID string, resource attrSet) map[string]any {
	return map[string]any{
		"type": typ, "subjects": []attrSet{subject},
		"roles":     []map[string]string{{"role_id": roleID}},
		"resources": []attrSet{resource},
	}
}

const readerRole = "crn:v1:bluemix:public:iam::::serviceRole:Reader"

// run is one pass over the steps. A pass with apply false only reads and
// prints; a pass with apply true makes what is missing.
type run struct {
	s       *Setup
	o       Options
	token   string
	acct    string
	apply   bool
	in      *bufio.Reader
	out     io.Writer
	creates int

	kp        kpKey
	rgID      string
	instGUID  string
	instKnown bool // false when the instance is still to be made
	roleCRN   string
	sidKnown  bool
	sidIAM    string
	sidCRN    string

	bucketExists bool
	bucketOurs   bool
	versionOn    bool
	lockOn       bool

	zoneID     string
	zoneAddrs  []zoneAddr
	zoneExists bool

	hmacID, hmacSecret string
	hmacKnown          bool
	keyWritten         bool // the COS key file was written in this pass

	wrapped   string // the wrapped backup key, as stored
	keyNeeded bool   // a new backup key will be made
}

func (r *run) say(format string, a ...any) { _, _ = fmt.Fprintf(r.out, format, a...) }

// step reports one object. exists says it is already there; create makes it.
func (r *run) step(name string, exists bool, desc string, create func() error) error {
	switch {
	case exists:
		if !r.apply {
			r.say("  ok            %s (already there; left as it is)\n", name)
		}
	case !r.apply:
		r.creates++
		r.say("  would create  %s\n", name)
		if desc != "" {
			r.say("%s\n", indent(desc, "                "))
		}
	default:
		r.creates++
		r.say("  creating      %s ... ", name)
		if err := create(); err != nil {
			r.say("failed\n")
			return fmt.Errorf("%s: %w", name, err)
		}
		r.say("done\n")
	}
	return nil
}

func pretty(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func (r *run) all(ctx context.Context) error {
	var err error
	if r.kp, err = parseRootKey(r.o.RootKeyCRN); err != nil {
		return err
	}
	for _, f := range []func(context.Context) error{
		r.preflight, r.instance, r.authorization, r.bucket, r.versioning, r.objectLock,
		r.role, r.serviceID, r.bucketPolicy, r.rootKeyPolicy, r.hmac, r.keyFile,
		r.zone, r.rule, r.backupKey, r.secretFile,
	} {
		if err = f(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ---- reads before anything is made ----

func (r *run) preflight(ctx context.Context) error {
	base := r.s.Endpoints.KeyProtect(r.kp.region)
	rep, err := r.call(ctx, http.MethodGet, base+"/api/v2/keys/"+url.PathEscape(r.kp.id), nil, map[string]string{"Bluemix-Instance": r.kp.instance})
	if _, err = must("reading the Key Protect root key (it must exist, and your login must be able to read it)", rep, err); err != nil {
		return err
	}
	q := url.Values{"account_id": {r.acct}, "name": {r.o.ResourceGroup}}
	rep, err = r.call(ctx, http.MethodGet, r.s.Endpoints.ResourceManager+"/v2/resource_groups?"+q.Encode(), nil, nil)
	if _, err = must("finding the resource group", rep, err); err != nil {
		return err
	}
	var groups struct {
		Resources []struct{ ID string } `json:"resources"`
	}
	if err = rep.decode(&groups); err != nil || len(groups.Resources) == 0 {
		return fmt.Errorf("resource group %q not found", r.o.ResourceGroup)
	}
	r.rgID = groups.Resources[0].ID
	return r.checkCaller(ctx)
}

// checkCaller refuses if the caller's own address is not in the zone: a rule
// that enforces would lock the caller out of the bucket.
func (r *run) checkCaller(ctx context.Context) error {
	if err := r.resolveZone(ctx); err != nil {
		return err
	}
	me := r.o.MyAddress
	if me == "" {
		var err error
		if me, err = r.s.Address.Address(ctx); err != nil {
			return err
		}
	}
	if zoneHolds(r.zoneAddrs, me) {
		return nil
	}
	if r.o.CBRMode == "enabled" {
		return fmt.Errorf("your address %s is not in the CBR zone, so an enforced rule would lock you out of the bucket. Add it (--admin-address %s), or use --cbr-mode report", me, me)
	}
	if !r.apply {
		r.say("  warning       your address %s is not in the CBR zone; the rule only reports, but enforce it only after you add it\n", me)
	}
	return nil
}

func (r *run) resolveZone(ctx context.Context) error {
	e := r.s.Endpoints.CBR
	if r.o.ZoneID != "" {
		rep, err := r.call(ctx, http.MethodGet, e+"/v1/zones/"+url.PathEscape(r.o.ZoneID), nil, nil)
		if rep.status == http.StatusNotFound {
			return fmt.Errorf("CBR zone %q does not exist", r.o.ZoneID)
		}
		if _, err = must("reading the CBR zone", rep, err); err != nil {
			return err
		}
		var z struct {
			Addresses []zoneAddr `json:"addresses"`
		}
		if err = rep.decode(&z); err != nil {
			return err
		}
		r.zoneID, r.zoneAddrs, r.zoneExists = r.o.ZoneID, z.Addresses, true
		return nil
	}
	rep, err := r.call(ctx, http.MethodGet, e+"/v1/zones?"+url.Values{"account_id": {r.acct}}.Encode(), nil, nil)
	if _, err = must("listing CBR zones", rep, err); err != nil {
		return err
	}
	var zl struct {
		Zones []struct {
			ID        string     `json:"id"`
			Name      string     `json:"name"`
			Addresses []zoneAddr `json:"addresses"`
		} `json:"zones"`
	}
	if err = rep.decode(&zl); err != nil {
		return err
	}
	for _, z := range zl.Zones {
		if z.Name == r.zoneName() {
			r.zoneID, r.zoneAddrs, r.zoneExists = z.ID, z.Addresses, true
			return nil
		}
	}
	r.zoneAddrs = []zoneAddr{{Type: "vpc", Value: r.o.VPCCRN}}
	for _, a := range r.o.AdminAddresses {
		za, err := parseAddress(a)
		if err != nil {
			return err
		}
		r.zoneAddrs = append(r.zoneAddrs, za)
	}
	return nil
}

func (r *run) zoneName() string { return r.o.Bucket + "-allowed" }

// ---- steps ----

func (r *run) instance(ctx context.Context) error {
	e := r.s.Endpoints.ResourceController
	rep, err := r.call(ctx, http.MethodGet, e+"/v2/resource_instances?"+url.Values{"name": {r.o.InstanceName}}.Encode(), nil, nil)
	if _, err = must("looking for the COS instance", rep, err); err != nil {
		return err
	}
	var list struct {
		Resources []struct{ GUID, Name string } `json:"resources"`
	}
	if err = rep.decode(&list); err != nil {
		return err
	}
	for _, i := range list.Resources {
		if i.Name == r.o.InstanceName {
			r.instGUID, r.instKnown = i.GUID, true
		}
	}
	body := map[string]any{"name": r.o.InstanceName, "target": "bluemix-global", "resource_group": r.rgID, "resource_plan_id": cosStandardPlan}
	return r.step("COS instance "+r.o.InstanceName, r.instKnown, pretty(body), func() error {
		rep, err := r.call(ctx, http.MethodPost, e+"/v2/resource_instances", body, nil)
		if _, err = must("making the COS instance", rep, err); err != nil {
			return err
		}
		var made struct{ GUID string }
		if err = rep.decode(&made); err != nil || made.GUID == "" {
			return fmt.Errorf("the answer holds no instance id")
		}
		r.instGUID, r.instKnown = made.GUID, true
		return nil
	})
}

func (r *run) authorization(ctx context.Context) error {
	e := r.s.Endpoints.IAM
	exists := false
	if r.instKnown {
		pols, err := r.listPolicies(ctx, url.Values{"account_id": {r.acct}, "type": {"authorization"}})
		if err != nil {
			return err
		}
		for _, p := range pols {
			if p.anySubject(map[string]string{"serviceName": COSServiceName, "serviceInstance": r.instGUID}) &&
				p.anyResource(map[string]string{"serviceName": r.kp.service, "serviceInstance": r.kp.instance}) {
				exists = true
			}
		}
	}
	guid := r.instGUID
	if !r.instKnown {
		guid = "(the new instance)"
	}
	body := policyBody("authorization",
		attrs("serviceName", COSServiceName, "accountId", r.acct, "serviceInstance", guid), readerRole,
		attrs("serviceName", r.kp.service, "accountId", r.acct, "serviceInstance", r.kp.instance))
	return r.step("authorization: COS may read Key Protect keys (for the bucket's encryption)", exists, pretty(body), func() error {
		body := policyBody("authorization",
			attrs("serviceName", COSServiceName, "accountId", r.acct, "serviceInstance", r.instGUID), readerRole,
			attrs("serviceName", r.kp.service, "accountId", r.acct, "serviceInstance", r.kp.instance))
		rep, err := r.call(ctx, http.MethodPost, e+"/v1/policies", body, nil)
		_, err = must("making the authorization policy", rep, err)
		return err
	})
}

func (r *run) listPolicies(ctx context.Context, q url.Values) ([]policy, error) {
	rep, err := r.call(ctx, http.MethodGet, r.s.Endpoints.IAM+"/v1/policies?"+q.Encode(), nil, nil)
	if _, err = must("listing IAM policies", rep, err); err != nil {
		return nil, err
	}
	var out struct {
		Policies []policy `json:"policies"`
	}
	if err = rep.decode(&out); err != nil {
		return nil, err
	}
	return out.Policies, nil
}

func (r *run) bucketURL() string { return r.s.Endpoints.COS(r.o.Region) + "/" + r.o.Bucket }

func (r *run) cosHeaders() map[string]string {
	if !r.instKnown {
		return nil
	}
	return map[string]string{"ibm-service-instance-id": r.instGUID}
}

func (r *run) bucket(ctx context.Context) error {
	rep, err := r.call(ctx, http.MethodHead, r.bucketURL(), nil, r.cosHeaders())
	if err != nil {
		return err
	}
	switch rep.status {
	case http.StatusOK:
		r.bucketExists = true
	case http.StatusNotFound:
	default:
		return fmt.Errorf("bucket %s: HTTP %d: the name may belong to someone else (bucket names are global) or your login cannot reach it", r.o.Bucket, rep.status)
	}
	if r.bucketExists {
		rep, err = r.call(ctx, http.MethodGet, r.bucketURL()+"/"+MarkerObject, nil, r.cosHeaders())
		if err != nil {
			return err
		}
		r.bucketOurs = rep.ok()
		if !r.bucketOurs && !r.o.ExistingBucket {
			return fmt.Errorf("bucket %s exists, and this command did not make it; it will not touch it. Add --existing-bucket to use it as it is (it must already have versioning and object lock)", r.o.Bucket)
		}
		r.versionOn, r.lockOn, err = r.bucketSettings(ctx)
		if err != nil {
			return err
		}
		if !r.bucketOurs && (!r.versionOn || !r.lockOn) {
			return fmt.Errorf("bucket %s was not made by this command and has versioning=%v and object lock=%v; both must be on. This command does not change a bucket it did not make", r.o.Bucket, r.versionOn, r.lockOn)
		}
		if !r.instKnown {
			return fmt.Errorf("bucket %s exists, but the COS instance %q does not: pass --instance with the name of the COS instance that holds the bucket", r.o.Bucket, r.o.InstanceName)
		}
	}
	desc := fmt.Sprintf("PUT %s (location %s-smart, encrypted with root key %s)\nthen a marker object %s", r.o.Bucket, r.o.Region, r.o.RootKeyCRN, MarkerObject)
	return r.step("bucket "+r.o.Bucket, r.bucketExists, desc, func() error {
		h := r.cosHeaders()
		h["ibm-sse-kp-encryption-algorithm"] = "AES256"
		h["ibm-sse-kp-customer-root-key-crn"] = r.o.RootKeyCRN
		h["Content-Type"] = "text/plain"
		cfg := []byte("<CreateBucketConfiguration><LocationConstraint>" + r.o.Region + "-smart</LocationConstraint></CreateBucketConfiguration>")
		rep, err := r.call(ctx, http.MethodPut, r.bucketURL(), cfg, h)
		if _, err = must("making the bucket", rep, err); err != nil {
			return err
		}
		r.bucketExists, r.bucketOurs = true, true
		rep, err = r.call(ctx, http.MethodPut, r.bucketURL()+"/"+MarkerObject, []byte(`{"madeBy":"kubectl backup cos-setup"}`), r.cosHeaders())
		_, err = must("writing the marker object", rep, err)
		return err
	})
}

func (r *run) bucketSettings(ctx context.Context) (versioning, lock bool, err error) {
	rep, err := r.call(ctx, http.MethodGet, r.bucketURL()+"?versioning", nil, r.cosHeaders())
	if _, err = must("reading bucket versioning", rep, err); err != nil {
		return false, false, err
	}
	versioning = strings.Contains(string(rep.body), "<Status>Enabled</Status>")
	rep, err = r.call(ctx, http.MethodGet, r.bucketURL()+"?object-lock", nil, r.cosHeaders())
	if err != nil {
		return false, false, err
	}
	lock = rep.ok() && strings.Contains(string(rep.body), "<ObjectLockEnabled>Enabled</ObjectLockEnabled>")
	return versioning, lock, nil
}

func md5Header(b []byte) map[string]string {
	sum := md5.Sum(b) //nolint:gosec // see import
	return map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:]), "Content-Type": "text/plain"}
}

func (r *run) versioning(ctx context.Context) error {
	cfg := []byte("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>")
	return r.step("bucket versioning", r.versionOn, "PUT ?versioning Status=Enabled", func() error {
		rep, err := r.call(ctx, http.MethodPut, r.bucketURL()+"?versioning", cfg, md5Header(cfg))
		_, err = must("turning on versioning", rep, err)
		r.versionOn = err == nil
		return err
	})
}

func (r *run) objectLock(ctx context.Context) error {
	cfg := []byte("<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>")
	return r.step("bucket object lock (no default keep time)", r.lockOn, "PUT ?object-lock ObjectLockEnabled=Enabled", func() error {
		rep, err := r.call(ctx, http.MethodPut, r.bucketURL()+"?object-lock", cfg, md5Header(cfg))
		_, err = must("turning on object lock", rep, err)
		r.lockOn = err == nil
		return err
	})
}

func (r *run) role(ctx context.Context) error {
	e := r.s.Endpoints.IAM
	q := url.Values{"account_id": {r.acct}, "service_name": {COSServiceName}}
	rep, err := r.call(ctx, http.MethodGet, e+"/v2/roles?"+q.Encode(), nil, nil)
	if _, err = must("listing custom roles", rep, err); err != nil {
		return err
	}
	var roles struct {
		Custom []struct {
			Name    string   `json:"name"`
			CRN     string   `json:"crn"`
			Actions []string `json:"actions"`
		} `json:"custom_roles"`
	}
	if err = rep.decode(&roles); err != nil {
		return err
	}
	found := false
	for _, c := range roles.Custom {
		if c.Name != RoleName {
			continue
		}
		got, want := append([]string(nil), c.Actions...), WriterActions()
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return fmt.Errorf("the custom role %s exists with other actions than this command needs; it does not change a role. Check it, or delete it yourself and run again", RoleName)
		}
		found, r.roleCRN = true, c.CRN
	}
	body := map[string]any{
		"name": RoleName, "display_name": RoleDisplayName, "account_id": r.acct, "service_name": COSServiceName,
		"description": "Backup export: read, write, extend locks, hide with a delete marker. No delete of versions, no skip of locks, no bucket changes.",
		"actions":     WriterActions(),
	}
	return r.step("custom role "+RoleName, found, pretty(body), func() error {
		rep, err := r.call(ctx, http.MethodPost, e+"/v2/roles", body, nil)
		if _, err = must("making the custom role", rep, err); err != nil {
			return err
		}
		var made struct{ CRN string }
		if err = rep.decode(&made); err != nil || made.CRN == "" {
			made.CRN = "crn:v1:bluemix:public:iam::a/" + r.acct + "::customRole:" + RoleName
		}
		r.roleCRN = made.CRN
		return nil
	})
}

func (r *run) sidName() string { return r.o.Bucket + "-driver" }

func (r *run) serviceID(ctx context.Context) error {
	e := r.s.Endpoints.IAM
	rep, err := r.call(ctx, http.MethodGet, e+"/v1/serviceids?"+url.Values{"account_id": {r.acct}, "name": {r.sidName()}}.Encode(), nil, nil)
	if _, err = must("looking for the service ID", rep, err); err != nil {
		return err
	}
	var list struct {
		ServiceIDs []struct {
			Name  string `json:"name"`
			IAMID string `json:"iam_id"`
			CRN   string `json:"crn"`
		} `json:"serviceids"`
	}
	if err = rep.decode(&list); err != nil {
		return err
	}
	for _, s := range list.ServiceIDs {
		if s.Name == r.sidName() {
			r.sidKnown, r.sidIAM, r.sidCRN = true, s.IAMID, s.CRN
		}
	}
	body := map[string]any{"account_id": r.acct, "name": r.sidName(), "description": "Driver key for the backup export bucket " + r.o.Bucket}
	return r.step("service ID "+r.sidName(), r.sidKnown, pretty(body), func() error {
		rep, err := r.call(ctx, http.MethodPost, e+"/v1/serviceids", body, nil)
		if _, err = must("making the service ID", rep, err); err != nil {
			return err
		}
		var made struct {
			IAMID string `json:"iam_id"`
			CRN   string `json:"crn"`
		}
		if err = rep.decode(&made); err != nil || made.IAMID == "" {
			return fmt.Errorf("the answer holds no service ID")
		}
		r.sidKnown, r.sidIAM, r.sidCRN = true, made.IAMID, made.CRN
		return nil
	})
}

func (r *run) subjectPolicies(ctx context.Context) ([]policy, error) {
	if !r.sidKnown {
		return nil, nil
	}
	return r.listPolicies(ctx, url.Values{"account_id": {r.acct}, "iam_id": {r.sidIAM}, "type": {"access"}})
}

func (r *run) bucketPolicy(ctx context.Context) error {
	pols, err := r.subjectPolicies(ctx)
	if err != nil {
		return err
	}
	res := map[string]string{"serviceName": COSServiceName, "serviceInstance": r.instGUID, "resourceType": "bucket", "resource": r.o.Bucket}
	exists := false
	for _, p := range pols {
		if r.instKnown && p.anyResource(res) && p.hasRole(r.roleCRN) {
			exists = true
		}
	}
	body := func() policy {
		var p policy
		b, _ := json.Marshal(policyBody("access", attrs("iam_id", r.sidIAM), r.roleCRN,
			attrs("accountId", r.acct, "serviceName", COSServiceName, "serviceInstance", r.instGUID, "resourceType", "bucket", "resource", r.o.Bucket)))
		_ = json.Unmarshal(b, &p)
		return p
	}
	desc := pretty(body())
	return r.step("policy: the service ID gets "+RoleName+" on bucket "+r.o.Bucket+" only", exists, desc, func() error {
		rep, err := r.call(ctx, http.MethodPost, r.s.Endpoints.IAM+"/v1/policies", body(), nil)
		_, err = must("making the bucket policy", rep, err)
		return err
	})
}

func (r *run) rootKeyPolicy(ctx context.Context) error {
	pols, err := r.subjectPolicies(ctx)
	if err != nil {
		return err
	}
	res := map[string]string{"serviceName": r.kp.service, "serviceInstance": r.kp.instance, "resourceType": "key", "resource": r.kp.id}
	exists := false
	for _, p := range pols {
		if p.anyResource(res) && p.hasRole(readerRole) {
			exists = true
		}
	}
	body := func() map[string]any {
		return policyBody("access", attrs("iam_id", r.sidIAM), readerRole,
			attrs("accountId", r.acct, "serviceName", r.kp.service, "serviceInstance", r.kp.instance, "resourceType", "key", "resource", r.kp.id))
	}
	return r.step("policy: the service ID gets Reader on the root key", exists, pretty(body()), func() error {
		rep, err := r.call(ctx, http.MethodPost, r.s.Endpoints.IAM+"/v1/policies", body(), nil)
		_, err = must("making the root key policy", rep, err)
		return err
	})
}

func (r *run) hmacName() string { return r.o.Bucket + "-hmac" }

func (r *run) hmac(ctx context.Context) error {
	e := r.s.Endpoints.ResourceController
	hmacExists := false
	if r.sidKnown {
		rep, err := r.call(ctx, http.MethodGet, e+"/v2/resource_keys?"+url.Values{"name": {r.hmacName()}}.Encode(), nil, nil)
		if _, err = must("looking for the HMAC key", rep, err); err != nil {
			return err
		}
		var list struct {
			Resources []keyReply `json:"resources"`
		}
		if err = rep.decode(&list); err != nil {
			return err
		}
		for _, k := range list.Resources {
			if k.Name == r.hmacName() {
				hmacExists = true
				r.setHMAC(k)
			}
		}
	}
	_, statErr := os.Stat(r.o.KeyFile)
	switch {
	case hmacExists && !r.hmacKnown && statErr != nil:
		return fmt.Errorf("the HMAC key %s exists, but IBM Cloud shows its secret only when the key is made, and it cannot be shown again. This command did not make another. To get a new one, delete the HMAC key (ibmcloud resource service-key-delete %s) and run this command again", r.hmacName(), r.hmacName())
	case !hmacExists && statErr == nil:
		return fmt.Errorf("the key file %s exists, but the HMAC key %s does not. A new HMAC key needs a new file, because the secret must be saved the moment it is made and a file is never replaced: move %s away, then run this command again", r.o.KeyFile, r.hmacName(), r.o.KeyFile)
	}
	var before map[string]bool
	body := map[string]any{"name": r.hmacName(), "source": r.instGUID, "role": "Writer",
		"parameters": map[string]any{"HMAC": true, "serviceid_crn": r.sidCRN}}
	made := false
	err := r.step("HMAC key "+r.hmacName()+" (the secret goes to the key file the moment the key is made, never to the screen; IBM Cloud also adds an instance-wide policy for the key, and this command removes that one policy)", hmacExists, pretty(body), func() error {
		pols, err := r.subjectPolicies(ctx)
		if err != nil {
			return err
		}
		before = map[string]bool{}
		for _, p := range pols {
			before[p.ID] = true
		}
		rep, err := r.call(ctx, http.MethodPost, e+"/v2/resource_keys", body, nil)
		if _, err = must("making the HMAC key", rep, err); err != nil {
			return err
		}
		made = true
		var k keyReply
		if err = rep.decode(&k); err != nil {
			return err
		}
		r.setHMAC(k)
		if !r.hmacKnown {
			return fmt.Errorf("the answer holds no HMAC key")
		}
		return r.writeKeyFile()
	})
	if err != nil || !r.sidKnown {
		return err
	}
	if made {
		if err = r.removeImplicitPolicy(ctx, before); err != nil {
			return err
		}
	}
	return r.checkNarrow(ctx)
}

// removeImplicitPolicy deletes the policy that IBM Cloud adds to the service
// ID when it makes an HMAC key: the Writer role on the whole COS instance. It
// deletes only a policy that was not in before, the policies of the service ID
// before the key was made. This is the one thing the command deletes, and it
// is a policy the command's own call made.
func (r *run) removeImplicitPolicy(ctx context.Context, before map[string]bool) error {
	pols, err := r.subjectPolicies(ctx)
	if err != nil {
		return fmt.Errorf("the HMAC key is made and its secret is in %s; checking the policies of the service ID failed: %w", r.o.KeyFile, err)
	}
	for _, p := range pols {
		if before[p.ID] || !r.instanceWide(p) {
			continue
		}
		rep, err := r.call(ctx, http.MethodDelete, r.s.Endpoints.IAM+"/v1/policies/"+url.PathEscape(p.ID), nil, nil)
		if _, err = must("removing the instance-wide policy "+p.ID+" that IBM Cloud added for the HMAC key", rep, err); err != nil {
			return fmt.Errorf("the HMAC key is made and its secret is in %s; %w. Remove that policy yourself (ibmcloud iam service-policy-delete), then run this command again", r.o.KeyFile, err)
		}
		r.say("  removed       policy %s: IBM Cloud added it for the HMAC key (the Writer role on the whole COS instance); it was not made by you, and it is the only thing this command deletes\n", p.ID)
	}
	return nil
}

// instanceWide says p is a policy for the service ID on this COS instance as a
// whole: no resource type, no resource.
func (r *run) instanceWide(p policy) bool {
	if !p.anySubject(map[string]string{"iam_id": r.sidIAM}) {
		return false
	}
	if len(p.Resources) != 1 {
		return false
	}
	res := p.Resources[0]
	return res.get("serviceName") == COSServiceName && res.get("serviceInstance") == r.instGUID &&
		res.get("resourceType") == "" && res.get("resource") == ""
}

type keyReply struct {
	Name        string `json:"name"`
	Credentials struct {
		HMAC struct {
			AccessKey string `json:"access_key_id"`
			SecretKey string `json:"secret_access_key"`
		} `json:"cos_hmac_keys"`
	} `json:"credentials"`
}

func (r *run) setHMAC(k keyReply) {
	if k.Credentials.HMAC.AccessKey != "" && k.Credentials.HMAC.SecretKey != "" {
		r.hmacID, r.hmacSecret, r.hmacKnown = k.Credentials.HMAC.AccessKey, k.Credentials.HMAC.SecretKey, true
	}
}

// checkNarrow makes sure the service ID can reach only the one bucket (and
// read the root key). The policy IBM Cloud adds for the HMAC key has been
// removed by now. Any other wide policy was not made by this command, so it is
// reported for the person to remove.
func (r *run) checkNarrow(ctx context.Context) error {
	pols, err := r.subjectPolicies(ctx)
	if err != nil {
		return err
	}
	for _, p := range pols {
		for _, res := range p.Resources {
			bucketOnly := res.get("serviceName") == COSServiceName && res.get("resourceType") == "bucket"
			rootKeyOnly := res.get("serviceName") == r.kp.service && res.get("resourceType") == "key"
			if !bucketOnly && !rootKeyOnly {
				return fmt.Errorf("the service ID %s has policy %s that gives access to the whole %q service, not one bucket. This command deletes only the policy IBM Cloud adds for the HMAC key, and this one was there before: remove it yourself (ibmcloud iam service-policy-delete), then run it again", r.sidName(), p.ID, res.get("serviceName"))
			}
		}
	}
	return nil
}

func (r *run) writeKeyFile() error {
	r.keyWritten = true
	return writePrivate(r.o.KeyFile, secretManifest(r.o.CredentialsSecret, r.o.Namespace, map[string]string{
		"access_key_id": r.hmacID, "secret_access_key": r.hmacSecret}))
}

func (r *run) keyFile(_ context.Context) error {
	_, statErr := os.Stat(r.o.KeyFile)
	desc := fmt.Sprintf("write Secret %s (access_key_id, secret_access_key) to %s, mode 0600", r.o.CredentialsSecret, r.o.KeyFile)
	return r.step("COS key file "+r.o.KeyFile, statErr == nil || r.keyWritten, desc, func() error {
		return r.writeKeyFile()
	})
}

func (r *run) zone(ctx context.Context) error {
	if r.o.ZoneID != "" {
		return nil
	}
	if err := r.resolveZone(ctx); err != nil {
		return err
	}
	body := map[string]any{"name": r.zoneName(), "account_id": r.acct, "description": "Cluster VPC and admin addresses for backup bucket " + r.o.Bucket, "addresses": r.zoneAddrs}
	return r.step("CBR zone "+r.zoneName(), r.zoneExists, pretty(body), func() error {
		rep, err := r.call(ctx, http.MethodPost, r.s.Endpoints.CBR+"/v1/zones", body, nil)
		if _, err = must("making the CBR zone", rep, err); err != nil {
			return err
		}
		var z struct{ ID string }
		if err = rep.decode(&z); err != nil || z.ID == "" {
			return fmt.Errorf("the answer holds no zone id")
		}
		r.zoneID, r.zoneExists = z.ID, true
		return nil
	})
}

const dataPlane = "crn:v1:bluemix:public:context-based-restrictions::::api-type:data-plane"

func (r *run) rule(ctx context.Context) error {
	e := r.s.Endpoints.CBR
	exists := false
	if r.instKnown {
		q := url.Values{"account_id": {r.acct}, "service_name": {COSServiceName}}
		rep, err := r.call(ctx, http.MethodGet, e+"/v1/rules?"+q.Encode(), nil, nil)
		if _, err = must("listing CBR rules", rep, err); err != nil {
			return err
		}
		var rules struct {
			Rules []struct {
				Resources []attrSet `json:"resources"`
				Mode      string    `json:"enforcement_mode"`
			} `json:"rules"`
		}
		if err = rep.decode(&rules); err != nil {
			return err
		}
		for _, ru := range rules.Rules {
			for _, res := range ru.Resources {
				if res.has(map[string]string{"resourceType": "bucket", "resource": r.o.Bucket}) {
					exists = true
					if ru.Mode != r.o.CBRMode && !r.apply {
						r.say("  note          the rule for the bucket is in mode %q; it is left as it is\n", ru.Mode)
					}
				}
			}
		}
	}
	body := func() map[string]any {
		zone := r.zoneID
		if zone == "" {
			zone = "(the new zone)"
		}
		return map[string]any{
			"description": "Backup bucket " + r.o.Bucket + ": only the cluster VPC and the admin addresses",
			"contexts":    []attrSet{attrs("networkZoneId", zone)},
			"resources": []attrSet{attrs("accountId", r.acct, "serviceName", COSServiceName, "serviceInstance", r.instGUID,
				"resourceType", "bucket", "resource", r.o.Bucket)},
			"operations":       map[string]any{"api_types": []map[string]string{{"api_type_id": dataPlane}}},
			"enforcement_mode": r.o.CBRMode,
		}
	}
	return r.step("CBR rule on bucket "+r.o.Bucket+" (mode "+r.o.CBRMode+")", exists, pretty(body()), func() error {
		rep, err := r.call(ctx, http.MethodPost, e+"/v1/rules", body(), nil)
		_, err = must("making the CBR rule", rep, err)
		return err
	})
}

// ---- the backup key ----

type wrappedDoc struct {
	Version    int    `json:"version"`
	WrappedKey string `json:"wrappedKey"`
	RootKeyCRN string `json:"rootKeyCRN"`
}

func (r *run) backupKey(ctx context.Context) error {
	exists := false
	if r.bucketExists {
		rep, err := r.call(ctx, http.MethodGet, r.bucketURL()+"/"+WrappedKeyObject, nil, r.cosHeaders())
		if err != nil {
			return err
		}
		if rep.ok() {
			var d wrappedDoc
			if err = rep.decode(&d); err != nil || d.WrappedKey == "" {
				return fmt.Errorf("the wrapped key in the bucket (%s) is unreadable; this command will not replace it", WrappedKeyObject)
			}
			exists, r.wrapped = true, d.WrappedKey
		}
	}
	r.keyNeeded = !exists
	desc := "make a 32 byte backup key; have Key Protect wrap it with the root key; show the readable key to you once and ask you to confirm you saved a copy outside IBM Cloud;\nthen store only the wrapped copy in the bucket (" + WrappedKeyObject + ") and in " + r.o.SecretFile
	return r.step("backup key", exists, desc, func() error {
		key, err := newBackupKey(r.s.random())
		if err != nil {
			return err
		}
		wrapped, err := r.wrap(ctx, key)
		if err != nil {
			return err
		}
		r.say("\n\nYour backup key. It is shown once and never stored in readable form:\n\n    %s\n\n", key)
		r.say("Without it AND without Key Protect nobody can read your backups. Store a copy outside IBM Cloud now.\nType %q to continue: ", savedPhrase)
		line, _ := r.in.ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(line), savedPhrase) {
			return fmt.Errorf("the backup key was not confirmed as saved; nothing about it was stored. Run the command again to make a new key (nothing is encrypted with this one yet)")
		}
		doc, _ := json.Marshal(wrappedDoc{Version: 1, WrappedKey: wrapped, RootKeyCRN: r.o.RootKeyCRN})
		rep, err := r.call(ctx, http.MethodPut, r.bucketURL()+"/"+WrappedKeyObject, doc, r.cosHeaders())
		if _, err = must("storing the wrapped key in the bucket", rep, err); err != nil {
			return err
		}
		r.wrapped = wrapped
		return r.writeSecretFile()
	})
}

func (r *run) wrap(ctx context.Context, key string) (string, error) {
	u := r.s.Endpoints.KeyProtect(r.kp.region) + "/api/v2/keys/" + url.PathEscape(r.kp.id) + "?action=wrap"
	rep, err := r.call(ctx, http.MethodPost, u, map[string]string{"plaintext": base64.StdEncoding.EncodeToString([]byte(key))},
		map[string]string{"Bluemix-Instance": r.kp.instance, "Content-Type": "application/vnd.ibm.kms.key_action+json"})
	if _, err = must("wrapping the backup key with Key Protect", rep, err); err != nil {
		return "", err
	}
	var out struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err = rep.decode(&out); err != nil || out.Ciphertext == "" {
		return "", fmt.Errorf("the Key Protect answer holds no wrapped key")
	}
	return out.Ciphertext, nil
}

func (r *run) writeSecretFile() error {
	return writePrivate(r.o.SecretFile, secretManifest(r.o.BackupKeySecret, r.o.Namespace, map[string]string{
		"wrappedKey": r.wrapped, "rootKeyCRN": r.o.RootKeyCRN}))
}

func (r *run) secretFile(_ context.Context) error {
	if !r.apply && r.keyNeeded {
		// The backup key step above writes it.
		r.say("  would create  Secret file %s (the wrapped key only)\n", r.o.SecretFile)
		return nil
	}
	_, statErr := os.Stat(r.o.SecretFile)
	return r.step("Secret file "+r.o.SecretFile+" (the wrapped key only)", statErr == nil, "", func() error { return r.writeSecretFile() })
}

// secretManifest renders a Kubernetes Secret. Values are quoted, in sorted order.
func secretManifest(name, namespace string, data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Secret\nmetadata:\n  name: " + name + "\n  namespace: " + namespace + "\ntype: Opaque\nstringData:\n")
	for _, k := range keys {
		b.WriteString("  " + k + ": " + strconv.Quote(data[k]) + "\n")
	}
	return b.String()
}

// writePrivate makes a file that only its owner can read. It never replaces a file.
func writePrivate(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the path is the person's own --key-file or --secret-file
	if err != nil {
		return fmt.Errorf("writing %s (it is never replaced; move it away if you want a new one): %w", path, err)
	}
	if _, err = f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

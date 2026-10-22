// Package cosrestore holds the calls that `kubectl backup restore --from-cos`
// makes to the COS bucket with the caller's own IBM Cloud login: reading the
// wrapped backup key and removing delete markers. The driver's own COS key
// cannot remove a delete marker, and the driver never runs this code. See
// docs/specs/2026-10-21-backup-cos-export.md.
package cosrestore

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cossetup"
)

// Client talks to the S3 interface of COS with the caller's IAM token.
type Client struct {
	HTTP  cossetup.Doer
	Token cossetup.TokenSource

	allowAnyHost bool // tests only
}

// bucketURL returns the base URL of a bucket. The endpoint is a host name, with
// an optional https:// in front. The token is sent only to an https address on
// an IBM Cloud domain.
func (c *Client) bucketURL(endpoint, bucket string) (string, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", fmt.Errorf("the endpoint %q is not a host name", endpoint)
	}
	base := strings.TrimSuffix(endpoint, "/") + "/" + bucket
	if !c.allowAnyHost {
		if err = cossetup.CheckHost(base); err != nil {
			return "", err
		}
	}
	return base, nil
}

// do sends one request with the token. Errors never hold the token.
func (c *Client) do(ctx context.Context, method, rawURL string) (int, []byte, error) {
	token, err := c.Token.Token(ctx)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, redact(rawURL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, nil
}

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(url)"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func failure(what string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return fmt.Errorf("%s: HTTP %d: %s", what, status, msg)
}

// ReadWrappedKey reads the backup key, wrapped by Key Protect, that
// `kubectl backup cos-setup` stored in the bucket.
func (c *Client) ReadWrappedKey(ctx context.Context, endpoint, bucket string) (string, error) {
	base, err := c.bucketURL(endpoint, bucket)
	if err != nil {
		return "", err
	}
	status, body, err := c.do(ctx, http.MethodGet, base+"/"+cossetup.WrappedKeyObject)
	if err != nil {
		return "", err
	}
	if status == http.StatusNotFound {
		return "", fmt.Errorf("the bucket %s holds no wrapped backup key (%s is where `kubectl backup cos-setup` stores it). Give the Secret that holds it (--backup-key-secret), or the readable key (--backup-key-file)", bucket, cossetup.WrappedKeyObject)
	}
	if status < 200 || status > 299 {
		return "", failure("reading the wrapped backup key", status, body)
	}
	var doc struct {
		WrappedKey string `json:"wrappedKey"`
	}
	if err = json.Unmarshal(body, &doc); err != nil || doc.WrappedKey == "" {
		return "", errors.New("the wrapped backup key in the bucket cannot be read")
	}
	return doc.WrappedKey, nil
}

type versionsPage struct {
	IsTruncated         bool   `xml:"IsTruncated"`
	NextKeyMarker       string `xml:"NextKeyMarker"`
	NextVersionIDMarker string `xml:"NextVersionIdMarker"`
	DeleteMarkers       []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
	} `xml:"DeleteMarker"`
}

// RemoveDeleteMarkers removes every delete marker below prefix. A delete made
// with the driver's key only hides data (a delete marker is added and the
// locked versions stay), and Kopia sees a hidden blob as missing. Removing the
// marker shows the blob again. Only delete markers are removed, each by its
// version: a delete without a version would add a marker, and no real version
// is ever deleted. It returns how many it removed.
func (c *Client) RemoveDeleteMarkers(ctx context.Context, endpoint, bucket, prefix string) (int, error) {
	if prefix == "" {
		return 0, errors.New("a prefix is required: the area of the policy")
	}
	base, err := c.bucketURL(endpoint, bucket)
	if err != nil {
		return 0, err
	}
	removed := 0
	keyMarker, versionMarker := "", ""
	for {
		q := url.Values{"versions": {""}, "prefix": {prefix}}
		if keyMarker != "" {
			q.Set("key-marker", keyMarker)
		}
		if versionMarker != "" {
			q.Set("version-id-marker", versionMarker)
		}
		status, body, err := c.do(ctx, http.MethodGet, base+"?"+q.Encode())
		if err != nil {
			return removed, err
		}
		if status < 200 || status > 299 {
			return removed, failure("listing the versions in the bucket", status, body)
		}
		var page versionsPage
		if err = xml.Unmarshal(body, &page); err != nil {
			return removed, fmt.Errorf("the list of versions cannot be read: %w", err)
		}
		for _, m := range page.DeleteMarkers {
			if m.Key == "" || m.VersionID == "" || !strings.HasPrefix(m.Key, prefix) {
				continue
			}
			target := base + "/" + escapeKey(m.Key) + "?versionId=" + url.QueryEscape(m.VersionID)
			status, body, err = c.do(ctx, http.MethodDelete, target)
			if err != nil {
				return removed, err
			}
			if status < 200 || status > 299 {
				return removed, failure("removing a delete marker", status, body)
			}
			removed++
		}
		if !page.IsTruncated {
			return removed, nil
		}
		if page.NextKeyMarker == keyMarker && page.NextVersionIDMarker == versionMarker {
			return removed, errors.New("the list of versions does not advance")
		}
		keyMarker, versionMarker = page.NextKeyMarker, page.NextVersionIDMarker
	}
}

// escapeKey escapes an object key for a URL path, keeping its slashes.
func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

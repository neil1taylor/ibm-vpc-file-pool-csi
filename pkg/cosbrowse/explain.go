package cosbrowse

import "strings"

// explain turns what the Kopia program printed to stderr into a sentence for a
// person. The raw text is never passed on: it can name hosts and key IDs, and
// it is not written for a user. It holds no password (Kopia never prints one),
// but nothing here relies on that.
func explain(stderr string) string {
	s := strings.ToLower(stderr)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("invalid repository password", "unable to decrypt", "invalid password", "cipher: message authentication failed"):
		return "the backup key does not open the repository in COS: the key is wrong, or the repository was made with another key"
	case has("nosuchbucket", "specified bucket does not exist", "bucket does not exist"):
		return "the bucket does not exist at this COS endpoint: check the bucket name and the endpoint"
	case has("accessdenied", "access denied", "invalidaccesskeyid", "signaturedoesnotmatch", "status code: 403", "forbidden"):
		return "COS refused the key: check the COS key Secret, that it may read this bucket, and the context-based restriction (CBR) rules"
	case has("repository not initialized", "not initialized", "kopia.repository", "nosuchkey", "status code: 404"):
		return "there is no backup repository at this bucket and prefix: check the prefix and the policy name"
	case has("x509", "certificate"):
		return "the certificate of the COS endpoint could not be checked"
	case has("no such host", "dial tcp", "connection refused", "i/o timeout", "timeout", "network is unreachable", "tls handshake"):
		return "COS cannot be reached from the cluster: check the endpoint, the network path and the context-based restriction (CBR) rules"
	case has("context canceled"):
		return "the session was closed"
	}
	return "COS could not be read (the helper log has the details)"
}

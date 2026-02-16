package ibmcloud

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseRegionFromEndpoint extracts the IBM Cloud region from a RIAAS endpoint URL.
// Examples: "https://eu-de.private.iaas.cloud.ibm.com" → "eu-de"
//
//	"https://us-south.iaas.cloud.ibm.com/v1" → "us-south"
func ParseRegionFromEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	// Host is like "eu-de.private.iaas.cloud.ibm.com" or "us-south.iaas.cloud.ibm.com"
	host := u.Host
	idx := strings.IndexByte(host, '.')
	if idx <= 0 {
		return ""
	}
	region := host[:idx]
	// Sanity check: region should contain a hyphen (e.g. "us-south", "eu-de")
	if !strings.Contains(region, "-") {
		return ""
	}
	return region
}

// regionFromZone extracts the region from a VPC zone name.
// For example, "us-south-1" → "us-south", "eu-de-2" → "eu-de".
func regionFromZone(zone string) string {
	if zone == "" {
		return ""
	}
	idx := strings.LastIndex(zone, "-")
	if idx <= 0 {
		return zone
	}
	return zone[:idx]
}

// mapHTTPError maps VPC API HTTP status codes to sentinel errors.
func mapHTTPError(statusCode int, sdkErr error) error {
	switch {
	case statusCode == 404:
		return fmt.Errorf("%w: %v", ErrShareNotFound, sdkErr)
	case statusCode == 429:
		return fmt.Errorf("%w: %v", ErrAPIRateLimit, sdkErr)
	case statusCode == 401 || statusCode == 403:
		return fmt.Errorf("%w: %v", ErrAuthentication, sdkErr)
	case statusCode >= 500:
		return fmt.Errorf("VPC API server error (HTTP %d): %w", statusCode, sdkErr)
	default:
		return fmt.Errorf("VPC API error (HTTP %d): %w", statusCode, sdkErr)
	}
}

// parseMountPathServer extracts the server component from an NFS mount path.
// The mount path format is "server:/export_path" where server is either an IP
// address (security_group mode) or a FQDN (vpc mode).
// Returns empty string if the format is unrecognized.
func parseMountPathServer(mountPath string) string {
	idx := strings.Index(mountPath, ":/")
	if idx <= 0 {
		return ""
	}
	return mountPath[:idx]
}

// parseStartFromURL extracts the "start" query parameter from a pagination URL.
// Returns nil if the URL is invalid or the parameter is absent.
func parseStartFromURL(href string) *string {
	u, err := url.Parse(href)
	if err != nil {
		return nil
	}
	start := u.Query().Get("start")
	if start == "" {
		return nil
	}
	return &start
}

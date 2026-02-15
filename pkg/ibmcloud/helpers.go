package ibmcloud

import (
	"fmt"
	"net/url"
	"strings"
)

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

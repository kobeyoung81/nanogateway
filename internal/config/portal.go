package config

import (
	"os"
	"strings"
)

// DefaultPortalURL is where the portal listens inside the compose network.
// The agent rows and the conversation projection are the portal's; the gateway
// asks it directly for the two reads it needs (the user's agents and a session's
// metadata on resume).
const DefaultPortalURL = "http://nanoportal:80"

// PortalURL returns the portal's base URL (NANOSEC_PORTAL_URL).
func PortalURL() string {
	if v := strings.TrimSpace(os.Getenv("NANOSEC_PORTAL_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultPortalURL
}

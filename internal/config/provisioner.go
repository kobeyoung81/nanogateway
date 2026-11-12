package config

import (
	"os"
	"strings"
)

// DefaultProvisionerURL is where the provisioner listens inside the compose
// network. Hypervisor tunnels terminate there now, so the terminal bridge is
// dialed against it rather than the bus.
const DefaultProvisionerURL = "http://nanorunner:8080"

// ProvisionerURL returns the provisioner's base URL (NANOSEC_PROVISIONER_URL).
func ProvisionerURL() string {
	if v := strings.TrimSpace(os.Getenv("NANOSEC_PROVISIONER_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultProvisionerURL
}

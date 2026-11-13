package networkhealth

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"time"
)

// ValidInterface accepts conventional Linux device names, including VLANs.
func ValidInterface(value string) bool {
	if value == "" || len(value) > 15 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// CheckRoute verifies the kernel route before a source-bound transport is used.
// It observes routing and never rewrites global host routes.
func CheckRoute(ctx context.Context, path Path) error {
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(bounded, "ip", "-j", "route", "get", path.TargetAddress, "from", path.SourceAddress).Output()
	if err != nil {
		return err
	}
	return CheckRouteJSON(output, path.SourceInterface)
}

func CheckRouteJSON(output []byte, expectedInterface string) error {
	var routes []struct {
		Dev  string `json:"dev"`
		Type string `json:"type"`
	}
	if len(output) > 65536 || json.Unmarshal(output, &routes) != nil || len(routes) != 1 || routes[0].Dev != expectedInterface || routes[0].Type != "" && routes[0].Type != "unicast" && !(routes[0].Type == "local" && expectedInterface == "lo") {
		return errors.New("kernel route differs from the measured network path")
	}
	return nil
}

// Package version exposes build and protocol version information.
package version

import "runtime"

const (
	Name            = "XIRANG"
	ProtocolVersion = "v1"
)

// These values are replaced with -ldflags by release builds.
var (
	Version   = "1.1.0"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// Info is safe to expose from public version endpoints.
type Info struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	ProtocolVersion string `json:"protocolVersion"`
	Commit          string `json:"commit"`
	BuildTime       string `json:"buildTime"`
	GoVersion       string `json:"goVersion"`
	OS              string `json:"os"`
	Architecture    string `json:"architecture"`
}

// Current returns version metadata for the running binary.
func Current() Info {
	return Info{
		Name:            Name,
		Version:         Version,
		ProtocolVersion: ProtocolVersion,
		Commit:          Commit,
		BuildTime:       BuildTime,
		GoVersion:       runtime.Version(),
		OS:              runtime.GOOS,
		Architecture:    runtime.GOARCH,
	}
}

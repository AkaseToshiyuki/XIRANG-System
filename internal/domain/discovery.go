package domain

import "time"

type DiscoveryScanStatus string

const (
	DiscoveryScanRunning   DiscoveryScanStatus = "running"
	DiscoveryScanCompleted DiscoveryScanStatus = "completed"
	DiscoveryScanFailed    DiscoveryScanStatus = "failed"
)

type DiscoveryScan struct {
	ID             string              `json:"id"`
	CIDRs          []string            `json:"cidrs"`
	Ports          []int               `json:"ports"`
	Status         DiscoveryScanStatus `json:"status"`
	ScannedHosts   int                 `json:"scannedHosts"`
	CandidatesSeen int                 `json:"candidatesSeen"`
	Failure        string              `json:"failure,omitempty"`
	StartedAt      time.Time           `json:"startedAt"`
	FinishedAt     *time.Time          `json:"finishedAt,omitempty"`
}

type DiscoveryCandidateStatus string

const (
	DiscoveryCandidateDiscovered DiscoveryCandidateStatus = "discovered"
	DiscoveryCandidateApproved   DiscoveryCandidateStatus = "approved"
	DiscoveryCandidateRejected   DiscoveryCandidateStatus = "rejected"
)

type DiscoveryCandidate struct {
	ID              string                     `json:"id"`
	Address         string                     `json:"address"`
	AgentPort       int                        `json:"agentPort"`
	OpenPorts       []int                      `json:"openPorts"`
	Banners         map[int]string             `json:"banners,omitempty"`
	Protocol        string                     `json:"protocol,omitempty"`
	AgentVersion    string                     `json:"agentVersion,omitempty"`
	Hostname        string                     `json:"hostname,omitempty"`
	Architecture    string                     `json:"architecture,omitempty"`
	Distribution    string                     `json:"distribution,omitempty"`
	KernelVersion   string                     `json:"kernelVersion,omitempty"`
	CPUCount        int                        `json:"cpuCount,omitempty"`
	MemoryBytes     int64                      `json:"memoryBytes,omitempty"`
	Interfaces      []NetworkInterfaceSnapshot `json:"interfaces,omitempty"`
	Fingerprint     string                     `json:"fingerprint,omitempty"`
	PairingRequired bool                       `json:"pairingRequired,omitempty"`
	EnrolledNodeID  string                     `json:"enrolledNodeId,omitempty"`
	Status          DiscoveryCandidateStatus   `json:"status"`
	FirstSeenAt     time.Time                  `json:"firstSeenAt"`
	LastSeenAt      time.Time                  `json:"lastSeenAt"`
	LastScanID      string                     `json:"lastScanId"`
	ApprovedAt      *time.Time                 `json:"approvedAt,omitempty"`
	RejectedAt      *time.Time                 `json:"rejectedAt,omitempty"`
	BootstrapTTL    time.Duration              `json:"bootstrapTtl,omitempty"`
}

type DiscoveryBootstrap struct {
	Candidate       DiscoveryCandidate `json:"candidate"`
	EnrollmentToken string             `json:"enrollmentToken,omitempty"`
	ExpiresAt       time.Time          `json:"expiresAt"`
	ControllerURL   string             `json:"controllerUrl"`
	InstallCommand  string             `json:"installCommand,omitempty"`
	NodeID          string             `json:"nodeId,omitempty"`
}

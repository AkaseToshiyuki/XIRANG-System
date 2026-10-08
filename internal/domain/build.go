package domain

import "time"

type BuildStatus string

const (
	BuildStatusRunning   BuildStatus = "running"
	BuildStatusCanceling BuildStatus = "canceling"
	BuildStatusSucceeded BuildStatus = "succeeded"
	BuildStatusFailed    BuildStatus = "failed"
	BuildStatusCanceled  BuildStatus = "canceled"
)

func (status BuildStatus) Terminal() bool {
	return status == BuildStatusSucceeded || status == BuildStatusFailed || status == BuildStatusCanceled
}

type BuildRecipe struct {
	Name                string            `json:"name"`
	Application         string            `json:"application,omitempty"`
	Variant             string            `json:"variant,omitempty"`
	RuntimeNodeIDs      []string          `json:"runtimeNodeIds,omitempty"`
	RuntimeEntrypoint   string            `json:"runtimeEntrypoint,omitempty"`
	RuntimeRequirements map[string]string `json:"runtimeRequirements,omitempty"`
	SourcePath          string            `json:"sourcePath"`
	SourceSHA256        string            `json:"sourceSha256"`
	Architecture        string            `json:"architecture"`
	BuilderArchitecture string            `json:"builderArchitecture,omitempty"`
	WorkerID            string            `json:"workerId,omitempty"`
	CrossCompile        bool              `json:"crossCompile,omitempty"`
	SourceSpaceID       string            `json:"sourceSpaceId,omitempty"`
	OutputSpaceID       string            `json:"outputSpaceId,omitempty"`
	PublishPath         string            `json:"publishPath,omitempty"`
	Command             []string          `json:"command"`
	Steps               [][]string        `json:"steps,omitempty"`
	OutputPath          string            `json:"outputPath"`
	Toolchain           string            `json:"toolchain"`
	Environment         map[string]string `json:"environment,omitempty"`
	MilliCPU            int64             `json:"milliCpu"`
	MemoryBytes         int64             `json:"memoryBytes"`
	Timeout             time.Duration     `json:"timeout"`
}

type BuildArtifact struct {
	URI                 string            `json:"uri"`
	SHA256              string            `json:"sha256"`
	Signature           string            `json:"signature"`
	PublicKey           string            `json:"publicKey"`
	Architecture        string            `json:"architecture"`
	ABI                 string            `json:"abi"`
	Toolchain           string            `json:"toolchain"`
	SBOMURI             string            `json:"sbomUri"`
	Application         string            `json:"application,omitempty"`
	Variant             string            `json:"variant,omitempty"`
	Entrypoint          string            `json:"entrypoint,omitempty"`
	RuntimeNodeIDs      []string          `json:"runtimeNodeIds,omitempty"`
	RuntimeRequirements map[string]string `json:"runtimeRequirements,omitempty"`
	RoutingUpdatedAt    *time.Time        `json:"routingUpdatedAt,omitempty"`
	RoutingApproved     bool              `json:"routingApproved,omitempty"`
}

type Build struct {
	ID         string        `json:"id"`
	OwnerID    string        `json:"ownerId,omitempty"`
	OwnerName  string        `json:"ownerName,omitempty"`
	Status     BuildStatus   `json:"status"`
	Recipe     BuildRecipe   `json:"recipe"`
	WorkerID   string        `json:"workerId"`
	LogTail    string        `json:"logTail,omitempty"`
	Failure    string        `json:"failure,omitempty"`
	Artifact   BuildArtifact `json:"artifact,omitempty"`
	CreatedAt  time.Time     `json:"createdAt"`
	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt *time.Time    `json:"finishedAt,omitempty"`
}

type BuildWorker struct {
	ID                    string                `json:"id"`
	Architecture          string                `json:"architecture"`
	Toolchains            []string              `json:"toolchains"`
	ToolchainCapabilities []ToolchainCapability `json:"toolchainCapabilities,omitempty"`
	Isolation             string                `json:"isolation"`
	Ready                 bool                  `json:"ready"`
	SupportsCancellation  bool                  `json:"supportsCancellation,omitempty"`
}

type ToolchainCapability struct {
	Name                string   `json:"name"`
	Version             string   `json:"version,omitempty"`
	TargetArchitectures []string `json:"targetArchitectures"`
	Executables         []string `json:"executables,omitempty"`
}

type BuildRetentionPolicy struct {
	MaximumRecords           int           `json:"maximumRecords"`
	MaximumAge               time.Duration `json:"maximumAge"`
	PreserveSuccessfulBuilds bool          `json:"preserveSuccessfulBuilds,omitempty"`
}

type BuildCleanupResult struct {
	DeletedRecords   int   `json:"deletedRecords"`
	DeletedArtifacts int   `json:"deletedArtifacts"`
	ReclaimedBytes   int64 `json:"reclaimedBytes"`
}

type BuildSigningKey struct {
	PublicKey   string    `json:"publicKey"`
	Fingerprint string    `json:"fingerprint"`
	ActivatedAt time.Time `json:"activatedAt"`
}

package domain

import "time"

// QueuePolicy is a durable scheduling policy. Zero concurrency and quota values
// are unlimited; a missing policy uses weight 1 with backfill enabled.
type QueuePolicy struct {
	Name                    string         `json:"name"`
	Weight                  int32          `json:"weight"`
	MaxConcurrent           int32          `json:"maxConcurrent,omitempty"`
	MaxConcurrentPerProject int32          `json:"maxConcurrentPerProject,omitempty"`
	ResourceQuota           ResourceVector `json:"resourceQuota"`
	ProjectResourceQuota    ResourceVector `json:"projectResourceQuota"`
	Backfill                bool           `json:"backfill"`
	AllowPreemption         bool           `json:"allowPreemption"`
	Preemptible             bool           `json:"preemptible"`
	PreemptionGraceSeconds  int32          `json:"preemptionGraceSeconds,omitempty"`
	MaxPreemptionsPerJob    int32          `json:"maxPreemptionsPerJob,omitempty"`
	MaxRecommendedScaleOut  int32          `json:"maxRecommendedScaleOut,omitempty"`
	ElasticityBudget        ResourceVector `json:"elasticityBudget"`
	QoS                     string         `json:"qos,omitempty"`
	MaxWallTimeSeconds      int64          `json:"maxWallTimeSeconds,omitempty"`
	FairShareHalfLifeHours  int32          `json:"fairShareHalfLifeHours,omitempty"`
	HistoricalUsageWeight   int32          `json:"historicalUsageWeight,omitempty"`
	ReservationStart        *time.Time     `json:"reservationStart,omitempty"`
	ReservationEnd          *time.Time     `json:"reservationEnd,omitempty"`
	ReservationNodeIDs      []string       `json:"reservationNodeIds,omitempty"`
	ReservationGroupIDs     []string       `json:"reservationGroupIds,omitempty"`
	UpdatedAt               time.Time      `json:"updatedAt"`
}

// ElasticityRecommendation is advisory. XIRANG never creates or invents a
// node from this object; an operator or an external provisioner may act on it.
type ElasticityRecommendation struct {
	Queue                    string         `json:"queue"`
	QueuedJobs               int32          `json:"queuedJobs"`
	QueuedResources          ResourceVector `json:"queuedResources"`
	CapacityDeficit          ResourceVector `json:"capacityDeficit"`
	SuggestedAdditionalNodes int32          `json:"suggestedAdditionalNodes"`
	LimitedByBudget          bool           `json:"limitedByBudget"`
	Reason                   string         `json:"reason"`
}

type ElasticitySnapshot struct {
	GeneratedAt     time.Time                  `json:"generatedAt"`
	Recommendations []ElasticityRecommendation `json:"recommendations"`
}

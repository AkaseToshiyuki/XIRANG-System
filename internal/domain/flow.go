package domain

import "time"

type NetworkFlow struct {
	Family             string `json:"family"`
	Protocol           string `json:"protocol"`
	SourceAddress      string `json:"sourceAddress"`
	SourcePort         uint16 `json:"sourcePort"`
	DestinationAddress string `json:"destinationAddress"`
	DestinationPort    uint16 `json:"destinationPort"`
	Direction          string `json:"direction"`
	Packets            uint64 `json:"packets"`
	Bytes              uint64 `json:"bytes"`
}

type NodeFlowObservation struct {
	MessageID  string        `json:"messageId"`
	NodeID     string        `json:"nodeId"`
	Flows      []NetworkFlow `json:"flows"`
	Truncated  bool          `json:"truncated"`
	MapEntries uint32        `json:"mapEntries"`
	Interval   time.Duration `json:"interval"`
	ObservedAt time.Time     `json:"observedAt"`
	ReceivedAt time.Time     `json:"receivedAt"`
}

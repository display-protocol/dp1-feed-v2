package models

// ChannelRegistry is the GET response and PUT request body for /api/v1/registry/channels.
// Publishers are ordered; each has separate static and living channel URL lists.
type ChannelRegistry struct {
	Publishers []ChannelRegistryPublisher `json:"publishers" binding:"required,dive"`
}

// ChannelRegistryPublisher is one curated publisher with optional DID and channel URL lists.
// On PUT, include at least one of static or living (or both); omitted keys are treated as empty.
// At least one channel URL is required in total across static and living (validated in the handler).
type ChannelRegistryPublisher struct {
	Name   string   `json:"name" binding:"required,min=1,max=256"`
	DID    string   `json:"did,omitempty" binding:"omitempty,max=2048"`
	Static []string `json:"static"`
	Living []string `json:"living"`
}

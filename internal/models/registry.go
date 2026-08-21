package models

// ChannelRegistry is the GET response and PUT request body for /api/v1/registry/channels.
// Publishers are ordered; each carries a single ordered channel URL list.
type ChannelRegistry struct {
	Publishers []ChannelRegistryPublisher `json:"publishers" binding:"required,dive"`
}

// ChannelRegistryPublisher is one curated publisher with an optional DID and its channel URLs.
// The registry is a curation gate, not a catalog mirror: it stores URLs only, and the
// handler enforces that each URL points at a channel resource of this API.
type ChannelRegistryPublisher struct {
	Name        string   `json:"name" binding:"required,min=1,max=256"`
	DID         string   `json:"did,omitempty" binding:"omitempty,max=2048"`
	ChannelURLs []string `json:"channel_urls"`
}

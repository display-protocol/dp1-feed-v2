package models

import (
	"time"

	"github.com/google/uuid"
)

// RegistryPublisher represents a curated channel publisher with ordered channel URLs.
type RegistryPublisher struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Position  int       `json:"-"` // Internal: maintains order
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RegistryPublisherChannel is a channel URL belonging to a publisher.
type RegistryPublisherChannel struct {
	ID          uuid.UUID `json:"id"`
	PublisherID uuid.UUID `json:"publisher_id"`
	ChannelURL  string    `json:"channel_url"`
	Position    int       `json:"-"` // Internal: maintains order within publisher
	CreatedAt   time.Time `json:"created_at"`
}

// RegistryItem is the API response shape: publisher name + array of channel URLs.
// Matches the TypeScript RegistryItem schema.
type RegistryItem struct {
	Name        string   `json:"name" binding:"required,min=1,max=256"`
	ChannelURLs []string `json:"channel_urls" binding:"required,min=1,max=10000,dive,url"`
}

// RegistryUpdateRequest is the PUT /api/v1/registry/channels body: array of RegistryItem.
// Matches TypeScript CuratedRegistry schema (array of RegistryItem).
type RegistryUpdateRequest []RegistryItem

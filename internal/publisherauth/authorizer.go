package publisherauth

import (
	"context"
	"strings"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Authorizer answers whether a publisher identity can mutate a given channel or playlist.
// Authentication is separate; callers pass the already-authenticated publisher key.
type Authorizer interface {
	CanManageChannel(ctx context.Context, channelRef, publisherKey string) (bool, error)
	CanManagePlaylist(ctx context.Context, playlistRef, publisherKey string) (bool, error)
}

type storeReader interface {
	GetChannel(ctx context.Context, idOrSlug string) (*store.ChannelRecord, error)
	ListChannelsForPlaylist(ctx context.Context, idOrSlug string) ([]store.ChannelRecord, error)
}

type storeAuthorizer struct {
	store storeReader
}

func New(st storeReader) Authorizer {
	return &storeAuthorizer{store: st}
}

func (a *storeAuthorizer) CanManageChannel(ctx context.Context, channelRef, publisherKey string) (bool, error) {
	ch, err := a.store.GetChannel(ctx, channelRef)
	if err != nil {
		return false, err
	}
	return publisherOwnsChannel(ch, publisherKey), nil
}

func (a *storeAuthorizer) CanManagePlaylist(ctx context.Context, playlistRef, publisherKey string) (bool, error) {
	channels, err := a.store.ListChannelsForPlaylist(ctx, playlistRef)
	if err != nil {
		return false, err
	}
	if len(channels) == 0 {
		return false, nil
	}
	for _, ch := range channels {
		if !publisherOwnsChannel(&ch, publisherKey) {
			return false, nil
		}
	}
	return true, nil
}

func publisherOwnsChannel(ch *store.ChannelRecord, publisherKey string) bool {
	if ch == nil || ch.Body.Publisher == nil {
		return false
	}
	return strings.TrimSpace(ch.Body.Publisher.Key) != "" &&
		strings.TrimSpace(ch.Body.Publisher.Key) == strings.TrimSpace(publisherKey)
}

package httpserver

import (
	"encoding/json"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Record builders for executor mocks: handlers serve store records' Raw bytes verbatim, so a mocked
// executor must return a record whose Raw carries the document the test asserts on. These panic on a
// marshal failure because they run inside mock setup closures that have no *testing.T.

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func playlistRec(p playlist.Playlist) store.PlaylistRecord {
	return store.PlaylistRecord{Raw: mustJSON(p), Body: p}
}

func playlistRecPtr(p playlist.Playlist) *store.PlaylistRecord {
	r := playlistRec(p)
	return &r
}

func groupRec(g playlistgroup.Group) store.PlaylistGroupRecord {
	return store.PlaylistGroupRecord{Raw: mustJSON(g), Body: g}
}

func groupRecPtr(g playlistgroup.Group) *store.PlaylistGroupRecord {
	r := groupRec(g)
	return &r
}

func channelRec(ch channels.Channel) store.ChannelRecord {
	return store.ChannelRecord{Raw: mustJSON(ch), Body: ch}
}

func channelRecPtr(ch channels.Channel) *store.ChannelRecord {
	r := channelRec(ch)
	return &r
}

package executor_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/notification"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// A playlist write is observable at every channel that lists the playlist, so it must emit
// channel.updated for each of them (issue #25). These tests pin that contract on the executor: the store
// reports the listing channels, the executor turns them into events, and the same deadline / detachment
// rules that govern channel mutations govern the playlist write.

var memberTestPlaylistID = uuid.MustParse("11111111-1111-1111-1111-111111111111")

// expectPlaylistReplace wires the read/verify/sign/validate path of ReplacePlaylist so a test only has to
// decide what the store's UpdatePlaylist does.
func expectPlaylistReplace(t *testing.T, mockStore *mocks.MockStore, mockDP1 *mocks.MockValidatorSigner) (*models.PlaylistReplaceRequest, *models.SignedIntent) {
	t.Helper()
	expectIntentOK(mockDP1)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	existing := []byte(`{"dpVersion":"1.1.0","id":"11111111-1111-1111-1111-111111111111","slug":"test-playlist","title":"Old","created":"2026-01-01T00:00:00Z","curators":[{"key":"did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"}],"items":[{"source":"https://old"}]}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(&store.PlaylistRecord{
		ID:   memberTestPlaylistID,
		Slug: "test-playlist",
		Raw:  existing,
		Body: mustDecodePlaylist(t, existing),
	}, nil).AnyTimes()
	signed := []byte(`{"replaced":true}`)
	parsed := mustDecodePlaylist(t, signed)
	mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil).AnyTimes()
	mockDP1.EXPECT().ValidatePlaylistWithExtension(signed).Return(&parsed, nil).AnyTimes()
	mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil).AnyTimes()

	req := validCreateReq()
	req.Title = "New title"
	req.Items = []playlist.PlaylistItem{{ID: testItemID, Source: "https://cdn.example.com/day1.html"}}
	req.Raw = mustJSONRaw(req)
	return req, replaceIntent(models.IntentTargetPlaylist, memberTestPlaylistID.String(), "test-playlist", testCuratorKid)
}

// expectPlaylistDelete wires the read/verify path of DeletePlaylist.
func expectPlaylistDelete(mockStore *mocks.MockStore, mockDP1 *mocks.MockValidatorSigner) *models.SignedDeleteRequest {
	body := playlist.Playlist{ID: memberTestPlaylistID.String(), Slug: "id-1", Curators: []identity.Entity{{Key: testCuratorKid}}}
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(&store.PlaylistRecord{ID: memberTestPlaylistID, Slug: "id-1", Body: body}, nil).AnyTimes()
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	return deleteReq(models.IntentTargetPlaylist, memberTestPlaylistID.String(), "id-1", testCuratorKid)
}

func channelURLs(ids ...uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, testPublicBase+"/api/v1/channels/"+id.String())
	}
	sort.Strings(out)
	return out
}

func eventURLs(t *testing.T, events []notification.Event, wantType notification.EventType) []string {
	t.Helper()
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if ev.Type != wantType {
			t.Fatalf("event type = %q, want %q", ev.Type, wantType)
		}
		out = append(out, ev.Channel.URL)
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var listingChannels = []uuid.UUID{
	uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001"),
	uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000002"),
	uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000003"),
}

func TestReplacePlaylist_notifiesEveryListingChannel(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	req, intent := expectPlaylistReplace(t, mockStore, mockDP1)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any(), gomock.Any()).Return(listingChannels, nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	if _, err := e.ReplacePlaylist(notifiedMutationContext(t), "keep-me", req, intent); err != nil {
		t.Fatal(err)
	}
	if got, want := eventURLs(t, notifications.events, notification.ChannelUpdated), channelURLs(listingChannels...); !sameStrings(got, want) {
		t.Fatalf("notified channels = %v, want %v", got, want)
	}
}

func TestDeletePlaylist_notifiesEveryListingChannel(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	req := expectPlaylistDelete(mockStore, mockDP1)
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any()).Return(listingChannels, nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	if err := e.DeletePlaylist(notifiedMutationContext(t), "id-1", req); err != nil {
		t.Fatal(err)
	}
	// The channel survives its member; the event says the channel changed, not that it was deleted.
	if got, want := eventURLs(t, notifications.events, notification.ChannelUpdated), channelURLs(listingChannels...); !sameStrings(got, want) {
		t.Fatalf("notified channels = %v, want %v", got, want)
	}
}

func TestPlaylistWrites_noListingChannels_noNotification(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	req, intent := expectPlaylistReplace(t, mockStore, mockDP1)
	del := expectPlaylistDelete(mockStore, mockDP1)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any(), gomock.Any()).Return(nil, nil)
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any()).Return(nil, nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	if _, err := e.ReplacePlaylist(notifiedMutationContext(t), "keep-me", req, intent); err != nil {
		t.Fatal(err)
	}
	if err := e.DeletePlaylist(notifiedMutationContext(t), "id-1", del); err != nil {
		t.Fatal(err)
	}
	if len(notifications.events) != 0 {
		t.Fatalf("events = %#v, want none for a playlist no channel lists", notifications.events)
	}
}

func TestPlaylistWrites_storeError_noNotification(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	req, intent := expectPlaylistReplace(t, mockStore, mockDP1)
	del := expectPlaylistDelete(mockStore, mockDP1)
	// A store that returns ids alongside an error models a bug, not a contract; the executor must trust
	// the error and never notify for a write that did not commit.
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any(), gomock.Any()).Return(listingChannels, errors.New("db down"))
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any()).Return(listingChannels, store.ErrConcurrentModification)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	if _, err := e.ReplacePlaylist(notifiedMutationContext(t), "keep-me", req, intent); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("replace error = %v, want db down", err)
	}
	if err := e.DeletePlaylist(notifiedMutationContext(t), "id-1", del); !errors.Is(err, store.ErrConcurrentModification) {
		t.Fatalf("delete error = %v, want ErrConcurrentModification", err)
	}
	if len(notifications.events) != 0 {
		t.Fatalf("events = %#v, want none after failed writes", notifications.events)
	}
}

// With extensions enabled and a client configured, a playlist write is a notified mutation and is held to
// the same rule as a channel write: it does not begin without a request deadline. No UpdatePlaylist /
// DeletePlaylist expectation is registered, so gomock proves the write never started.
func TestPlaylistWrites_notifiedRequiresDeadline(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	req, intent := expectPlaylistReplace(t, mockStore, mockDP1)
	del := expectPlaylistDelete(mockStore, mockDP1)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, intent); err == nil || !strings.Contains(err.Error(), "requires a request deadline") {
		t.Fatalf("replace without deadline: %v", err)
	}
	if err := e.DeletePlaylist(context.Background(), "id-1", del); err == nil || !strings.Contains(err.Error(), "requires a request deadline") {
		t.Fatalf("delete without deadline: %v", err)
	}
	if len(notifications.events) != 0 {
		t.Fatalf("events = %#v, want none", notifications.events)
	}
}

// With extensions disabled channels are unreachable, so a playlist write is a plain write: no deadline
// requirement, no notification — even if the store still reports listing channels from rows left over
// from a configuration that once had extensions on.
func TestPlaylistWrites_extensionsDisabled_plainWrite(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	req, intent := expectPlaylistReplace(t, mockStore, mockDP1)
	del := expectPlaylistDelete(mockStore, mockDP1)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any(), gomock.Any()).Return(listingChannels[:1], nil)
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any()).Return(listingChannels[:1], nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase, executor.WithNotificationClient(notifications))
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, intent); err != nil {
		t.Fatal(err)
	}
	if err := e.DeletePlaylist(context.Background(), "id-1", del); err != nil {
		t.Fatal(err)
	}
	if len(notifications.events) != 0 {
		t.Fatalf("events = %#v, want none with extensions disabled", notifications.events)
	}
}

// Mirrors TestDeleteChannel_notificationSurvivesRequestCancellationAfterCommit for the playlist path: a
// disconnect right after the row commits must not suppress the member-change events, and delivery keeps
// the route deadline rather than running unbounded.
func TestReplacePlaylist_notificationSurvivesRequestCancellationAfterCommit(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	req, intent := expectPlaylistReplace(t, mockStore, mockDP1)

	deadlineCtx := notifiedMutationContext(t)
	wantDeadline, _ := deadlineCtx.Deadline()
	ctx, cancel := context.WithCancel(deadlineCtx)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(mutationCtx context.Context, _ string, _ []byte, _ time.Time) ([]uuid.UUID, error) {
			cancel() // The row committed just before the HTTP request context was canceled.
			if err := mutationCtx.Err(); err != nil {
				t.Fatalf("mutation context error after request cancellation = %v, want detached context", err)
			}
			return listingChannels[:1], nil
		})

	notifications := &contextRecordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	if _, err := e.ReplacePlaylist(ctx, "keep-me", req, intent); err != nil {
		t.Fatal(err)
	}
	if notifications.contextErr != nil {
		t.Fatalf("notification context error = %v, want detached post-commit context", notifications.contextErr)
	}
	if !notifications.hasDeadline || !notifications.deadline.Equal(wantDeadline) {
		t.Fatalf("notification deadline = %v, %t; want %v, true", notifications.deadline, notifications.hasDeadline, wantDeadline)
	}
	if len(notifications.events) != 1 || notifications.events[0].Type != notification.ChannelUpdated {
		t.Fatalf("notification events = %#v", notifications.events)
	}
}

// blockingNotificationClient holds every Notify until release is closed and records the peak number of
// concurrent calls, so a test can observe the fan-out bound from outside the executor.
type blockingNotificationClient struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	total    int
	arrived  chan struct{}
	release  chan struct{}
}

func (c *blockingNotificationClient) Notify(ctx context.Context, _ notification.Event) error {
	c.mu.Lock()
	c.inFlight++
	c.total++
	if c.inFlight > c.peak {
		c.peak = c.inFlight
	}
	c.mu.Unlock()
	c.arrived <- struct{}{}
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
	return nil
}

func TestReplacePlaylist_notificationFanOutIsBounded(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	req, intent := expectPlaylistReplace(t, mockStore, mockDP1)

	const channelCount = 20
	ids := make([]uuid.UUID, channelCount)
	for i := range ids {
		ids[i] = uuid.New()
	}
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), memberTestPlaylistID.String(), gomock.Any(), gomock.Any()).Return(ids, nil)

	client := &blockingNotificationClient{arrived: make(chan struct{}, channelCount), release: make(chan struct{})}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(client))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.ReplacePlaylist(ctx, "keep-me", req, intent)
		done <- err
	}()

	// Exactly the bound arrives while everything is blocked; the rest must wait for a slot.
	for range 8 {
		select {
		case <-client.arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("fewer than 8 concurrent deliveries started")
		}
	}
	select {
	case <-client.arrived:
		t.Fatal("a ninth delivery started while eight were blocked")
	case <-time.After(100 * time.Millisecond):
	}
	close(client.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.peak > 8 {
		t.Fatalf("peak concurrent deliveries = %d, want <= 8", client.peak)
	}
	if client.total != channelCount {
		t.Fatalf("deliveries = %d, want %d", client.total, channelCount)
	}
}

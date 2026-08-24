// Package notification defines the outbound client boundary for channel lifecycle events.
package notification

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// EventType identifies a channel lifecycle transition.
type EventType string

const (
	ChannelAdded   EventType = "channel.added"
	ChannelUpdated EventType = "channel.updated"
	ChannelDeleted EventType = "channel.deleted"
)

// ChannelRef identifies the canonical feed URL affected by an event.
type ChannelRef struct {
	URL string `json:"url"`
}

// Event is the transport-neutral channel notification payload.
type Event struct {
	Type    EventType  `json:"type"`
	Time    time.Time  `json:"time"`
	Channel ChannelRef `json:"channel"`
}

// Client receives channel lifecycle events after the feed mutation commits.
type Client interface {
	Notify(ctx context.Context, event Event) error
}

// NamedClient gives a configured client an operator-facing name without changing its transport contract.
type NamedClient struct {
	Name   string
	Client Client
}

// Dispatcher sends to every configured client and logs failures without failing the committed mutation.
// This is intentionally best-effort: durable retry would require an outbox and background delivery owner.
type Dispatcher struct {
	logger  *zap.Logger
	timeout time.Duration
	clients []NamedClient
}

// NewDispatcher constructs a best-effort multi-client dispatcher.
func NewDispatcher(logger *zap.Logger, timeout time.Duration, clients []NamedClient) *Dispatcher {
	return &Dispatcher{logger: logger, timeout: timeout, clients: clients}
}

// Notify fans out concurrently under one aggregate deadline and attempts every client even when another fails.
// The client list is operator configuration, so concurrency is bounded by the configured list size.
func (d *Dispatcher) Notify(ctx context.Context, event Event) error {
	dispatchCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, client := range d.clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.Client.Notify(dispatchCtx, event); err != nil {
				d.logger.Error("channel notification failed",
					zap.String("client", client.Name),
					zap.String("event_type", string(event.Type)),
					zap.String("channel_url", event.Channel.URL),
					zap.Error(err),
				)
			}
		}()
	}
	wg.Wait()
	return nil
}

// WebhookClient delivers events using the signed webhook contract shared with event consumers.
type WebhookClient struct {
	endpoint   string
	privateKey *ecdsa.PrivateKey
	publicKey  string
	httpClient *http.Client
	now        func() time.Time
	newEventID func() string
}

// ParseP256PrivateKeyHex parses one fixed-width P-256 private scalar.
func ParseP256PrivateKeyHex(value string) (*ecdsa.PrivateKey, error) {
	encoded, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(encoded) != 32 {
		return nil, fmt.Errorf("P-256 private key must be exactly 32 bytes encoded as 64 hexadecimal characters")
	}
	privateKey, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), encoded)
	if err != nil {
		return nil, fmt.Errorf("P-256 private key scalar is outside the valid range")
	}
	return privateKey, nil
}

// P256PublicKeyString returns the public key format used by Webhook-Public-Key.
func P256PublicKeyString(publicKey *ecdsa.PublicKey) (string, error) {
	if publicKey == nil || publicKey.Curve == nil || publicKey.Curve.Params().Name != elliptic.P256().Params().Name {
		return "", fmt.Errorf("webhook public key must be a valid P-256 point")
	}
	encoded, err := publicKey.Bytes()
	if err != nil {
		return "", fmt.Errorf("webhook public key must be a valid P-256 point: %w", err)
	}
	return "p256:" + base64.RawURLEncoding.EncodeToString(encoded), nil
}

// NewWebhookClient validates configuration and constructs a P-256-authenticated webhook client.
func NewWebhookClient(endpoint string, privateKey *ecdsa.PrivateKey, httpClient *http.Client) (*WebhookClient, error) {
	endpoint, err := ValidateWebhookEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if privateKey == nil {
		return nil, fmt.Errorf("notification private key is required")
	}
	publicKey, err := P256PublicKeyString(&privateKey.PublicKey)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &WebhookClient{
		endpoint:   endpoint,
		privateKey: privateKey,
		publicKey:  publicKey,
		httpClient: httpClient,
		now:        time.Now,
		newEventID: func() string { return "evt_" + uuid.NewString() },
	}, nil
}

// ValidateWebhookEndpoint rejects URL schemes that net/http cannot deliver.
// Fragments are also rejected because HTTP clients never send them to servers.
func ValidateWebhookEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) {
		return "", fmt.Errorf("notification endpoint must be an absolute HTTP(S) URL")
	}
	if parsed.Fragment != "" {
		return "", fmt.Errorf("notification endpoint must not contain a fragment")
	}
	return endpoint, nil
}

// Notify signs the exact JSON request bytes and accepts any 2xx response.
func (c *WebhookClient) Notify(ctx context.Context, event Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode channel event: %w", err)
	}
	eventID := c.newEventID()
	timestamp := strconv.FormatInt(c.now().Unix(), 10)

	signed := append([]byte(eventID+"."+timestamp+"."), body...)
	digest := sha256.Sum256(signed)
	r, s, err := ecdsa.Sign(rand.Reader, c.privateKey, digest[:])
	if err != nil {
		return fmt.Errorf("sign channel event: %w", err)
	}
	signatureBytes := make([]byte, 64)
	r.FillBytes(signatureBytes[:32])
	s.FillBytes(signatureBytes[32:])
	signature := base64.RawURLEncoding.EncodeToString(signatureBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create notification request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Webhook-Id", eventID)
	req.Header.Set("Webhook-Timestamp", timestamp)
	req.Header.Set("Webhook-Public-Key", c.publicKey)
	req.Header.Set("Webhook-Signature", "p256="+signature)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil
	}

	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return fmt.Errorf("notification endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
}

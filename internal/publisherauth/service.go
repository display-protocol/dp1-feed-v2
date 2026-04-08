package publisherauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/display-protocol/dp1-go/sign"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/go-webauthn/webauthn/protocol"
	walib "github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
)

const (
	ProofTypeWalletAddress = "wallet_address"
	ProofTypeENSName       = "ens_name"

	ceremonyTypeRegister = "register_passkey"
	ceremonyTypeLogin    = "login_passkey"
	ceremonyTypeWallet   = "link_wallet"
)

type Principal struct {
	AccountID    uuid.UUID
	DisplayName  string
	PublisherKey string
	ProofCount   int
}

type Proof struct {
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	Value      string          `json:"value"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	VerifiedAt time.Time       `json:"verifiedAt"`
}

type Account struct {
	ID           uuid.UUID `json:"id"`
	DisplayName  string    `json:"displayName"`
	PublisherKey string    `json:"publisherKey"`
	Proofs       []Proof   `json:"proofs"`
}

type Service interface {
	LookupSession(ctx context.Context, token string) (*Principal, error)
	DeleteSession(ctx context.Context, token string) error
	BootstrapLocalSession(ctx context.Context, displayName string) (*Principal, string, error)
	BeginRegistration(ctx context.Context, displayName string) (*protocol.CredentialCreation, string, error)
	FinishRegistration(ctx context.Context, token string, raw []byte) (*Principal, string, error)
	BeginLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error)
	FinishLogin(ctx context.Context, token string, raw []byte) (*Principal, string, error)
	GetAccount(ctx context.Context, accountID uuid.UUID) (*Account, error)
	BeginWalletProof(ctx context.Context, accountID uuid.UUID, address string) (string, string, error)
	FinishWalletProof(ctx context.Context, accountID uuid.UUID, token, signature string) (*Proof, error)
	VerifyENSProof(ctx context.Context, accountID uuid.UUID, name string) (*Proof, error)
}

type service struct {
	pool                 *pgxpool.Pool
	webauthn             *walib.WebAuthn
	sessionTTL           time.Duration
	ceremonyTTL          time.Duration
	domainResolverURL    string
	domainResolverAPIKey string
	ensClient            *ethclient.Client
}

type webauthnUser struct {
	id          uuid.UUID
	displayName string
	credentials []walib.Credential
}

type registrationState struct {
	AccountID uuid.UUID         `json:"accountId"`
	Session   walib.SessionData `json:"session"`
}

type loginState struct {
	Session walib.SessionData `json:"session"`
}

type walletState struct {
	AccountID uuid.UUID `json:"accountId"`
	Address   string    `json:"address"`
	Message   string    `json:"message"`
}

type resolverLookupResponse struct {
	Data struct {
		Lookup []struct {
			Address string `json:"address"`
			Error   string `json:"error"`
		} `json:"lookup"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func NewService(pool *pgxpool.Pool, cfg config.PublisherAuthConfig) (Service, error) {
	wa, err := walib.New(&walib.Config{
		RPID:          cfg.RPID,
		RPDisplayName: cfg.RPDisplayName,
		RPOrigins:     cfg.RPOrigins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, err
	}
	return &service{
		pool:                 pool,
		webauthn:             wa,
		sessionTTL:           cfg.SessionTTL,
		ceremonyTTL:          cfg.CeremonyTTL,
		domainResolverURL:    strings.TrimSpace(cfg.DomainResolverURL),
		domainResolverAPIKey: strings.TrimSpace(cfg.DomainResolverAPIKey),
		ensClient:            mustDialENS(cfg.ENSRPCURL),
	}, nil
}

func (u webauthnUser) WebAuthnID() []byte {
	raw := u.id
	return raw[:]
}

func (u webauthnUser) WebAuthnName() string {
	return u.displayName
}

func (u webauthnUser) WebAuthnDisplayName() string {
	return u.displayName
}

func (u webauthnUser) WebAuthnCredentials() []walib.Credential {
	return u.credentials
}

func (s *service) BeginRegistration(ctx context.Context, displayName string) (*protocol.CredentialCreation, string, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return nil, "", fmt.Errorf("display name is required")
	}

	accountID := uuid.New()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate identity key: %w", err)
	}
	identityKey, err := sign.Ed25519DIDKey(priv.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, "", fmt.Errorf("derive identity key: %w", err)
	}

	user := webauthnUser{id: accountID, displayName: displayName}
	options, session, err := s.webauthn.BeginRegistration(user)
	if err != nil {
		return nil, "", err
	}

	state, err := json.Marshal(registrationState{AccountID: accountID, Session: *session})
	if err != nil {
		return nil, "", err
	}
	token, tokenHash, err := randomToken()
	if err != nil {
		return nil, "", err
	}

	_, err = s.pool.Exec(ctx, `
INSERT INTO publisher_accounts (id, display_name, identity_key, identity_private_key_hex)
VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO NOTHING`, accountID, displayName, identityKey, hex.EncodeToString(priv))
	if err != nil {
		return nil, "", fmt.Errorf("insert publisher account: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO publisher_ceremonies (id, ceremony_type, token_hash, publisher_account_id, state, expires_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6)`,
		uuid.New(), ceremonyTypeRegister, tokenHash, accountID, state, time.Now().Add(s.ceremonyTTL))
	if err != nil {
		return nil, "", fmt.Errorf("store registration ceremony: %w", err)
	}
	return options, token, nil
}

func (s *service) BootstrapLocalSession(ctx context.Context, displayName string) (*Principal, string, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return nil, "", fmt.Errorf("display name is required")
	}

	row := s.pool.QueryRow(ctx, `
SELECT id
FROM publisher_accounts
WHERE lower(display_name) = lower($1)
ORDER BY created_at ASC
LIMIT 1`, displayName)

	var accountID uuid.UUID
	if err := row.Scan(&accountID); err == nil {
		return s.createSession(ctx, accountID)
	}

	accountID = uuid.New()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate identity key: %w", err)
	}
	identityKey, err := sign.Ed25519DIDKey(priv.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, "", fmt.Errorf("derive identity key: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO publisher_accounts (id, display_name, identity_key, identity_private_key_hex)
VALUES ($1, $2, $3, $4)`,
		accountID, displayName, identityKey, hex.EncodeToString(priv))
	if err != nil {
		return nil, "", fmt.Errorf("insert publisher account: %w", err)
	}
	return s.createSession(ctx, accountID)
}

func (s *service) FinishRegistration(ctx context.Context, token string, raw []byte) (*Principal, string, error) {
	state, err := s.loadCeremony(ctx, ceremonyTypeRegister, token)
	if err != nil {
		return nil, "", err
	}
	var reg registrationState
	if err := json.Unmarshal(state, &reg); err != nil {
		return nil, "", err
	}
	account, creds, err := s.loadWebAuthnUser(ctx, reg.AccountID)
	if err != nil {
		return nil, "", err
	}
	account.credentials = creds
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		return nil, "", err
	}
	credential, err := s.webauthn.CreateCredential(account, reg.Session, parsed)
	if err != nil {
		return nil, "", err
	}
	if err := s.storeCredential(ctx, reg.AccountID, "primary", credential); err != nil {
		return nil, "", err
	}
	if err := s.deleteCeremony(ctx, token); err != nil {
		return nil, "", err
	}
	principal, sessionToken, err := s.createSession(ctx, reg.AccountID)
	if err != nil {
		return nil, "", err
	}
	return principal, sessionToken, nil
}

func (s *service) BeginLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error) {
	options, session, err := s.webauthn.BeginDiscoverableLogin()
	if err != nil {
		return nil, "", err
	}
	state, err := json.Marshal(loginState{Session: *session})
	if err != nil {
		return nil, "", err
	}
	token, tokenHash, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO publisher_ceremonies (id, ceremony_type, token_hash, state, expires_at)
VALUES ($1, $2, $3, $4::jsonb, $5)`,
		uuid.New(), ceremonyTypeLogin, tokenHash, state, time.Now().Add(s.ceremonyTTL))
	if err != nil {
		return nil, "", err
	}
	return options, token, nil
}

func (s *service) FinishLogin(ctx context.Context, token string, raw []byte) (*Principal, string, error) {
	state, err := s.loadCeremony(ctx, ceremonyTypeLogin, token)
	if err != nil {
		return nil, "", err
	}
	var login loginState
	if err := json.Unmarshal(state, &login); err != nil {
		return nil, "", err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return nil, "", err
	}
	accountUser, _, err := s.webauthn.ValidatePasskeyLogin(func(_, userHandle []byte) (walib.User, error) {
		return s.loadUserByHandle(ctx, userHandle)
	}, login.Session, parsed)
	if err != nil {
		return nil, "", err
	}
	user, ok := accountUser.(webauthnUser)
	if !ok {
		return nil, "", fmt.Errorf("unexpected passkey user type")
	}
	if err := s.deleteCeremony(ctx, token); err != nil {
		return nil, "", err
	}
	principal, sessionToken, err := s.createSession(ctx, user.id)
	if err != nil {
		return nil, "", err
	}
	return principal, sessionToken, nil
}

func (s *service) BeginWalletProof(ctx context.Context, accountID uuid.UUID, address string) (string, string, error) {
	addr, err := normalizeWalletAddress(address)
	if err != nil {
		return "", "", err
	}
	nonce, _, err := randomToken()
	if err != nil {
		return "", "", err
	}
	message := fmt.Sprintf("Feral File publisher proof\nAccount: %s\nAddress: %s\nNonce: %s", accountID.String(), addr, nonce)
	state, err := json.Marshal(walletState{AccountID: accountID, Address: addr, Message: message})
	if err != nil {
		return "", "", err
	}
	token, tokenHash, err := randomToken()
	if err != nil {
		return "", "", err
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO publisher_ceremonies (id, ceremony_type, token_hash, publisher_account_id, state, expires_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6)`,
		uuid.New(), ceremonyTypeWallet, tokenHash, accountID, state, time.Now().Add(s.ceremonyTTL))
	if err != nil {
		return "", "", err
	}
	return message, token, nil
}

func (s *service) FinishWalletProof(ctx context.Context, accountID uuid.UUID, token, signature string) (*Proof, error) {
	state, err := s.loadCeremony(ctx, ceremonyTypeWallet, token)
	if err != nil {
		return nil, err
	}
	var wallet walletState
	if err := json.Unmarshal(state, &wallet); err != nil {
		return nil, err
	}
	if wallet.AccountID != accountID {
		return nil, fmt.Errorf("wallet proof account mismatch")
	}
	addr, err := recoverWalletAddress(wallet.Message, signature)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(addr, wallet.Address) {
		return nil, fmt.Errorf("wallet proof signature does not match requested address")
	}
	proof := &Proof{
		ID:         uuid.New(),
		Type:       ProofTypeWalletAddress,
		Value:      strings.ToLower(addr),
		Metadata:   json.RawMessage(`{"scheme":"eip191_personal_sign"}`),
		VerifiedAt: time.Now().UTC(),
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO publisher_proofs (id, publisher_account_id, proof_type, proof_value, metadata, verified_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6)
ON CONFLICT (proof_type, proof_value) DO UPDATE
SET publisher_account_id = EXCLUDED.publisher_account_id,
    metadata = EXCLUDED.metadata,
    verified_at = EXCLUDED.verified_at`,
		proof.ID, accountID, proof.Type, proof.Value, proof.Metadata, proof.VerifiedAt)
	if err != nil {
		return nil, err
	}
	if err := s.deleteCeremony(ctx, token); err != nil {
		return nil, err
	}
	return proof, nil
}

func (s *service) VerifyENSProof(ctx context.Context, accountID uuid.UUID, name string) (*Proof, error) {
	normalized, err := normalizeENSName(name)
	if err != nil {
		return nil, err
	}
	resolvedAddr, err := s.resolveENSAddress(ctx, normalized)
	if err != nil {
		return nil, err
	}
	owned, err := s.accountHasWalletProof(ctx, accountID, resolvedAddr)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, fmt.Errorf("ENS name does not resolve to one of this account's verified wallet proofs")
	}
	proof := &Proof{
		ID:         uuid.New(),
		Type:       ProofTypeENSName,
		Value:      normalized,
		Metadata:   json.RawMessage(fmt.Sprintf(`{"resolvedAddress":"%s"}`, strings.ToLower(resolvedAddr))),
		VerifiedAt: time.Now().UTC(),
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO publisher_proofs (id, publisher_account_id, proof_type, proof_value, metadata, verified_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6)
ON CONFLICT (proof_type, proof_value) DO UPDATE
SET publisher_account_id = EXCLUDED.publisher_account_id,
    metadata = EXCLUDED.metadata,
    verified_at = EXCLUDED.verified_at`,
		proof.ID, accountID, proof.Type, proof.Value, proof.Metadata, proof.VerifiedAt)
	if err != nil {
		return nil, err
	}
	return proof, nil
}

func (s *service) LookupSession(ctx context.Context, token string) (*Principal, error) {
	tokenHash := hashToken(token)
	row := s.pool.QueryRow(ctx, `
SELECT a.id, a.display_name, a.identity_key, COUNT(p.id)
FROM publisher_sessions s
JOIN publisher_accounts a ON a.id = s.publisher_account_id
LEFT JOIN publisher_proofs p ON p.publisher_account_id = a.id
WHERE s.token_hash = $1 AND s.expires_at > now()
GROUP BY a.id, a.display_name, a.identity_key`, tokenHash)
	var principal Principal
	if err := row.Scan(&principal.AccountID, &principal.DisplayName, &principal.PublisherKey, &principal.ProofCount); err != nil {
		return nil, err
	}
	_, _ = s.pool.Exec(ctx, `UPDATE publisher_sessions SET last_used_at = now() WHERE token_hash = $1`, tokenHash)
	return &principal, nil
}

func (s *service) DeleteSession(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM publisher_sessions WHERE token_hash = $1`, hashToken(token))
	return err
}

func (s *service) GetAccount(ctx context.Context, accountID uuid.UUID) (*Account, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, display_name, identity_key FROM publisher_accounts WHERE id = $1`, accountID)
	account := Account{Proofs: make([]Proof, 0)}
	if err := row.Scan(&account.ID, &account.DisplayName, &account.PublisherKey); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, proof_type, proof_value, metadata, verified_at
FROM publisher_proofs
WHERE publisher_account_id = $1
ORDER BY verified_at ASC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var proof Proof
		if err := rows.Scan(&proof.ID, &proof.Type, &proof.Value, &proof.Metadata, &proof.VerifiedAt); err != nil {
			return nil, err
		}
		account.Proofs = append(account.Proofs, proof)
	}
	return &account, rows.Err()
}

func (s *service) accountHasWalletProof(ctx context.Context, accountID uuid.UUID, address string) (bool, error) {
	row := s.pool.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM publisher_proofs
  WHERE publisher_account_id = $1 AND proof_type = $2 AND lower(proof_value) = lower($3)
)`, accountID, ProofTypeWalletAddress, address)
	var ok bool
	if err := row.Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

func (s *service) createSession(ctx context.Context, accountID uuid.UUID) (*Principal, string, error) {
	token, tokenHash, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO publisher_sessions (id, publisher_account_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4)`, uuid.New(), accountID, tokenHash, time.Now().Add(s.sessionTTL))
	if err != nil {
		return nil, "", err
	}
	principal, err := s.LookupSession(ctx, token)
	if err != nil {
		return nil, "", err
	}
	return principal, token, nil
}

func (s *service) loadCeremony(ctx context.Context, ceremonyType, token string) ([]byte, error) {
	row := s.pool.QueryRow(ctx, `
SELECT state
FROM publisher_ceremonies
WHERE ceremony_type = $1 AND token_hash = $2 AND expires_at > now()`,
		ceremonyType, hashToken(token))
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *service) deleteCeremony(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM publisher_ceremonies WHERE token_hash = $1`, hashToken(token))
	return err
}

func (s *service) storeCredential(ctx context.Context, accountID uuid.UUID, label string, credential *walib.Credential) error {
	if credential == nil {
		return fmt.Errorf("nil credential")
	}
	body, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO publisher_credentials (id, publisher_account_id, credential_id, label, body)
VALUES ($1, $2, $3, $4, $5::jsonb)`,
		uuid.New(), accountID, base64.RawURLEncoding.EncodeToString(credential.ID), label, body)
	return err
}

func (s *service) loadWebAuthnUser(ctx context.Context, accountID uuid.UUID) (webauthnUser, []walib.Credential, error) {
	row := s.pool.QueryRow(ctx, `SELECT display_name FROM publisher_accounts WHERE id = $1`, accountID)
	var displayName string
	if err := row.Scan(&displayName); err != nil {
		return webauthnUser{}, nil, err
	}
	credentials, err := s.loadCredentials(ctx, accountID)
	if err != nil {
		return webauthnUser{}, nil, err
	}
	return webauthnUser{id: accountID, displayName: displayName, credentials: credentials}, credentials, nil
}

func (s *service) loadUserByHandle(ctx context.Context, handle []byte) (webauthnUser, error) {
	if len(handle) != 16 {
		return webauthnUser{}, fmt.Errorf("invalid user handle")
	}
	accountID, err := uuid.FromBytes(handle)
	if err != nil {
		return webauthnUser{}, err
	}
	user, _, err := s.loadWebAuthnUser(ctx, accountID)
	return user, err
}

func (s *service) loadCredentials(ctx context.Context, accountID uuid.UUID) ([]walib.Credential, error) {
	rows, err := s.pool.Query(ctx, `SELECT body FROM publisher_credentials WHERE publisher_account_id = $1 ORDER BY created_at ASC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []walib.Credential{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var credential walib.Credential
		if err := json.Unmarshal(raw, &credential); err != nil {
			return nil, err
		}
		out = append(out, credential)
	}
	return out, rows.Err()
}

func randomToken() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	return token, hashToken(token), nil
}

func mustDialENS(rawURL string) *ethclient.Client {
	url := strings.TrimSpace(rawURL)
	if url == "" {
		return nil
	}
	client, err := ethclient.Dial(url)
	if err != nil {
		return nil
	}
	return client
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normalizeWalletAddress(address string) (string, error) {
	addr := strings.TrimSpace(address)
	if !common.IsHexAddress(addr) {
		return "", fmt.Errorf("wallet address must be a valid hex address")
	}
	return common.HexToAddress(addr).Hex(), nil
}

func normalizeENSName(name string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(name))
	value = strings.TrimSuffix(value, ".")
	if value == "" || !strings.Contains(value, ".") {
		return "", fmt.Errorf("ENS name must include a dot suffix like .eth")
	}
	return value, nil
}

func ensNamehash(name string) common.Hash {
	node := common.Hash{}
	if name == "" {
		return node
	}
	labels := strings.Split(name, ".")
	for i := len(labels) - 1; i >= 0; i-- {
		labelHash := ethcrypto.Keccak256Hash([]byte(labels[i]))
		node = ethcrypto.Keccak256Hash(node.Bytes(), labelHash.Bytes())
	}
	return node
}

var (
	ensRegistryAddress = common.HexToAddress("0x00000000000C2E074eC69A0dFb2997BA6C7d2e1e")
	ensRegistryABI     = mustABI(`[{"inputs":[{"internalType":"bytes32","name":"node","type":"bytes32"}],"name":"resolver","outputs":[{"internalType":"address","name":"","type":"address"}],"stateMutability":"view","type":"function"}]`)
	ensResolverABI     = mustABI(`[{"inputs":[{"internalType":"bytes32","name":"node","type":"bytes32"}],"name":"addr","outputs":[{"internalType":"address payable","name":"","type":"address"}],"stateMutability":"view","type":"function"}]`)
)

func mustABI(raw string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(raw))
	if err != nil {
		panic(err)
	}
	return parsed
}

func (s *service) resolveENSAddress(ctx context.Context, name string) (string, error) {
	if s.domainResolverURL != "" {
		addr, err := s.resolveENSAddressViaService(ctx, name)
		if err == nil {
			return addr, nil
		}
		if s.ensClient == nil {
			return "", err
		}
	}
	if s.ensClient == nil {
		return "", fmt.Errorf("ENS verification is not configured")
	}
	return s.resolveENSAddressViaRPC(ctx, name)
}

func (s *service) resolveENSAddressViaService(ctx context.Context, name string) (string, error) {
	payload := map[string]any{
		"query": "query($chain:String!,$name:String!){lookup(inputs:[{chain:$chain,name:$name,skipCache:false}]){address error}}",
		"variables": map[string]string{
			"chain": "ethereum",
			"name":  name,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode ENS resolver request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.domainResolverURL, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("build ENS resolver request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.domainResolverAPIKey != "" {
		req.Header.Set("X-API-KEY", s.domainResolverAPIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ENS resolver request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read ENS resolver response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ENS resolver returned %s", resp.Status)
	}
	var result resolverLookupResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode ENS resolver response: %w", err)
	}
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("ENS resolver error: %s", result.Errors[0].Message)
	}
	if len(result.Data.Lookup) == 0 {
		return "", fmt.Errorf("ENS name does not resolve to an address")
	}
	if msg := strings.TrimSpace(result.Data.Lookup[0].Error); msg != "" {
		return "", fmt.Errorf("ENS resolver error: %s", msg)
	}
	addr, err := normalizeWalletAddress(result.Data.Lookup[0].Address)
	if err != nil {
		return "", fmt.Errorf("ENS resolver returned invalid address")
	}
	return addr, nil
}

func (s *service) resolveENSAddressViaRPC(ctx context.Context, name string) (string, error) {
	node := ensNamehash(name)
	data, err := ensRegistryABI.Pack("resolver", node)
	if err != nil {
		return "", err
	}
	res, err := s.ensClient.CallContract(ctx, ethereum.CallMsg{To: &ensRegistryAddress, Data: data}, nil)
	if err != nil {
		return "", fmt.Errorf("ENS resolver lookup failed: %w", err)
	}
	decoded, err := ensRegistryABI.Unpack("resolver", res)
	if err != nil || len(decoded) != 1 {
		return "", fmt.Errorf("ENS resolver decode failed")
	}
	resolverAddr, ok := decoded[0].(common.Address)
	if !ok || resolverAddr == (common.Address{}) {
		return "", fmt.Errorf("ENS name has no resolver")
	}
	data, err = ensResolverABI.Pack("addr", node)
	if err != nil {
		return "", err
	}
	res, err = s.ensClient.CallContract(ctx, ethereum.CallMsg{To: &resolverAddr, Data: data}, nil)
	if err != nil {
		return "", fmt.Errorf("ENS addr lookup failed: %w", err)
	}
	decoded, err = ensResolverABI.Unpack("addr", res)
	if err != nil || len(decoded) != 1 {
		return "", fmt.Errorf("ENS addr decode failed")
	}
	addr, ok := decoded[0].(common.Address)
	if !ok || addr == (common.Address{}) {
		return "", fmt.Errorf("ENS name does not resolve to an address")
	}
	return addr.Hex(), nil
}

func recoverWalletAddress(message, signature string) (string, error) {
	sigHex := strings.TrimPrefix(strings.TrimSpace(signature), "0x")
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return "", fmt.Errorf("decode wallet signature: %w", err)
	}
	if len(sig) != 65 {
		return "", fmt.Errorf("wallet signature must be 65 bytes")
	}
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	hash := accounts.TextHash([]byte(message))
	pubKey, err := ethcrypto.SigToPub(hash, sig)
	if err != nil {
		return "", fmt.Errorf("recover wallet address: %w", err)
	}
	return ethcrypto.PubkeyToAddress(*pubKey).Hex(), nil
}

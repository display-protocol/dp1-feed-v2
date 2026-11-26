package executor_test

import "strings"

// storedSignatures is the `"signatures":[...]` member of a stored-row fixture: one entry per kid, all in
// role. A row the feed has stored always carries signatures the schema accepts, so fixtures standing in
// for one must too. The values are well-formed placeholders, not real signatures — unit tests mock
// verification.
func storedSignatures(role string, kids ...string) string {
	entries := make([]string, 0, len(kids))
	for _, kid := range kids {
		entries = append(entries, `{"alg":"ed25519","kid":"`+kid+`","ts":"`+testCreatedRFC+`",`+
			`"payload_hash":"sha256:`+strings.Repeat("0", 64)+`","role":"`+role+`","sig":"c2ln"}`)
	}
	return `"signatures":[` + strings.Join(entries, ",") + `]`
}

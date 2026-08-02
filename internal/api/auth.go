package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
)

// Consumers maps an API key to the name of the consumer holding it.
//
// Per-consumer keys rather than one shared secret: reconciliation data is
// restricted to whoever handles SSG's finances, and a shared secret cannot be
// revoked for one caller.
type Consumers map[string]string

// ParseConsumers reads "name:key,name:key" from configuration.
//
// Keys are compared in constant time against a SHA-256 digest rather than the
// raw string: comparing raw strings of differing length leaks length, and
// hashing makes every comparison the same width.
func ParseConsumers(spec string) (Consumers, error) {
	out := Consumers{}
	if strings.TrimSpace(spec) == "" {
		return out, nil
	}

	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		name, key, ok := strings.Cut(entry, ":")
		name, key = strings.TrimSpace(name), strings.TrimSpace(key)
		if !ok || name == "" || key == "" {
			return nil, fmt.Errorf("API_KEYS entry %q is not name:key", entry)
		}
		if len(key) < 24 {
			// A short key is a guessable key. Refusing at startup beats
			// discovering it in an access log.
			return nil, fmt.Errorf("API key for %q is shorter than 24 characters", name)
		}
		if _, dup := out[digest(key)]; dup {
			return nil, fmt.Errorf("two consumers share an API key")
		}
		out[digest(key)] = name
	}

	return out, nil
}

func digest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return string(sum[:])
}

// Lookup returns the consumer holding a key, in constant time with respect to
// which key matched.
func (c Consumers) Lookup(key string) (string, bool) {
	if key == "" {
		return "", false
	}

	want := digest(key)
	var (
		name  string
		found bool
	)
	// Iterate every entry rather than returning early, so timing does not reveal
	// how far down the list a key sits.
	for stored, consumer := range c {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(want)) == 1 {
			name, found = consumer, true
		}
	}
	return name, found
}

// authenticate rejects requests without a recognised key.
//
// Reconciliation data is buyer-payment mapping, so it is restricted by default:
// an empty consumer set denies everything rather than allowing everything. A
// service that silently becomes public when its configuration is missing is the
// failure mode worth engineering against.
func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			key = r.Header.Get("X-API-Key")
		}

		name, ok := s.consumers.Lookup(key)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="reconciliation"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		s.log.Debug("request", "consumer", name, "path", r.URL.Path)
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

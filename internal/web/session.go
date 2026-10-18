package web

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// A session is a signed-in user. Its tokens stay on the server; the
// browser holds only the session ID (ADR-0018, item 7).
type session struct {
	id        string
	principal string
	signedIn  time.Time // for administration, which needs a recent sign-in
	last      time.Time // last request; guarded by sessions.mu

	// cacheMu guards admin, which remembers for a minute whether the user
	// administers a namespace, to show its Settings tab.
	cacheMu sync.Mutex
	admin   map[string]adminCheck

	// mu guards the tokens and serializes refreshes, which call the
	// provider while holding it.
	mu      sync.Mutex
	token   string    // F1 bearer token
	refresh string    // OIDC refresh token; "" for none
	expiry  time.Time // of token; zero for no expiry
}

type adminCheck struct {
	ok bool
	at time.Time
}

// tokens returns the current token and what is needed to refresh it.
func (s *session) tokens() (token, refresh string, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.refresh, s.expiry
}

// A pending login is an OIDC authorization request in progress, keyed by
// its state.
type pendingLogin struct {
	verifier string // PKCE code verifier
	next     string // where to go after sign-in
	created  time.Time
}

// pendingTTL bounds how long a user may take at the provider.
const pendingTTL = 10 * time.Minute

// sessions is the in-memory session store. A restart signs everyone out.
type sessions struct {
	idle time.Duration
	now  func() time.Time

	mu      sync.Mutex
	byID    map[string]*session
	pending map[string]pendingLogin
}

func newSessions(idle time.Duration) *sessions {
	return &sessions{idle: idle, now: time.Now, byID: map[string]*session{}, pending: map[string]pendingLogin{}}
}

// newID returns 32 random bytes, base64url-encoded.
func newID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (st *sessions) create(principal, token, refresh string, expiry time.Time) *session {
	now := st.now()
	s := &session{id: newID(), principal: principal, token: token, refresh: refresh, expiry: expiry,
		signedIn: now, last: now, admin: map[string]adminCheck{}}
	st.mu.Lock()
	st.byID[s.id] = s
	st.mu.Unlock()
	return s
}

// get returns the session with id, if it exists and is not idle too long,
// and marks it used.
func (st *sessions) get(id string) *session {
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.byID[id]
	if s == nil {
		return nil
	}
	if now.Sub(s.last) > st.idle {
		delete(st.byID, id)
		return nil
	}
	s.last = now
	return s
}

func (st *sessions) delete(id string) {
	st.mu.Lock()
	delete(st.byID, id)
	st.mu.Unlock()
}

func (st *sessions) addPending(state string, p pendingLogin) {
	p.created = st.now()
	st.mu.Lock()
	st.pending[state] = p
	st.mu.Unlock()
}

// takePending removes and returns the pending login for state. Each state
// is good for one callback.
func (st *sessions) takePending(state string) (pendingLogin, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := st.pending[state]
	delete(st.pending, state)
	if ok && st.now().Sub(p.created) > pendingTTL {
		return p, false
	}
	return p, ok
}

// sweep drops idle sessions and stale pending logins.
func (st *sessions) sweep() {
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, s := range st.byID {
		if now.Sub(s.last) > st.idle {
			delete(st.byID, id)
		}
	}
	for k, p := range st.pending {
		if now.Sub(p.created) > pendingTTL {
			delete(st.pending, k)
		}
	}
}

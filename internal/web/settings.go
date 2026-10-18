package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/f1/pkg/f1client"
)

// maxForm limits the body of every form f1web accepts (ADR-0020, item 13).
const maxForm = 64 << 10

// tokenDays are the expiries offered for a new token. F1 refuses those
// beyond its auth.token_max_ttl (90 days by default).
var tokenDays = []int{7, 30, 60, 90}

// settingsRoutes registers the user's own settings (ADR-0020, item 8).
func (s *Server) settingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/settings", s.page(s.profile))
	mux.HandleFunc("GET /-/settings/tokens", s.page(s.tokens))
	mux.HandleFunc("POST /-/settings/tokens", s.page(s.createToken))
	mux.HandleFunc("GET /-/settings/tokens/{id}/revoke", s.page(s.confirmRevokeToken))
	mux.HandleFunc("POST /-/settings/tokens/{id}/revoke", s.page(s.revokeToken))
	mux.HandleFunc("GET /-/settings/s3keys", s.page(s.s3keys))
	mux.HandleFunc("POST /-/settings/s3keys", s.page(s.createS3Key))
	mux.HandleFunc("GET /-/settings/s3keys/{key}/revoke", s.page(s.confirmRevokeS3Key))
	mux.HandleFunc("POST /-/settings/s3keys/{key}/revoke", s.page(s.revokeS3Key))
}

// settingsPage is the data of the settings pages.
type settingsPage struct {
	Tab    string // profile, tokens, s3keys
	Me     f1client.Me
	Tokens []f1client.Token
	S3Keys []f1client.S3Key
	Days   []int
	Now    time.Time
	// Secret is a credential just created, shown once (item 9).
	Secret *secret
}

type secret struct {
	ID, Value string // token ID or S3 access key, and the secret
}

// Expired reports whether t has expired.
func (p *settingsPage) Expired(t f1client.Token) bool {
	return t.Expires != nil && !p.Now.Before(*t.Expires)
}

// secretLine is a credential in the "secret" template.
type secretLine struct{ Name, Label, Value string }

// secretBox is the "secret" template's data: a title, then name, label
// and value for each line.
func secretBox(title string, lines ...string) map[string]any {
	var out []secretLine
	for i := 0; i+2 < len(lines); i += 3 {
		out = append(out, secretLine{lines[i], lines[i+1], lines[i+2]})
	}
	return map[string]any{"Title": title, "Lines": out}
}

// confirmPage asks before a destructive action (item 10).
type confirmPage struct {
	Repo     *repoView // a namespace's page; nil for the user's settings
	Admin    bool      // an administration page
	Tab      string
	Question string
	Detail   string
	Action   string // URL the form posts to
	Button   string
	Back     string
	Fields   map[string]string // hidden fields of the form
}

func (s *Server) profile(w http.ResponseWriter, r *http.Request, rq *request) error {
	me, err := rq.api.Me(r.Context())
	if err != nil {
		return err
	}
	return s.show(w, r, rq, "settings", "Settings", &settingsPage{Tab: "profile", Me: me})
}

func (s *Server) tokens(w http.ResponseWriter, r *http.Request, rq *request) error {
	return s.showTokens(w, r, rq, http.StatusOK, "", nil)
}

// showTokens renders the token page with a message or a new secret.
func (s *Server) showTokens(w http.ResponseWriter, r *http.Request, rq *request, status int, msg string, sec *secret) error {
	toks, err := rq.api.Tokens(r.Context())
	if err != nil {
		return err
	}
	p := &settingsPage{Tab: "tokens", Tokens: toks, Days: tokenDays, Now: time.Now(), Secret: sec}
	s.render(w, r, status, "tokens", pageData{User: rq.sess.principal, Title: "Tokens", Message: msg, Data: p})
	return nil
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := parseForm(w, r); err != nil {
		return s.showTokens(w, r, rq, http.StatusBadRequest, "The form is too large.", nil)
	}
	days, err := strconv.Atoi(r.PostFormValue("days"))
	if err != nil || days <= 0 {
		return s.showTokens(w, r, rq, http.StatusBadRequest, "Choose when the token expires.", nil)
	}
	t, err := rq.api.CreateToken(r.Context(), f1client.CreateTokenRequest{
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Expires:     time.Now().Add(time.Duration(days) * 24 * time.Hour),
	})
	if msg, ok := refused(err); ok {
		return s.showTokens(w, r, rq, http.StatusBadRequest, msg, nil)
	}
	if err != nil {
		return err
	}
	// The secret is on this response only, which is not stored (no-store)
	// and not a redirect, so reloading does not show it again.
	return s.showTokens(w, r, rq, http.StatusOK, "", &secret{ID: t.ID, Value: t.Secret})
}

func (s *Server) confirmRevokeToken(w http.ResponseWriter, r *http.Request, rq *request) error {
	id := r.PathValue("id")
	return s.show(w, r, rq, "confirm", "Revoke token", &confirmPage{Tab: "tokens",
		Question: "Revoke token " + id + "?",
		Detail:   "Programs that use it stop working. If you signed in here with it, you will be signed out.",
		Action:   "/-/settings/tokens/" + url.PathEscape(id) + "/revoke", Button: "Revoke", Back: "/-/settings/tokens"})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := rq.api.DeleteToken(r.Context(), r.PathValue("id")); err != nil && f1client.Code(err) != f1client.CodeNotFound {
		return err
	}
	http.Redirect(w, r, "/-/settings/tokens", http.StatusSeeOther)
	return nil
}

func (s *Server) s3keys(w http.ResponseWriter, r *http.Request, rq *request) error {
	return s.showS3Keys(w, r, rq, nil)
}

func (s *Server) showS3Keys(w http.ResponseWriter, r *http.Request, rq *request, sec *secret) error {
	keys, err := rq.api.S3Keys(r.Context())
	if err != nil {
		return err
	}
	return s.show(w, r, rq, "s3keys", "S3 keys", &settingsPage{Tab: "s3keys", S3Keys: keys, Secret: sec})
}

func (s *Server) createS3Key(w http.ResponseWriter, r *http.Request, rq *request) error {
	k, err := rq.api.CreateS3Key(r.Context())
	if err != nil {
		return err
	}
	return s.showS3Keys(w, r, rq, &secret{ID: k.AccessKey, Value: k.Secret})
}

func (s *Server) confirmRevokeS3Key(w http.ResponseWriter, r *http.Request, rq *request) error {
	key := r.PathValue("key")
	return s.show(w, r, rq, "confirm", "Revoke S3 key", &confirmPage{Tab: "s3keys",
		Question: "Revoke S3 key " + key + "?",
		Detail:   "S3 tools and programs that use it stop working.",
		Action:   "/-/settings/s3keys/" + url.PathEscape(key) + "/revoke", Button: "Revoke", Back: "/-/settings/s3keys"})
}

func (s *Server) revokeS3Key(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := rq.api.DeleteS3Key(r.Context(), r.PathValue("key")); err != nil && f1client.Code(err) != f1client.CodeNotFound {
		return err
	}
	http.Redirect(w, r, "/-/settings/s3keys", http.StatusSeeOther)
	return nil
}

// parseForm parses a form body of at most maxForm bytes.
func parseForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	return r.ParseForm()
}

// refused returns F1's message for a request it refused as invalid, to
// show on the form.
func refused(err error) (string, bool) {
	var ae *f1client.APIError
	if errors.As(err, &ae) && (ae.Error_.Code == f1client.CodeBadRequest || ae.Error_.Code == f1client.CodeLastAdmin) {
		return "F1 refused this: " + ae.Error_.Message + ".", true
	}
	return "", false
}

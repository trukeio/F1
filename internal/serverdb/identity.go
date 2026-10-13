package serverdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LocalIssuer is the issuer of principals created with "f1d user add".
const LocalIssuer = "local"

// TokenPrefix starts every local token, so that verifiers can tell local
// tokens from JWTs without a lookup.
const TokenPrefix = "f1_"

// Errors for identities.
var (
	ErrPrincipalTaken = errors.New("serverdb: principal name belongs to another issuer")
	ErrNoPrincipal    = errors.New("serverdb: no such principal")
	ErrNoToken        = errors.New("serverdb: no such token")
)

// Principal is a user known to the server.
type Principal struct {
	Name     string // "user:<name>"
	Issuer   string
	Disabled bool
	Created  time.Time
}

// ValidPrincipal checks a principal or group name: "user:<name>" or
// "group:<name>" with a non-empty name without whitespace or control
// characters.
func ValidPrincipal(p string) error {
	kind, name, ok := strings.Cut(p, ":")
	if !ok || (kind != "user" && kind != "group") || name == "" {
		return fmt.Errorf("invalid principal %q: use user:<name> or group:<name>", p)
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("invalid principal %q: whitespace or control character", p)
		}
	}
	return nil
}

// AddUser creates a local user with the given groups.
func (d *DB) AddUser(ctx context.Context, name string, groups []string) error {
	if err := ValidPrincipal(name); err != nil || !strings.HasPrefix(name, "user:") {
		return fmt.Errorf("serverdb: %q is not a user principal (user:<name>)", name)
	}
	for _, g := range groups {
		if err := ValidPrincipal(g); err != nil || !strings.HasPrefix(g, "group:") {
			return fmt.Errorf("serverdb: %q is not a group (group:<name>)", g)
		}
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		var issuer string
		err := tx.QueryRowContext(ctx, `SELECT issuer FROM principals WHERE name = ?`, name).Scan(&issuer)
		switch {
		case err == nil && issuer != LocalIssuer:
			return fmt.Errorf("%w: %s is authenticated by %s", ErrPrincipalTaken, name, issuer)
		case err == nil:
			return fmt.Errorf("serverdb: user %s already exists", name)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO principals(name, issuer, created_at) VALUES (?, ?, ?)`, name, LocalIssuer, time.Now().Unix()); err != nil {
			return err
		}
		for _, g := range groups {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memberships(principal, group_name) VALUES (?, ?)`, name, g); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetUserGroups replaces a local user's groups.
func (d *DB) SetUserGroups(ctx context.Context, name string, groups []string) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		var issuer string
		if err := tx.QueryRowContext(ctx, `SELECT issuer FROM principals WHERE name = ?`, name).Scan(&issuer); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrNoPrincipal, name)
			}
			return err
		}
		if issuer != LocalIssuer {
			return fmt.Errorf("serverdb: the groups of %s come from its tokens", name)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memberships WHERE principal = ?`, name); err != nil {
			return err
		}
		for _, g := range groups {
			if err := ValidPrincipal(g); err != nil || !strings.HasPrefix(g, "group:") {
				return fmt.Errorf("serverdb: %q is not a group (group:<name>)", g)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO memberships(principal, group_name) VALUES (?, ?)`, name, g); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetDisabled disables or enables a principal. A disabled principal's
// tokens, S3 keys and OIDC tokens are refused.
func (d *DB) SetDisabled(ctx context.Context, name string, disabled bool) error {
	res, err := d.db.ExecContext(ctx, `UPDATE principals SET disabled = ? WHERE name = ?`, boolInt(disabled), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNoPrincipal, name)
	}
	return nil
}

// Principals lists all principals.
func (d *DB) Principals(ctx context.Context) ([]Principal, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT name, issuer, disabled, created_at FROM principals ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		var p Principal
		var dis int
		var created int64
		if err := rows.Scan(&p.Name, &p.Issuer, &dis, &created); err != nil {
			return nil, err
		}
		p.Disabled, p.Created = dis != 0, time.Unix(created, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// LookupPrincipal returns a principal.
func (d *DB) LookupPrincipal(ctx context.Context, name string) (Principal, error) {
	p := Principal{Name: name}
	var dis int
	var created int64
	err := d.db.QueryRowContext(ctx, `SELECT issuer, disabled, created_at FROM principals WHERE name = ?`, name).Scan(&p.Issuer, &dis, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("%w: %s", ErrNoPrincipal, name)
	}
	p.Disabled, p.Created = dis != 0, time.Unix(created, 0)
	return p, err
}

// LocalGroups returns a local user's groups.
func (d *DB) LocalGroups(ctx context.Context, name string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT group_name FROM memberships WHERE principal = ? ORDER BY group_name`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SeenExternal records an externally authenticated principal: it is created
// on first use (just in time), and its current groups are kept for requests
// that carry no token (S3 keys). A name owned by another issuer is refused.
func (d *DB) SeenExternal(ctx context.Context, name, issuer string, groups []string) (Principal, error) {
	g, err := json.Marshal(groups)
	if err != nil {
		return Principal{}, err
	}
	var p Principal
	err = d.tx(ctx, func(tx *sql.Tx) error {
		p = Principal{Name: name, Issuer: issuer}
		var owner string
		var dis int
		err := tx.QueryRowContext(ctx, `SELECT issuer, disabled FROM principals WHERE name = ?`, name).Scan(&owner, &dis)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO principals(name, issuer, created_at, groups, groups_seen_at) VALUES (?, ?, ?, ?, ?)`,
				name, issuer, time.Now().Unix(), string(g), time.Now().Unix())
			return err
		case err != nil:
			return err
		case owner != issuer:
			return fmt.Errorf("%w: %s is authenticated by %s, not %s", ErrPrincipalTaken, name, owner, issuer)
		}
		p.Disabled = dis != 0
		_, err = tx.ExecContext(ctx, `UPDATE principals SET groups = ?, groups_seen_at = ? WHERE name = ?`, string(g), time.Now().Unix(), name)
		return err
	})
	return p, err
}

// LastGroups returns the groups an external principal had at its last
// token use, or nil if that is older than maxAge.
func (d *DB) LastGroups(ctx context.Context, name string, maxAge time.Duration) ([]string, error) {
	var g string
	var seen int64
	err := d.db.QueryRowContext(ctx, `SELECT groups, groups_seen_at FROM principals WHERE name = ?`, name).Scan(&g, &seen)
	if err != nil {
		return nil, err
	}
	if time.Since(time.Unix(seen, 0)) > maxAge {
		return nil, nil
	}
	var groups []string
	return groups, json.Unmarshal([]byte(g), &groups)
}

// Token is a local API token.
type Token struct {
	ID          string
	Principal   string
	Description string
	Created     time.Time
	Expires     time.Time // zero: never
	Revoked     bool
}

// CreateToken creates a local token for a principal and returns the secret
// token, which is shown once and stored only as a hash.
func (d *DB) CreateToken(ctx context.Context, principal, description string, ttl time.Duration) (Token, string, error) {
	if _, err := d.LookupPrincipal(ctx, principal); err != nil {
		return Token{}, "", err
	}
	secret := TokenPrefix + randomString(32)
	sum := sha256.Sum256([]byte(secret))
	t := Token{ID: "tok_" + randomString(8), Principal: principal, Description: description, Created: time.Now()}
	var exp int64
	if ttl > 0 {
		t.Expires = t.Created.Add(ttl)
		exp = t.Expires.Unix()
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO tokens(id, principal, hash, description, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, principal, sum[:], description, t.Created.Unix(), exp)
	return t, secret, err
}

// ErrBadToken is returned for unknown, expired or revoked tokens.
var ErrBadToken = errors.New("serverdb: invalid token")

// VerifyToken returns the token's principal if the token is valid.
func (d *DB) VerifyToken(ctx context.Context, secret string) (Principal, error) {
	sum := sha256.Sum256([]byte(secret))
	var name string
	var exp, revoked int64
	err := d.db.QueryRowContext(ctx, `SELECT principal, expires_at, revoked_at FROM tokens WHERE hash = ?`, sum[:]).Scan(&name, &exp, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrBadToken
	}
	if err != nil {
		return Principal{}, err
	}
	if revoked != 0 || (exp != 0 && time.Now().Unix() >= exp) {
		return Principal{}, ErrBadToken
	}
	return d.LookupPrincipal(ctx, name)
}

// RevokeToken revokes a token by id.
func (d *DB) RevokeToken(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNoToken, id)
	}
	return nil
}

// Tokens lists the tokens of a principal ("" for all).
func (d *DB) Tokens(ctx context.Context, principal string) ([]Token, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT id, principal, description, created_at, expires_at, revoked_at FROM tokens
		WHERE ? = '' OR principal = ? ORDER BY created_at, id`, principal, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created, exp, revoked int64
		if err := rows.Scan(&t.ID, &t.Principal, &t.Description, &created, &exp, &revoked); err != nil {
			return nil, err
		}
		t.Created, t.Revoked = time.Unix(created, 0), revoked != 0
		if exp != 0 {
			t.Expires = time.Unix(exp, 0)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// S3Key is an S3 access key.
type S3Key struct {
	AccessKey string
	Secret    string
	Principal string
	Created   time.Time
}

// CreateS3Key creates an S3 access key for a principal.
func (d *DB) CreateS3Key(ctx context.Context, principal string) (S3Key, error) {
	if _, err := d.LookupPrincipal(ctx, principal); err != nil {
		return S3Key{}, err
	}
	k := S3Key{AccessKey: "F1" + strings.ToUpper(randomString(18)), Secret: randomString(40), Principal: principal, Created: time.Now()}
	_, err := d.db.ExecContext(ctx, `INSERT INTO s3keys(access_key, secret, principal, created_at) VALUES (?, ?, ?, ?)`,
		k.AccessKey, k.Secret, principal, k.Created.Unix())
	return k, err
}

// S3Keys lists the active S3 keys of a principal, without secrets.
func (d *DB) S3Keys(ctx context.Context, principal string) ([]S3Key, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT access_key, created_at FROM s3keys WHERE principal = ? AND revoked_at = 0 ORDER BY created_at`, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []S3Key
	for rows.Next() {
		k := S3Key{Principal: principal}
		var created int64
		if err := rows.Scan(&k.AccessKey, &created); err != nil {
			return nil, err
		}
		k.Created = time.Unix(created, 0)
		out = append(out, k)
	}
	return out, rows.Err()
}

// LookupS3Key returns an active S3 key with its secret (for SigV4).
func (d *DB) LookupS3Key(ctx context.Context, accessKey string) (S3Key, error) {
	k := S3Key{AccessKey: accessKey}
	var created int64
	err := d.db.QueryRowContext(ctx, `SELECT secret, principal, created_at FROM s3keys WHERE access_key = ? AND revoked_at = 0`, accessKey).
		Scan(&k.Secret, &k.Principal, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrBadToken
	}
	k.Created = time.Unix(created, 0)
	return k, err
}

// RevokeS3Key revokes a principal's S3 key.
func (d *DB) RevokeS3Key(ctx context.Context, principal, accessKey string) error {
	res, err := d.db.ExecContext(ctx, `UPDATE s3keys SET revoked_at = ? WHERE access_key = ? AND principal = ? AND revoked_at = 0`,
		time.Now().Unix(), accessKey, principal)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNoToken, accessKey)
	}
	return nil
}

// ACLEntry grants perms on paths starting with Prefix in Namespace ("*":
// server-level operations) to Principal (a user or a group).
type ACLEntry struct {
	Namespace string
	Principal string
	Prefix    string
	Perms     []string
}

// Permissions.
const (
	PermRead  = "read"
	PermWrite = "write"
	PermLock  = "lock"
	PermAdmin = "admin"
)

// ServerNamespace is the ACL namespace for server-level operations.
const ServerNamespace = "*"

// SetACL creates or replaces an ACL entry. Empty perms deny: they override
// grants of shorter prefixes.
func (d *DB) SetACL(ctx context.Context, e ACLEntry) error {
	if err := ValidPrincipal(e.Principal); err != nil {
		return err
	}
	for _, p := range e.Perms {
		switch p {
		case PermRead, PermWrite, PermLock, PermAdmin:
		default:
			return fmt.Errorf("serverdb: unknown permission %q (read, write, lock, admin)", p)
		}
	}
	perms := append([]string(nil), e.Perms...)
	sort.Strings(perms)
	_, err := d.db.ExecContext(ctx, `INSERT OR REPLACE INTO acl(namespace, principal, path_prefix, perms) VALUES (?, ?, ?, ?)`,
		e.Namespace, e.Principal, e.Prefix, strings.Join(perms, ","))
	return err
}

// RemoveACL removes an ACL entry.
func (d *DB) RemoveACL(ctx context.Context, ns, principal, prefix string) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM acl WHERE namespace = ? AND principal = ? AND path_prefix = ?`, ns, principal, prefix)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("serverdb: no ACL entry for %s %q in %s", principal, prefix, ns)
	}
	return nil
}

// ACL returns the entries of a namespace, ordered by principal and prefix.
func (d *DB) ACL(ctx context.Context, ns string) ([]ACLEntry, error) {
	return d.acl(ctx, `SELECT namespace, principal, path_prefix, perms FROM acl WHERE namespace = ? ORDER BY principal, path_prefix`, ns)
}

// ACLFor returns the entries of a namespace that apply to any of the given
// principals (a user and its groups).
func (d *DB) ACLFor(ctx context.Context, ns string, principals []string) ([]ACLEntry, error) {
	if len(principals) == 0 {
		return nil, nil
	}
	q := `SELECT namespace, principal, path_prefix, perms FROM acl WHERE namespace = ? AND principal IN (?` +
		strings.Repeat(",?", len(principals)-1) + `)`
	args := []any{ns}
	for _, p := range principals {
		args = append(args, p)
	}
	return d.acl(ctx, q, args...)
}

// ACLNamespaces returns the namespaces that have entries for any of the
// principals.
func (d *DB) ACLNamespaces(ctx context.Context, principals []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(principals) == 0 {
		return out, nil
	}
	q := `SELECT DISTINCT namespace FROM acl WHERE perms != '' AND principal IN (?` + strings.Repeat(",?", len(principals)-1) + `)`
	args := make([]any, len(principals))
	for i, p := range principals {
		args[i] = p
	}
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out[ns] = true
	}
	return out, rows.Err()
}

func (d *DB) acl(ctx context.Context, q string, args ...any) ([]ACLEntry, error) {
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ACLEntry
	for rows.Next() {
		var e ACLEntry
		var perms string
		if err := rows.Scan(&e.Namespace, &e.Principal, &e.Prefix, &perms); err != nil {
			return nil, err
		}
		if perms != "" {
			e.Perms = strings.Split(perms, ",")
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:n]
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

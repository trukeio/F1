package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/trukeio/f1/internal/hash"
)

// ErrNoUpload is returned for an unknown multipart upload.
var ErrNoUpload = errors.New("cache: no such upload")

// UploadAttrs are the settings of a multipart upload given when it is
// created, applied to the object when it is completed.
type UploadAttrs struct {
	ContentType  string            `json:"ct,omitempty"`
	Meta         map[string]string `json:"meta,omitempty"`
	Checksum     string            `json:"cks,omitempty"`      // algorithm: crc32, crc32c, crc64nvme, sha1, sha256
	ChecksumType string            `json:"cks_type,omitempty"` // COMPOSITE or FULL_OBJECT
}

// Upload is an open multipart upload.
type Upload struct {
	ID        string
	Ref       string
	Key       string
	Principal string
	Created   time.Time
	Attrs     UploadAttrs
}

// Part is an uploaded part.
type Part struct {
	Number   int
	File     hash.Hash
	Size     int64
	ETag     string // hex MD5
	Checksum []byte // nil if the upload has no checksum algorithm
	Created  time.Time
}

// CreateUpload records a new upload.
func (c *DB) CreateUpload(ctx context.Context, u Upload) error {
	attrs, err := json.Marshal(u.Attrs)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO uploads(id, ref, key, principal, created, attrs) VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, u.Ref, u.Key, u.Principal, u.Created.UnixNano(), string(attrs))
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}

// GetUpload returns an open upload, or ErrNoUpload.
func (c *DB) GetUpload(ctx context.Context, id string) (Upload, error) {
	ups, err := c.uploads(ctx, `WHERE id = ?`, id)
	if err != nil {
		return Upload{}, err
	}
	if len(ups) == 0 {
		return Upload{}, ErrNoUpload
	}
	return ups[0], nil
}

// Uploads returns the open uploads of a ref whose key starts with prefix, in
// (key, id) order, starting after (keyMarker, idMarker), at most limit (0:
// all).
func (c *DB) Uploads(ctx context.Context, ref, prefix, keyMarker, idMarker string, limit int) ([]Upload, error) {
	q := `WHERE ref = ? AND instr(key, ?) = 1 AND (key > ? OR (key = ? AND id > ?)) ORDER BY key, id`
	args := []any{ref, prefix, keyMarker, keyMarker, idMarker}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return c.uploads(ctx, q, args...)
}

// CompletedUpload is the result of a completed upload, kept for the upload
// expiry time so that a repeated CompleteMultipartUpload gets it again.
type CompletedUpload struct {
	ID      string `json:"-"`
	Ref     string `json:"-"`
	Key     string `json:"-"`
	ETag    string `json:"etag"`
	Version string `json:"version,omitempty"`
	CksAlg  string `json:"cks_alg,omitempty"`
	Cks     string `json:"cks,omitempty"`
	CksType string `json:"cks_type,omitempty"`
}

// CompleteUpload removes an upload and its parts and records its result,
// in one transaction. It returns ErrNoUpload if the upload is gone.
func (c *DB) CompleteUpload(ctx context.Context, res CompletedUpload) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return c.tx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, res.ID)
		if err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrNoUpload
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM upload_parts WHERE upload = ?`, res.ID); err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO completed_uploads(id, ref, key, result, created) VALUES (?, ?, ?, ?, ?)`,
			res.ID, res.Ref, res.Key, string(b), time.Now().UnixNano())
		if err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		return nil
	})
}

// GetCompleted returns the result of a completed upload, or ErrNoUpload.
func (c *DB) GetCompleted(ctx context.Context, id string) (CompletedUpload, error) {
	res := CompletedUpload{ID: id}
	var b string
	err := c.db.QueryRowContext(ctx, `SELECT ref, key, result FROM completed_uploads WHERE id = ?`, id).Scan(&res.Ref, &res.Key, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return res, ErrNoUpload
	}
	if err != nil {
		return res, fmt.Errorf("cache: %w", err)
	}
	return res, json.Unmarshal([]byte(b), &res)
}

// ForgetCompleted removes records of uploads completed before t.
func (c *DB) ForgetCompleted(ctx context.Context, t time.Time) error {
	if _, err := c.db.ExecContext(ctx, `DELETE FROM completed_uploads WHERE created < ?`, t.UnixNano()); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}

// ExpiredUploads returns the ids of uploads created before t.
func (c *DB) ExpiredUploads(ctx context.Context, t time.Time) ([]string, error) {
	ups, err := c.uploads(ctx, `WHERE created < ?`, t.UnixNano())
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(ups))
	for i, u := range ups {
		ids[i] = u.ID
	}
	return ids, nil
}

func (c *DB) uploads(ctx context.Context, where string, args ...any) ([]Upload, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, ref, key, principal, created, attrs FROM uploads `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		var u Upload
		var created int64
		var attrs string
		if err := rows.Scan(&u.ID, &u.Ref, &u.Key, &u.Principal, &created, &attrs); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
		u.Created = time.Unix(0, created)
		if err := json.Unmarshal([]byte(attrs), &u.Attrs); err != nil {
			return nil, fmt.Errorf("cache: upload %s: %w", u.ID, err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// PutPart records a part, replacing an earlier part with the same number.
// It fails with ErrNoUpload if the upload is gone (completed or aborted
// concurrently).
func (c *DB) PutPart(ctx context.Context, upload string, p Part) error {
	return c.tx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM uploads WHERE id = ?`, upload).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNoUpload
			}
			return fmt.Errorf("cache: %w", err)
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO upload_parts(upload, number, file, size, etag, checksum, created) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			upload, p.Number, p.File[:], p.Size, p.ETag, p.Checksum, p.Created.UnixNano())
		if err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		return nil
	})
}

// Parts returns the parts of an upload with numbers greater than after, in
// order, at most limit (0: all).
func (c *DB) Parts(ctx context.Context, upload string, after, limit int) ([]Part, error) {
	q := `SELECT number, file, size, etag, checksum, created FROM upload_parts WHERE upload = ? AND number > ? ORDER BY number`
	args := []any{upload, after}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		var p Part
		var file []byte
		var created int64
		if err := rows.Scan(&p.Number, &file, &p.Size, &p.ETag, &p.Checksum, &created); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
		if len(file) != hash.Size {
			return nil, fmt.Errorf("cache: upload %s part %d: bad file hash", upload, p.Number)
		}
		copy(p.File[:], file)
		p.Created = time.Unix(0, created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteUpload removes an upload and its parts. It returns ErrNoUpload if
// the upload does not exist, so that of two concurrent completions only one
// succeeds.
func (c *DB) DeleteUpload(ctx context.Context, id string) error {
	return c.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNoUpload
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM upload_parts WHERE upload = ?`, id); err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		return nil
	})
}

// PartFiles returns the Files of every part of every open upload (GC
// roots).
func (c *DB) PartFiles(ctx context.Context) ([]hash.Hash, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT file FROM upload_parts`)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	defer rows.Close()
	var out []hash.Hash
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
		var h hash.Hash
		copy(h[:], b)
		out = append(out, h)
	}
	return out, rows.Err()
}

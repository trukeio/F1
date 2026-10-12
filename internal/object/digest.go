package object

import (
	"errors"

	"github.com/trukeio/f1/internal/hash"
)

// DigestRecord holds the external digests of one File (D21). Encoded as
// [file, sha1git, sha256, md5].
type DigestRecord struct {
	_       struct{} `cbor:",toarray"`
	File    hash.Hash
	SHA1Git [20]byte // SHA-1 of "blob <size>\x00" ‖ content
	SHA256  [32]byte // SHA-256 of the content
	MD5     [16]byte // MD5 of the content
}

func (*DigestRecord) Type() Type { return TypeDigestRecord }

// Validate checks the DigestRecord invariants.
func (d *DigestRecord) Validate() error {
	if d.File.IsZero() {
		return errors.New("zero file")
	}
	return nil
}

// GitRaw holds the exact bytes of a Git tree object that F1 cannot re-encode
// canonically. Encoded as a CBOR byte string.
type GitRaw []byte

func (*GitRaw) Type() Type { return TypeGitRaw }

// Validate checks the GitRaw invariants.
func (g *GitRaw) Validate() error {
	if len(*g) == 0 {
		return errors.New("empty git raw object")
	}
	return nil
}

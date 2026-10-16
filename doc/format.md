# F1 on-disk format, version 1

This document specifies every byte of the F1 object and pack formats. Everything here is permanent once phase 1 is merged: changing it changes object hashes and breaks deduplication. Changes require an ADR and a new format version. The decisions behind it are in ADR-0001 (D1–D5, D20, D21) and ADR-0002.

The reference implementation is in `internal/chunk`, `internal/object`, `internal/pack` and `internal/content`. Golden vectors in `testdata/golden/` pin the format; the tests that check them are named `TestGolden*`.

Conventions: all fixed-width integers are little-endian. `‖` is concatenation. `BLAKE3(x)` is BLAKE3 with 32-byte output, no key.

## 1. Hashes

An object hash is `BLAKE3(tag ‖ payload)`, where `tag` is the object's 1-byte type and `payload` is:

- for a Chunk, the raw (uncompressed) chunk bytes;
- for every other type, the canonical CBOR encoding of the object (section 3).

The tag gives domain separation: a chunk and a CBOR object with the same bytes have different hashes.

| Tag | Type |
|---|---|
| `0x01` | Chunk |
| `0x02` | File |
| `0x03` | FileIndex |
| `0x04` | Tree node |
| `0x05` | Commit |
| `0x06` | Tag |
| `0x07` | DigestRecord |
| `0x08` | GitRaw |

Tag `0x00` is never an object type; in packs it marks the end of the records (section 5).

Hex form: 64 lowercase hex digits.

## 2. Chunking (FastCDC)

Content is split with FastCDC using normalized chunking level 2.

- `MinSize` = 16384, `AvgSize` = 65536, `MaxSize` = 262144 bytes.
- Gear table: 256 `uint64` values. Value `i` is bytes `[8i, 8i+8)`, read little-endian, of the BLAKE3 extendable output (XOF) of the ASCII string `F1 FastCDC gear table v1`. The table is committed in `internal/chunk/gear.go`, and `TestGearTable` checks it against this definition.
- Masks: `maskS = 0xFFFFC00000000000` (the 18 most significant bits), `maskL = 0xFFFC000000000000` (the 14 most significant bits).

Cut point of the first chunk of a buffer `data` holding `n` bytes, where `n ≥ MaxSize` unless the buffer ends the input:

```
if n <= MinSize: return n
n = min(n, MaxSize)
normal = min(AvgSize, n)
fp = 0 (uint64, wrapping arithmetic)
for i in [MinSize, normal):  fp = (fp << 1) + gear[data[i]]; if fp & maskS == 0: return i + 1
for i in [normal, n):        fp = (fp << 1) + gear[data[i]]; if fp & maskL == 0: return i + 1
return n
```

The first `MinSize` bytes of a chunk are not hashed. The stream is cut repeatedly from the start. Empty input produces no chunks. Every chunk except the last is between `MinSize` and `MaxSize` bytes long; the last is between 1 and `MaxSize` bytes. Boundaries depend only on the content, not on how the input is read.

## 3. Canonical CBOR

All objects except Chunk are CBOR (RFC 8949) in core deterministic encoding (§4.2.1):

- integers, lengths and map sizes in their shortest form;
- definite lengths only (no indefinite-length items);
- map keys sorted by the bytewise order of their encodings;
- no duplicate map keys, no tags, no floating-point values;
- text strings are valid UTF-8.

Additional F1 rules:

- Hashes are byte strings of exactly 32 bytes; SHA-1 values of 20, SHA-256 values of 32 and MD5 values of 16 bytes.
- "Absent" and "empty" are not distinguished for the optional fields marked "omitted when empty" below: such a field is omitted when empty and must not be encoded with an empty value. Required array fields are always present, as `[]` when empty.
- A decoder accepts an encoding only if re-encoding the decoded value reproduces it byte for byte, and the value passes the validation rules below. Every object therefore has exactly one valid encoding.

Notation: `[a, b]` is a CBOR array, `{0: a, 1: b?}` a map with unsigned integer keys where `?` marks a field omitted when empty, `bstr` a byte string, `tstr` a text string, `uint`/`int` an integer.

## 4. Objects

### 4.1 Chunk (0x01)

Raw bytes; not CBOR. Length 1 to `MaxSize`. The empty file has no chunks.

### 4.2 File (0x02) and FileIndex (0x03)

```
Ref       = [hash: bstr(32), size: uint]          ; size = content bytes covered, ≥ 1
File      = [size: uint, level: uint, refs: [Ref*]]
FileIndex = [level: uint, refs: [Ref*]]
```

- `level` 0: each ref is a chunk, and `size` is the chunk length (at most `MaxSize`).
- `level` n > 0: each ref is a FileIndex of level n − 1.
- File: `size` equals the sum of the ref sizes; `refs` is empty exactly when `size` is 0; at most 4096 refs; `level` ≤ 4; a File with `level` > 0 has at least 2 refs.
- FileIndex: 1 to 4096 refs; `level` ≤ 3.

Ref sizes let a reader find the chunk at any byte offset by prefix sums, without reading earlier chunks.

**Canonical layout.** Given the chunk refs of a file in order:

```
refs = chunk refs; level = 0
while len(refs) > 4096:
    split refs into groups, left to right: a group ends after ref r when
        (len(group) ≥ 256 and (r.hash[0] | r.hash[1] << 8) & 0x3FF == 0)
        or len(group) == 4096
    the last group takes the remaining refs
    refs = [ Ref(hash of FileIndex{level, group}, sum of group sizes) for each group ]
    level = level + 1
File = [sum of chunk sizes, level, refs]
```

Groups are content-defined, so an edit in a large file changes only the FileIndex objects around it.

### 4.3 Tree node (0x04)

A directory, or the root of a `flat` namespace, is a prolly tree whose nodes are Tree objects.

```
TreeNode = {0: level: uint, 1: entries: [Entry*]?, 2: children: [Child*]?}
Entry    = {0: name: bstr, 1: kind: uint, 2: target: bstr(32), 3: attrs: Attrs?}
Child    = [first_name: bstr, node: bstr(32), count: uint]
```

- Level 0 (leaf): `entries` sorted strictly by `name` in byte order, at most 512 entries, no `children`. The empty tree is the leaf `{0: 0}`.
- Level ≥ 1 (interior): 1 to 512 `children` sorted strictly by `first_name`, no `entries`. `first_name` is the first entry name in the child's subtree, `node` is the child's hash, `count` (≥ 1) is the number of leaf entries below it.
- `name` is non-empty.

Entry kinds:

| Kind | Name | Target | Required attrs |
|---|---|---|---|
| 1 | file | File | — |
| 2 | exec | File | — |
| 3 | symlink | File holding the link text | — |
| 4 | dir | root of the subtree | — |
| 5 | gitlink | zero hash (32 zero bytes) | `git_sha1` |
| 6 | lfs | File holding the real content | `lfs_pointer` |

`Attrs` is a CBOR map with text keys; every field is omitted when empty, and an entry without attributes omits key 3 (an empty map is invalid):

| Key | Type | Meaning | Allowed on |
|---|---|---|---|
| `ct` | tstr | S3 content type | any |
| `meta` | map tstr → tstr | S3 user metadata, and stored standard headers (below) | any |
| `etag` | tstr | S3 ETag, without quotes | any |
| `mtime` | int | write time, Unix ns | any |
| `cks` | map tstr → bstr | S3 checksums sent by the client (below) | any |
| `exec` | true | executable LFS file | lfs |
| `git_sha1` | bstr(20) | submodule commit | gitlink (required) |
| `lfs_pointer` | bstr | exact LFS pointer bytes | lfs (required) |
| `git_raw` | bstr(32) | hash of a GitRaw with the exact Git tree | dir |

Unknown keys are invalid in format version 1.

Conventions inside the S3 attributes (ADR-0007); they use the types above and do not change the encoding:

- `meta` keys are the lower-case names after `x-amz-meta-`. A key starting with `:` holds a standard header kept with the object: `:cache-control`, `:content-disposition`, `:content-encoding`, `:content-language`, `:expires`. No `x-amz-meta-` header can produce such a name.
- `cks` keys are checksum algorithms: `crc32`, `crc32c`, `crc64nvme`, `sha1`, `sha256`. A key `<algorithm>` holds a full-object checksum (big-endian CRC, or the hash). A key `<algorithm>-<n>` holds the composite checksum of a multipart upload with `n` parts: the algorithm over the concatenated part checksums.

**Node boundaries (D20).** Each level is built left to right from its items (entries at level 0, children above). An item is added to the current node, and the node ends after it when:

- the node has at least 16 items and the item's boundary hash has `hash[0] & 0x3F == 0`; or
- the node has 512 items; or
- the sum of the encoded sizes (in bytes) of the node's items, each item's CBOR encoding as in the grammar above, is 131072 or more.

The last node of a level takes the remaining items. Boundary hashes:

- leaf: `BLAKE3(name)`;
- interior level L (L ≥ 1): `BLAKE3(uint8(L) ‖ first_name)`.

These are plain BLAKE3 digests without a type tag; they are not object identities. Building starts with the leaf level; each next level is built from one Child per node of the level below. The first level that forms a single node is the root. A single leaf (including the empty leaf) is its own root.

On average a node holds about 64 + 16 items. The boundary depends only on names, so changing an entry's target or attributes never moves a boundary.

### 4.4 Commit (0x05)

```
Person      = [name: bstr, email: bstr, time: int, tz: tstr]   ; time in Unix seconds, tz like "+0200"
ExtraHeader = [key: bstr, value: bstr]
Commit = {
  0: tree: bstr(32),
  1: parents: [bstr(32)*],          ; always present, [] for a root commit; no duplicates
  2: author: Person,
  3: committer: Person,
  4: message: bstr,                 ; raw, always present
  5: git_extra: [ExtraHeader*]?,    ; Git headers not modeled above, in original order
  6: git_raw_hint: bstr?,           ; exact Git commit bytes, only when re-encoding fails
  7: git_tree_raw: bstr(32)?        ; GitRaw of a non-canonical root Git tree
}
```

`tree` and parents are non-zero hashes; extra header keys are non-empty.

Exporting a commit with `git_raw_hint` uses the raw bytes only if they name the same tree and parents. Git allows a commit to list a parent more than once, which F1 parents do not: a commit imported from Git lists each parent once, in first-occurrence order, and keeps the exact bytes in `git_raw_hint`. The raw parents are therefore compared after removing repeats.

### 4.5 Tag (0x06)

```
Tag = {
  0: object: bstr(32),              ; F1 hash of the tagged object
  1: type: tstr,                    ; "commit", "tree", "blob" or "tag"
  2: name: bstr,                    ; non-empty
  3: tagger: Person?,
  4: message: bstr,
  5: git_extra: [ExtraHeader*]?,
  6: git_raw_hint: bstr?
}
```

### 4.6 DigestRecord (0x07)

```
DigestRecord = [file: bstr(32), sha1git: bstr(20), sha256: bstr(32), md5: bstr(16)]
```

`sha1git` = SHA-1(`"blob " ‖ decimal size ‖ 0x00 ‖ content`), the Git blob ID. `sha256` and `md5` are digests of the content. A DigestRecord is written after its File and is referenced by no other object.

### 4.7 GitRaw (0x08)

```
GitRaw = bstr        ; non-empty: the exact bytes of a Git tree object (without the "tree <size>\0" header)
```

## 5. Pack files and pack streams

A pack holds objects in records, followed by an index and a trailer. Packs on disk and pack streams in the native API use the same format.

```
pack    = header records 0x00 index trailer
header  = "F1PK" version:u32                     ; version = 1 (8 bytes)
record  = type:u8 flags:u8 hash:[32] size:u32 stored:u32 payload:[stored]   ; 42-byte header
index   = entry*                                 ; sorted strictly by hash
entry   = hash:[32] type:u8 flags:u8 offset:u64 size:u32 stored:u32          ; 50 bytes
trailer = index_offset:u64 count:u32 checksum:[32] "F1PE"                   ; 48 bytes
```

- `type` is the object type (`0x01`–`0x08`). A `0x00` byte where a record would start ends the records, so a pack stream can be read sequentially without the index.
- `flags`: bit 0 set means the payload is zstd-compressed (one zstd frame); all other bits are 0. Without bit 0, `stored` equals `size`.
- `size` is the uncompressed payload length, at most 64 MiB; `hash` is the object hash of the uncompressed payload.
- Compression: a payload is stored compressed only if `20 × compressed ≤ 19 × size` (zstd saves at least 5 %). The compressed bytes depend on the zstd implementation; this does not affect hashes or the validity of a pack.
- A pack contains each hash at most once.
- `offset` in an index entry is the offset of the record header from the start of the pack; the entry repeats the record header fields.
- `index_offset` is the offset of the first index entry, immediately after the `0x00` byte. `count` is the number of entries; the index ends where the trailer starts.
- `checksum` = BLAKE3 of every byte of the pack before the checksum field (from the header through `count`).

Readers check the header, trailer magic, index order and bounds when opening a pack, check each record header against the index when reading it, and verify every payload against its hash. A full verification also checks the checksum and that the records are contiguous and match the index one to one.

## 6. Golden vectors

| File | Content |
|---|---|
| `testdata/golden/chunks.json` | chunk boundaries and hashes of fixed inputs |
| `testdata/golden/objects.json` | canonical encoding and hash of every object type |
| `testdata/golden/file-layout.json` | File hashes for synthetic chunk lists up to 20,000 chunks |
| `testdata/golden/tree.json` | leaf and interior boundary decisions, and prolly tree root hashes |
| `testdata/golden/pack.json` | the exact bytes of a small pack |

Test inputs are generated from the BLAKE3 XOF of fixed seed strings (`internal/testutil.Bytes`), so the vectors need no binary input files.

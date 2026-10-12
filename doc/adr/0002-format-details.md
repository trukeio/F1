# ADR-0002: On-disk format details

- Status: accepted (format permanent from this merge)
- Date: 2026-10-12
- Deciders: Rolf Veen

## Context

ADR-0001 fixes the hash, chunking parameters, object encoding and prolly tree parameters, but phase 1 has to settle many byte-level details the plan leaves open. Once phase 1 is merged they are permanent (`doc/plan.md`, "How to use this document"). This ADR records them; `doc/format.md` is the normative specification.

## Decision

1. **Chunk hashes are tagged.** A chunk hash is `BLAKE3(0x01 ‖ bytes)`, like every other object (D5). D3 is read as "hash the uncompressed bytes", not as "hash without a tag". This gives domain separation between chunks and CBOR objects.

2. **FastCDC variant.**
   - Normalized chunking level 2.
   - The gear hash `fp = (fp << 1) + gear[b]` starts at `MinSize`.
   - Masks select the 18 most significant bits before `AvgSize` and the 14 most significant bits from `AvgSize` on; the high bits of the gear hash depend on the last 64 bytes.
   - The gear table is the BLAKE3 XOF of `F1 FastCDC gear table v1`, read as 256 little-endian `uint64` values.
   - Measured on random data: average chunk 74 KB, 1.5 GB/s on an i5-9400.

3. **CBOR schemas.**
   - Objects with fixed fields are CBOR arrays: File, FileIndex, Ref, Child, Person, ExtraHeader, DigestRecord.
   - Objects with optional fields are maps with small unsigned integer keys: Tree node, Entry, Commit, Tag.
   - Entry attrs are a map with text keys, as the plan specifies.
   - Optional fields are omitted when empty and may not be encoded empty; required arrays are always present (`[]`).
   - Decoding is strict: unknown keys, tags, indefinite lengths and duplicate keys are rejected, and an encoding is accepted only if re-encoding reproduces it byte for byte. Every object therefore has exactly one valid encoding, which matters once clients upload objects (native API).

4. **File layout.**
   - `File = [size, level, refs]` and `FileIndex = [level, refs]`, with `Ref = [hash, size]`.
   - The plan's two shapes ("direct list or index") become one shape with a level, which also covers the "second level if needed" case uniformly (up to 4 levels).
   - Files with more than 4096 chunks group refs into FileIndex objects at content-defined boundaries: at least 256 refs, a 10-bit mask on the ref hash, at most 4096 refs. This replaces fixed groups of 4096, so an edit in a large file rewrites about one FileIndex instead of every FileIndex after the edit.

5. **Tree nodes.**
   - The root is the first level that forms a single node.
   - The byte cap counts the CBOR encodings of a node's items.
   - The "low 6 bits" of the boundary hash means `hash[0] & 0x3F`.
   - The level byte in the interior boundary hash is `uint8(level)`, starting at 1 for the lowest interior level.
   - The plan's "a tree with fewer than 16 entries is a single leaf" follows from the min-16 rule, except when the 128 KiB cap forces a split.
   - The gitlink target is the zero hash, and its commit is in `attrs.git_sha1`.

6. **GitRaw** holds the Git tree object body without the `tree <size>\0` header, as a CBOR byte string.

7. **Compression rule** (D4) applies to every record, not only chunks: a payload is stored compressed when `20 × compressed ≤ 19 × uncompressed`. zstd runs at the default level, without frame checksums, since payloads are verified by their hash.

8. **Pack format.**
   - Little-endian, with a 42-byte record header that carries type, flags, hash, uncompressed and stored length.
   - A `0x00` end-of-records byte, so packs can be read as streams (native API uploads).
   - A sorted 50-byte-per-entry footer index that repeats type, flags, offset and both lengths (the plan's "type and uncompressed length in the footer").
   - A 48-byte trailer with index offset, count, the BLAKE3 checksum of everything before the checksum, and an end magic.
   - Maximum object size 64 MiB.

9. **Ingest takes a size hint.** The Git blob SHA-1 needs the size before the content. When the size is known (Content-Length, S3 decoded length, Git pack object header), all digests are computed in one pass. When it is not, ingest reads the stored content back once to compute the SHA-1. In both cases the DigestRecord is written before ingest returns (D21).

## Consequences

- Golden vectors in `testdata/golden/` pin all of the above. Changing any of it after the merge needs a new ADR and a format version change.
- Strict decoding costs one re-encode per decoded object. This is negligible next to hashing and decompression.
- Content-defined FileIndex grouping means FileIndex objects vary in size (256–4096 refs).

## Alternatives considered

- **Untagged chunk hashes (`BLAKE3(bytes)`).** These would match some other systems' chunk IDs, but there is no interoperability need, and they lose domain separation.
- **String-keyed maps for every object.** These are more self-describing but larger, and they make tree nodes and File objects, the most numerous objects, noticeably bigger.
- **Fixed FileIndex groups of 4096.** These are simpler, but any insertion rewrites every FileIndex after the edit, about 160 KB of index per 256 MiB of file.
- **Lenient CBOR decoding.** This would accept several encodings of the same logical object, so identical content could get different hashes depending on the client.

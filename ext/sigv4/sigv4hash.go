// Package sigv4 is the AWS SigV4 request-body signer for htmx-go, shipped as an
// in-path RequestTransformer (see sigv4.go, the js/wasm half). Gothic apps
// deployed on AWS run behind CloudFront OAC (SigV4) → Lambda Function URL with
// authorization_type = AWS_IAM. Every htmx request that carries a body MUST send
// header x-amz-content-sha256 = sha256-hex(request body) or CloudFront's SigV4
// body check rejects it with HTTP 403.
//
// This file holds the PURE, host-testable body-hashing logic — it has NO
// syscall/js dependency and carries NO build tag, so it compiles and runs under
// the standard host toolchain and can be unit-tested without a WASM runtime
// (`go test ./ext/sigv4/`). The js/wasm half (sigv4.go) is a thin adapter that
// reads the in-path request's urlencoded body and calls Sha256Hex here.
//
// # Why this exists as its own copy (not an import)
//
// The dependency DAG forbids htmx-go from importing the core module, so this
// package cannot import core's awssign helper — it carries its own copy of the
// encoding + hashing logic and its own vector tests instead.
//
// # Byte-exactness is the whole game
//
// htmx serializes a urlencoded body with encodeURIComponent(key)+"="+
// encodeURIComponent(value) per FormData entry (in insertion order), joined by
// "&". encodeURIComponent here means the JAVASCRIPT function, NOT Go's net/url —
// url.QueryEscape encodes space as "+" and escapes ! * ' ( ), both of which would
// change the bytes → wrong SHA-256 → HTTP 403. EncodeBody + encodeURIComponent
// below match JS encodeURIComponent exactly (space → %20, unreserved set
// A-Z a-z 0-9 - _ . ! ~ * ' ( ) passes through literally, every other UTF-8 byte
// → %XX uppercase). The signer actually hashes htmx's OWN urlencoded body string
// (TransformableRequest.EncodedBody, i.e. htmx's urlEncode) so byte-identity to
// the sent bytes is guaranteed by construction; EncodeBody exists so the vector
// tests can assert the encoding independently.
package sigv4

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// EmptyBodyHash is sha256("") in lowercase hex — the value to send when the
// request has no body (a GET/DELETE with its params in the URL, or a POST with
// no parameters). Hardcoded so the common empty-body path never hashes.
const EmptyBodyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// UnsignedPayload is the SigV4 sentinel that tells CloudFront's body check to
// skip hashing the body. Used for multipart/form-data requests, whose
// random-boundary body the browser fills in and we cannot reproduce.
const UnsignedPayload = "UNSIGNED-PAYLOAD"

// ContentSha256Header is the request header CloudFront OAC / SigV4 checks against
// the body hash.
const ContentSha256Header = "x-amz-content-sha256"

// upperHex is the uppercase hex alphabet used for percent-encoding. JavaScript's
// encodeURIComponent emits UPPERCASE hex (%20, %C3), so we must too — lowercase
// would change the body bytes and break the signature.
const upperHex = "0123456789ABCDEF"

// isUnreserved reports whether a byte is in JavaScript encodeURIComponent's
// unreserved set — the bytes that pass through literally: A-Z a-z 0-9 and the
// punctuation - _ . ! ~ * ' ( ). Every other byte is percent-encoded.
func isUnreserved(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}

// encodeURIComponent replicates JavaScript's encodeURIComponent EXACTLY — it is
// deliberately NOT net/url. Every byte of s's UTF-8 encoding either passes
// through literally (the unreserved set) or becomes %XX with UPPERCASE hex.
// Space → %20 (never "+"). Because a Go string is already UTF-8 bytes, iterating
// byte-by-byte is inherently per-UTF-8-byte encoding: e.g. 'é' (0xC3 0xA9) →
// "%C3%A9".
func encodeURIComponent(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		b := s[i]
		if isUnreserved(b) {
			sb.WriteByte(b)
			continue
		}
		sb.WriteByte('%')
		sb.WriteByte(upperHex[b>>4])
		sb.WriteByte(upperHex[b&0x0f])
	}
	return sb.String()
}

// EncodeBody builds the application/x-www-form-urlencoded body EXACTLY as htmx
// does: for each [key, value] pair in the given order,
// encodeURIComponent(key) + "=" + encodeURIComponent(value), joined by "&". The
// pair order MUST match htmx's FormData insertion order. Split out from the hash
// so tests can assert the exact body bytes independently of the digest; the live
// signer hashes htmx's own body string via Sha256Hex.
func EncodeBody(entries [][2]string) string {
	var sb strings.Builder
	for i := range entries {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(encodeURIComponent(entries[i][0]))
		sb.WriteByte('=')
		sb.WriteString(encodeURIComponent(entries[i][1]))
	}
	return sb.String()
}

// Sha256Hex returns the lowercase-hex SHA-256 of s. This is the primitive the
// live signer uses to hash htmx's own urlencoded body string (guaranteeing
// byte-identity to the sent bytes). crypto/sha256 is synchronous, so the WASM
// adapter can call it inline inside the request transformer.
func Sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ContentHashHex returns the x-amz-content-sha256 header value for a urlencoded
// body built from entries: sha256 of EncodeBody(entries) as lowercase hex. When
// entries is empty the body is empty, so it returns the precomputed
// EmptyBodyHash without hashing. Retained (alongside Sha256Hex) so the vector
// tests can assert the entry→digest path directly.
func ContentHashHex(entries [][2]string) string {
	if len(entries) == 0 {
		return EmptyBodyHash
	}
	return Sha256Hex(EncodeBody(entries))
}

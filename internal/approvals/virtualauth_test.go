package approvals

// A test-only virtual WebAuthn authenticator (ES256, "none" attestation).
// It exists only in _test.go files, so it is never compiled into the
// binary and can never serve as a software approval fallback.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
)

type vAuth struct {
	key        *ecdsa.PrivateKey
	credID     []byte
	count      uint32
	userHandle []byte
	// Flags to report; both default to true.
	noUP, noUV bool
	// origin overrides the clientData origin (default testOrigin).
	origin string
	// rpID overrides the RP ID hashed into authenticator data.
	rpID string
}

const testOrigin = "http://localhost:4318"

func newVAuth(t *testing.T) *vAuth {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	rand.Read(id)
	return &vAuth{key: k, credID: id}
}

var b64 = base64.RawURLEncoding

func (a *vAuth) flags(extra byte) byte {
	var f byte = extra
	if !a.noUP {
		f |= 0x01
	}
	if !a.noUV {
		f |= 0x04
	}
	return f
}

func (a *vAuth) authData(extra byte, attested []byte) []byte {
	rp := a.rpID
	if rp == "" {
		rp = RPID
	}
	h := sha256.Sum256([]byte(rp))
	a.count++
	out := append([]byte(nil), h[:]...)
	out = append(out, a.flags(extra))
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], a.count)
	out = append(out, c[:]...)
	return append(out, attested...)
}

func (a *vAuth) clientData(t *testing.T, typ string, challenge []byte) []byte {
	t.Helper()
	o := a.origin
	if o == "" {
		o = testOrigin
	}
	b, err := json.Marshal(map[string]any{"type": typ, "challenge": b64.EncodeToString(challenge), "origin": o, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (a *vAuth) coseKey(t *testing.T) []byte {
	t.Helper()
	x := a.key.PublicKey.X.FillBytes(make([]byte, 32))
	y := a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	b, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// register answers navigator.credentials.create.
func (a *vAuth) register(t *testing.T, c *protocol.CredentialCreation) []byte {
	t.Helper()
	switch id := c.Response.User.ID.(type) {
	case protocol.URLEncodedBase64:
		a.userHandle = []byte(id)
	case string: // decoded from JSON, as a browser sees it
		h, err := b64.DecodeString(id)
		if err != nil {
			t.Fatal(err)
		}
		a.userHandle = h
	}
	att := make([]byte, 16) // zero AAGUID
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(a.credID)))
	att = append(att, l[:]...)
	att = append(att, a.credID...)
	att = append(att, a.coseKey(t)...)
	ad := a.authData(0x40, att)
	obj, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	if err != nil {
		t.Fatal(err)
	}
	cd := a.clientData(t, "webauthn.create", c.Response.Challenge)
	out, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(a.credID), "rawId": b64.EncodeToString(a.credID), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(cd), "attestationObject": b64.EncodeToString(obj)},
	})
	return out
}

// assert answers navigator.credentials.get.
func (a *vAuth) assert(t *testing.T, opts *protocol.CredentialAssertion) []byte {
	t.Helper()
	return a.assertChallenge(t, opts.Response.Challenge)
}

func (a *vAuth) assertChallenge(t *testing.T, challenge []byte) []byte {
	t.Helper()
	ad := a.authData(0, nil)
	cd := a.clientData(t, "webauthn.get", challenge)
	ch := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte(nil), ad...), ch[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	resp := map[string]any{"clientDataJSON": b64.EncodeToString(cd), "authenticatorData": b64.EncodeToString(ad), "signature": b64.EncodeToString(sig)}
	if a.userHandle != nil {
		resp["userHandle"] = b64.EncodeToString(a.userHandle)
	}
	out, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(a.credID), "rawId": b64.EncodeToString(a.credID), "type": "public-key", "response": resp,
	})
	return out
}

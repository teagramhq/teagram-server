package discovery_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/discovery"
)

func TestDocumentIsCanonicalAndContainsOnlyPublicIdentity(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc, err := discovery.NewDocument("mtproto.example.com:443", 2, &key.PublicKey, false)
	if err != nil {
		t.Fatalf("new document: %v", err)
	}
	first, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	second, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document a second time: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("document is not deterministic:\n%s\n%s", first, second)
	}
	if string(first) != `{"version":1,"mtproto":{"endpoint":"mtproto.example.com:443","dc_id":2,"rsa_spki":"`+doc.MTProto.RSASPKI+`"},"websocket":{"endpoint":"wss://mtproto.example.com/apiws"}}` {
		t.Fatalf("document JSON = %s", first)
	}
	if bytes.Contains(first, []byte("private")) {
		t.Fatalf("document contains private-key material: %s", first)
	}
	der, err := base64.StdEncoding.DecodeString(doc.MTProto.RSASPKI)
	if err != nil {
		t.Fatalf("decode SPKI: %v", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatalf("parse SPKI: %v", err)
	}
	if got, ok := parsed.(*rsa.PublicKey); !ok || got.N.Cmp(key.N) != 0 || got.E != key.E {
		t.Fatal("document SPKI does not identify the loaded public key")
	}
}

func TestDocumentAdvertisesDemoGuestSignUpAsLiteralTrue(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc, err := discovery.NewDocument("mtproto.example.com:443", 2, &key.PublicKey, true)
	if err != nil {
		t.Fatalf("new document: %v", err)
	}
	data, err := discovery.MarshalDocument(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	if !bytes.HasSuffix(data, []byte(`,"demo":{"guest_sign_up":true}}`)) {
		t.Fatalf("document does not contain the exact enabled capability: %s", data)
	}
	if doc.MTProto.Endpoint != "mtproto.example.com:443" || doc.MTProto.DCID != 2 || doc.WebSocket.Endpoint != "wss://mtproto.example.com/apiws" {
		t.Fatalf("identity or transport changed when demo capability was enabled: %+v", doc)
	}
}

func TestMarshalDocumentAllowsListenerSpecificWebSocketHost(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc, err := discovery.NewDocument("tailnet.example.test:2443", 2, &key.PublicKey, true)
	if err != nil {
		t.Fatalf("new document: %v", err)
	}
	doc.WebSocket.Endpoint = "wss://server.teagram.io/apiws"
	doc.Turnstile = &discovery.Turnstile{SiteKey: "public-site-key"}

	data, err := discovery.MarshalDocument(doc)
	if err != nil {
		t.Fatalf("marshal listener-scoped document: %v", err)
	}
	var served discovery.Document
	if err := json.Unmarshal(data, &served); err != nil {
		t.Fatalf("decode listener-scoped document: %v", err)
	}
	if served.MTProto.Endpoint != "tailnet.example.test:2443" {
		t.Fatalf("MTProto endpoint = %q, want tailnet.example.test:2443", served.MTProto.Endpoint)
	}
	if served.WebSocket.Endpoint != "wss://server.teagram.io/apiws" {
		t.Fatalf("WebSocket endpoint = %q, want public host", served.WebSocket.Endpoint)
	}
	endpoint, err := url.Parse(served.WebSocket.Endpoint)
	if err != nil {
		t.Fatalf("parse WebSocket endpoint: %v", err)
	}
	enteredHost := "server.teagram.io"
	if endpoint.Hostname() != enteredHost {
		t.Fatalf("WebSocket host = %q, want entered host %q", endpoint.Hostname(), enteredHost)
	}
	if served.Demo == nil || !served.Demo.GuestSignUp || served.Turnstile == nil || served.Turnstile.SiteKey != "public-site-key" {
		t.Fatalf("public discovery capabilities were lost: %+v", served)
	}
}

func TestMarshalDocumentRejectsMalformedWebSocketEndpoints(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc, err := discovery.NewDocument("tailnet.example.test:2443", 2, &key.PublicKey, false)
	if err != nil {
		t.Fatalf("new document: %v", err)
	}
	for _, endpoint := range []string{
		"https://server.teagram.io/apiws",
		"wss://server.teagram.io:443/apiws",
		"wss://server.teagram.io/other",
		"wss://user@server.teagram.io/apiws",
		"wss://server.teagram.io/apiws?host=other.example.test",
	} {
		t.Run(endpoint, func(t *testing.T) {
			doc.WebSocket.Endpoint = endpoint
			if _, err := discovery.MarshalDocument(doc); err == nil {
				t.Fatalf("marshal document accepted malformed WebSocket endpoint %q", endpoint)
			}
		})
	}
}

func TestMarshalDocumentRejectsDisabledDemoCapability(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc, err := discovery.NewDocument("mtproto.example.com:443", 2, &key.PublicKey, false)
	if err != nil {
		t.Fatalf("new document: %v", err)
	}
	doc.Demo = &discovery.DemoCapabilities{}
	if _, err := discovery.MarshalDocument(doc); err == nil {
		t.Fatal("marshal document succeeded with a false demo capability")
	}
}

func TestMarshalDocumentAdvertisesOnlyValidatedTurnstileSiteKey(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc, err := discovery.NewDocument("mtproto.example.com:443", 2, &key.PublicKey, false)
	if err != nil {
		t.Fatalf("new document: %v", err)
	}
	doc.Turnstile = &discovery.Turnstile{SiteKey: "public_site-key123"}
	data, err := discovery.MarshalDocument(doc)
	if err != nil {
		t.Fatalf("marshal document with site key: %v", err)
	}
	if !bytes.HasSuffix(data, []byte(`,"turnstile":{"site_key":"public_site-key123"}}`)) {
		t.Fatalf("document does not contain the public widget key: %s", data)
	}
	for _, invalid := range []string{"", "private key", strings.Repeat("a", 257)} {
		doc.Turnstile = &discovery.Turnstile{SiteKey: invalid}
		if _, err := discovery.MarshalDocument(doc); err == nil {
			t.Fatalf("marshal document succeeded with invalid site key %q", invalid)
		}
	}
}

func TestPreflightRequestHasExactWireShape(t *testing.T) {
	t.Parallel()

	var nonce [discovery.NonceSize]byte
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	request, err := discovery.BuildPreflightRequest(nonce[:])
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	want := append([]byte(discovery.RequestMagic), nonce[:]...)
	if !bytes.Equal(request, want) {
		t.Fatalf("request = %x, want %x", request, want)
	}
}

func TestPreflightResponseHasExactWireShape(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var nonce [discovery.NonceSize]byte
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	response, err := discovery.BuildPreflightResponse(2, &key.PublicKey, nonce[:])
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	if !bytes.Equal(response[:discovery.ResponseMagicSize], []byte(discovery.ResponseMagic)) {
		t.Fatalf("response magic = %q", response[:discovery.ResponseMagicSize])
	}
	if !bytes.Equal(response[discovery.ResponseMagicSize:discovery.ResponseMagicSize+discovery.NonceSize], nonce[:]) {
		t.Fatal("response nonce is not echoed")
	}
	bodyLenOffset := discovery.ResponseMagicSize + discovery.NonceSize
	bodyLen := binary.BigEndian.Uint32(response[bodyLenOffset : bodyLenOffset+4])
	spkiLenOffset := bodyLenOffset + 4 + 4
	spkiLen := binary.BigEndian.Uint16(response[spkiLenOffset : spkiLenOffset+2])
	if int(bodyLen) != 6+int(spkiLen) {
		t.Fatalf("body length = %d, SPKI length = %d", bodyLen, spkiLen)
	}
	if len(response) != spkiLenOffset+2+int(spkiLen) {
		t.Fatalf("response length = %d, want %d", len(response), spkiLenOffset+2+int(spkiLen))
	}
}

func TestParsePreflightResponseRejectsLengthBoundaries(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var nonce [discovery.NonceSize]byte
	response, err := discovery.BuildPreflightResponse(2, &key.PublicKey, nonce[:])
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	bodyLenOffset := discovery.ResponseMagicSize + discovery.NonceSize
	spkiLenOffset := bodyLenOffset + 4 + 4
	bodyLen := binary.BigEndian.Uint32(response[bodyLenOffset : bodyLenOffset+4])
	cases := map[string]func([]byte){
		"body below minimum": func(raw []byte) {
			binary.BigEndian.PutUint32(raw[bodyLenOffset:bodyLenOffset+4], 5)
		},
		"body above maximum": func(raw []byte) {
			binary.BigEndian.PutUint32(raw[bodyLenOffset:bodyLenOffset+4], discovery.MaxSPKILength+7)
		},
		"body truncated": func(raw []byte) {
			binary.BigEndian.PutUint32(raw[bodyLenOffset:bodyLenOffset+4], bodyLen-1)
		},
		"body overlong": func(raw []byte) {
			binary.BigEndian.PutUint32(raw[bodyLenOffset:bodyLenOffset+4], bodyLen+1)
		},
		"SPKI zero": func(raw []byte) {
			binary.BigEndian.PutUint16(raw[spkiLenOffset:spkiLenOffset+2], 0)
		},
		"SPKI above maximum": func(raw []byte) {
			binary.BigEndian.PutUint16(raw[spkiLenOffset:spkiLenOffset+2], discovery.MaxSPKILength+1)
		},
		"trailing bytes":     func(raw []byte) {},
		"truncated response": func(raw []byte) {},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			raw := bytes.Clone(response)
			switch name {
			case "trailing bytes":
				raw = append(raw, 1)
			case "truncated response":
				raw = raw[:len(raw)-1]
			default:
				mutate(raw)
			}
			if _, _, err := discovery.ParsePreflightResponse(raw, nonce[:]); err == nil {
				t.Fatal("malformed response was accepted")
			}
		})
	}
}

package jws

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/notaryproject/notation-go"
	"github.com/notaryproject/notation-go/plugin"
)

var validMetadata = plugin.Metadata{
	Name: "foo", Description: "friendly", Version: "1", URL: "example.com",
	SupportedContractVersions: []string{plugin.ContractVersion},
	Capabilities:              []plugin.Capability{plugin.CapabilitySignatureGenerator},
}

type mockRunner struct {
	resp []interface{}
	err  []error
	n    int
}

func (r *mockRunner) Run(ctx context.Context, req plugin.Request) (interface{}, error) {
	defer func() { r.n++ }()
	return r.resp[r.n], r.err[r.n]
}

type mockSignerPlugin struct {
	KeyID      string
	KeySpec    notation.KeySpec
	Sign       func(payload []byte) []byte
	SigningAlg notation.SignatureAlgorithm
	Cert       []byte
	n          int
}

func (s *mockSignerPlugin) Run(ctx context.Context, req plugin.Request) (interface{}, error) {
	var chain [][]byte
	if len(s.Cert) != 0 {
		chain = append(chain, s.Cert)
	}
	if req != nil {
		// Test json roundtrip.
		jsonReq, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(jsonReq, req)
		if err != nil {
			return nil, err
		}
	}
	defer func() { s.n++ }()
	switch s.n {
	case 0:
		return &validMetadata, nil
	case 1:
		return &plugin.DescribeKeyResponse{KeyID: s.KeyID, KeySpec: s.KeySpec}, nil
	case 2:
		var signed []byte
		if s.Sign != nil {
			signed = s.Sign(req.(*plugin.GenerateSignatureRequest).Payload)
		}
		return &plugin.GenerateSignatureResponse{
			KeyID:            s.KeyID,
			SigningAlgorithm: s.SigningAlg,
			Signature:        signed,
			CertificateChain: chain,
		}, nil
	}
	panic("too many calls")
}

func testSignerError(t *testing.T, signer pluginSigner, wantEr string) {
	t.Helper()
	_, err := signer.Sign(context.Background(), notation.Descriptor{}, notation.SignOptions{})
	if err == nil || !strings.Contains(err.Error(), wantEr) {
		t.Errorf("Signer.Sign() error = %v, wantErr %v", err, wantEr)
	}
}

func TestSigner_Sign_RunMetadataFails(t *testing.T) {
	signer := pluginSigner{
		runner: &mockRunner{[]interface{}{nil}, []error{errors.New("failed")}, 0},
	}
	testSignerError(t, signer, "metadata command failed")
}

func TestSigner_Sign_NoCapability(t *testing.T) {
	m := validMetadata
	m.Capabilities = []plugin.Capability{""}
	signer := pluginSigner{
		runner: &mockRunner{[]interface{}{&m}, []error{nil}, 0},
	}
	testSignerError(t, signer, "does not have signing capabilities")
}

func TestSigner_Sign_DescribeKeyFailed(t *testing.T) {
	signer := pluginSigner{
		runner: &mockRunner{[]interface{}{&validMetadata, nil}, []error{nil, errors.New("failed")}, 0},
	}
	testSignerError(t, signer, "describe-key command failed")
}

func TestSigner_Sign_DescribeKeyKeyIDMismatch(t *testing.T) {
	signer := pluginSigner{
		runner: &mockSignerPlugin{KeyID: "2", KeySpec: notation.RSA_2048},
		keyID:  "1",
	}
	testSignerError(t, signer, "keyID in describeKey response \"2\" does not match request \"1\"")
}

func TestSigner_Sign_KeySpecNotSupported(t *testing.T) {
	signer := pluginSigner{
		runner: &mockSignerPlugin{KeyID: "1", KeySpec: "custom"},
		keyID:  "1",
	}
	testSignerError(t, signer, "keySpec \"custom\" for key \"1\" is not supported")
}

func TestSigner_Sign_PayloadNotValid(t *testing.T) {
	signer := pluginSigner{
		runner: &mockRunner{[]interface{}{
			&validMetadata,
			&plugin.DescribeKeyResponse{KeyID: "1", KeySpec: notation.RSA_2048},
		}, []error{nil, nil}, 0},
		keyID: "1",
	}
	_, err := signer.Sign(context.Background(), notation.Descriptor{}, notation.SignOptions{Expiry: time.Now().Add(-100)})
	wantEr := "token is expired"
	if err == nil || !strings.Contains(err.Error(), wantEr) {
		t.Errorf("Signer.Sign() error = %v, wantErr %v", err, wantEr)
	}
}

func TestSigner_Sign_GenerateSignatureKeyIDMismatch(t *testing.T) {
	signer := pluginSigner{
		runner: &mockRunner{[]interface{}{
			&validMetadata,
			&plugin.DescribeKeyResponse{KeyID: "1", KeySpec: notation.RSA_2048},
			&plugin.GenerateSignatureResponse{KeyID: "2"},
		}, []error{nil, nil, nil}, 0},
		keyID: "1",
	}
	testSignerError(t, signer, "keyID in generateSignature response \"2\" does not match request \"1\"")
}

func TestSigner_Sign_UnsuportedAlgorithm(t *testing.T) {
	signer := pluginSigner{
		runner: &mockSignerPlugin{KeyID: "1", KeySpec: notation.RSA_2048, SigningAlg: "custom"},
		keyID:  "1",
	}
	testSignerError(t, signer, "signing algorithm \"custom\" in generateSignature response is not supported")
}

func TestSigner_Sign_NoCertChain(t *testing.T) {
	signer := pluginSigner{
		runner: &mockSignerPlugin{
			KeyID:      "1",
			KeySpec:    notation.RSA_2048,
			SigningAlg: notation.RSASSA_PSS_SHA_256,
		},
		keyID: "1",
	}
	testSignerError(t, signer, "empty certificate chain")
}

func TestSigner_Sign_MalformedCert(t *testing.T) {
	signer := pluginSigner{
		runner: &mockSignerPlugin{
			KeyID:      "1",
			KeySpec:    notation.RSA_2048,
			SigningAlg: notation.RSASSA_PSS_SHA_256,
			Cert:       []byte("mocked"),
		},
		keyID: "1",
	}
	testSignerError(t, signer, "x509: malformed certificate")
}

func TestSigner_Sign_SignatureVerifyError(t *testing.T) {
	_, cert, err := generateKeyCertPair()
	if err != nil {
		t.Fatalf("generateKeyCertPair() error = %v", err)
	}
	signer := pluginSigner{
		runner: &mockSignerPlugin{
			KeyID:      "1",
			KeySpec:    notation.RSA_2048,
			SigningAlg: notation.RSASSA_PSS_SHA_256,
			Sign:       func(payload []byte) []byte { return []byte("r a w") },
			Cert:       cert.Raw,
		},
		keyID: "1",
	}
	testSignerError(t, signer, "verification error")
}

func validSign(t *testing.T, key interface{}) func([]byte) []byte {
	t.Helper()
	return func(payload []byte) []byte {
		signed, err := jwt.SigningMethodPS256.Sign(string(payload), key)
		if err != nil {
			t.Fatal(err)
		}
		encSigned, err := base64.RawURLEncoding.DecodeString(signed)
		if err != nil {
			t.Fatal(err)
		}
		return encSigned
	}
}

func TestSigner_Sign_CertWithoutDigitalSignatureBit(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(0),
		Subject:               pkix.Name{CommonName: "test"},
		KeyUsage:              x509.KeyUsageEncipherOnly,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	signer := pluginSigner{
		runner: &mockSignerPlugin{
			KeyID:      "1",
			KeySpec:    notation.RSA_2048,
			SigningAlg: notation.RSASSA_PSS_SHA_256,
			Sign:       validSign(t, key),
			Cert:       certBytes,
		},
		keyID: "1",
	}
	testSignerError(t, signer, "keyUsage must have the bit positions for digitalSignature set")
}

func TestSigner_Sign_CertWithout_idkpcodeSigning(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(0),
		Subject:               pkix.Name{CommonName: "test"},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	signer := pluginSigner{
		runner: &mockSignerPlugin{
			KeyID:      "1",
			KeySpec:    notation.RSA_2048,
			SigningAlg: notation.RSASSA_PSS_SHA_256,
			Sign:       validSign(t, key),
			Cert:       certBytes,
		},
		keyID: "1",
	}
	testSignerError(t, signer, "extKeyUsage must contain")
}

func TestSigner_Sign_CertBasicConstraintCA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(0),
		Subject:               pkix.Name{CommonName: "test"},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	signer := pluginSigner{
		runner: &mockSignerPlugin{
			KeyID:      "1",
			KeySpec:    notation.RSA_2048,
			SigningAlg: notation.RSASSA_PSS_SHA_256,
			Sign:       validSign(t, key),
			Cert:       certBytes,
		},
		keyID: "1",
	}
	testSignerError(t, signer, "if the basicConstraints extension is present, the CA field MUST be set false")
}

func TestSigner_Sign_Valid(t *testing.T) {
	key, cert, err := generateKeyCertPair()
	if err != nil {
		t.Fatal(err)
	}
	signer := pluginSigner{
		runner: &mockSignerPlugin{
			KeyID:      "1",
			KeySpec:    notation.RSA_2048,
			SigningAlg: notation.RSASSA_PSS_SHA_256,
			Sign:       validSign(t, key),
			Cert:       cert.Raw,
		},
		keyID: "1",
	}
	data, err := signer.Sign(context.Background(), notation.Descriptor{}, notation.SignOptions{})
	if err != nil {
		t.Errorf("Signer.Sign() error = %v, wantErr nil", err)
	}
	var got notation.JWSEnvelope
	err = json.Unmarshal(data, &got)
	if err != nil {
		t.Fatal(err)
	}
	want := notation.JWSEnvelope{
		Protected: "eyJhbGciOiJQUzI1NiIsImN0eSI6ImFwcGxpY2F0aW9uL3ZuZC5jbmNmLm5vdGFyeS5wYXlsb2FkLnYxK2pzb24ifQ",
		Header: notation.JWSUnprotectedHeader{
			CertChain: [][]byte{cert.Raw},
		},
	}
	if got.Protected != want.Protected {
		t.Errorf("Signer.Sign() Protected %v, want %v", got.Protected, want.Protected)
	}
	if _, err = base64.RawURLEncoding.DecodeString(got.Signature); err != nil {
		t.Errorf("Signer.Sign() Signature %v is not encoded as Base64URL", got.Signature)
	}
	if !reflect.DeepEqual(got.Header, want.Header) {
		t.Errorf("Signer.Sign() Header %v, want %v", got.Header, want.Header)
	}
	v := NewVerifier()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	v.VerifyOptions.Roots = roots
	if _, err := v.Verify(context.Background(), data, notation.VerifyOptions{}); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}

type mockEnvelopePlugin struct {
	err          error
	envelopeType string
	certChain    [][]byte
	key          interface{}
}

func (s *mockEnvelopePlugin) Run(ctx context.Context, req plugin.Request) (interface{}, error) {
	switch req.Command() {
	case plugin.CommandGetMetadata:
		m := validMetadata
		m.Capabilities[0] = plugin.CapabilityEnvelopeGenerator
		return &m, nil
	case plugin.CommandGenerateEnvelope:
		if s.err != nil {
			return nil, s.err
		}
		key := s.key
		tmpkey, cert, err := generateKeyCertPair()
		if err != nil {
			return nil, err
		}
		if key == nil {
			key = tmpkey
		}
		keySpec, err := keySpecFromKey(key)
		if err != nil {
			return nil, err
		}
		alg := keySpec.SignatureAlgorithm().JWS()
		req1 := req.(*plugin.GenerateEnvelopeRequest)
		t := &jwt.Token{
			Method: jwt.GetSigningMethod(alg),
			Header: map[string]interface{}{
				"alg": alg,
				"cty": notation.MediaTypePayload,
			},
			Claims: struct {
				jwt.RegisteredClaims
				Subject json.RawMessage `json:"subject"`
			}{
				RegisteredClaims: jwt.RegisteredClaims{
					IssuedAt: jwt.NewNumericDate(time.Now()),
				},
				Subject: req1.Payload,
			},
		}
		signed, err := t.SignedString(key)
		if err != nil {
			return nil, err
		}
		parts := strings.Split(signed, ".")
		if len(parts) != 3 {
			return nil, errors.New("invalid compact serialization")
		}
		envelope := notation.JWSEnvelope{
			Protected: parts[0],
			Payload:   parts[1],
			Signature: parts[2],
		}
		if s.certChain != nil {
			// Override cert chain.
			envelope.Header.CertChain = s.certChain
		} else {
			envelope.Header.CertChain = [][]byte{cert.Raw}
		}
		data, err := json.Marshal(envelope)
		if err != nil {
			return nil, err
		}
		envType := s.envelopeType
		if envType == "" {
			envType = req1.SignatureEnvelopeType
		}
		return &plugin.GenerateEnvelopeResponse{
			SignatureEnvelope:     data,
			SignatureEnvelopeType: envType,
		}, nil
	}
	panic("too many calls")
}
func TestPluginSigner_SignEnvelope_RunFailed(t *testing.T) {
	signer := pluginSigner{
		runner: &mockEnvelopePlugin{err: errors.New("failed")},
		keyID:  "1",
	}
	_, err := signer.Sign(context.Background(), notation.Descriptor{
		MediaType: notation.MediaTypePayload,
		Size:      1,
	}, notation.SignOptions{})
	if err == nil || err.Error() != "generate-envelope command failed: failed" {
		t.Errorf("Signer.Sign() error = %v, wantErr nil", err)
	}
}

func TestPluginSigner_SignEnvelope_InvalidEnvelopeType(t *testing.T) {
	signer := pluginSigner{
		runner: &mockEnvelopePlugin{envelopeType: "other"},
		keyID:  "1",
	}
	_, err := signer.Sign(context.Background(), notation.Descriptor{
		MediaType: notation.MediaTypePayload,
		Size:      1,
	}, notation.SignOptions{})
	if err == nil || err.Error() != "signatureEnvelopeType in generateEnvelope response \"other\" does not match request \"application/vnd.cncf.notary.v2.jws.v1\"" {
		t.Errorf("Signer.Sign() error = %v, wantErr nil", err)
	}
}

func TestPluginSigner_SignEnvelope_EmptyCert(t *testing.T) {
	signer := pluginSigner{
		runner: &mockEnvelopePlugin{certChain: make([][]byte, 0)},
		keyID:  "1",
	}
	_, err := signer.Sign(context.Background(), notation.Descriptor{
		MediaType: notation.MediaTypePayload,
		Size:      1,
	}, notation.SignOptions{})
	if err == nil || err.Error() != "envelope content does not match envelope format" {
		t.Errorf("Signer.Sign() error = %v, wantErr nil", err)
	}
}

func TestPluginSigner_SignEnvelope_MalformedCertChain(t *testing.T) {
	signer := pluginSigner{
		runner: &mockEnvelopePlugin{certChain: [][]byte{make([]byte, 0)}},
		keyID:  "1",
	}
	_, err := signer.Sign(context.Background(), notation.Descriptor{
		MediaType: notation.MediaTypePayload,
		Size:      1,
	}, notation.SignOptions{})
	if err == nil || err.Error() != "x509: malformed certificate" {
		t.Errorf("Signer.Sign() error = %v, wantErr nil", err)
	}
}

func TestPluginSigner_SignEnvelope_CertBasicConstraintCA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(0),
		Subject:               pkix.Name{CommonName: "test"},
		KeyUsage:              x509.KeyUsageEncipherOnly,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	signer := pluginSigner{
		runner: &mockEnvelopePlugin{
			key:       key,
			certChain: [][]byte{certBytes},
		},
		keyID: "1",
	}
	_, err = signer.Sign(context.Background(), notation.Descriptor{
		MediaType: notation.MediaTypePayload,
		Size:      1,
	}, notation.SignOptions{})
	if err == nil || err.Error() != "signing certificate does not meet the minimum requirements: keyUsage must have the bit positions for digitalSignature set" {
		t.Errorf("Signer.Sign() error = %v, wantErr nil", err)
	}
}

func TestPluginSigner_SignEnvelope_SignatureVerifyError(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := pluginSigner{
		runner: &mockEnvelopePlugin{key: key},
		keyID:  "1",
	}
	_, err = signer.Sign(context.Background(), notation.Descriptor{
		MediaType: notation.MediaTypePayload,
		Size:      1,
	}, notation.SignOptions{})
	if err == nil || err.Error() != "crypto/rsa: verification error" {
		t.Errorf("Signer.Sign() error = %v, wantErr nil", err)
	}
}

func TestPluginSigner_SignEnvelope_Valid(t *testing.T) {
	signer := pluginSigner{
		runner: &mockEnvelopePlugin{},
		keyID:  "1",
	}
	_, err := signer.Sign(context.Background(), notation.Descriptor{
		MediaType: notation.MediaTypePayload,
		Size:      1,
	}, notation.SignOptions{})
	if err != nil {
		t.Errorf("Signer.Sign() error = %v, wantErr nil", err)
	}
}

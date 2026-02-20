package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	v1 "cryptoledger/gen/ledger/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"testing"
)

func TestAuditActorComesFromVerifiedCertificate(t *testing.T) {
	req := &v1.PostTransactionRequest{ActorId: "caller-supplied"}
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "authenticated-client"}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}}}})
	called := false
	handler := func(_ context.Context, r any) (any, error) {
		called = true
		if got := r.(*v1.PostTransactionRequest).ActorId; got != "authenticated-client" {
			t.Fatalf("actor %s", got)
		}
		return nil, nil
	}
	if _, err := Identity(ctx, req, nil, handler); err != nil || !called {
		t.Fatalf("identity: %v", err)
	}
	if req.ActorId != "caller-supplied" {
		t.Fatal("input request was mutated")
	}
	called = false
	_, err := Identity(context.Background(), req, nil, handler)
	if status.Code(err) != codes.Unauthenticated || called {
		t.Fatal("missing certificate accepted")
	}
}

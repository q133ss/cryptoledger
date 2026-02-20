package mtls

import (
	"context"
	v1 "cryptoledger/gen/ledger/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Identity binds posted audit records to the certificate verified by TLS.
func Identity(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	post, ok := req.(*v1.PostTransactionRequest)
	if !ok {
		return handler(ctx, req)
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return nil, status.Error(codes.Unauthenticated, "verified certificate required")
	}
	leaf := tlsInfo.State.VerifiedChains[0][0]
	actor := leaf.Subject.CommonName
	if len(leaf.URIs) > 0 {
		actor = leaf.URIs[0].String()
	}
	if actor == "" {
		return nil, status.Error(codes.Unauthenticated, "empty certificate identity")
	}
	authenticated := proto.Clone(post).(*v1.PostTransactionRequest)
	authenticated.ActorId = actor
	return handler(ctx, authenticated)
}

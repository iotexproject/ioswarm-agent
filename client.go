package main

import (
	"context"
	"crypto/tls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// agentCred implements grpc.PerRPCCredentials.
// It sends both x-ioswarm-agent-id and x-ioswarm-token in every RPC.
type agentCred struct {
	agentID    string
	token      string
	requireTLS bool
}

func (c *agentCred) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"x-ioswarm-agent-id": c.agentID,
		"x-ioswarm-token":    c.token,
	}, nil
}

func (c *agentCred) RequireTransportSecurity() bool {
	return c.requireTLS
}

// dialCoordinator creates a gRPC connection to the coordinator.
// If apiKey and agentID are provided, HMAC auth metadata is attached to every call.
func dialCoordinator(addr, agentID, apiKey string, tlsCert string, useTLS bool) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	if tlsCert != "" {
		creds, err := credentials.NewClientTLSFromFile(tlsCert, "")
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else if useTLS {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	if apiKey != "" && agentID != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(&agentCred{
			agentID:    agentID,
			token:      apiKey,
			requireTLS: useTLS || tlsCert != "",
		}))
	}

	// State diff messages can exceed gRPC's default 4MB limit for blocks with
	// large state changes (contract deployments, batch operations).
	opts = append(opts, grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(32*1024*1024),
		grpc.MaxCallSendMsgSize(32*1024*1024),
	))

	return grpc.NewClient(addr, opts...)
}

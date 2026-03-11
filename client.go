package main

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// agentCred implements grpc.PerRPCCredentials.
// It sends both x-ioswarm-agent-id and x-ioswarm-token in every RPC.
type agentCred struct {
	agentID string
	token   string
}

func (c *agentCred) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"x-ioswarm-agent-id": c.agentID,
		"x-ioswarm-token":    c.token,
	}, nil
}

func (c *agentCred) RequireTransportSecurity() bool {
	return false
}

// dialCoordinator creates a gRPC connection to the coordinator.
// If apiKey and agentID are provided, HMAC auth metadata is attached to every call.
func dialCoordinator(addr, agentID, apiKey string, tlsCert string) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	if tlsCert != "" {
		creds, err := credentials.NewClientTLSFromFile(tlsCert, "")
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	if apiKey != "" && agentID != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(&agentCred{
			agentID: agentID,
			token:   apiKey,
		}))
	}

	return grpc.NewClient(addr, opts...)
}

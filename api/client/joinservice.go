/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"context"
	"errors"
	"io"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/types"
)

// JoinServiceClient is a client for the JoinService, which runs on both the
// auth and proxy.
type JoinServiceClient struct {
	grpcClient proto.JoinServiceClient
}

// NewJoinServiceClient returns a new JoinServiceClient wrapping the given grpc
// client.
func NewJoinServiceClient(grpcClient proto.JoinServiceClient) *JoinServiceClient {
	return &JoinServiceClient{
		grpcClient: grpcClient,
	}
}

// RegisterIAMChallengeResponseFunc is a function type meant to be passed to
// RegisterUsingIAMMethod. It must return a *proto.RegisterUsingIAMMethodRequest
// for a given challenge, or an error.
type RegisterIAMChallengeResponseFunc func(challenge string) (*proto.RegisterUsingIAMMethodRequest, error)

// RegisterAzureChallengeResponseFunc is a function type meant to be passed to
// RegisterUsingAzureMethod. It must return a
// *proto.RegisterUsingAzureMethodRequest for a given challenge, or an error.
type RegisterAzureChallengeResponseFunc func(challenge string) (*proto.RegisterUsingAzureMethodRequest, error)

// RegisterTPMChallengeResponseFunc is a function type meant to be passed to
// RegisterUsingTPMMethod. It must return a
// *proto.RegisterUsingTPMMethodChallengeResponse for a given challenge, or an
// error.
type RegisterTPMChallengeResponseFunc func(challenge *proto.TPMEncryptedCredential) (*proto.RegisterUsingTPMMethodChallengeResponse, error)

// RegisterOracleChallengeResponseFunc is a function type meant to be passed to
// RegisterUsingOracleMethod: It must return a
// *proto.OracleSignedRequest for a given challenge, or an error.
type RegisterOracleChallengeResponseFunc func(challenge string) (*proto.OracleSignedRequest, error)

// RegisterUsingBoundKeypairChallengeResponseFunc is a function to be passed to
// RegisterUsingBoundKeypair. It must return a new follow-up request for the
// server response, or an error.
type RegisterUsingBoundKeypairChallengeResponseFunc func(challenge *proto.RegisterUsingBoundKeypairMethodResponse) (*proto.RegisterUsingBoundKeypairMethodRequest, error)

// RegisterUsingIAMMethod registers the caller using the IAM join method and
// returns signed certs to join the cluster.
//
// The caller must provide a ChallengeResponseFunc which returns a
// *types.RegisterUsingTokenRequest with a signed sts:GetCallerIdentity request
// including the challenge as a signed header.
func (c *JoinServiceClient) RegisterUsingIAMMethod(ctx context.Context, challengeResponse RegisterIAMChallengeResponseFunc) (*proto.Certs, error) {
	// Make sure the gRPC stream is closed when this returns
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// initiate the streaming rpc
	iamJoinClient, err := c.grpcClient.RegisterUsingIAMMethod(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// wait for the challenge string from auth
	challenge, err := iamJoinClient.Recv()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// get challenge response from the caller
	req, err := challengeResponse(challenge.Challenge)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// forward the challenge response from the caller to auth
	if err := iamJoinClient.Send(req); err != nil {
		return nil, trace.Wrap(err)
	}

	// wait for the certs from auth and return to the caller
	certsResp, err := iamJoinClient.Recv()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return certsResp.Certs, nil
}

// RegisterUsingAzureMethod registers the caller using the Azure join method and
// returns signed certs to join the cluster.
//
// The caller must provide a ChallengeResponseFunc which returns a
// *proto.RegisterUsingAzureMethodRequest with a signed attested data document
// including the challenge as a nonce.
func (c *JoinServiceClient) RegisterUsingAzureMethod(ctx context.Context, challengeResponse RegisterAzureChallengeResponseFunc) (*proto.Certs, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	azureJoinClient, err := c.grpcClient.RegisterUsingAzureMethod(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	challenge, err := azureJoinClient.Recv()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	req, err := challengeResponse(challenge.Challenge)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if err := azureJoinClient.Send(req); err != nil {
		return nil, trace.Wrap(err)
	}

	certsResp, err := azureJoinClient.Recv()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return certsResp.Certs, nil
}

// RegisterUsingTPMMethod registers the caller using the TPM join method and
// returns signed certs to join the cluster. The caller must provide a
// ChallengeResponseFunc which returns a *proto.RegisterUsingTPMMethodRequest
// for a given challenge, or an error.
func (c *JoinServiceClient) RegisterUsingTPMMethod(
	ctx context.Context,
	initReq *proto.RegisterUsingTPMMethodInitialRequest,
	solveChallenge RegisterTPMChallengeResponseFunc,
) (*proto.Certs, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := c.grpcClient.RegisterUsingTPMMethod(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer stream.CloseSend()

	err = stream.Send(&proto.RegisterUsingTPMMethodRequest{
		Payload: &proto.RegisterUsingTPMMethodRequest_Init{
			Init: initReq,
		},
	})
	if err != nil {
		return nil, trace.Wrap(err, "sending initial request")
	}

	res, err := stream.Recv()
	if err != nil {
		return nil, trace.Wrap(err, "receiving challenge")
	}

	challenge := res.GetChallengeRequest()
	if challenge == nil {
		return nil, trace.BadParameter(
			"expected ChallengeRequest payload, got %T",
			res.Payload,
		)
	}

	solution, err := solveChallenge(challenge)
	if err != nil {
		return nil, trace.Wrap(err, "solving challenge")
	}

	err = stream.Send(&proto.RegisterUsingTPMMethodRequest{
		Payload: &proto.RegisterUsingTPMMethodRequest_ChallengeResponse{
			ChallengeResponse: solution,
		},
	})
	if err != nil {
		return nil, trace.Wrap(err, "sending solution")
	}

	res, err = stream.Recv()
	if err != nil {
		return nil, trace.Wrap(err, "receiving certs")
	}
	certs := res.GetCerts()
	if certs == nil {
		return nil, trace.BadParameter(
			"expected Certs payload, got %T",
			res.Payload,
		)
	}

	return certs, nil
}

// RegisterUsingOracleMethod registers the caller using the Oracle join method and
// returns signed certs to join the cluster. The caller must provide a
// ChallengeResponseFunc which returns a *proto.OracleSignedRequest
// for a given challenge, or an error.
func (c *JoinServiceClient) RegisterUsingOracleMethod(
	ctx context.Context,
	tokenReq *types.RegisterUsingTokenRequest,
	oracleRequestFromChallenge RegisterOracleChallengeResponseFunc,
) (*proto.Certs, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	oracleJoinClient, err := c.grpcClient.RegisterUsingOracleMethod(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := oracleJoinClient.Send(&proto.RegisterUsingOracleMethodRequest{
		Request: &proto.RegisterUsingOracleMethodRequest_RegisterUsingTokenRequest{
			RegisterUsingTokenRequest: tokenReq,
		},
	}); err != nil {
		return nil, trace.Wrap(err)
	}

	challengeResp, err := oracleJoinClient.Recv()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	challenge := challengeResp.GetChallenge()
	if challenge == "" {
		return nil, trace.BadParameter("missing challenge")
	}
	oracleSignedReq, err := oracleRequestFromChallenge(challenge)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := oracleJoinClient.Send(&proto.RegisterUsingOracleMethodRequest{
		Request: &proto.RegisterUsingOracleMethodRequest_OracleRequest{
			OracleRequest: oracleSignedReq,
		},
	}); err != nil {
		return nil, trace.Wrap(err)
	}

	certsResp, err := oracleJoinClient.Recv()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	certs := certsResp.GetCerts()
	if certs == nil {
		return nil, trace.BadParameter("expected certificate response, got %T", certsResp.Response)
	}
	return certs, nil
}

// BoundKeypairRegistrationResponse is the response on a successful registration attempt.
type BoundKeypairRegistrationResponse struct {
	// Certs is the generated certificate bundle.
	Certs *proto.Certs

	// BoundPublicKey is the public key bound at the completion of the joining
	// process, in ssh authorized_hosts format.
	BoundPublicKey string

	// JoinState is a compact serialized JWT containing join state, to be stored
	// by the client and verified on subsequent join attempts.
	JoinState []byte
}

// RegisterUsingBoundKeypairMethod attempts to register the caller using
// bound-keypair join method. If successful, the public key registered with auth
// and a certificate bundle is returned, or an error. Clients must provide a
// callback to handle interactive challenges and keypair rotation requests.
func (c *JoinServiceClient) RegisterUsingBoundKeypairMethod(
	ctx context.Context,
	initReq *proto.RegisterUsingBoundKeypairInitialRequest,
	challengeFunc RegisterUsingBoundKeypairChallengeResponseFunc,
) (*BoundKeypairRegistrationResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := c.grpcClient.RegisterUsingBoundKeypairMethod(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer stream.CloseSend()

	err = stream.Send(&proto.RegisterUsingBoundKeypairMethodRequest{
		Payload: &proto.RegisterUsingBoundKeypairMethodRequest_Init{
			Init: initReq,
		},
	})
	if err != nil {
		return nil, trace.Wrap(err, "sending initial request")
	}

	// Unlike other methods, the server may send multiple challenges,
	// particularly during keypair rotation. We'll iterate through all responses
	// here instead to ensure we handle everything.
	for {
		res, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, trace.Wrap(err, "receiving intermediate bound keypair join response")
		}

		switch kind := res.GetResponse().(type) {
		case *proto.RegisterUsingBoundKeypairMethodResponse_Certs:
			// If we get certs, we're done, so just return the result.
			certs := kind.Certs.GetCerts()
			if certs == nil {
				return nil, trace.BadParameter("expected Certs, got %T", kind.Certs.Certs)
			}

			// If we receive a cert bundle, we can return early. Even if we
			// logically should have expected to receive a 2nd challenge if we
			// e.g. requested keypair rotation, skipping it just means the new
			// keypair won't be stored. That said, we'll rely on the server to
			// raise an error if rotation fails or is otherwise skipped or not
			// allowed.

			return &BoundKeypairRegistrationResponse{
				Certs:          certs,
				BoundPublicKey: kind.Certs.GetPublicKey(),
				JoinState:      kind.Certs.JoinState,
			}, nil
		default:
			// Forward all other responses to the challenge handler.
			nextRequest, err := challengeFunc(res)
			if err != nil {
				return nil, trace.Wrap(err, "solving challenge")
			}

			if err := stream.Send(nextRequest); err != nil {
				return nil, trace.Wrap(err, "sending solution")
			}
		}
	}

	// Ideally the server will emit a proper error instead of just hanging up on
	// us.
	return nil, trace.AccessDenied("server declined to send certs during bound-keypair join attempt")
}

// RegisterUsingToken registers the caller using a token and returns signed
// certs.
// This is used where a more specific RPC has not been introduced for the join
// method.
func (c *JoinServiceClient) RegisterUsingToken(
	ctx context.Context, req *types.RegisterUsingTokenRequest,
) (*proto.Certs, error) {
	return c.grpcClient.RegisterUsingToken(ctx, req)
}

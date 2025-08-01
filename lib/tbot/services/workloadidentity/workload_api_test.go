// Teleport
// Copyright (C) 2025 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package workloadidentity

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	headerv1 "github.com/gravitational/teleport/api/gen/proto/go/teleport/header/v1"
	workloadidentityv1pb "github.com/gravitational/teleport/api/gen/proto/go/teleport/workloadidentity/v1"
	"github.com/gravitational/teleport/api/types"
	apiutils "github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/lib/tbot/bot"
	"github.com/gravitational/teleport/lib/tbot/bot/connection"
	"github.com/gravitational/teleport/lib/tbot/workloadidentity"
	"github.com/gravitational/teleport/lib/utils/log/logtest"
	"github.com/gravitational/teleport/tool/teleport/testenv"
)

func TestBotWorkloadIdentityAPI(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	log := logtest.NewLogger()
	process := testenv.MakeTestServer(t, defaultTestServerOpts(t, log))
	setWorkloadIdentityX509CAOverride(ctx, t, process)
	rootClient := testenv.MakeDefaultAuthClient(t, process)

	role, err := types.NewRole("issue-foo", types.RoleSpecV6{
		Allow: types.RoleConditions{
			WorkloadIdentityLabels: map[string]apiutils.Strings{
				"foo": []string{"bar"},
			},
			Rules: []types.Rule{
				{
					Resources: []string{types.KindWorkloadIdentity},
					Verbs:     []string{types.VerbRead, types.VerbList},
				},
			},
		},
	})
	require.NoError(t, err)
	role, err = rootClient.UpsertRole(ctx, role)
	require.NoError(t, err)

	workloadIdentity := &workloadidentityv1pb.WorkloadIdentity{
		Kind:    types.KindWorkloadIdentity,
		Version: types.V1,
		Metadata: &headerv1.Metadata{
			Name: "foo-bar-bizz",
			Labels: map[string]string{
				"foo": "bar",
			},
		},
		Spec: &workloadidentityv1pb.WorkloadIdentitySpec{
			Spiffe: &workloadidentityv1pb.WorkloadIdentitySPIFFE{
				Id: "/valid/{{ user.bot_name }}/{{ workload.unix.pid }}",
			},
		},
	}
	workloadIdentity, err = rootClient.WorkloadIdentityResourceServiceClient().
		CreateWorkloadIdentity(ctx, &workloadidentityv1pb.CreateWorkloadIdentityRequest{
			WorkloadIdentity: workloadIdentity,
		})
	require.NoError(t, err)

	tmpDir := t.TempDir()
	listenAddr := url.URL{
		Scheme: "unix",
		Path:   filepath.Join(tmpDir, "workload.sock"),
	}
	onboarding, _ := makeBot(t, rootClient, "api", role.GetName())

	authAddr, err := process.AuthAddr()
	require.NoError(t, err)

	connCfg := connection.Config{
		Address:     authAddr.Addr,
		AddressKind: connection.AddressKindAuth,
		Insecure:    true,
	}
	require.NoError(t, err)

	trustBundleCache := workloadidentity.NewTrustBundleCacheFacade()
	crlCache := workloadidentity.NewCRLCacheFacade()

	b, err := bot.New(bot.Config{
		Connection: connCfg,
		Logger:     log,
		Onboarding: *onboarding,
		Services: []bot.ServiceBuilder{
			trustBundleCache.BuildService,
			crlCache.BuildService,
			WorkloadAPIServiceBuilder(
				&WorkloadAPIConfig{
					Selector: bot.WorkloadIdentitySelector{
						Name: workloadIdentity.GetMetadata().GetName(),
					},
					Listen: listenAddr.String(),
				},
				trustBundleCache,
				crlCache,
				bot.DefaultCredentialLifetime,
			),
		},
	})
	require.NoError(t, err)

	// Spin up goroutine for bot to run in
	botCtx, cancelBot := context.WithCancel(ctx)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := b.Run(botCtx)
		assert.NoError(t, err, "bot should not exit with error")
		cancelBot()
	}()
	t.Cleanup(func() {
		// Shut down bot and make sure it exits.
		cancelBot()
		wg.Wait()
	})

	// Wait for the socket to be ready before trying to connect. The SPIFFE SDK
	// and gRPC should handle this for us, but we've seen this test flake in CI.
	require.Eventually(t, func() bool {
		_, err := os.Stat(listenAddr.String())
		return err != nil
	}, 5*time.Second, 100*time.Millisecond, "socket not ready within 5s")

	client, err := workloadapi.New(ctx, workloadapi.WithAddr(listenAddr.String()))
	require.NoError(t, err)

	source, err := workloadapi.NewX509Source(
		ctx,
		workloadapi.WithClient(client),
	)
	require.NoError(t, err)
	defer source.Close()

	// Test FetchX509SVID
	svid, err := source.GetX509SVID()
	require.NoError(t, err)

	expectedSPIFFEID := fmt.Sprintf("spiffe://root/valid/api/%d", os.Getpid())
	require.Equal(t, expectedSPIFFEID, svid.ID.String())
	// the override includes a chain with a single certificate
	require.Len(t, svid.Certificates, 2)
	require.Equal(t, expectedSPIFFEID, svid.Certificates[0].URIs[0].String())
	_, _, err = x509svid.Verify(svid.Certificates, source)
	require.NoError(t, err)

	// Test FetchX509Bundles
	set, err := client.FetchX509Bundles(ctx)
	require.NoError(t, err)
	_, _, err = x509svid.Verify(svid.Certificates, set)
	require.NoError(t, err)

	// Test FetchJWTSVID
	jwtSVID, err := client.FetchJWTSVID(ctx, jwtsvid.Params{
		Audience: "example.com",
	})
	require.NoError(t, err)

	// Check against ValidateJWTSVID
	parsed, err := client.ValidateJWTSVID(ctx, jwtSVID.Marshal(), "example.com")
	require.NoError(t, err)
	require.Equal(t, expectedSPIFFEID, parsed.ID.String())
	// Perform local validation with bundles from FetchJWTBundles
	jwtBundles, err := client.FetchJWTBundles(ctx)
	require.NoError(t, err)
	_, err = jwtsvid.ParseAndValidate(jwtSVID.Marshal(), jwtBundles, []string{"example.com"})
	require.NoError(t, err)

	// Check CRL is delivered - we have to manually craft the client for this
	// since the current go-spiffe SDK doesn't support this.
	// TODO(noah): I'll raise some changes upstream to add CRL field support to
	// the go-spiffe SDK, and then we can remove this code.
	conn, err := grpc.NewClient(
		listenAddr.String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	spiffeWorkloadAPI := workload.NewSpiffeWorkloadAPIClient(conn)
	stream, err := spiffeWorkloadAPI.FetchX509SVID(ctx, &workload.X509SVIDRequest{})
	require.NoError(t, err)

	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Crl, 1)
	crl, err := x509.ParseRevocationList(resp.Crl[0])
	require.NoError(t, err)
	require.Empty(t, crl.RevokedCertificateEntries)
	tb, ok := set.Get(svid.ID.TrustDomain())
	require.True(t, ok)
	require.NoError(t, crl.CheckSignatureFrom(tb.X509Authorities()[0]))
}

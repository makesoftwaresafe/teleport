/*
Copyright 2017 Gravitational, Inc.

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

package local

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/client/proto"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/lite"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services/suite"

	"github.com/gravitational/trace"
)

func TestTrustedClusterCRUD(t *testing.T) {
	ctx := context.Background()

	bk, err := lite.New(ctx, backend.Params{"path": t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bk.Close()) })

	presenceBackend := NewPresenceService(bk)

	tc, err := types.NewTrustedCluster("foo", types.TrustedClusterSpecV2{
		Enabled:              true,
		Roles:                []string{"bar", "baz"},
		Token:                "qux",
		ProxyAddress:         "quux",
		ReverseTunnelAddress: "quuz",
	})
	require.NoError(t, err)

	// we just insert this one for get all
	stc, err := types.NewTrustedCluster("bar", types.TrustedClusterSpecV2{
		Enabled:              false,
		Roles:                []string{"baz", "aux"},
		Token:                "quux",
		ProxyAddress:         "quuz",
		ReverseTunnelAddress: "corge",
	})
	require.NoError(t, err)

	// create trusted clusters
	_, err = presenceBackend.UpsertTrustedCluster(ctx, tc)
	require.NoError(t, err)
	_, err = presenceBackend.UpsertTrustedCluster(ctx, stc)
	require.NoError(t, err)

	// get trusted cluster make sure it's correct
	gotTC, err := presenceBackend.GetTrustedCluster(ctx, "foo")
	require.NoError(t, err)
	require.Equal(t, "foo", gotTC.GetName())
	require.True(t, gotTC.GetEnabled())
	require.EqualValues(t, []string{"bar", "baz"}, gotTC.GetRoles())
	require.Equal(t, "qux", gotTC.GetToken())
	require.Equal(t, "quux", gotTC.GetProxyAddress())
	require.Equal(t, "quuz", gotTC.GetReverseTunnelAddress())

	// get all clusters
	allTC, err := presenceBackend.GetTrustedClusters(ctx)
	require.NoError(t, err)
	require.Len(t, allTC, 2)

	// delete cluster
	err = presenceBackend.DeleteTrustedCluster(ctx, "foo")
	require.NoError(t, err)

	// make sure it's really gone
	_, err = presenceBackend.GetTrustedCluster(ctx, "foo")
	require.Error(t, err)
	require.ErrorIs(t, err, trace.NotFound("key /trustedclusters/foo is not found"))
}

// TestApplicationServersCRUD verifies backend operations on app servers.
func TestApplicationServersCRUD(t *testing.T) {
	ctx := context.Background()
	clock := clockwork.NewFakeClock()

	backend, err := lite.NewWithConfig(ctx, lite.Config{
		Path:  t.TempDir(),
		Clock: clock,
	})
	require.NoError(t, err)

	presence := NewPresenceService(backend)

	// Make an app and an app server.
	appA, err := types.NewAppV3(types.Metadata{Name: "a"},
		types.AppSpecV3{URI: "http://localhost:8080"})
	require.NoError(t, err)
	serverA, err := types.NewAppServerV3(types.Metadata{
		Name: appA.GetName(),
	}, types.AppServerSpecV3{
		Hostname: "localhost",
		HostID:   uuid.New().String(),
		App:      appA,
	})
	require.NoError(t, err)

	// Make a legacy app server.
	appBLegacy := &types.App{Name: "b", URI: "http://localhost:8081"}
	appB, err := types.NewAppV3FromLegacyApp(appBLegacy)
	require.NoError(t, err)
	serverBLegacy, err := types.NewServer(uuid.New().String(), types.KindAppServer,
		types.ServerSpecV2{
			Hostname: "localhost",
			Apps:     []*types.App{appBLegacy},
		})
	require.NoError(t, err)
	serverB, err := types.NewAppServerV3(types.Metadata{
		Name: appBLegacy.Name,
	}, types.AppServerSpecV3{
		Hostname: "localhost",
		HostID:   serverBLegacy.GetName(),
		App:      appB,
	})
	require.NoError(t, err)

	// No app servers should be registered initially
	out, err := presence.GetApplicationServers(ctx, apidefaults.Namespace)
	require.NoError(t, err)
	require.Equal(t, 0, len(out))

	// Create app server.
	lease, err := presence.UpsertApplicationServer(ctx, serverA)
	require.NoError(t, err)
	require.Equal(t, &types.KeepAlive{}, lease)

	// Create legacy app server.
	lease, err = presence.UpsertAppServer(ctx, serverBLegacy)
	require.NoError(t, err)
	require.Equal(t, &types.KeepAlive{}, lease)

	// Make sure all app servers are registered.
	out, err = presence.GetApplicationServers(ctx, serverA.GetNamespace())
	require.NoError(t, err)
	require.Empty(t, cmp.Diff([]types.AppServer{serverA, serverB}, out,
		cmpopts.IgnoreFields(types.Metadata{}, "ID")))

	// Delete app server.
	err = presence.DeleteApplicationServer(ctx, serverA.GetNamespace(), serverA.GetHostID(), serverA.GetName())
	require.NoError(t, err)

	// Expect only the legacy one to be returned.
	out, err = presence.GetApplicationServers(ctx, apidefaults.Namespace)
	require.NoError(t, err)
	require.Empty(t, cmp.Diff([]types.AppServer{serverB}, out,
		cmpopts.IgnoreFields(types.Metadata{}, "ID")))

	// Upsert server with TTL.
	serverA.SetExpiry(clock.Now().UTC().Add(time.Hour))
	lease, err = presence.UpsertApplicationServer(ctx, serverA)
	require.NoError(t, err)
	require.Equal(t, &types.KeepAlive{
		Type:      types.KeepAlive_APP,
		LeaseID:   lease.LeaseID,
		Name:      serverA.GetName(),
		Namespace: serverA.GetNamespace(),
		HostID:    serverA.GetHostID(),
		Expires:   serverA.Expiry(),
	}, lease)

	// Delete all app servers.
	err = presence.DeleteAllApplicationServers(ctx, serverA.GetNamespace())
	require.NoError(t, err)

	// Expect only legacy one to be returned.
	out, err = presence.GetApplicationServers(ctx, apidefaults.Namespace)
	require.NoError(t, err)
	require.Empty(t, cmp.Diff([]types.AppServer{serverB}, out,
		cmpopts.IgnoreFields(types.Metadata{}, "ID")))
}

func TestDatabaseServersCRUD(t *testing.T) {
	ctx := context.Background()
	clock := clockwork.NewFakeClock()

	backend, err := lite.NewWithConfig(ctx, lite.Config{
		Path:  t.TempDir(),
		Clock: clock,
	})
	require.NoError(t, err)

	presence := NewPresenceService(backend)

	// Create a database server.
	server, err := types.NewDatabaseServerV3(types.Metadata{
		Name: "foo",
	}, types.DatabaseServerSpecV3{
		Protocol: defaults.ProtocolPostgres,
		URI:      "localhost:5432",
		Hostname: "localhost",
		HostID:   uuid.New().String(),
	})
	require.NoError(t, err)

	// Initially expect not to be returned any servers.
	out, err := presence.GetDatabaseServers(ctx, apidefaults.Namespace)
	require.NoError(t, err)
	require.Equal(t, 0, len(out))

	// Upsert server.
	lease, err := presence.UpsertDatabaseServer(ctx, server)
	require.NoError(t, err)
	require.Equal(t, &types.KeepAlive{}, lease)

	// Check again, expect a single server to be found.
	out, err = presence.GetDatabaseServers(ctx, server.GetNamespace())
	require.NoError(t, err)
	server.SetResourceID(out[0].GetResourceID())
	require.EqualValues(t, []types.DatabaseServer{server}, out)

	// Make sure can't delete with empty namespace or host ID or name.
	err = presence.DeleteDatabaseServer(ctx, server.GetNamespace(), server.GetHostID(), "")
	require.Error(t, err)
	require.IsType(t, trace.BadParameter(""), err)
	err = presence.DeleteDatabaseServer(ctx, server.GetNamespace(), "", server.GetName())
	require.Error(t, err)
	require.IsType(t, trace.BadParameter(""), err)
	err = presence.DeleteDatabaseServer(ctx, "", server.GetHostID(), server.GetName())
	require.Error(t, err)
	require.IsType(t, trace.BadParameter(""), err)

	// Remove the server.
	err = presence.DeleteDatabaseServer(ctx, server.GetNamespace(), server.GetHostID(), server.GetName())
	require.NoError(t, err)

	// Now expect no servers to be returned.
	out, err = presence.GetDatabaseServers(ctx, apidefaults.Namespace)
	require.NoError(t, err)
	require.Equal(t, 0, len(out))

	// Upsert server with TTL.
	server.SetExpiry(clock.Now().UTC().Add(time.Hour))
	lease, err = presence.UpsertDatabaseServer(ctx, server)
	require.NoError(t, err)
	require.Equal(t, &types.KeepAlive{
		Type:      types.KeepAlive_DATABASE,
		LeaseID:   lease.LeaseID,
		Name:      server.GetName(),
		Namespace: server.GetNamespace(),
		HostID:    server.GetHostID(),
		Expires:   server.Expiry(),
	}, lease)

	// Make sure can't delete all with empty namespace.
	err = presence.DeleteAllDatabaseServers(ctx, "")
	require.Error(t, err)
	require.IsType(t, trace.BadParameter(""), err)

	// Delete all.
	err = presence.DeleteAllDatabaseServers(ctx, server.GetNamespace())
	require.NoError(t, err)

	// Now expect no servers to be returned.
	out, err = presence.GetDatabaseServers(ctx, apidefaults.Namespace)
	require.NoError(t, err)
	require.Equal(t, 0, len(out))
}

func TestNodeCRUD(t *testing.T) {
	ctx := context.Background()
	lite, err := lite.NewWithConfig(ctx, lite.Config{Path: t.TempDir()})
	require.NoError(t, err)

	presence := NewPresenceService(lite)

	node1, err := types.NewServerWithLabels("node1", types.KindNode, types.ServerSpecV2{}, nil)
	require.NoError(t, err)

	node2, err := types.NewServerWithLabels("node2", types.KindNode, types.ServerSpecV2{}, nil)
	require.NoError(t, err)

	t.Run("CreateNode", func(t *testing.T) {
		// Initially expect no nodes to be returned.
		nodes, err := presence.GetNodes(ctx, apidefaults.Namespace)
		require.NoError(t, err)
		require.Equal(t, 0, len(nodes))

		// Create nodes
		_, err = presence.UpsertNode(ctx, node1)
		require.NoError(t, err)
		_, err = presence.UpsertNode(ctx, node2)
		require.NoError(t, err)
	})

	// Run NodeGetters in nested subtests to allow parallelization.
	t.Run("NodeGetters", func(t *testing.T) {
		t.Run("List Nodes", func(t *testing.T) {
			t.Parallel()
			// list nodes one at a time, last page should be empty
			nodes, nextKey, err := presence.ListNodes(ctx, proto.ListNodesRequest{
				Namespace: apidefaults.Namespace,
				Limit:     1,
			})
			require.NoError(t, err)
			require.EqualValues(t, 1, len(nodes))
			require.Empty(t, cmp.Diff([]types.Server{node1}, nodes,
				cmpopts.IgnoreFields(types.Metadata{}, "ID")))
			require.EqualValues(t, backend.NextPaginationKey(node1), nextKey)

			nodes, nextKey, err = presence.ListNodes(ctx, proto.ListNodesRequest{
				Namespace: apidefaults.Namespace,
				Limit:     1,
				StartKey:  nextKey,
			})
			require.NoError(t, err)
			require.EqualValues(t, 1, len(nodes))
			require.Empty(t, cmp.Diff([]types.Server{node2}, nodes,
				cmpopts.IgnoreFields(types.Metadata{}, "ID")))
			require.EqualValues(t, backend.NextPaginationKey(node2), nextKey)

			nodes, nextKey, err = presence.ListNodes(ctx, proto.ListNodesRequest{
				Namespace: apidefaults.Namespace,
				Limit:     1,
				StartKey:  nextKey,
			})
			require.NoError(t, err)
			require.EqualValues(t, 0, len(nodes))
			require.EqualValues(t, "", nextKey)

			// ListNodes should fail if namespace isn't provided
			_, _, err = presence.ListNodes(ctx, proto.ListNodesRequest{
				Limit: 1,
			})
			require.IsType(t, &trace.BadParameterError{}, err.(*trace.TraceErr).OrigError())

			// ListNodes should fail if limit is nonpositive
			_, _, err = presence.ListNodes(ctx, proto.ListNodesRequest{
				Namespace: apidefaults.Namespace,
			})
			require.IsType(t, &trace.BadParameterError{}, err.(*trace.TraceErr).OrigError())

			_, _, err = presence.ListNodes(ctx, proto.ListNodesRequest{
				Namespace: apidefaults.Namespace,
				Limit:     -1,
			})
			require.IsType(t, &trace.BadParameterError{}, err.(*trace.TraceErr).OrigError())
		})
		t.Run("GetNodes", func(t *testing.T) {
			t.Parallel()
			// Get all nodes, transparently handle limit exceeded errors
			nodes, err := presence.GetNodes(ctx, apidefaults.Namespace)
			require.NoError(t, err)
			require.EqualValues(t, len(nodes), 2)
			require.Empty(t, cmp.Diff([]types.Server{node1, node2}, nodes,
				cmpopts.IgnoreFields(types.Metadata{}, "ID")))

			// GetNodes should fail if namespace isn't provided
			_, err = presence.GetNodes(ctx, "")
			require.IsType(t, &trace.BadParameterError{}, err.(*trace.TraceErr).OrigError())
		})
		t.Run("GetNode", func(t *testing.T) {
			t.Parallel()
			// Get Node
			node, err := presence.GetNode(ctx, apidefaults.Namespace, "node1")
			require.NoError(t, err)
			require.Empty(t, cmp.Diff(node1, node,
				cmpopts.IgnoreFields(types.Metadata{}, "ID")))

			// GetNode should fail if node name isn't provided
			_, err = presence.GetNode(ctx, apidefaults.Namespace, "")
			require.IsType(t, &trace.BadParameterError{}, err.(*trace.TraceErr).OrigError())

			// GetNode should fail if namespace isn't provided
			_, err = presence.GetNode(ctx, "", "node1")
			require.IsType(t, &trace.BadParameterError{}, err.(*trace.TraceErr).OrigError())
		})
	})

	t.Run("DeleteNode", func(t *testing.T) {
		// Delete node.
		err = presence.DeleteNode(ctx, apidefaults.Namespace, node1.GetName())
		require.NoError(t, err)

		// Expect node not found
		_, err := presence.GetNode(ctx, apidefaults.Namespace, "node1")
		require.IsType(t, trace.NotFound(""), err)
	})

	t.Run("DeleteAllNodes", func(t *testing.T) {
		// Delete nodes
		err = presence.DeleteAllNodes(ctx, apidefaults.Namespace)
		require.NoError(t, err)

		// Now expect no nodes to be returned.
		nodes, err := presence.GetNodes(ctx, apidefaults.Namespace)
		require.NoError(t, err)
		require.Equal(t, 0, len(nodes))
	})
}

func TestListResources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := clockwork.NewFakeClock()

	tests := map[string]struct {
		resourceType           string
		createResourceFunc     func(context.Context, *PresenceService, string, map[string]string) error
		deleteAllResourcesFunc func(context.Context, *PresenceService) error
		expectedType           types.Resource
	}{
		"DatabaseServers": {
			resourceType: types.KindDatabaseServer,
			createResourceFunc: func(ctx context.Context, presence *PresenceService, name string, labels map[string]string) error {
				server, err := types.NewDatabaseServerV3(types.Metadata{
					Name:   name,
					Labels: labels,
				}, types.DatabaseServerSpecV3{
					Protocol: defaults.ProtocolPostgres,
					URI:      "localhost:5432",
					Hostname: "localhost",
					HostID:   uuid.New().String(),
				})
				if err != nil {
					return err
				}

				// Upsert server.
				_, err = presence.UpsertDatabaseServer(ctx, server)
				return err
			},
			deleteAllResourcesFunc: func(ctx context.Context, presence *PresenceService) error {
				return presence.DeleteAllDatabaseServers(ctx, apidefaults.Namespace)
			},
		},
		"DatabaseServersSameHost": {
			resourceType: types.KindDatabaseServer,
			createResourceFunc: func(ctx context.Context, presence *PresenceService, name string, labels map[string]string) error {
				server, err := types.NewDatabaseServerV3(types.Metadata{
					Name:   name,
					Labels: labels,
				}, types.DatabaseServerSpecV3{
					Protocol: defaults.ProtocolPostgres,
					URI:      "localhost:5432",
					Hostname: "localhost",
					HostID:   "some-host",
				})
				if err != nil {
					return err
				}

				// Upsert server.
				_, err = presence.UpsertDatabaseServer(ctx, server)
				return err
			},
			deleteAllResourcesFunc: func(ctx context.Context, presence *PresenceService) error {
				return presence.DeleteAllDatabaseServers(ctx, apidefaults.Namespace)
			},
		},
		"AppServers": {
			resourceType: types.KindAppServer,
			createResourceFunc: func(ctx context.Context, presence *PresenceService, name string, labels map[string]string) error {
				app, err := types.NewAppV3(types.Metadata{
					Name:   name,
					Labels: labels,
				}, types.AppSpecV3{
					URI: "localhost",
				})
				if err != nil {
					return err
				}

				server, err := types.NewAppServerV3(types.Metadata{
					Name:   name,
					Labels: labels,
				}, types.AppServerSpecV3{
					Hostname: "localhost",
					HostID:   uuid.New().String(),
					App:      app,
				})
				if err != nil {
					return err
				}

				// Upsert server.
				_, err = presence.UpsertApplicationServer(ctx, server)
				return err
			},
			deleteAllResourcesFunc: func(ctx context.Context, presence *PresenceService) error {
				return presence.DeleteAllApplicationServers(ctx, apidefaults.Namespace)
			},
		},
		"AppServersSameHost": {
			resourceType: types.KindAppServer,
			createResourceFunc: func(ctx context.Context, presence *PresenceService, name string, labels map[string]string) error {
				app, err := types.NewAppV3(types.Metadata{
					Name:   name,
					Labels: labels,
				}, types.AppSpecV3{
					URI: "localhost",
				})
				if err != nil {
					return err
				}

				server, err := types.NewAppServerV3(types.Metadata{
					Name:   name,
					Labels: labels,
				}, types.AppServerSpecV3{
					Hostname: "localhost",
					HostID:   "some-host",
					App:      app,
				})
				if err != nil {
					return err
				}

				// Upsert server.
				_, err = presence.UpsertApplicationServer(ctx, server)
				return err
			},
			deleteAllResourcesFunc: func(ctx context.Context, presence *PresenceService) error {
				return presence.DeleteAllApplicationServers(ctx, apidefaults.Namespace)
			},
		},
		"KubeService": {
			resourceType: types.KindKubeService,
			createResourceFunc: func(ctx context.Context, presence *PresenceService, name string, labels map[string]string) error {
				server, err := types.NewServerWithLabels(name, types.KindKubeService, types.ServerSpecV2{
					KubernetesClusters: []*types.KubernetesCluster{
						{Name: name, StaticLabels: labels},
					},
				}, labels)
				if err != nil {
					return err
				}

				// Upsert server.
				return presence.UpsertKubeService(ctx, server)
			},
			deleteAllResourcesFunc: func(ctx context.Context, presence *PresenceService) error {
				return presence.DeleteAllKubeServices(ctx)
			},
		},
		"Node": {
			resourceType: types.KindNode,
			createResourceFunc: func(ctx context.Context, presence *PresenceService, name string, labels map[string]string) error {
				server, err := types.NewServerWithLabels(name, types.KindNode, types.ServerSpecV2{}, labels)
				if err != nil {
					return err
				}

				// Upsert server.
				_, err = presence.UpsertNode(ctx, server)
				return err
			},
			deleteAllResourcesFunc: func(ctx context.Context, presence *PresenceService) error {
				return presence.DeleteAllNodes(ctx, apidefaults.Namespace)
			},
		},
		"NodeWithDynamicLabels": {
			resourceType: types.KindNode,
			createResourceFunc: func(ctx context.Context, presence *PresenceService, name string, labels map[string]string) error {
				dynamicLabels := make(map[string]types.CommandLabelV2)
				for name, value := range labels {
					dynamicLabels[name] = types.CommandLabelV2{
						Period:  types.NewDuration(time.Second),
						Command: []string{name},
						Result:  value,
					}
				}

				server, err := types.NewServer(name, types.KindNode, types.ServerSpecV2{
					CmdLabels: dynamicLabels,
				})
				if err != nil {
					return err
				}

				// Upsert server.
				_, err = presence.UpsertNode(ctx, server)
				return err
			},
			deleteAllResourcesFunc: func(ctx context.Context, presence *PresenceService) error {
				return presence.DeleteAllNodes(ctx, apidefaults.Namespace)
			},
		},
	}

	for testName, test := range tests {
		testName := testName
		test := test
		t.Run(testName, func(t *testing.T) {
			t.Parallel()
			backend, err := lite.NewWithConfig(ctx, lite.Config{
				Path:  t.TempDir(),
				Clock: clock,
			})
			require.NoError(t, err)

			presence := NewPresenceService(backend)

			resp, err := presence.ListResources(ctx, proto.ListResourcesRequest{
				Limit:        1,
				ResourceType: test.resourceType,
				StartKey:     "",
			})
			require.NoError(t, err)
			require.Empty(t, resp.Resources)
			require.Empty(t, resp.NextKey)
			require.Empty(t, resp.TotalCount)

			resourcesPerPage := 4
			totalWithLabels := 7
			totalWithoutLabels := 8
			labels := map[string]string{"env": "test"}
			totalResources := totalWithLabels + totalWithoutLabels

			// with labels
			for i := 0; i < totalWithLabels; i++ {
				err = test.createResourceFunc(ctx, presence, fmt.Sprintf("foo-%d", i), labels)
				require.NoError(t, err)
			}

			// without labels
			for i := 0; i < totalWithoutLabels; i++ {
				err = test.createResourceFunc(ctx, presence, fmt.Sprintf("foo-label-%d", i), map[string]string{})
				require.NoError(t, err)
			}

			resultResourcesLen := 0
			require.Eventually(t, func() bool {
				resp, err = presence.ListResources(ctx, proto.ListResourcesRequest{
					Limit:        int32(resourcesPerPage),
					Namespace:    apidefaults.Namespace,
					ResourceType: test.resourceType,
					StartKey:     resp.NextKey,
				})
				require.NoError(t, err)
				require.Empty(t, resp.TotalCount)

				resultResourcesLen += len(resp.Resources)
				if resultResourcesLen == totalResources {
					require.Empty(t, resp.NextKey)
				}
				return resultResourcesLen == totalResources
			}, time.Second, 100*time.Millisecond)

			// list resources only with matching labels
			resultResourcesWithLabelsLen := 0
			require.Eventually(t, func() bool {
				resp, err = presence.ListResources(ctx, proto.ListResourcesRequest{
					Limit:        int32(resourcesPerPage),
					Namespace:    apidefaults.Namespace,
					ResourceType: test.resourceType,
					StartKey:     resp.NextKey,
					Labels:       labels,
				})
				require.NoError(t, err)
				require.Empty(t, resp.TotalCount)

				resultResourcesWithLabelsLen += len(resp.Resources)
				if resultResourcesWithLabelsLen == totalWithLabels {
					require.Empty(t, resp.NextKey)
				}
				return resultResourcesWithLabelsLen == totalWithLabels
			}, time.Second, 100*time.Millisecond)

			// list resources only with matching search keywords
			resultResourcesWithSearchKeywordsLen := 0
			require.Eventually(t, func() bool {
				resp, err = presence.ListResources(ctx, proto.ListResourcesRequest{
					Limit:          int32(resourcesPerPage),
					Namespace:      apidefaults.Namespace,
					ResourceType:   test.resourceType,
					StartKey:       resp.NextKey,
					SearchKeywords: []string{"env", "test"},
				})
				require.NoError(t, err)
				require.Empty(t, resp.TotalCount)

				resultResourcesWithSearchKeywordsLen += len(resp.Resources)
				if resultResourcesWithSearchKeywordsLen == totalWithLabels {
					require.Empty(t, resp.NextKey)
				}
				return resultResourcesWithSearchKeywordsLen == totalWithLabels
			}, time.Second, 100*time.Millisecond)

			// list resources only with matching expression
			resultResourcesWithMatchExprsLen := 0
			require.Eventually(t, func() bool {
				resp, err = presence.ListResources(ctx, proto.ListResourcesRequest{
					Limit:               int32(resourcesPerPage),
					Namespace:           apidefaults.Namespace,
					ResourceType:        test.resourceType,
					StartKey:            resp.NextKey,
					PredicateExpression: `labels.env == "test"`,
				})
				require.NoError(t, err)
				require.Empty(t, resp.TotalCount)

				resultResourcesWithMatchExprsLen += len(resp.Resources)
				if resultResourcesWithMatchExprsLen == totalWithLabels {
					require.Empty(t, resp.NextKey)
				}
				return resultResourcesWithMatchExprsLen == totalWithLabels
			}, time.Second, 100*time.Millisecond)

			// delete everything
			err = test.deleteAllResourcesFunc(ctx, presence)
			require.NoError(t, err)

			resp, err = presence.ListResources(ctx, proto.ListResourcesRequest{
				Limit:        1,
				Namespace:    apidefaults.Namespace,
				ResourceType: test.resourceType,
				StartKey:     "",
			})
			require.NoError(t, err)
			require.Empty(t, resp.NextKey)
			require.Empty(t, resp.Resources)
			require.Empty(t, resp.TotalCount)
		})
	}
}

func TestListResources_Helpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := clockwork.NewFakeClock()
	namespace := apidefaults.Namespace
	bend, err := lite.NewWithConfig(ctx, lite.Config{
		Path:  t.TempDir(),
		Clock: clock,
	})
	require.NoError(t, err)
	presence := NewPresenceService(bend)

	tests := []struct {
		name  string
		fetch func(proto.ListResourcesRequest) (*types.ListResourcesResponse, error)
	}{
		{
			name: "listResources",
			fetch: func(req proto.ListResourcesRequest) (*types.ListResourcesResponse, error) {
				return presence.listResources(ctx, req)
			},
		},
		{
			name: "listResourcesWithSort",
			fetch: func(req proto.ListResourcesRequest) (*types.ListResourcesResponse, error) {
				return presence.listResourcesWithSort(ctx, req)
			},
		},
		{
			name: "FakePaginate",
			fetch: func(req proto.ListResourcesRequest) (*types.ListResourcesResponse, error) {
				nodes, err := presence.GetNodes(ctx, namespace)
				require.NoError(t, err)

				return FakePaginate(types.Servers(nodes).AsResources(), req)
			},
		},
	}

	t.Run("test fetching when there is 0 upserted nodes", func(t *testing.T) {
		req := proto.ListResourcesRequest{
			ResourceType: types.KindNode,
			Limit:        5,
		}
		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				resp, err := tc.fetch(req)
				require.NoError(t, err)
				require.Empty(t, resp.NextKey)
				require.Empty(t, resp.Resources)
				require.Empty(t, resp.TotalCount)
			})
		}
	})

	// Add some test servers.
	for i := 0; i < 20; i++ {
		server := suite.NewServer(types.KindNode, uuid.New().String(), "127.0.0.1:2022", namespace)
		_, err = presence.UpsertNode(ctx, server)
		require.NoError(t, err)
	}

	// Test servers have been inserted.
	nodes, err := presence.GetNodes(ctx, namespace)
	require.NoError(t, err)
	require.Len(t, nodes, 20)

	t.Run("test invalid limit value", func(t *testing.T) {
		req := proto.ListResourcesRequest{
			ResourceType: types.KindNode,
			Namespace:    namespace,
		}
		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				_, err := tc.fetch(req)
				require.True(t, trace.IsBadParameter(err))
			})
		}
	})

	t.Run("test retrieving entire list upfront", func(t *testing.T) {
		req := proto.ListResourcesRequest{
			ResourceType: types.KindNode,
			Namespace:    namespace,
			Limit:        int32(len(nodes)),
		}
		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				resp, err := tc.fetch(req)
				require.NoError(t, err)
				require.Empty(t, resp.NextKey)
				require.Empty(t, resp.TotalCount)

				fetchedNodes, err := types.ResourcesWithLabels(resp.Resources).AsServers()
				require.NoError(t, err)
				require.Equal(t, nodes, fetchedNodes)
			})
		}
	})

	t.Run("test first, middle, last fetching", func(t *testing.T) {
		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				// First fetch.
				resp, err := tc.fetch(proto.ListResourcesRequest{
					ResourceType: types.KindNode,
					Namespace:    namespace,
					Limit:        10,
				})
				require.NoError(t, err)
				require.Len(t, resp.Resources, 10)
				require.Empty(t, resp.TotalCount)

				fetchedNodes, err := types.ResourcesWithLabels(resp.Resources).AsServers()
				require.NoError(t, err)
				require.Equal(t, nodes[:10], fetchedNodes)
				require.Equal(t, backend.GetPaginationKey(nodes[10]), resp.NextKey) // 11th item

				// Middle fetch.
				resp, err = tc.fetch(proto.ListResourcesRequest{
					ResourceType: types.KindNode,
					Namespace:    namespace,
					StartKey:     resp.NextKey,
					Limit:        5,
				})
				require.NoError(t, err)
				require.Len(t, resp.Resources, 5)
				require.Empty(t, resp.TotalCount)

				fetchedNodes, err = types.ResourcesWithLabels(resp.Resources).AsServers()
				require.NoError(t, err)
				require.Equal(t, nodes[10:15], fetchedNodes)
				require.Equal(t, backend.GetPaginationKey(nodes[15]), resp.NextKey) // 16th item

				// Last fetch.
				resp, err = presence.listResources(ctx, proto.ListResourcesRequest{
					ResourceType: types.KindNode,
					Namespace:    namespace,
					StartKey:     resp.NextKey,
					Limit:        5,
				})
				require.NoError(t, err)
				require.Len(t, resp.Resources, 5)
				require.Empty(t, resp.TotalCount)

				fetchedNodes, err = types.ResourcesWithLabels(resp.Resources).AsServers()
				require.NoError(t, err)
				require.Equal(t, nodes[15:20], fetchedNodes)
				require.Empty(t, resp.NextKey)
			})
		}
	})

	t.Run("test one result filter", func(t *testing.T) {
		targetVal := nodes[14].GetName()
		req := proto.ListResourcesRequest{
			ResourceType:   types.KindNode,
			Namespace:      namespace,
			StartKey:       "",
			Limit:          5,
			SearchKeywords: []string{targetVal},
		}
		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				resp, err := tc.fetch(req)
				require.NoError(t, err)
				require.Len(t, resp.Resources, 1)
				require.Equal(t, targetVal, resp.Resources[0].GetName())
				require.Empty(t, resp.NextKey)
				require.Empty(t, resp.TotalCount)
			})
		}
	})
}

func TestFakePaginate_TotalCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := clockwork.NewFakeClock()
	namespace := apidefaults.Namespace
	bend, err := lite.NewWithConfig(ctx, lite.Config{
		Path:  t.TempDir(),
		Clock: clock,
	})
	require.NoError(t, err)
	presence := NewPresenceService(bend)

	// Add some control servers.
	server := suite.NewServer(types.KindNode, "foo-bar", "127.0.0.1:2022", namespace)
	_, err = presence.UpsertNode(ctx, server)
	require.NoError(t, err)

	server = suite.NewServer(types.KindNode, "foo-baz", "127.0.0.1:2022", namespace)
	_, err = presence.UpsertNode(ctx, server)
	require.NoError(t, err)

	server = suite.NewServer(types.KindNode, "foo-qux", "127.0.0.1:2022", namespace)
	_, err = presence.UpsertNode(ctx, server)
	require.NoError(t, err)

	// Add some test servers.
	for i := 0; i < 10; i++ {
		server := suite.NewServer(types.KindNode, uuid.New().String(), "127.0.0.1:2022", namespace)
		_, err = presence.UpsertNode(ctx, server)
		require.NoError(t, err)
	}

	// Test servers have been inserted.
	nodes, err := presence.GetNodes(ctx, namespace)
	require.NoError(t, err)
	require.Len(t, nodes, 13)

	// Convert to resources.
	resources := types.Servers(nodes).AsResources()

	t.Run("total count without filter", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name  string
			limit int
		}{
			{
				name:  "single",
				limit: 1,
			},
			{
				name:  "even",
				limit: 4,
			},
			{
				name:  "odd",
				limit: 5,
			},
			{
				name:  "max",
				limit: len(nodes),
			},
		}

		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				req := proto.ListResourcesRequest{
					ResourceType:   types.KindNode,
					Limit:          int32(tc.limit),
					NeedTotalCount: true,
				}

				// First fetch.
				resp, err := FakePaginate(resources, req)
				require.NoError(t, err)
				require.Len(t, resp.Resources, tc.limit)
				require.Equal(t, resources[0:tc.limit], resp.Resources)
				require.Equal(t, len(nodes), resp.TotalCount)

				// Next fetch should return same amount of totals.
				if tc.limit != len(nodes) {
					require.NotEmpty(t, resp.NextKey)

					req.StartKey = resp.NextKey
					resp, err = FakePaginate(resources, req)
					require.NoError(t, err)
					require.Len(t, resp.Resources, tc.limit)
					require.Equal(t, resources[tc.limit:tc.limit*2], resp.Resources)
					require.Equal(t, len(nodes), resp.TotalCount)
				} else {
					require.Empty(t, resp.NextKey)
					require.Equal(t, resources, resp.Resources)
					require.Equal(t, len(nodes), resp.TotalCount)
				}
			})
		}
	})

	t.Run("total count with no match", func(t *testing.T) {
		t.Parallel()
		req := proto.ListResourcesRequest{
			ResourceType:   types.KindNode,
			Limit:          5,
			NeedTotalCount: true,
			SearchKeywords: []string{"not-found"},
		}
		resp, err := FakePaginate(resources, req)
		require.NoError(t, err)
		require.Empty(t, resp.Resources)
		require.Empty(t, resp.NextKey)
		require.Empty(t, resp.TotalCount)
	})

	t.Run("total count with all matches", func(t *testing.T) {
		t.Parallel()
		req := proto.ListResourcesRequest{
			ResourceType:   types.KindNode,
			Limit:          5,
			NeedTotalCount: true,
			SearchKeywords: []string{"foo"},
		}
		resp, err := FakePaginate(resources, req)
		require.NoError(t, err)
		require.Len(t, resp.Resources, 3)
		require.Empty(t, resp.NextKey)
		require.Equal(t, 3, resp.TotalCount)
	})
}

func TestPresenceService_CancelSemaphoreLease(t *testing.T) {
	ctx := context.Background()
	bk, err := lite.New(ctx, backend.Params{"path": t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bk.Close()) })
	presence := NewPresenceService(bk)

	maxLeases := 5
	leases := make([]*types.SemaphoreLease, maxLeases)

	// Acquire max number of leases
	request := types.AcquireSemaphoreRequest{
		SemaphoreKind: "test",
		SemaphoreName: "test",
		MaxLeases:     int64(maxLeases),
		Expires:       time.Now().Add(time.Hour),
		Holder:        "test",
	}
	for i := range leases {
		lease, err := presence.AcquireSemaphore(ctx, request)
		require.NoError(t, err)
		require.NotNil(t, lease)

		leases[i] = lease
	}

	// Validate a semaphore exists with the correct number of leases
	semaphores, err := presence.GetSemaphores(ctx, types.SemaphoreFilter{
		SemaphoreKind: "test",
		SemaphoreName: "test",
	})
	require.NoError(t, err)
	require.Len(t, semaphores, 1)
	require.Len(t, semaphores[0].LeaseRefs(), maxLeases)

	// Cancel the leases concurrently and ensure that all
	// cancellations are honored
	errCh := make(chan error, maxLeases)
	for _, l := range leases {
		l := l
		go func() {
			errCh <- presence.CancelSemaphoreLease(ctx, *l)
		}()
	}

	for i := 0; i < maxLeases; i++ {
		err := <-errCh
		require.NoError(t, err)
	}

	// Validate the semaphore still exists but all leases were removed
	semaphores, err = presence.GetSemaphores(ctx, types.SemaphoreFilter{
		SemaphoreKind: "test",
		SemaphoreName: "test",
	})
	require.NoError(t, err)
	require.Len(t, semaphores, 1)
	require.Empty(t, semaphores[0].LeaseRefs())
}

func TestListResources_SortAndDeduplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	backend, err := lite.NewWithConfig(ctx, lite.Config{
		Path:  t.TempDir(),
		Clock: clockwork.NewFakeClock(),
	})
	require.NoError(t, err)

	presence := NewPresenceService(backend)

	// Define some resource names for testing.
	names := []string{"d", "b", "d", "a", "a", "b"}
	uniqueNames := []string{"a", "b", "d"}

	tests := []struct {
		name            string
		kind            string
		insertResources func()
		wantNames       []string
	}{
		{
			name: "KindDatabaseServer",
			kind: types.KindDatabaseServer,
			insertResources: func() {
				for i := 0; i < len(names); i++ {
					db, err := types.NewDatabaseServerV3(types.Metadata{
						Name: fmt.Sprintf("name-%v", i),
					}, types.DatabaseServerSpecV3{
						HostID:   "_",
						Hostname: "_",
						Database: &types.DatabaseV3{
							Metadata: types.Metadata{
								Name: names[i],
							},
							Spec: types.DatabaseSpecV3{
								Protocol: "_",
								URI:      "_",
							},
						},
					})
					require.NoError(t, err)
					_, err = presence.UpsertDatabaseServer(ctx, db)
					require.NoError(t, err)
				}
			},
		},
		{
			name: "KindAppServer",
			kind: types.KindAppServer,
			insertResources: func() {
				for i := 0; i < len(names); i++ {
					server, err := types.NewAppServerV3(types.Metadata{
						Name: fmt.Sprintf("name-%v", i),
					}, types.AppServerSpecV3{
						HostID: "_",
						App:    &types.AppV3{Metadata: types.Metadata{Name: names[i]}, Spec: types.AppSpecV3{URI: "_"}},
					})
					require.NoError(t, err)
					_, err = presence.UpsertApplicationServer(ctx, server)
					require.NoError(t, err)
				}
			},
		},
		{
			name: "KindKubernetesCluster",
			kind: types.KindKubernetesCluster,
			insertResources: func() {
				for i := 0; i < len(names); i++ {
					server, err := types.NewServer(fmt.Sprintf("name-%v", i), types.KindKubeService, types.ServerSpecV2{
						KubernetesClusters: []*types.KubernetesCluster{
							// Test dedup inside this list as well as from each service.
							{Name: names[i]},
							{Name: names[i]},
						},
					})
					require.NoError(t, err)
					_, err = presence.UpsertKubeServiceV2(ctx, server)
					require.NoError(t, err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.insertResources()

			// Fetch all resources
			fetchedResources := make([]types.ResourceWithLabels, 0, len(uniqueNames))
			resp, err := presence.ListResources(ctx, proto.ListResourcesRequest{
				ResourceType:   tc.kind,
				NeedTotalCount: true,
				Limit:          2,
				SortBy:         types.SortBy{Field: types.ResourceMetadataName, IsDesc: true},
			})
			require.NoError(t, err)
			require.Len(t, resp.Resources, 2)
			require.Equal(t, resp.TotalCount, len(uniqueNames))
			fetchedResources = append(fetchedResources, resp.Resources...)

			resp, err = presence.ListResources(ctx, proto.ListResourcesRequest{
				ResourceType:   tc.kind,
				NeedTotalCount: true,
				StartKey:       resp.NextKey,
				Limit:          2,
				SortBy:         types.SortBy{Field: types.ResourceMetadataName, IsDesc: true},
			})
			require.NoError(t, err)
			require.Len(t, resp.Resources, 1)
			require.Equal(t, len(uniqueNames), resp.TotalCount)
			fetchedResources = append(fetchedResources, resp.Resources...)

			r := types.ResourcesWithLabels(fetchedResources)
			var extractedErr error
			var extractedNames []string

			switch tc.kind {
			case types.KindDatabaseServer:
				s, err := r.AsDatabaseServers()
				require.NoError(t, err)
				extractedNames, extractedErr = types.DatabaseServers(s).GetFieldVals(types.ResourceMetadataName)

			case types.KindAppServer:
				s, err := r.AsAppServers()
				require.NoError(t, err)
				extractedNames, extractedErr = types.AppServers(s).GetFieldVals(types.ResourceMetadataName)

			default:
				s, err := r.AsKubeClusters()
				require.NoError(t, err)
				require.Len(t, s, 3)
				extractedNames, extractedErr = types.KubeClusters(s).GetFieldVals(types.ResourceMetadataName)
			}

			require.NoError(t, extractedErr)
			require.ElementsMatch(t, uniqueNames, extractedNames)
			require.IsDecreasing(t, extractedNames)
		})
	}
}

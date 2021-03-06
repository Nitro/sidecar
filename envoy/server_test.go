package envoy

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/Nitro/sidecar/catalog"
	"github.com/Nitro/sidecar/config"
	"github.com/Nitro/sidecar/envoy/adapter"
	"github.com/Nitro/sidecar/service"

	api "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	hcm "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	tcpp "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/tcp_proxy/v2"
	envoy_discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v2"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v2"
	xds "github.com/envoyproxy/go-control-plane/pkg/server/v2"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"google.golang.org/grpc"

	"github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"

	. "github.com/smartystreets/goconvey/convey"
)

const (
	bindIP = "192.168.168.168"
)

var (
	validators = map[string]func(*any.Any, service.Service){
		resource.ListenerType: validateListener,
		resource.EndpointType: validateEndpoints,
		resource.ClusterType:  validateCluster,
	}
)

func validateListener(serialisedListener *any.Any, svc service.Service) {
	listener := &api.Listener{}
	err := ptypes.UnmarshalAny(serialisedListener, listener)
	So(err, ShouldBeNil)
	So(listener.Name, ShouldEqual, adapter.SvcName(svc.Name, svc.Ports[0].ServicePort))
	So(listener.GetAddress().GetSocketAddress().GetAddress(), ShouldEqual, bindIP)
	So(listener.GetAddress().GetSocketAddress().GetPortValue(), ShouldEqual, svc.Ports[0].ServicePort)
	filterChains := listener.GetFilterChains()
	So(filterChains, ShouldHaveLength, 1)
	filters := filterChains[0].GetFilters()
	So(filters, ShouldHaveLength, 1)

	switch svc.ProxyMode {
	case "http":
		So(filters[0].GetName(), ShouldEqual, wellknown.HTTPConnectionManager)
		connectionManager := &hcm.HttpConnectionManager{}
		err = ptypes.UnmarshalAny(filters[0].GetTypedConfig(), connectionManager)
		So(err, ShouldBeNil)
		So(connectionManager.GetStatPrefix(), ShouldEqual, "ingress_http")
		So(connectionManager.GetRouteConfig(), ShouldNotBeNil)
		So(connectionManager.GetRouteConfig().GetVirtualHosts(), ShouldHaveLength, 1)
		virtualHost := connectionManager.GetRouteConfig().GetVirtualHosts()[0]
		So(virtualHost.GetName(), ShouldEqual, svc.Name)
		So(virtualHost.GetRoutes(), ShouldHaveLength, 1)
		route := virtualHost.GetRoutes()[0].GetRoute()
		So(route, ShouldNotBeNil)
		So(route.GetCluster(), ShouldEqual, adapter.SvcName(svc.Name, svc.Ports[0].ServicePort))
		So(route.GetTimeout(), ShouldNotBeNil)
	case "tcp":
		So(filters[0].GetName(), ShouldEqual, wellknown.TCPProxy)
		connectionManager := &tcpp.TcpProxy{}
		err = ptypes.UnmarshalAny(filters[0].GetTypedConfig(), connectionManager)
		So(err, ShouldBeNil)
		So(connectionManager.GetStatPrefix(), ShouldEqual, "ingress_tcp")
		So(connectionManager.GetCluster(), ShouldEqual, adapter.SvcName(svc.Name, svc.Ports[0].ServicePort))
	case "ws":
		So(filters[0].GetName(), ShouldEqual, wellknown.HTTPConnectionManager)
		connectionManager := &hcm.HttpConnectionManager{}
		err = ptypes.UnmarshalAny(filters[0].GetTypedConfig(), connectionManager)
		So(err, ShouldBeNil)
		So(connectionManager.GetStatPrefix(), ShouldEqual, "ingress_http")
		So(connectionManager.GetRouteConfig(), ShouldNotBeNil)
		So(connectionManager.GetRouteConfig().GetVirtualHosts(), ShouldHaveLength, 1)
		virtualHost := connectionManager.GetRouteConfig().GetVirtualHosts()[0]
		So(virtualHost.GetName(), ShouldEqual, svc.Name)
		So(virtualHost.GetRoutes(), ShouldHaveLength, 1)
		route := virtualHost.GetRoutes()[0].GetRoute()
		So(route, ShouldNotBeNil)
		So(route.GetCluster(), ShouldEqual, adapter.SvcName(svc.Name, svc.Ports[0].ServicePort))
		So(route.GetTimeout(), ShouldNotBeNil)

		// websocket stuff
		upgradeConfigs := connectionManager.GetUpgradeConfigs()
		So(len(upgradeConfigs), ShouldEqual, 1)
		So(upgradeConfigs[0].UpgradeType, ShouldEqual, "websocket")
	}
}

func validateEndpoints(serialisedAssignment *any.Any, svc service.Service) {
	assignment := &api.ClusterLoadAssignment{}
	err := ptypes.UnmarshalAny(serialisedAssignment, assignment)
	So(err, ShouldBeNil)
	So(assignment.GetClusterName(), ShouldEqual, adapter.SvcName(svc.Name, svc.Ports[0].ServicePort))

	localityEndpoints := assignment.GetEndpoints()
	So(localityEndpoints, ShouldHaveLength, 1)

	endpoints := localityEndpoints[0].GetLbEndpoints()
	So(endpoints, ShouldHaveLength, 1)
	So(endpoints[0].GetEndpoint().GetAddress().GetSocketAddress().GetAddress(), ShouldEqual, svc.Ports[0].IP)
	So(endpoints[0].GetEndpoint().GetAddress().GetSocketAddress().GetPortValue(), ShouldEqual, svc.Ports[0].Port)
}

func validateCluster(serialisedCluster *any.Any, svc service.Service) {
	cluster := &api.Cluster{}
	err := ptypes.UnmarshalAny(serialisedCluster, cluster)
	So(err, ShouldBeNil)
	So(cluster.Name, ShouldEqual, adapter.SvcName(svc.Name, svc.Ports[0].ServicePort))
	So(cluster.GetConnectTimeout().GetNanos(), ShouldEqual, 500000000)
	So(cluster.GetType(), ShouldEqual, api.Cluster_EDS)
	So(cluster.GetEdsClusterConfig(), ShouldNotBeNil)
	So(cluster.GetEdsClusterConfig().GetEdsConfig(), ShouldNotBeNil)
	So(cluster.GetEdsClusterConfig().GetEdsConfig().GetAds(), ShouldNotBeNil)
	So(cluster.GetLoadAssignment(), ShouldBeNil)
}

// EnvoyMock is used to validate the Envoy state by making the same gRPC stream calls
// to the Server as Envoy would
type EnvoyMock struct {
	nonces map[string]string
}

func NewEnvoyMock() EnvoyMock {
	return EnvoyMock{
		nonces: make(map[string]string),
	}
}

func (sv *EnvoyMock) GetResource(stream envoy_discovery.AggregatedDiscoveryService_StreamAggregatedResourcesClient, resource string, hostname string) []*any.Any {
	nonce, ok := sv.nonces[resource]
	if !ok {
		// Set the initial nonce to 0 for each resource type. The control plane will increment
		// it after each call, so we need to pass back the value we last received.
		nonce = "0"
	}
	err := stream.Send(&api.DiscoveryRequest{
		VersionInfo: "1",
		Node: &core.Node{
			Id: hostname,
		},
		TypeUrl:       resource,
		ResponseNonce: nonce,
	})
	if err != nil && err != io.EOF {
		So(err, ShouldBeNil)
	}

	// Recv() blocks until the stream ctx expires if the message sent via
	// Send() is not recognised / valid
	response, err := stream.Recv()

	So(err, ShouldBeNil)

	sv.nonces[resource] = response.GetNonce()

	return response.Resources
}

func (sv *EnvoyMock) ValidateResources(stream envoy_discovery.AggregatedDiscoveryService_StreamAggregatedResourcesClient, svc service.Service, hostname string) {
	for resourceType, validator := range validators {
		resources := sv.GetResource(stream, resourceType, hostname)
		So(resources, ShouldHaveLength, 1)
		validator(resources[0], svc)
	}
}

// SnapshotCache is a light wrapper around cache.SnapshotCache which lets us
// get a notification after calling SetSnapshot via the Waiter chan
type SnapshotCache struct {
	cache.SnapshotCache
	Waiter chan struct{}
}

func (c *SnapshotCache) SetSnapshot(node string, snapshot cache.Snapshot) error {
	err := c.SnapshotCache.SetSnapshot(node, snapshot)

	c.Waiter <- struct{}{}

	return err
}

func NewSnapshotCache() *SnapshotCache {
	return &SnapshotCache{
		SnapshotCache: cache.NewSnapshotCache(true, cache.IDHash{}, nil),
		Waiter:        make(chan struct{}),
	}
}

func Test_PortForServicePort(t *testing.T) {
	Convey("Run()", t, func() {
		config := config.EnvoyConfig{
			UseGRPCAPI: true,
			BindIP:     bindIP,
		}

		log.SetOutput(ioutil.Discard)

		state := catalog.NewServicesState()

		dummyHostname := "carcasone"
		baseTime := time.Now().UTC()
		httpSvc := service.Service{
			ID:        "deadbeef123",
			Name:      "bocaccio",
			Created:   baseTime,
			Hostname:  dummyHostname,
			Updated:   baseTime,
			Status:    service.ALIVE,
			ProxyMode: "http",
			Ports: []service.Port{
				{IP: "127.0.0.1", Port: 9990, ServicePort: 10100},
			},
		}

		anotherHTTPSvc := service.Service{
			ID:        "deadbeef456",
			Name:      "bocaccio",
			Created:   baseTime,
			Hostname:  dummyHostname,
			Updated:   baseTime,
			Status:    service.ALIVE,
			ProxyMode: "http",
			Ports: []service.Port{
				{IP: "127.0.0.1", Port: 9991, ServicePort: 10100},
			},
		}

		tcpSvc := service.Service{
			ID:        "undeadbeef",
			Name:      "tolstoy",
			Created:   baseTime,
			Hostname:  state.Hostname,
			Updated:   baseTime,
			Status:    service.ALIVE,
			ProxyMode: "tcp",
			Ports: []service.Port{
				{IP: "127.0.0.1", Port: 666, ServicePort: 10101},
			},
		}

		wsSvc := service.Service{
			ID:        "deadbeef666",
			Name:      "kafka",
			Created:   baseTime,
			Hostname:  dummyHostname,
			Updated:   baseTime,
			Status:    service.ALIVE,
			ProxyMode: "ws",
			Ports: []service.Port{
				{IP: "127.0.0.1", Port: 6666, ServicePort: 10102},
			},
		}

		ctx, cancel := context.WithCancel(context.Background())
		Reset(func() {
			cancel()
		})

		// Use a custom SnapshotCache in the xdsServer so we can block after updating
		// the state until the Server gets a chance to set a new snapshot in the cache
		snapshotCache := NewSnapshotCache()
		server := &Server{
			config:        config,
			state:         state,
			snapshotCache: snapshotCache,
			xdsServer:     xds.NewServer(ctx, snapshotCache, &xdsCallbacks{}),
		}

		// The gRPC listener will be assigned a random port and will be owned
		// and managed by the gRPC server
		lis, err := net.Listen("tcp", ":0")
		So(err, ShouldBeNil)
		So(lis.Addr(), ShouldHaveSameTypeAs, &net.TCPAddr{})

		// Using a FreeLooper instead would make it run too often, triggering
		// spurious locking on the state, which can cause the tests to time out
		go server.Run(ctx, director.NewTimedLooper(director.FOREVER, 10*time.Millisecond, make(chan error)), lis)

		Convey("sends the Envoy state via gRPC", func() {
			conn, err := grpc.DialContext(ctx,
				fmt.Sprintf(":%d", lis.Addr().(*net.TCPAddr).Port),
				grpc.WithInsecure(), grpc.WithBlock(),
			)
			So(err, ShouldBeNil)

			// 100 milliseconds should give us enough time to run hundreds of server transactions
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			Reset(func() {
				cancel()
			})

			stream, err := envoy_discovery.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
			So(err, ShouldBeNil)

			envoyMock := NewEnvoyMock()

			Convey("for a HTTP service", func() {
				state.AddServiceEntry(httpSvc)
				<-snapshotCache.Waiter

				envoyMock.ValidateResources(stream, httpSvc, state.Hostname)

				Convey("and removes it after it gets tombstoned", func() {
					httpSvc.Tombstone()
					httpSvc.Updated.Add(1 * time.Millisecond)
					state.AddServiceEntry(httpSvc)
					<-snapshotCache.Waiter

					for resourceType := range validators {
						resources := envoyMock.GetResource(stream, resourceType, state.Hostname)
						So(resources, ShouldHaveLength, 0)
					}
				})

				Convey("and places another instance of the same service in the same cluster", func() {
					// Make sure this other service instance was more recently updated than httpSvc
					anotherHTTPSvc.Updated = anotherHTTPSvc.Updated.Add(1 * time.Millisecond)
					state.AddServiceEntry(anotherHTTPSvc)
					<-snapshotCache.Waiter

					resources := envoyMock.GetResource(stream, resource.EndpointType, state.Hostname)
					So(resources, ShouldHaveLength, 1)
					assignment := &api.ClusterLoadAssignment{}
					err := ptypes.UnmarshalAny(resources[0], assignment)
					So(err, ShouldBeNil)
					So(assignment.GetEndpoints(), ShouldHaveLength, 1)
					var ports sort.IntSlice
					for _, endpoint := range assignment.GetEndpoints()[0].GetLbEndpoints() {
						ports = append(ports,
							int(endpoint.GetEndpoint().GetAddress().GetSocketAddress().GetPortValue()))
					}
					ports.Sort()
					So(ports, ShouldResemble, sort.IntSlice{9990, 9991})
				})
			})

			Convey("for a TCP service", func() {
				state.AddServiceEntry(tcpSvc)
				<-snapshotCache.Waiter

				envoyMock.ValidateResources(stream, tcpSvc, state.Hostname)
			})

			Convey("for a Websocket service", func() {
				state.AddServiceEntry(wsSvc)
				<-snapshotCache.Waiter

				envoyMock.ValidateResources(stream, wsSvc, state.Hostname)
			})

			Convey("and skips tombstones", func() {
				httpSvc.Tombstone()
				state.AddServiceEntry(httpSvc)
				<-snapshotCache.Waiter

				for resourceType := range validators {
					resources := envoyMock.GetResource(stream, resourceType, state.Hostname)
					So(resources, ShouldHaveLength, 0)
				}
			})

			Convey("and triggers an update when expiring a server with only one service running", func(c C) {
				state.AddServiceEntry(httpSvc)
				<-snapshotCache.Waiter

				done := make(chan struct{})
				go func() {
					select {
					case <-snapshotCache.Waiter:
						close(done)
					case <-time.After(100 * time.Millisecond):
						c.So(true, ShouldEqual, false)
					}
				}()

				state.ExpireServer(dummyHostname)
				<-done

				for resourceType := range validators {
					resources := envoyMock.GetResource(stream, resourceType, state.Hostname)
					So(resources, ShouldHaveLength, 0)
				}
			})
		})
	})
}

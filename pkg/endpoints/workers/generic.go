package workers

import (
	"context"
	"fmt"
	log "log/slog"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
)

type Endpoint interface {
	ProcessInstance(ctx context.Context, service *v1.Service, leaderElectionActive *bool) error
	Clear(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool)
	GetEndpoints(service *v1.Service, id string) ([]string, error)
	RemoveEgress(service *v1.Service, lastKnownGoodEndpoint *string)
	Delete(service *v1.Service, id string) error
	SetInstanceEndpointsStatus(service *v1.Service, state bool) error
}

func New(config *kubevip.Config, provider providers.Provider, bgpServer *bgp.Server, instances *[]*instance.Instance, configuredLocalRoutes *sync.Map) Endpoint {
	generic := newGeneric(config, provider, instances, configuredLocalRoutes)

	if config.EnableRoutingTable {
		return newRoutingTable(generic)
	}
	if config.EnableBGP {
		return newBGP(generic, bgpServer)
	}

	return &generic
}

type generic struct {
	config                *kubevip.Config
	provider              providers.Provider
	instances             *[]*instance.Instance
	configuredLocalRoutes *sync.Map
}

func newGeneric(config *kubevip.Config, provider providers.Provider, instances *[]*instance.Instance, configuredLocalRoutes *sync.Map) generic {
	return generic{
		config:                config,
		provider:              provider,
		instances:             instances,
		configuredLocalRoutes: configuredLocalRoutes,
	}
}

func (g *generic) ProcessInstance(_ context.Context, _ *v1.Service, _ *bool) error {
	return nil
}

func (g *generic) Clear(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	g.clearEgress(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
}

func (g *generic) clearEgress(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	if *lastKnownGoodEndpoint != "" {
		log.Warn("existing  endpoint has been removed, no remaining endpoints for leaderElection", "provider", g.provider.GetLabel(), "endpoint", lastKnownGoodEndpoint)
		if err := egress.Teardown(*lastKnownGoodEndpoint, service.Spec.LoadBalancerIP, service.Namespace, service.Annotations, g.config.EgressWithNftables); err != nil {
			log.Error("error removing redundant egress rules", "err", err)
		}

		*lastKnownGoodEndpoint = "" // reset endpoint
		if g.config.EnableServicesElection || g.config.EnableLeaderElection {
			cancel() // stop services watcher
		}
		*leaderElectionActive = false
	}
}

func (g *generic) GetEndpoints(_ *v1.Service, id string) ([]string, error) {
	return g.getLocalEndpoints(id)
}

func (g *generic) getLocalEndpoints(id string) ([]string, error) {
	// Build endpoints
	var endpoints []string
	var err error
	if endpoints, err = g.provider.GetLocalEndpoints(id, g.config); err != nil {
		return nil, fmt.Errorf("[%s] error getting local endpoints: %w", g.provider.GetLabel(), err)
	}

	return endpoints, nil
}

func (g *generic) getAllEndpoints(service *v1.Service, id string) ([]string, error) {
	// Build endpoints
	var err error
	var endpoints []string
	if !g.config.EnableLeaderElection && !g.config.EnableServicesElection &&
		service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
		if endpoints, err = g.provider.GetAllEndpoints(); err != nil {
			return nil, fmt.Errorf("[%s] error getting all endpoints: %w", g.provider.GetLabel(), err)
		}
	} else {
		if endpoints, err = g.provider.GetLocalEndpoints(id, g.config); err != nil {
			return nil, fmt.Errorf("[%s] error getting local endpoints: %w", g.provider.GetLabel(), err)
		}
	}

	return endpoints, nil
}

func (g *generic) RemoveEgress(_ *v1.Service, _ *string) {
}

func (g *generic) Delete(_ *v1.Service, _ string) error {
	return nil
}

func (g *generic) SetInstanceEndpointsStatus(_ *v1.Service, _ bool) error {
	return nil
}

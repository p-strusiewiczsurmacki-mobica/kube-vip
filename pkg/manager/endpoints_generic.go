package manager

import (
	"context"
	"fmt"
	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
)

type EndpointWorker interface {
	ProcessInstance(ctx context.Context, service *v1.Service, leaderElectionActive *bool) error
	Clear(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool)
	GetEndpoints(service *v1.Service, id string) ([]string, error)
	RemoveEgress(service *v1.Service, lastKnownGoodEndpoint *string)
	Delete(service *v1.Service, id string) error
}

func NewEndpointWorker(config *kubevip.Config, provider EpProvider) EndpointWorker {
	if config.EnableRoutingTable {
		return NewRoutingTable(config, provider)
	}
	if config.EnableBGP {
		return NewBGP(config, provider)
	}
	return &Generic{
		config:   config,
		provider: provider,
	}
}

type Generic struct {
	config   *kubevip.Config
	provider EpProvider
}

func (g *Generic) ProcessInstance(_ context.Context, _ *v1.Service, _ *bool) error {
	return nil
}

func (g *Generic) Clear(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	g.clearEgress(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
}

func (g *Generic) clearEgress(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	if *lastKnownGoodEndpoint != "" {
		log.Warn("existing  endpoint has been removed, no remaining endpoints for leaderElection", "provider", g.provider.getLabel(), "endpoint", lastKnownGoodEndpoint)
		if err := TeardownEgress(*lastKnownGoodEndpoint, service.Spec.LoadBalancerIP, service.Namespace, service.Annotations); err != nil {
			log.Error("error removing redundant egress rules", "err", err)
		}

		*lastKnownGoodEndpoint = "" // reset endpoint
		if g.sm.config.EnableServicesElection || g.sm.config.EnableLeaderElection {
			cancel() // stop services watcher
		}
		*leaderElectionActive = false
	}
}

func (g *Generic) GetEndpoints(_ *v1.Service, id string) ([]string, error) {
	return g.getLocalEndpoints(id)
}

func (g *Generic) getLocalEndpoints(id string) ([]string, error) {
	// Build endpoints
	var endpoints []string
	var err error
	if endpoints, err = g.provider.getLocalEndpoints(id, g.sm.config); err != nil {
		return nil, fmt.Errorf("[%s] error getting local endpoints: %w", g.provider.getLabel(), err)
	}

	return endpoints, nil
}

func (g *Generic) getAllEndpoints(service *v1.Service, id string) ([]string, error) {
	// Build endpoints
	var err error
	var endpoints []string
	if !g.sm.config.EnableLeaderElection && !g.sm.config.EnableServicesElection &&
		service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
		if endpoints, err = g.provider.getAllEndpoints(); err != nil {
			return nil, fmt.Errorf("[%s] error getting all endpoints: %w", g.provider.getLabel(), err)
		}
	} else {
		if endpoints, err = g.provider.getLocalEndpoints(id, g.sm.config); err != nil {
			return nil, fmt.Errorf("[%s] error getting local endpoints: %w", g.provider.getLabel(), err)
		}
	}

	return endpoints, nil
}

func (g *Generic) RemoveEgress(_ *v1.Service, _ *string) {
}

func (g *Generic) Delete(_ *v1.Service, _ string) error {
	return nil
}

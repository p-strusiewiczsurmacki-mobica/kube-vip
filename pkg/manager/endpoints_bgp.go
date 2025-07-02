package manager

import (
	"context"
	"fmt"
	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	v1 "k8s.io/api/core/v1"
)

type BGP struct {
	Generic
}

func NewBGP(sm *Manager, provider epProvider) EndpointWorker {
	return &BGP{
		Generic: Generic{
			sm:       sm,
			provider: provider,
		},
	}
}

func (b *BGP) ProcessInstance(ctx context.Context, service *v1.Service, leaderElectionActive *bool) error {
	instance := b.sm.findServiceInstance(service)
	if instance != nil {
		for _, cluster := range instance.Clusters {
			for i := range cluster.Network {
				address := fmt.Sprintf("%s/%s", cluster.Network[i].IP(), b.sm.config.VIPSubnet)
				log.Debug("attempting to advertise BGP service", "provider", b.provider.getLabel(), "ip", address)
				err := b.sm.bgpServer.AddHost(address)
				if err != nil {
					log.Error("error adding BGP host", "provider", b.provider.getLabel(), "err", err)
				} else {
					log.Info("added BGP host", "provider",
						b.provider.getLabel(), "ip", address, "service name", service.Name, "namespace", service.Namespace)
					// configuredLocalRoutes.Store(string(service.UID), true)
					*leaderElectionActive = true
				}
			}
		}
	}
	return nil
}

func (b *BGP) Clear(lastKnownGoodEndpoint *string, service *v1.Service, cancel context.CancelFunc, leaderElectionActive *bool) {
	if !b.sm.config.EnableServicesElection && !b.sm.config.EnableLeaderElection {
		// If BGP mode is enabled - routes should be deleted
		if instance := b.sm.findServiceInstance(service); instance != nil {
			for _, cluster := range instance.Clusters {
				for i := range cluster.Network {
					address := fmt.Sprintf("%s/%s", cluster.Network[i].IP(), b.sm.config.VIPSubnet)
					err := b.sm.bgpServer.DelHost(address)
					if err != nil {
						log.Error("deleting BGP host", "provider", b.provider.getLabel(), "ip", address, "err", err)
					} else {
						log.Info("deleted BGP host", "provider",
							b.provider.getLabel(), "ip", address, "service name", service.Name, "namespace", service.Namespace)
						// configuredLocalRoutes.Store(string(service.UID), false)
						*leaderElectionActive = false
					}
				}
			}

		}
	}

	b.clearEgress(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
}

func (b *BGP) GetEndpoints(service *v1.Service, id string) ([]string, error) {
	return b.getAllEndpoints(service, id)
}

func (b *BGP) Delete(service *v1.Service, id string) error {
	// When no-leader-elecition mode
	if !b.sm.config.EnableServicesElection && !b.sm.config.EnableLeaderElection {
		// find all existing local endpoints
		endpoints, err := b.GetEndpoints(service, id)
		if err != nil {
			return fmt.Errorf("[%s] error getting endpoints: %w", b.provider.getLabel(), err)
		}

		// If there were local endpoints deleted
		if len(endpoints) > 0 {
			b.deleteAction(service)
		}
	}

	return nil
}

func (b *BGP) deleteAction(service *v1.Service) {
	b.sm.ClearBGPHosts(service)
}

func (sm *Manager) ClearBGPHosts(service *v1.Service) {
	if instance := sm.findServiceInstance(service); instance != nil {
		sm.clearBGPHostsByInstance(instance)
	}
}

func (sm *Manager) clearBGPHostsByInstance(instance *cluster.Instance) {
	for _, cluster := range instance.Clusters {
		for i := range cluster.Network {
			address := fmt.Sprintf("%s/%s", cluster.Network[i].IP(), sm.config.VIPSubnet)
			err := sm.bgpServer.DelHost(address)
			if err != nil {
				log.Error("[endpoint] error deleting BGP host", "err", err)
			} else {
				log.Debug("[endpoint] deleted BGP host", "ip",
					address, "service name", instance.ServiceSnapshot.Name, "namespace", instance.ServiceSnapshot.Namespace)
			}
		}
	}
}

func (b *BGP) SetInstanceEndpointsStatus(_ *v1.Service, _ []string) error {
	return nil
}

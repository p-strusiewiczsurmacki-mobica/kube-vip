package manager

import (
	"context"
	"fmt"
	"net"
	"strings"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/iptables"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/watch"
)

type Processor struct {
	config   *kubevip.Config
	provider EpProvider
	worker   EndpointWorker
}

func NewEndpointProcessor(config *kubevip.Config, provider EpProvider) *Processor {
	return &Processor{
		config:   config,
		provider: provider,
	}
}

func (p *Processor) AddModify(ctx context.Context, event watch.Event, cancel context.CancelFunc,
	lastKnownGoodEndpoint *string, service *v1.Service, id string, leaderElectionActive *bool) (bool, error) {

	var err error
	if err = p.provider.loadObject(event.Object, cancel); err != nil {
		return false, fmt.Errorf("[%s] error loading k8s object: %w", p.provider.getLabel(), err)
	}

	p.worker = NewEndpointWorker(p.config, p.provider) // TODO: this could be created on kube-vip's start

	endpoints, err := p.worker.GetEndpoints(service, id)
	if err != nil {
		return false, err
	}

	// Find out if we have any local endpoints
	// if out endpoint is empty then populate it
	// if not, go through the endpoints and see if ours still exists
	// If we have a local endpoint then begin the leader Election, unless it's already running
	//

	// Check that we have local endpoints
	if len(endpoints) != 0 {
		// Ignore IPv4
		if service.Annotations[EgressIPv6] == "true" && net.ParseIP(endpoints[0]).To4() != nil {
			return true, nil
		}

		p.updateLastKnownGoodEndpoint(lastKnownGoodEndpoint, endpoints, service, leaderElectionActive, cancel)

		// start leader election if it's enabled and not already started
		if !*leaderElectionActive && p.config.EnableServicesElection {
			go startLeaderElection(ctx, p.sm, leaderElectionActive, service)
		}

		isRouteConfigured, err := isRouteConfigured(service.UID)
		if err != nil {
			return false, fmt.Errorf("[%s] error while checking if route is configured: %w", p.provider.getLabel(), err)
		}

		// There are local endpoints available on the node
		if !p.config.EnableServicesElection && !p.config.EnableLeaderElection && !isRouteConfigured {
			if err := p.worker.ProcessInstance(ctx, service, leaderElectionActive); err != nil {
				return false, fmt.Errorf("failed to process non-empty instance: %w", err)
			}
		}
	} else {
		// There are no local endpoints
		p.worker.Clear(lastKnownGoodEndpoint, service, cancel, leaderElectionActive)
	}

	// Set the service accordingly
	p.updateAnnotations(service, lastKnownGoodEndpoint)

	log.Debug("watcher", "provider",
		p.provider.getLabel(), "service name", service.Name, "namespace", service.Namespace, "endpoints", len(endpoints), "last endpoint", lastKnownGoodEndpoint, "active leader election", leaderElectionActive)

	return false, nil
}

func (p *Processor) Delete(service *v1.Service, id string) error {
	if err := p.worker.Delete(service, id); err != nil {
		return fmt.Errorf("[%s] error deleting service: %w", p.provider.getLabel(), err)
	}
	return nil
}

func (p *Processor) updateLastKnownGoodEndpoint(lastKnownGoodEndpoint *string, endpoints []string, service *v1.Service, leaderElectionActive *bool, cancel context.CancelFunc) {
	// if we haven't populated one, then do so
	if *lastKnownGoodEndpoint == "" {
		*lastKnownGoodEndpoint = endpoints[0]
		return
	}

	// check out previous endpoint exists
	stillExists := false

	for x := range endpoints {
		if endpoints[x] == *lastKnownGoodEndpoint {
			stillExists = true
		}
	}
	// If the last endpoint no longer exists, we cancel our leader Election, and set another endpoint as last known good
	if !stillExists {
		p.worker.RemoveEgress(service, lastKnownGoodEndpoint)
		if *leaderElectionActive && (p.config.EnableServicesElection || p.config.EnableLeaderElection) {
			log.Warn("existing endpoint has been removed, restarting leaderElection", "provider", p.provider.getLabel(), "endpoint", lastKnownGoodEndpoint)
			// Stop the existing leaderElection
			cancel()
			// disable last leaderElection flag
			*leaderElectionActive = false
		}
		// Set our active endpoint to an existing one
		*lastKnownGoodEndpoint = endpoints[0]
	}
}

func (p *Processor) updateAnnotations(service *v1.Service, lastKnownGoodEndpoint *string) {
	// Set the service accordingly
	if service.Annotations[Egress] == "true" {
		activeEndpointAnnotation := ActiveEndpoint

		if p.config.EnableEndpointSlices && p.provider.getProtocol() == string(discoveryv1.AddressTypeIPv6) {
			activeEndpointAnnotation = ActiveEndpointIPv6
		}
		service.Annotations[activeEndpointAnnotation] = *lastKnownGoodEndpoint
	}
}

func startLeaderElection(ctx context.Context, sm *Manager, leaderElectionActive *bool, service *v1.Service) {
	// This is a blocking function, that will restart (in the event of failure)
	for {
		// if the context isn't cancelled restart
		if ctx.Err() != context.Canceled {
			*leaderElectionActive = true
			err := sm.StartServicesLeaderElection(ctx, service)
			if err != nil {
				log.Error(err.Error())
			}
			*leaderElectionActive = false
		} else {
			*leaderElectionActive = false
			break
		}
	}
}

func TeardownEgress(config *kubevip.Config, podIP, vipIP, namespace string, annotations map[string]string) error {
	// Look up the destination ports from the annotations on the service
	destinationPorts := annotations[EgressDestinationPorts]
	deniedNetworks := annotations[EgressDeniedNetworks]
	allowedNetworks := annotations[EgressAllowedNetworks]

	protocol := iptables.ProtocolIPv4
	if vip.IsIPv6(podIP) {
		protocol = iptables.ProtocolIPv6
	}

	i, err := vip.CreateIptablesClient(config.EgressWithNftables, namespace, protocol)
	if err != nil {
		return fmt.Errorf("error Creating iptables client [%s]", err)
	}

	if deniedNetworks != "" {
		networks := strings.Split(deniedNetworks, ",")
		for x := range networks {
			err = i.DeleteMangleReturnForNetwork(vip.MangleChainName, networks[x])
			if err != nil {
				return fmt.Errorf("error deleting rules in mangle chain [%s], error [%s]", vip.MangleChainName, err)
			}
		}
	}

	if allowedNetworks != "" {
		networks := strings.Split(allowedNetworks, ",")
		for x := range networks {
			err = i.DeleteMangleMarkingForNetwork(podIP, vip.MangleChainName, networks[x])
			if err != nil {
				return fmt.Errorf("error deleting rules in mangle chain [%s], error [%s]", vip.MangleChainName, err)
			}
		}
	} else {
		// Remove the marking of egress packets
		err = i.DeleteMangleMarking(podIP, vip.MangleChainName)
		if err != nil {
			return fmt.Errorf("error changing iptables rules for egress [%s]", err)
		}
	}

	// Clear up SNAT rules
	if destinationPorts != "" {
		fixedPorts := strings.Split(destinationPorts, ",")

		for _, fixedPort := range fixedPorts {
			var proto, port string

			data := strings.Split(fixedPort, ":")
			if len(data) == 0 {
				continue
			} else if len(data) == 1 {
				proto = "tcp"
				port = data[0]
			} else {
				proto = data[0]
				port = data[1]
			}

			err = i.DeleteSourceNatForDestinationPort(podIP, vipIP, port, proto)
			if err != nil {
				return fmt.Errorf("error changing iptables rules for egress [%s]", err)
			}

		}
	} else {
		err = i.DeleteSourceNat(podIP, vipIP)
		if err != nil {
			return fmt.Errorf("error changing iptables rules for egress [%s]", err)
		}
	}

	err = vip.DeleteExistingSessions(podIP, false, destinationPorts, "")
	if err != nil {
		return fmt.Errorf("error changing iptables rules for egress [%s]", err)
	}
	return nil
}

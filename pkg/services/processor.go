package services

import (
	"context"
	"fmt"
	log "log/slog"
	"reflect"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type Processor struct {
	config        *kubevip.Config
	lbClassFilter func(svc *v1.Service, config *kubevip.Config) bool

	// TODO: Fix the naming of these contexts

	// activeServiceLoadBalancer keeps track of services that already have a leaderElection in place
	activeServiceLoadBalancer map[string]context.Context

	// activeServiceLoadBalancer keeps track of services that already have a leaderElection in place
	activeServiceLoadBalancerCancel map[string]func()

	// activeService keeps track of services that already have a leaderElection in place
	activeService map[string]bool

	// watchedService keeps track of services that are already being watched
	watchedService map[string]bool

	// Keeps track of all running instances
	serviceInstances []*instance.Instance

	mutex sync.Mutex

	bgpServer *bgp.Server

	// watchedService keeps track of routes that has been configured on the node
	configuredLocalRoutes sync.Map

	serviceFunc func(context.Context, *v1.Service) error

	clientSet *kubernetes.Clientset

	rwClientSet *kubernetes.Clientset

	shutdownChan chan struct{}

	svcLocks map[string]*sync.Mutex
}

func NewServicesProcessor(config *kubevip.Config, serviceInstances []*instance.Instance, bgpServer *bgp.Server,
	serviceFunc func(context.Context, *v1.Service) error, clientSet *kubernetes.Clientset, rwClientSet *kubernetes.Clientset, shutdownChan chan struct{}) *Processor {
	lbClassFilterFunc := lbClassFilter
	if config.LoadBalancerClassLegacyHandling {
		lbClassFilterFunc = lbClassFilterLegacy
	}
	return &Processor{
		config:                          config,
		lbClassFilter:                   lbClassFilterFunc,
		activeServiceLoadBalancer:       make(map[string]context.Context),
		activeServiceLoadBalancerCancel: make(map[string]func()),
		activeService:                   make(map[string]bool),
		watchedService:                  make(map[string]bool),
		serviceInstances:                serviceInstances,
		bgpServer:                       bgpServer,
		serviceFunc:                     serviceFunc,
		clientSet:                       clientSet,
		rwClientSet:                     rwClientSet,
		shutdownChan:                    shutdownChan,
		svcLocks:                        make(map[string]*sync.Mutex),
	}
}

func (p *Processor) AddOrModify(ctx context.Context, event watch.Event) (bool, error) {

	// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
	svc, ok := event.Object.(*v1.Service)
	if !ok {
		return false, fmt.Errorf("unable to parse Kubernetes services from API watcher")
	}

	// We only care about LoadBalancer services
	if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
		return true, nil
	}

	// Check if we ignore this service
	if svc.Annotations["kube-vip.io/ignore"] == "true" {
		log.Info("ignore annotation for kube-vip", "service name", svc.Name)
		return true, nil
	}

	// Select loadbalancer class filtering function

	// Check the loadBalancer class
	if p.filterLBClass(svc) {
		return true, nil
	}

	svcAddresses := instance.FetchServiceAddresses(svc)

	// We only care about LoadBalancer services that have been allocated an address
	if len(svcAddresses) <= 0 {
		return true, nil
	}

	// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
	if event.Type == watch.Modified {
		for _, addr := range svcAddresses {
			// log.Debugf("(svcs) Retreiving local addresses, to ensure that this modified address doesn't exist: %s", addr)
			f, err := vip.GarbageCollect(p.config.Interface, addr)
			if err != nil {
				log.Error("(svcs) cleaning existing address error", "err", err)
			}
			if f {
				log.Warn("(svcs) already found existing config", "address", addr, "adapter", p.config.Interface)
			}
		}
		// This service has been modified, but it was also active..
		if p.activeService[string(svc.UID)] {

			i := instance.FindServiceInstance(svc, p.serviceInstances)
			if i != nil {
				originalService := instance.FetchServiceAddresses(i.ServiceSnapshot)
				newService := instance.FetchServiceAddresses(svc)
				if !reflect.DeepEqual(originalService, newService) {

					// Calls the cancel function of the context
					if p.activeServiceLoadBalancerCancel[string(svc.UID)] != nil {
						log.Warn("(svcs) The load balancer has changed, cancelling original load balancer")
						p.activeServiceLoadBalancerCancel[string(svc.UID)]()
						<-p.activeServiceLoadBalancer[string(svc.UID)].Done()
						log.Warn("(svcs) waiting for load balancer to finish")
					}
					err := p.deleteService(svc.UID)
					if err != nil {
						log.Error("(svc) unable to remove", "service", svc.UID)
					}
					p.activeService[string(svc.UID)] = false
					p.watchedService[string(svc.UID)] = false
					delete(p.activeServiceLoadBalancer, string(svc.UID))
					p.configuredLocalRoutes.Store(string(svc.UID), false)
				}
				// in theory this should never fail

			}

		}
	}

	// Architecture walkthrough: (Had to do this as this code path is making my head hurt)

	// Is the service active (bool), if not then process this new service
	// Does this service use an election per service?
	//

	if !p.activeService[string(svc.UID)] {
		log.Debug("(svcs) has been added/modified with addresses", "service name", svc.Name, "ip", instance.FetchServiceAddresses(svc))

		p.activeServiceLoadBalancer[string(svc.UID)], p.activeServiceLoadBalancerCancel[string(svc.UID)] = context.WithCancel(ctx)

		if p.config.EnableServicesElection || // Service Election
			((p.config.EnableRoutingTable || p.config.EnableBGP) && // Routing table mode or BGP
				(!p.config.EnableLeaderElection && !p.config.EnableServicesElection)) { // No leaderelection or services election

			// If this load balancer Traffic Policy is "local"
			if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {

				// Start an endpoint watcher if we're not watching it already
				if !p.watchedService[string(svc.UID)] {
					// background the endpoint watcher
					go func() {
						if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
							// Add Endpoint or EndpointSlices watcher
							// var provider providers.Provider
							// if !p.config.EnableEndpointSlices {
							// 	provider = providers.NewEndpoints()
							// } else {
							// 	provider = providers.NewEndpointslices()
							// }
							// if err := sm.watchEndpoint(p.activeServiceLoadBalancer[string(svc.UID)], p.config.NodeName, svc, provider); err != nil {
							// 	log.Error(err.Error())
							// }
						}
					}()

					if (p.config.EnableRoutingTable || p.config.EnableBGP) && (!p.config.EnableLeaderElection && !p.config.EnableServicesElection) {
						go func() {
							err := p.serviceFunc(p.activeServiceLoadBalancer[string(svc.UID)], svc)
							if err != nil {
								log.Error(err.Error())
							}
						}()
					}
					// We're now watching this service
					p.watchedService[string(svc.UID)] = true
				}
			} else if (p.config.EnableBGP || p.config.EnableRoutingTable) && (!p.config.EnableLeaderElection && !p.config.EnableServicesElection) {
				go func() {
					if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
						// Add Endpoint watcher
						var provider providers.Provider
						if !p.config.EnableEndpointSlices {
							provider = providers.NewEndpoints()
						} else {
							provider = providers.NewEndpointslices()
						}
						if err := p.watchEndpoint(p.activeServiceLoadBalancer[string(svc.UID)], p.config.NodeName, svc, provider); err != nil {
							log.Error(err.Error())
						}
					}
				}()

				go func() {
					err := p.serviceFunc(p.activeServiceLoadBalancer[string(svc.UID)], svc)
					if err != nil {
						log.Error(err.Error())
					}
				}()
			} else {

				go func() {
					for {
						select {
						case <-p.activeServiceLoadBalancer[string(svc.UID)].Done():
							log.Warn("(svcs) restartable service watcher ending", "uid", svc.UID)
							return
						default:
							log.Info("(svcs) restartable service watcher starting", "uid", svc.UID)
							err := p.serviceFunc(p.activeServiceLoadBalancer[string(svc.UID)], svc)

							if err != nil {
								log.Error(err.Error())
							}
						}
					}

				}()
			}
		} else {
			// Increment the waitGroup before the service Func is called (Done is completed in there)
			err := p.serviceFunc(p.activeServiceLoadBalancer[string(svc.UID)], svc)
			if err != nil {
				log.Error(err.Error())
			}
		}
		p.activeService[string(svc.UID)] = true
	}

	return false, nil
}

func (p *Processor) Delete(service *v1.Service, id string) error {
	return nil
}

func (p *Processor) filterLBClass(svc *v1.Service) bool {
	return p.lbClassFilter(svc, p.config)
}

func lbClassFilterLegacy(svc *v1.Service, config *kubevip.Config) bool {
	if svc == nil {
		log.Info("(svcs) service is nil, ignoring")
		return true
	}
	if svc.Spec.LoadBalancerClass != nil {
		// if this isn't nil then it has been configured, check if it the kube-vip loadBalancer class
		if *svc.Spec.LoadBalancerClass != config.LoadBalancerClassName {
			log.Info("(svcs) specified the wrong loadBalancer class", "service name", svc.Name, "lbClass", *svc.Spec.LoadBalancerClass)
			return true
		}
	} else if config.LoadBalancerClassOnly {
		// if kube-vip is configured to only recognize services with kube-vip's lb class, then ignore the services without any lb class
		log.Info("(svcs) kube-vip configured to only recognize services with kube-vip's lb class but the service didn't specify any loadBalancer class, ignoring", "service name", svc.Name)
		return true
	}
	return false
}

func lbClassFilter(svc *v1.Service, config *kubevip.Config) bool {
	if svc == nil {
		log.Info("(svcs) service is nil, ignoring")
		return true
	}
	if svc.Spec.LoadBalancerClass == nil && config.LoadBalancerClassName != "" {
		log.Info("(svcs) no loadBalancer class, ignoring", "service name", svc.Name, "expected lbClass", config.LoadBalancerClassName)
		return true
	}
	if svc.Spec.LoadBalancerClass == nil && config.LoadBalancerClassName == "" {
		return false
	}
	if *svc.Spec.LoadBalancerClass != config.LoadBalancerClassName {
		log.Info("(svcs) specified wrong loadBalancer class, ignoring", "service name", svc.Name, "wrong lbClass", *svc.Spec.LoadBalancerClass, "expected lbClass", config.LoadBalancerClassName)
		return true
	}
	return false
}

func (p *Processor) deleteService(uid types.UID) error {
	// protect multiple calls
	p.mutex.Lock()
	defer p.mutex.Unlock()

	var updatedInstances []*instance.Instance
	var serviceInstance *instance.Instance
	found := false
	for x := range p.serviceInstances {
		log.Debug("service lookup", "target UID", uid, "found UID ", p.serviceInstances[x].ServiceSnapshot.UID)
		// Add the running services to the new array
		if p.serviceInstances[x].ServiceSnapshot.UID != uid {
			updatedInstances = append(updatedInstances, p.serviceInstances[x])
		} else {
			// Flip the found when we match
			found = true
			serviceInstance = p.serviceInstances[x]
		}
	}
	// If we've been through all services and not found the correct one then error
	if !found {
		// TODO: - fix UX
		// return fmt.Errorf("unable to find/stop service [%s]", uid)
		return nil
	}

	// Determine if this this VIP is shared with other loadbalancers
	shared := false
	vipSet := make(map[string]interface{})
	for x := range updatedInstances {
		for _, vip := range instance.FetchServiceAddresses(updatedInstances[x].ServiceSnapshot) { //updatedInstances[x].ServiceSnapshot.Spec.LoadBalancerIP {
			vipSet[vip] = nil
		}
	}
	for _, vip := range instance.FetchServiceAddresses(serviceInstance.ServiceSnapshot) {
		if _, found := vipSet[vip]; found {
			shared = true
		}
	}
	if !shared {
		for x := range serviceInstance.Clusters {
			serviceInstance.Clusters[x].Stop()
		}
		if serviceInstance.IsDHCP {
			serviceInstance.DhcpClient.Stop()
			macvlan, err := netlink.LinkByName(serviceInstance.DhcpInterface)
			if err != nil {
				return fmt.Errorf("error finding VIP Interface: %v", err)
			}

			err = netlink.LinkDel(macvlan)
			if err != nil {
				return fmt.Errorf("error deleting DHCP Link : %v", err)
			}
		}
		// TODO: Implement dual-stack loadbalancer support if BGP is enabled
		for i := range serviceInstance.VipConfigs {
			if serviceInstance.VipConfigs[i].EnableBGP {
				cidrVip := fmt.Sprintf("%s/%s", serviceInstance.VipConfigs[i].VIP, serviceInstance.VipConfigs[i].VIPCIDR)
				err := p.bgpServer.DelHost(cidrVip)
				if err != nil {
					return fmt.Errorf("[BGP] error deleting BGP host: %v", err)
				}
				log.Debug("[BGP] delete", "host", cidrVip)
			}
		}

		// We will need to tear down the egress
		if serviceInstance.ServiceSnapshot.Annotations[kubevip.Egress] == "true" {
			if serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint] != "" {
				log.Info("egress re-write enabled", "service", serviceInstance.ServiceSnapshot.Name)
				err := egress.Teardown(serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint], serviceInstance.ServiceSnapshot.Spec.LoadBalancerIP, serviceInstance.ServiceSnapshot.Namespace, serviceInstance.ServiceSnapshot.Annotations, p.config.EgressWithNftables)
				if err != nil {
					log.Error("egress teardown", "err", err)
				}
			}
		}
	}

	// Update the service array
	p.serviceInstances = updatedInstances

	log.Info("Removed instance from manager", "uid", uid, "remaining advertised services", len(p.serviceInstances))

	return nil
}

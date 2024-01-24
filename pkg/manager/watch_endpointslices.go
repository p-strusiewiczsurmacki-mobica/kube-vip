package manager

import (
	"context"
	"fmt"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

func (sm *Manager) watchEndpointSlices(ctx context.Context, id string, service *v1.Service, wg *sync.WaitGroup) error {
	log.Infof("[endpointslices] watching for service [%s] in namespace [%s]", service.Name, service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	leaderContext, cancel := context.WithCancel(context.Background())
	var leaderElectionActive bool
	defer cancel()

	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/service-name": service.Name}}

	opts := metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	}

	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.DiscoveryV1().EndpointSlices(service.Namespace).Watch(ctx, opts)
		},
	})
	if err != nil {
		cancel()
		return fmt.Errorf("[endpointslices] error creating endpointslices watcher: %s", err.Error())
	}

	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			log.Debug("[endpointslices] context cancelled")
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-sm.shutdownChan:
			log.Debug("[endpointslices] shutdown called")
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-exitFunction:
			log.Debug("[endpointslices] function ending")
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		}
	}()

	ch := rw.ResultChan()

	for event := range ch {
		lastKnownGoodEndpoint := ""
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:

			eps, ok := event.Object.(*discoveryv1.EndpointSlice)
			if !ok {
				cancel()
				return fmt.Errorf("[endpointslices] unable to parse Kubernetes services from API watcher")
			}

			// Build endpoints
			var endpoints []string
			if (sm.config.EnableBGP || sm.config.EnableRoutingTable) && !sm.config.EnableLeaderElection && !sm.config.EnableServicesElection &&
				service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
				endpoints = getAllEndpointsFromEndpointslices(eps)
			} else {
				endpoints = getLocalEndpointsFromEndpointslices(eps, id, sm.config)
			}

			sm.processEndpoints(leaderContext, cancel, endpoints, &lastKnownGoodEndpoint, &leaderElectionActive, id, service, wg, "endpointslices")

			log.Debugf("[endpointslices watcher] service %s/%s: local endpoint(s) [%d], known good [%s], active election [%t]",
				service.Namespace, service.Name, len(endpoints), lastKnownGoodEndpoint, leaderElectionActive)

		case watch.Deleted:
			// When no-leade-elecition mode
			if !sm.config.EnableServicesElection && !sm.config.EnableLeaderElection {
				// find all existing local endpoints
				eps, ok := event.Object.(*discoveryv1.EndpointSlice)
				if !ok {
					cancel()
					return fmt.Errorf("unable to parse Kubernetes services from API watcher")
				}

				var endpoints []string
				if (sm.config.EnableBGP || sm.config.EnableRoutingTable) && !sm.config.EnableLeaderElection && !sm.config.EnableServicesElection &&
					service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
					endpoints = getAllEndpointsFromEndpointslices(eps)
				} else {
					endpoints = getLocalEndpointsFromEndpointslices(eps, id, sm.config)
				}

				// If there were local endpints deleted
				if len(endpoints) > 0 {
					// Delete all routes in routing table mode
					if sm.config.EnableRoutingTable {
						sm.clearRoutes(service)
					}

					// Delete all hosts in BGP mode
					if sm.config.EnableBGP {
						sm.clearBGPHosts(service)
					}
				}
			}

			// Close the goroutine that will end the retry watcher, then exit the endpoint watcher function
			close(exitFunction)
			log.Infof("[endpointslices] deleted stopping watching for [%s] in namespace [%s]", service.Name, service.Namespace)
			return nil
		case watch.Error:
			errObject := apierrors.FromObject(event.Object)
			statusErr, _ := errObject.(*apierrors.StatusError)
			log.Errorf("[endpointslices] -> %v", statusErr)
		}
	}
	close(exitFunction)
	log.Infof("[endpointslices] stopping watching for [%s] in namespace [%s]", service.Name, service.Namespace)
	return nil //nolint:govet
}

func getLocalEndpointsFromEndpointslices(eps *discoveryv1.EndpointSlice, id string, config *kubevip.Config) []string {
	var localendpoints []string

	shortname, shortnameErr := getShortname(id)
	if shortnameErr != nil {
		if config.EnableRoutingTable && (!config.EnableLeaderElection && !config.EnableServicesElection) {
			log.Debugf("[endpointslices] %v, shortname will not be used", shortnameErr)
		} else {
			log.Errorf("[endpointslices] %v", shortnameErr)
		}
	}

	for i := range eps.Endpoints {
		for j := range eps.Endpoints[i].Addresses {
			// 1. Compare the hostname on the endpoint to the hostname
			// 2. Compare the nodename on the endpoint to the hostname
			// 3. Drop the FQDN to a shortname and compare to the nodename on the endpoint

			// 1. Compare the Hostname first (should be FQDN)
			log.Debugf("[endpointslices] processing endpoint [%s]", eps.Endpoints[i].Addresses[j])
			if eps.Endpoints[i].Hostname != nil && id == *eps.Endpoints[i].Hostname {
				if *eps.Endpoints[i].Conditions.Serving {
					log.Debugf("[endpointslices] found endpoint - address: %s, hostname: %s", eps.Endpoints[i].Addresses[j], *eps.Endpoints[i].Hostname)
					localendpoints = append(localendpoints, eps.Endpoints[i].Addresses[j])
				}
			} else {
				// 2. Compare the Nodename (from testing could be FQDN or short)
				if eps.Endpoints[i].NodeName != nil {
					if id == *eps.Endpoints[i].NodeName && *eps.Endpoints[i].Conditions.Serving {
						if eps.Endpoints[i].Hostname != nil {
							log.Debugf("[endpointslices] found endpoint - address: %s, hostname: %s, node: %s", eps.Endpoints[i].Addresses[j], *eps.Endpoints[i].Hostname, *eps.Endpoints[i].NodeName)
						} else {
							log.Debugf("[endpointslices] found endpoint - address: %s, node: %s", eps.Endpoints[i].Addresses[j], *eps.Endpoints[i].NodeName)
						}
						localendpoints = append(localendpoints, eps.Endpoints[i].Addresses[j])
					} else if shortnameErr != nil && shortname == *eps.Endpoints[i].NodeName && *eps.Endpoints[i].Conditions.Serving {
						log.Debugf("[endpointslices] found endpoint - address: %s, shortname: %s, node: %s", eps.Endpoints[i].Addresses[j], shortname, *eps.Endpoints[i].NodeName)
						localendpoints = append(localendpoints, eps.Endpoints[i].Addresses[j])

					}
				}
			}
		}
	}
	return localendpoints
}

func getAllEndpointsFromEndpointslices(eps *discoveryv1.EndpointSlice) []string {
	endpoints := []string{}
	for _, ep := range eps.Endpoints {
		endpoints = append(endpoints, ep.Addresses...)
	}
	return endpoints
}

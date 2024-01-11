package manager

import (
	"context"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
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
		activeEndpointAnnotation := activeEndpoint
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:

			eps, ok := event.Object.(*discoveryv1.EndpointSlice)
			if !ok {
				cancel()
				return fmt.Errorf("[endpointslices] unable to parse Kubernetes services from API watcher")
			}

			if eps.AddressType == discoveryv1.AddressTypeIPv6 {
				activeEndpointAnnotation = activeEndpointIPv6
			}

			// Build endpoints
			localendpoints := getLocalEndpointsFromEndpointslices(eps, id)

			// Find out if we have any local endpoints
			// if out endpoint is empty then populate it
			// if not, go through the endpoints and see if ours still exists
			// If we have a local endpoint then begin the leader Election, unless it's already running
			//

			// Check that we have local endpoints
			if len(localendpoints) != 0 {
				// if we haven't populated one, then do so
				if lastKnownGoodEndpoint != "" {

					// check out previous endpoint exists
					stillExists := false

					for x := range localendpoints {
						if localendpoints[x] == lastKnownGoodEndpoint {
							stillExists = true
						}
					}
					// If the last endpoint no longer exists, we cancel our leader Election
					if !stillExists && leaderElectionActive {
						log.Warnf("[endpointslices] existing endpoint [%s] has been removed, restarting leaderElection", lastKnownGoodEndpoint)
						// Stop the existing leaderElection
						cancel()
						// Set our active endpoint to an existing one
						lastKnownGoodEndpoint = localendpoints[0]
						// disable last leaderElection flag
						leaderElectionActive = false
					}

				} else {
					lastKnownGoodEndpoint = localendpoints[0]
				}

				// Set the service accordingly
				if service.Annotations[egress] == "true" {
					service.Annotations[activeEndpointAnnotation] = lastKnownGoodEndpoint
				}

				if !leaderElectionActive && sm.config.EnableServicesElection {
					go func() {
						leaderContext, cancel = context.WithCancel(context.Background())

						// This is a blocking function, that will restart (in the event of failure)
						for {
							// if the context isn't cancelled restart
							if leaderContext.Err() != context.Canceled {
								leaderElectionActive = true
								err = sm.StartServicesLeaderElection(leaderContext, service, wg)
								if err != nil {
									log.Error(err)
								}
								leaderElectionActive = false
							} else {
								leaderElectionActive = false
								break
							}
						}
					}()
				}

				// There are local endpoints available on the node, therefore route(s) should be added to the table
				if !sm.config.EnableServicesElection && !sm.config.EnableLeaderElection && sm.config.EnableRoutingTable && !configuredLocalRoutes[string(service.UID)] {
					if instance := sm.findServiceInstance(service); instance != nil {
						for _, cluster := range instance.clusters {
							err := cluster.Network.AddRoute()
							if err != nil {
								log.Errorf("[endpointslices] error adding route: %s\n", err.Error())
							} else {
								log.Infof("[endpointslices] added route: %s, service: %s/%s, interface: %s, table: %d",
									cluster.Network.IP(), service.Namespace, service.Name, cluster.Network.Interface(), sm.config.RoutingTableID)
							}
						}
					}
					configuredLocalRoutes[string(service.UID)] = true
					leaderElectionActive = true
				}
			} else {
				// There are no local enpoints - routes should be deleted
				if !sm.config.EnableServicesElection && !sm.config.EnableLeaderElection && sm.config.EnableRoutingTable && configuredLocalRoutes[string(service.UID)] {
					sm.clearRoutes(service)
					configuredLocalRoutes[string(service.UID)] = false
					leaderElectionActive = false
				}

				// If there are no local endpoints, and we had one then remove it and stop the leaderElection
				if lastKnownGoodEndpoint != "" {
					log.Warnf("[endpointslices] existing endpoint [%s] has been removed, no remaining endpoints for leaderElection", lastKnownGoodEndpoint)
					lastKnownGoodEndpoint = "" // reset endpoint
					cancel()                   // stop services watcher
					leaderElectionActive = false
				}
			}
			log.Debugf("[endpointslices watcher] local endpoint(s) [%d], known good [%s], active election [%t]", len(localendpoints), lastKnownGoodEndpoint, leaderElectionActive)

		case watch.Deleted:
			if !sm.config.EnableServicesElection && !sm.config.EnableLeaderElection && sm.config.EnableRoutingTable {
				ep, ok := event.Object.(*v1.Endpoints)
				if !ok {
					cancel()
					return fmt.Errorf("[endpointslices] unable to parse Kubernetes services from API watcher")
				}
				localEndpoints := getLocalEndpoints(ep, id)
				if len(localEndpoints) > 0 {
					sm.clearRoutes(service)
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

func (sm *Manager) updateServiceEndpointSlicesAnnotation(endpoint, endpointIPv6 string, service *v1.Service) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := sm.clientSet.CoreV1().Services(service.Namespace).Get(context.TODO(), service.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		currentServiceCopy.Annotations[activeEndpoint] = endpoint
		currentServiceCopy.Annotations[activeEndpointIPv6] = endpointIPv6

		_, err = sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("Error updating Service Spec [%s] : %v", currentServiceCopy.Name, err)
			return err
		}
		return nil
	})

	if retryErr != nil {
		log.Errorf("Failed to set Services: %v", retryErr)
		return retryErr
	}
	return nil
}

func getLocalEndpointsFromEndpointslices(eps *discoveryv1.EndpointSlice, id string) []string {
	var localendpoints []string
	for i := range eps.Endpoints {
		for j := range eps.Endpoints[i].Addresses {
			// 1. Compare the hostname on the endpoint to the hostname
			// 2. Compare the nodename on the endpoint to the hostname
			// 3. Drop the FQDN to a shortname and compare to the nodename on the endpoint

			// 1. Compare the Hostname first (should be FQDN)
			if eps.Endpoints[i].Hostname != nil && id == *eps.Endpoints[i].Hostname {
				log.Debugf("[endpointslices] address: %s, hostname: %s", eps.Endpoints[i].Addresses[j], *eps.Endpoints[i].Hostname)
				if *eps.Endpoints[i].Conditions.Serving {
					localendpoints = append(localendpoints, eps.Endpoints[i].Addresses[j])
				}
			} else {
				// 2. Compare the Nodename (from testing could be FQDN or short)
				if eps.Endpoints[i].NodeName != nil {
					if eps.Endpoints[i].Hostname != nil {
						log.Debugf("[endpointslices] address: %s, hostname: %s, node: %s", eps.Endpoints[i].Addresses[j], *eps.Endpoints[i].Hostname, *eps.Endpoints[i].NodeName)
					} else {
						log.Debugf("[endpointslices] address: %s, node: %s", eps.Endpoints[i].Addresses[j], *eps.Endpoints[i].NodeName)
					}

					if id == *eps.Endpoints[i].NodeName && *eps.Endpoints[i].Conditions.Serving {
						localendpoints = append(localendpoints, eps.Endpoints[i].Addresses[j])
					} else {
						// 3. Compare to shortname
						shortname, err := getShortname(id)
						if err != nil {
							log.Errorf("[endpointslices] %v", err)
						} else {
							log.Debugf("[endpointslices] address: %s, shortname: %s, node: %s", eps.Endpoints[i].Addresses[j], shortname, *eps.Endpoints[i].NodeName)

							if shortname == *eps.Endpoints[i].NodeName && *eps.Endpoints[i].Conditions.Serving {
								localendpoints = append(localendpoints, eps.Endpoints[i].Addresses[j])
							}
						}
					}
				}
			}
		}
	}
	return localendpoints
}

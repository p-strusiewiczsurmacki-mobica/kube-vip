package services

import (
	"context"
	"fmt"

	log "log/slog"

	"github.com/davecgh/go-spew/spew"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/trafficmirror"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

// This function handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (p *Processor) ServicesWatcher(ctx context.Context, serviceFunc func(context.Context, *v1.Service) error) error {
	// first start port mirroring if enabled
	if err := p.startTrafficMirroringIfEnabled(); err != nil {
		return err
	}
	defer func() {
		// clean up traffic mirror related config
		err := p.stopTrafficMirroringIfEnabled()
		if err != nil {
			log.Error("Stopping traffic mirroring", "err", err)
		}
	}()

	if p.config.ServiceNamespace == "" {
		// v1.NamespaceAll is actually "", but we'll stay with the const in case things change upstream
		p.config.ServiceNamespace = v1.NamespaceAll
		log.Info("(svcs) starting services watcher for all namespaces")
	} else {
		log.Info("(svcs) starting services watcher", "namespace", p.config.ServiceNamespace)
	}

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return p.rwClientSet.CoreV1().Services(p.config.ServiceNamespace).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return fmt.Errorf("error creating services watcher: %s", err.Error())
	}
	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-p.shutdownChan:
			log.Debug("(svcs) shutdown called")
			// Stop the retry watcher
			rw.Stop()
			return
		case <-exitFunction:
			log.Debug("(svcs) function ending")
			// Stop the retry watcher
			rw.Stop()
			return
		}
	}()
	ch := rw.ResultChan()

	// Used for tracking an active endpoint / pod
	for event := range ch {
		p.CountServiceWatchEvent.With(prometheus.Labels{"type": string(event.Type)}).Add(1)

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			restart, err := p.AddOrModify(ctx, event, serviceFunc)
			if err != nil {
				return fmt.Errorf("add/modify service error: %w", err)
			}
			if restart {
				break
			}
		case watch.Deleted:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("unable to parse Kubernetes services from API watcher")
			}
			if p.activeService[string(svc.UID)] {

				// We only care about LoadBalancer services
				if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
					break
				}

				// We can ignore this service
				if svc.Annotations["kube-vip.io/ignore"] == "true" {
					log.Info("(svcs)ignore annotation for kube-vip", "service name", svc.Name)
					break
				}

				isRouteConfigured, err := endpoints.IsRouteConfigured(svc.UID, &p.configuredLocalRoutes)
				if err != nil {
					return fmt.Errorf("error while checkig if route is configured: %w", err)
				}
				// If no leader election is enabled, delete routes here
				if !p.config.EnableLeaderElection && !p.config.EnableServicesElection &&
					p.config.EnableRoutingTable && isRouteConfigured {
					if errs := endpoints.ClearRoutes(svc, &p.ServiceInstances); len(errs) == 0 {
						p.configuredLocalRoutes.Store(string(svc.UID), false)
					}
				}

				// If this is an active service then and additional leaderElection will handle stopping
				err = p.deleteService(svc.UID)
				if err != nil {
					log.Error(err.Error())
				}

				// Calls the cancel function of the context
				if p.activeServiceLoadBalancerCancel[string(svc.UID)] != nil {
					p.activeServiceLoadBalancerCancel[string(svc.UID)]()
				}
				p.activeService[string(svc.UID)] = false
				p.watchedService[string(svc.UID)] = false
			}

			if (p.config.EnableBGP || p.config.EnableRoutingTable) && p.config.EnableLeaderElection && !p.config.EnableServicesElection {
				if p.config.EnableBGP {
					instance := instance.FindServiceInstance(svc, p.ServiceInstances)
					for _, vip := range instance.VipConfigs {
						vipCidr := fmt.Sprintf("%s/%s", vip.VIP, vip.VIPCIDR)
						err = p.bgpServer.DelHost(vipCidr)
						if err != nil {
							log.Error("error deleting host", "ip", vipCidr, "err", err)
						}
					}
				} else {
					endpoints.ClearRoutes(svc, &p.ServiceInstances)
				}
			}

			log.Info("(svcs) deleted", "service name", svc.Name, "namespace", svc.Namespace)
		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes services")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Error(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
			}

			status := statusErr.ErrStatus
			log.Error("services", "err", status)
		default:
		}
	}
	close(exitFunction)
	log.Warn("Stopping watching services for type: LoadBalancer in all namespaces")
	return nil
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

func (p *Processor) serviceInterface() string {
	svcIf := p.config.Interface
	if p.config.ServicesInterface != "" {
		svcIf = p.config.ServicesInterface
	}
	return svcIf
}

func (p *Processor) startTrafficMirroringIfEnabled() error {
	if p.config.MirrorDestInterface != "" {
		svcIf := p.serviceInterface()
		log.Info("mirroring traffic", "src", svcIf, "dest", p.config.MirrorDestInterface)
		if err := trafficmirror.MirrorTrafficFromNIC(svcIf, p.config.MirrorDestInterface); err != nil {
			return err
		}
	} else {
		log.Debug("skip starting traffic mirroring since it's not enabled.")
	}
	return nil
}

func (p *Processor) stopTrafficMirroringIfEnabled() error {
	if p.config.MirrorDestInterface != "" {
		svcIf := p.serviceInterface()
		log.Info("clean up qdisc config", "interface", svcIf)
		if err := trafficmirror.CleanupQDSICFromNIC(svcIf); err != nil {
			return err
		}
	} else {
		log.Debug("skip stopping traffic mirroring since it's not enabled.")
	}
	return nil
}

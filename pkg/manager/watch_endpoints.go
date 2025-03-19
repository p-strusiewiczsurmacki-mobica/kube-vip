package manager

import (
	"context"
	"fmt"
	"strings"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
)

func (sm *Manager) watchEndpoint(ctx context.Context, id string, service *v1.Service, provider providers.Provider) error {
	log.Info("watching", "provider", provider.GetLabel(), "service_name", service.Name, "namespace", service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues

	leaderCtx, cancel := context.WithCancel(svcCtx.ctx)
	defer cancel()

	var leaderElectionActive bool

	rw, err := provider.CreateRetryWatcher(leaderCtx, sm.rwClientSet, service)
	if err != nil {
		return fmt.Errorf("[%s] error watching endpoints: %w", provider.getLabel(), err)
	}

	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-svcCtx.ctx.Done():
			log.Debug("context cancelled", "provider", provider.getLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-sm.shutdownChan:
			log.Debug("shutdown called", "provider", provider.getLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		case <-exitFunction:
			log.Debug("function ending", "provider", provider.getLabel())
			// Stop the retry watcher
			rw.Stop()
			// Cancel the context, which will in turn cancel the leadership
			cancel()
			return
		}
	}()

	ch := rw.ResultChan()

	epProcessor := endpoints.NewEndpointProcessor(sm.config, provider, sm.bgpServer, &sm.serviceInstances, &configuredLocalRoutes)

	var lastKnownGoodEndpoint string
	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {

		case watch.Added, watch.Modified:
			restart, err := epProcessor.AddOrModify(ctx, event, cancel, &lastKnownGoodEndpoint, service, id, &leaderElectionActive, sm.StartServicesLeaderElection)
			if restart {
				continue
			} else if err != nil {
				return fmt.Errorf("[%s] error while processing add/modify event: %w", provider.GetLabel(), err)
			}

		case watch.Deleted:
			if err := epProcessor.Delete(service, id); err != nil {
				return fmt.Errorf("[%s] error while processing delete event: %w", provider.GetLabel(), err)
			}

			// Close the goroutine that will end the retry watcher, then exit the endpoint watcher function
			close(exitFunction)
			log.Info("stopping watching", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)

			return nil
		case watch.Error:
			errObject := apierrors.FromObject(event.Object)
			statusErr, _ := errObject.(*apierrors.StatusError)
			log.Error("watch error", "provider", provider.GetLabel(), "err", statusErr)
		}
	}
	close(exitFunction)
	log.Info("stopping watching", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)
	return nil //nolint:govet
}

func (svcCtx *serviceContext) isNetworkConfigured(ip string) bool {
	_, exists := svcCtx.configuredNetworks.Load(ip)
	return exists
}

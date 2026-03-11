package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"
)

type debauncer struct {
	input  <-chan watch.Event
	output chan watch.Event
	closed chan any
}

func newDebauncer(input <-chan watch.Event) *debauncer {
	return &debauncer{
		input:  input,
		output: make(chan watch.Event),
		closed: make(chan any),
	}
}

func (d *debauncer) Run(ctx context.Context) {
	t := time.NewTicker(time.Second * 2)
	defer t.Stop()
	var event *watch.Event

	for {
		select {
		case <-ctx.Done():
			close(d.closed)
			return
		case tmp := <-d.input:
			log.Debug("EVENT", "got", tmp)
			event = &tmp
			t.Reset(time.Second * 2)
		case <-t.C:
			if event != nil {
				d.output <- *event
				event = nil
			}

		}
	}
}

func (p *Processor) watchEndpoint(svcCtx *servicecontext.Context, id string, service *v1.Service, provider providers.Provider) error {
	log.Info("watching", "provider", provider.GetLabel(), "service_name", service.Name, "namespace", service.Namespace)
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues

	rw, err := provider.CreateRetryWatcher(svcCtx.Ctx, p.rwClientSet, service)
	if err != nil {
		return fmt.Errorf("[%s] error watching endpoints: %w", provider.GetLabel(), err)
	}

	ch := rw.ResultChan()

	debauncer := newDebauncer(ch)

	wg := sync.WaitGroup{}

	defer func() {
		rw.Stop()
		wg.Wait()
	}()

	wg.Go(func() {
		debauncer.Run(svcCtx.Ctx)
		<-svcCtx.Ctx.Done()
		log.Debug("context cancelled", "provider", provider.GetLabel())
		<-debauncer.closed
		rw.Stop()
	})

	epProcessor := endpoints.NewEndpointProcessor(p.config, provider, p.bgpServer, &p.ServiceInstances, p.leaseMgr, p.TunnelMgr)

	var lastKnownGoodEndpoint string
	for event := range debauncer.output {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {

		case watch.Added, watch.Modified:
			log.Debug("EVENT EXECUTION - ADD OR MODIFY")
			restart, err := epProcessor.AddOrModify(svcCtx, event, &lastKnownGoodEndpoint, service, id, p.StartServicesLeaderElection, &wg)
			if restart {
				continue
			} else if err != nil {
				return fmt.Errorf("[%s] error while processing add/modify event: %w", provider.GetLabel(), err)
			}
		case watch.Deleted:
			log.Debug("EVENT EXECUTION - DELETED")
			if err := epProcessor.Delete(svcCtx.Ctx, service, id); err != nil {
				return fmt.Errorf("[%s] error while processing delete event: %w", provider.GetLabel(), err)
			}

			log.Info("stopping watching", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)
			return nil
		case watch.Error:
			log.Debug("EVENT EXECUTION - ERROR")
			errObject := apierrors.FromObject(event.Object)
			statusErr, _ := errObject.(*apierrors.StatusError)
			log.Error("watch error", "provider", provider.GetLabel(), "err", statusErr)
		}
	}
	log.Info("stopping watching", "provider", provider.GetLabel(), "service name", service.Name, "namespace", service.Namespace)
	return nil //nolint:govet
}

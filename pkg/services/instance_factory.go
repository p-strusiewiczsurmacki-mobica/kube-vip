package services

import (
	"context"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node"
	"github.com/kube-vip/kube-vip/pkg/route"
	v1 "k8s.io/api/core/v1"
)

type serviceInstanceFactory interface {
	Create(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error)
}

type defaultServiceInstanceFactory struct {
	config           *kubevip.Config
	interfaceManager *networkinterface.Manager
	arpManager       *arp.Manager
	routeManager     *route.Manager
	labelManager     node.Labeler
}

func newServiceInstanceFactory(config *kubevip.Config, interfaceManager *networkinterface.Manager,
	arpManager *arp.Manager, routeManager *route.Manager, labelManager node.Labeler) serviceInstanceFactory {
	return &defaultServiceInstanceFactory{
		config:           config,
		interfaceManager: interfaceManager,
		arpManager:       arpManager,
		routeManager:     routeManager,
		labelManager:     labelManager,
	}
}

func (f *defaultServiceInstanceFactory) Create(ctx context.Context, service *v1.Service,
	wg *sync.WaitGroup) (*instance.Instance, error) {
	return instance.NewInstance(ctx, service, f.config, f.interfaceManager, f.arpManager,
		f.routeManager, f.labelManager, wg)
}

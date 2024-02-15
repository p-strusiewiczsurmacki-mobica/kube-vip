package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/kube-vip/kube-vip/pkg/vip"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

func (sm *Manager) watchRoutes(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			err := sm.checkRoutes()
			if err != nil {
				log.Warn("[routes] issue while watching routes: %w", err)
			}
			time.Sleep(time.Second * 10)
		}
	}
}

func (sm *Manager) checkRoutes() error {
	routes, err := vip.GetAllRoutes(sm.config.RoutingTableID, sm.config.RoutingProtocol)
	if err != nil {
		return fmt.Errorf("error while checking routes: %w", err)
	}
	for i := range routes {
		found := false
		for _, instance := range sm.serviceInstances {
			for _, cluster := range instance.clusters {
				for n := range cluster.Network {
					r := cluster.Network[n].PrepareRoute()
					if r.Dst.String() == routes[i].Dst.String() {
						found = true
					}
				}
			}
		}
		if !found {
			err = netlink.RouteDel(&(routes[i]))
			if err != nil {
				return fmt.Errorf("error deleting route [%s], table [%d], protocol [%d]: %w",
					routes[i].Dst.String(), sm.config.RoutingTableID, sm.config.RoutingProtocol, err)
			}
		}
	}
	return nil
}

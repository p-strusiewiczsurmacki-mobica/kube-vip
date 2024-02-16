package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/kube-vip/kube-vip/pkg/vip"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

const defaultSleepDuration = time.Second * 10

func (sm *Manager) watchRoutes(ctx context.Context) {
	log.Debugf("[routes] started watcher")
	for {
		time.Sleep(defaultSleepDuration)
		select {
		case <-ctx.Done():
			log.Debugf("[routes] context cancelled")
			return
		default:
			err := sm.checkRoutes()
			if err != nil {
				log.Warnf("[routes] issue while watching routes: %v", err)
			}
		}
	}
}

func (sm *Manager) checkRoutes() error {
	routes, err := vip.GetRoutes(sm.config.RoutingTableID, sm.config.RoutingProtocol)
	if err != nil {
		return fmt.Errorf("error getting routes: %w", err)
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
			log.Debugf("[route] deleted route: %v", routes[i])
		}
	}
	return nil
}

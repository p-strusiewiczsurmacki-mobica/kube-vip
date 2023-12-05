package manager

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/kube-vip/kube-vip/pkg/vip"
)

const (
	hwAddrKey                = "kube-vip.io/hwaddr"
	requestedIP              = "kube-vip.io/requestedIP"
	vipHost                  = "kube-vip.io/vipHost"
	egress                   = "kube-vip.io/egress"
	egressDestinationPorts   = "kube-vip.io/egress-destination-ports"
	egressSourcePorts        = "kube-vip.io/egress-source-ports"
	activeEndpoint           = "kube-vip.io/active-endpoint"
	activeEndpointIPv6       = "kube-vip.io/active-endpoint-ipv6"
	flushContrack            = "kube-vip.io/flush-conntrack"
	loadbalancerIPAnnotation = "kube-vip.io/loadbalancerIPs"
	loadbalancerHostname     = "kube-vip.io/loadbalancerHostname"
)

func (sm *Manager) syncServices(_ context.Context, svc *v1.Service, wg *sync.WaitGroup) error {
	defer wg.Done()

	log.Debugf("[STARTING] Service Sync")

	// Iterate through the synchronising services
	foundInstance := false
	newServiceAddresses := fetchServiceAddress(svc)
	newServiceUID := string(svc.UID)

	ingressIPs := []string{}

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		ingressIPs = append(ingressIPs, ingress.IP)
	}

	shouldBreake := false

	for x := range sm.serviceInstances {
		if shouldBreake {
			break
		}
		for _, newServiceAddress := range newServiceAddresses {
			log.Debugf("isDHCP: %t, newServiceAddress: %s", sm.serviceInstances[x].isDHCP, newServiceAddress)
			if sm.serviceInstances[x].UID == newServiceUID {
				// If the found instance's DHCP configuration doesn't match the new service, delete it.
				if (sm.serviceInstances[x].isDHCP && newServiceAddress != "0.0.0.0") ||
					(!sm.serviceInstances[x].isDHCP && newServiceAddress == "0.0.0.0") ||
					(!sm.serviceInstances[x].isDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, newServiceAddress)) ||
					(len(svc.Status.LoadBalancer.Ingress) > 0 && !comparePortsAndPortStatuses(svc)) ||
					(sm.serviceInstances[x].isDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, sm.serviceInstances[x].dhcpInterfaceIP)) {
					if err := sm.deleteService(newServiceUID); err != nil {
						return err
					}
					shouldBreake = true
					break
				}
				foundInstance = true
			}
		}
	}

	// This instance wasn't found, we need to add it to the manager
	if !foundInstance && len(newServiceAddresses) > 0 {
		if err := sm.addService(svc); err != nil {
			return err
		}
	}

	return nil
}

func comparePortsAndPortStatuses(svc *v1.Service) bool {
	portsStatus := svc.Status.LoadBalancer.Ingress[0].Ports
	if len(portsStatus) != len(svc.Spec.Ports) {
		return false
	}
	for i, portSpec := range svc.Spec.Ports {
		if portsStatus[i].Port != portSpec.Port || portsStatus[i].Protocol != portSpec.Protocol {
			return false
		}
	}
	return true
}

func (sm *Manager) addService(svc *v1.Service) error {
	startTime := time.Now()

	newService, err := NewInstance(svc, sm.config)
	if err != nil {
		return err
	}

	log.Infof("(svcs) adding VIP [%s] for [%s/%s]", newService.Vip, newService.serviceSnapshot.Namespace, newService.serviceSnapshot.Name)

	newService.cluster.StartLoadBalancerService(newService.vipConfig, sm.bgpServer)

	sm.upnpMap(newService)

	if newService.isDHCP {
		go func() {
			for ip := range newService.dhcpClient.IPChannel() {
				log.Debugf("IP %s may have changed", ip)
				newService.vipConfig.VIP = ip
				newService.dhcpInterfaceIP = ip
				if !sm.config.EnableRoutingTable || (sm.config.EnableRoutingTable && !sm.config.DisableServiceUpdates) {
					if err := sm.updateStatus(newService); err != nil {
						log.Warnf("error updating svc: %s", err)
					}
				}
			}
			log.Debugf("IP update channel closed, stopping")
		}()
	}

	sm.serviceInstances = append(sm.serviceInstances, newService)

	if !sm.config.DisableServiceUpdates {
		log.Infof("(svcs) will update [%s/%s]", newService.serviceSnapshot.Namespace, newService.serviceSnapshot.Name)
		if err := sm.updateStatus(newService); err != nil {
			// delete service to collect garbage
			if deleteErr := sm.deleteService(newService.UID); err != nil {
				return deleteErr
			}
			return err
		}
	}

	serviceIPs := fetchServiceAddress(svc)

	// Check if we need to flush any conntrack connections (due to some dangling conntrack connections)
	if svc.Annotations[flushContrack] == "true" {
		log.Debugf("Flushing conntrack rules for service [%s]", svc.Name)
		for _, serviceIP := range serviceIPs {
			err = vip.DeleteExistingSessions(serviceIP, false)
			if err != nil {
				log.Errorf("Error flushing any remaining egress connections [%s]", err)
			}
			err = vip.DeleteExistingSessions(serviceIP, true)
			if err != nil {
				log.Errorf("Error flushing any remaining ingress connections [%s]", err)
			}
		}
	}

	// Check if egress is enabled on the service, if so we'll need to configure some rules
	if svc.Annotations[egress] == "true" {
		log.Debugf("Enabling egress for the service [%s]", svc.Name)
		if svc.Annotations[activeEndpoint] != "" {
			// We will need to modify the iptables rules
			err = sm.iptablesCheck()
			if err != nil {
				log.Errorf("Error configuring egress for loadbalancer [%s]", err)
			}
			errList := []error{}
			for _, serviceIP := range serviceIPs {
				podIPs := svc.Annotations[activeEndpoint]
				if sm.config.EnableEndpointSlices && vip.IsIPv6(serviceIP) {
					podIPs = svc.Annotations[activeEndpointIPv6]
				}
				err = sm.configureEgress(serviceIP, podIPs, svc.Annotations[egressDestinationPorts], svc.Namespace)
				if err != nil {
					errList = append(errList, err)
					log.Errorf("Error configuring egress for loadbalancer [%s]", err)
				}
			}
			if len(errList) == 0 {
				if !sm.config.EnableEndpointSlices {
					err = sm.updateServiceEndpointAnnotation(svc.Annotations[activeEndpoint], svc)
					if err != nil {
						log.Errorf("Error configuring egress annotation for loadbalancer [%s]", err)
					}
				} else {
					err = sm.updateServiceEndpointSlicesAnnotation(svc.Annotations[activeEndpoint],
						svc.Annotations[activeEndpointIPv6], svc)
					if err != nil {
						log.Errorf("Error configuring egress annotation for loadbalancer [%s]", err)
					}
				}

			}
		}
	}
	finishTime := time.Since(startTime)
	log.Infof("[service] synchronised in %dms", finishTime.Milliseconds())

	return nil
}

func (sm *Manager) deleteService(uid string) error {
	// protect multiple calls
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	var updatedInstances []*Instance
	var serviceInstance *Instance
	found := false
	for x := range sm.serviceInstances {
		log.Debugf("Looking for [%s], found [%s]", uid, sm.serviceInstances[x].UID)
		// Add the running services to the new array
		if sm.serviceInstances[x].UID != uid {
			updatedInstances = append(updatedInstances, sm.serviceInstances[x])
		} else {
			// Flip the found when we match
			found = true
			serviceInstance = sm.serviceInstances[x]
		}
	}
	// If we've been through all services and not found the correct one then error
	if !found {
		// TODO: - fix UX
		// return fmt.Errorf("unable to find/stop service [%s]", uid)
		return nil
	}
	shared := false
	for x := range updatedInstances {
		if updatedInstances[x].Vip == serviceInstance.Vip {
			shared = true
		}
	}
	if !shared {
		serviceInstance.cluster.Stop()
		if serviceInstance.isDHCP {
			serviceInstance.dhcpClient.Stop()
			macvlan, err := netlink.LinkByName(serviceInstance.dhcpInterface)
			if err != nil {
				return fmt.Errorf("error finding VIP Interface: %v", err)
			}

			err = netlink.LinkDel(macvlan)
			if err != nil {
				return fmt.Errorf("error deleting DHCP Link : %v", err)
			}
		}
		if serviceInstance.vipConfig.EnableBGP {
			cidrVip := fmt.Sprintf("%s/%s", serviceInstance.vipConfig.VIP, serviceInstance.vipConfig.VIPCIDR)
			err := sm.bgpServer.DelHost(cidrVip)
			return err
		}

		// We will need to tear down the egress
		if serviceInstance.serviceSnapshot.Annotations[egress] == "true" {
			if serviceInstance.serviceSnapshot.Annotations[activeEndpoint] != "" {

				log.Infof("service [%s] has an egress re-write enabled", serviceInstance.serviceSnapshot.Name)
				err := sm.TeardownEgress(serviceInstance.serviceSnapshot.Annotations[activeEndpoint], serviceInstance.serviceSnapshot.Spec.LoadBalancerIP, serviceInstance.serviceSnapshot.Annotations[egressDestinationPorts], serviceInstance.serviceSnapshot.Namespace)
				if err != nil {
					log.Errorf("%v", err)
				}
			}
		}
	}

	// Update the service array
	sm.serviceInstances = updatedInstances

	log.Infof("Removed [%s] from manager, [%d] advertised services remain", uid, len(sm.serviceInstances))

	return nil
}

func (sm *Manager) upnpMap(s *Instance) {
	// If upnp is enabled then update the gateway/router with the address
	// TODO - work out if we need to mapping.Reclaim()
	if sm.upnp != nil {
		vips := vip.GetIPs(s.Vip)
		for _, vip := range vips {
			log.Infof("[UPNP] Adding map to [%s:%d - %s]", vip, s.Port, s.serviceSnapshot.Name)
			if err := sm.upnp.AddPortMapping(int(s.Port), int(s.Port), 0, vip, strings.ToUpper(s.Type), s.serviceSnapshot.Name); err == nil {
				log.Infof("service should be accessible externally on port [%d]", s.Port)
			} else {
				sm.upnp.Reclaim()
				log.Errorf("unable to map port to gateway [%s]", err.Error())
			}
		}
	}
}

func (sm *Manager) updateStatus(i *Instance) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := sm.clientSet.CoreV1().Services(i.serviceSnapshot.Namespace).Get(context.TODO(), i.serviceSnapshot.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		id, err := os.Hostname()
		if err != nil {
			return err
		}

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		// If we're using ARP then we can only broadcast the VIP from one place, add an annotation to the service
		if sm.config.EnableARP {
			// Add the current host
			currentServiceCopy.Annotations[vipHost] = id
		}
		if i.dhcpInterfaceHwaddr != "" || i.dhcpInterfaceIP != "" {
			currentServiceCopy.Annotations[hwAddrKey] = i.dhcpInterfaceHwaddr
			currentServiceCopy.Annotations[requestedIP] = i.dhcpInterfaceIP
		}

		updatedService, err := sm.clientSet.CoreV1().Services(currentService.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("Error updating Service Spec [%s] : %v", i.serviceSnapshot.Name, err)
			return err
		}

		ports := make([]v1.PortStatus, 0, len(i.serviceSnapshot.Spec.Ports))
		for _, port := range i.serviceSnapshot.Spec.Ports {
			ports = append(ports, v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
			})
		}

		ingress := []v1.LoadBalancerIngress{}

		vips := vip.GetIPs(i.vipConfig.VIP)

		for _, v := range vips {
			if !vip.IsIP(v) {
				ips, err := vip.LookupHost(v, sm.config.DNSMode)
				if err != nil {
					return err
				}
				for _, ip := range ips {
					i := v1.LoadBalancerIngress{
						IP:    ip,
						Ports: ports,
					}
					ingress = append(ingress, i)
				}
			} else {
				i := v1.LoadBalancerIngress{
					IP:    v,
					Ports: ports,
				}
				ingress = append(ingress, i)
			}
		}
		updatedService.Status.LoadBalancer.Ingress = ingress

		_, err = sm.clientSet.CoreV1().Services(updatedService.Namespace).UpdateStatus(context.TODO(), updatedService, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("Error updating Service %s/%s Status: %v", i.serviceSnapshot.Namespace, i.serviceSnapshot.Name, err)
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

// fetchServiceAddress tries to get the address
// from annotation kube-vip.io/loadbalancerIPs, then Service.Status.LoadBalancer.Ingress
// and if not available fallbacks to spec.loadbalancerIP
func fetchServiceAddress(s *v1.Service) []string {
	annotationAvailable := false
	addresses := []string{}
	if s.Annotations != nil {
		var v string
		if v, annotationAvailable = s.Annotations[loadbalancerIPAnnotation]; annotationAvailable {
			return vip.GetIPs(v)
		}
	}
	if !annotationAvailable {
		if len(s.Status.LoadBalancer.Ingress) > 0 {
			for _, ingress := range s.Status.LoadBalancer.Ingress {
				addresses = append(addresses, ingress.IP)
			}
			return addresses
		}
	}

	if s.Spec.LoadBalancerIP != "" {
		return []string{s.Spec.LoadBalancerIP}
	}

	return []string{}
}

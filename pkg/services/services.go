package services

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	log "log/slog"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/manager"
	"github.com/kube-vip/kube-vip/pkg/upnp"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

type Manager struct {
	config    *kubevip.Config
	clientSet *kubernetes.Clientset
	bgpServer *bgp.Server
	// Keeps track of all running instances
	serviceInstances []*instance.Instance

	// This mutex is to protect calls from various goroutines
	mutex sync.Mutex
}

func (c *Manager) syncServices(ctx context.Context, svc *v1.Service) error {
	log.Debug("[STARTING] Service Sync")

	// Iterate through the synchronising services
	foundInstance := false
	newServiceAddresses := fetchServiceAddresses(svc)
	newServiceUID := svc.UID

	ingressIPs := []string{}

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		ingressIPs = append(ingressIPs, ingress.IP)
	}

	shouldBreake := false

	for x := range c.serviceInstances {
		if shouldBreake {
			break
		}
		for _, newServiceAddress := range newServiceAddresses {
			log.Debug("service", "IsDHCP", c.serviceInstances[x].IsDHCP, "newServiceAddress", newServiceAddress)
			if c.serviceInstances[x].ServiceSnapshot.UID == newServiceUID {
				// If the found instance's DHCP configuration doesn't match the new service, delete it.
				if (c.serviceInstances[x].IsDHCP && newServiceAddress != "0.0.0.0") ||
					(!c.serviceInstances[x].IsDHCP && newServiceAddress == "0.0.0.0") ||
					(!c.serviceInstances[x].IsDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, newServiceAddress)) ||
					(len(svc.Status.LoadBalancer.Ingress) > 0 && !comparePortsAndPortStatuses(svc)) ||
					(c.serviceInstances[x].IsDHCP && len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(ingressIPs, c.serviceInstances[x].DhcpInterfaceIP)) {
					if err := c.deleteService(newServiceUID); err != nil {
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
		if err := c.addService(ctx, svc); err != nil {
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

func (c *Manager) addService(ctx context.Context, svc *v1.Service) error {
	startTime := time.Now()

	addresses := fetchServiceAddresses(svc)

	newService, err := instance.New(svc, c.config, addresses)
	if err != nil {
		return err
	}

	for x := range newService.VipConfigs {
		newService.Clusters[x].StartLoadBalancerService(newService.VipConfigs[x], c.bgpServer)
	}

	c.upnpMap(ctx, newService)

	if newService.IsDHCP && len(newService.VipConfigs) == 1 {
		go func() {
			for ip := range newService.DHCPClient.IPChannel() {
				log.Debug("IP changed", "ip", ip)
				newService.VipConfigs[0].VIP = ip
				newService.DhcpInterfaceIP = ip
				if !c.config.DisableServiceUpdates {
					if err := c.updateStatus(newService); err != nil {
						log.Warn("updating svc", "err", err)
					}
				}
			}
			log.Debug("IP update channel closed, stopping")
		}()
	}

	c.serviceInstances = append(c.serviceInstances, newService)

	if !c.config.DisableServiceUpdates {
		log.Debug("service update", "namespace", newService.ServiceSnapshot.Namespace, "name", newService.ServiceSnapshot.Name)
		if err := c.updateStatus(newService); err != nil {
			// delete service to collect garbage
			if deleteErr := c.deleteService(newService.ServiceSnapshot.UID); deleteErr != nil {
				return deleteErr
			}
			return err
		}
	}

	serviceIPs := fetchServiceAddresses(svc)
	// Check if we need to flush any conntrack connections (due to some dangling conntrack connections)
	if svc.Annotations[manager.FlushContrack] == "true" {

		log.Debug("Flushing conntrack rules", "service", svc.Name)
		for _, serviceIP := range serviceIPs {
			err = vip.DeleteExistingSessions(serviceIP, false, svc.Annotations[manager.EgressDestinationPorts], svc.Annotations[manager.EgressSourcePorts])
			if err != nil {
				log.Error("flushing any remaining egress connections", "err", err)
			}
			err = vip.DeleteExistingSessions(serviceIP, true, svc.Annotations[manager.EgressDestinationPorts], svc.Annotations[manager.EgressSourcePorts])
			if err != nil {
				log.Error("flushing any remaining ingress connections", "err", err)
			}
		}
	}

	// Check if egress is enabled on the service, if so we'll need to configure some rules
	if svc.Annotations[manager.Egress] == "true" && len(serviceIPs) > 0 {
		log.Debug("enabling egress", "service", svc.Name)
		// We will need to modify the iptables rules
		err = c.iptablesCheck()
		if err != nil {
			log.Error("configuring egress", "service", svc.Name, "err", err)
		}
		var podIP string
		errList := []error{}

		// Should egress be IPv6
		if svc.Annotations[manager.EgressIPv6] == "true" {
			// Does the service have an active IPv6 endpoint
			if svc.Annotations[manager.ActiveEndpointIPv6] != "" {
				for _, serviceIP := range serviceIPs {
					if c.config.EnableEndpointSlices && vip.IsIPv6(serviceIP) {

						podIP = svc.Annotations[manager.ActiveEndpointIPv6]

						err = c.configureEgress(serviceIP, podIP, svc.Namespace, svc.Annotations)
						if err != nil {
							errList = append(errList, err)
							log.Error("configuring egress", "service", svc.Name, "err", err)
						}
					}
				}
			}
		} else if svc.Annotations[manager.ActiveEndpoint] != "" { // Not expected to be IPv6, so should be an IPv4 address
			for _, serviceIP := range serviceIPs {
				podIPs := svc.Annotations[manager.ActiveEndpoint]
				if c.config.EnableEndpointSlices && vip.IsIPv6(serviceIP) {
					podIPs = svc.Annotations[manager.ActiveEndpointIPv6]
				}
				err = c.configureEgress(serviceIP, podIPs, svc.Namespace, svc.Annotations)
				if err != nil {
					errList = append(errList, err)
					log.Error("configuring egress", "service", svc.Name, "err", err)
				}
			}
		}
		if len(errList) == 0 {
			var provider manager.EpProvider
			if !c.config.EnableEndpointSlices {
				provider = &manager.EndpointsProvider{Label: "endpoints"}
			} else {
				provider = &manager.EndpointslicesProvider{Label: "endpointslices"}
			}
			err = provider.UpdateServiceAnnotation(svc.Annotations[manager.ActiveEndpoint], svc.Annotations[manager.ActiveEndpointIPv6], svc, c.clientSet)
			if err != nil {
				log.Error("configuring egress", "service", svc.Name, "err", err)
			}
		}
	}

	finishTime := time.Since(startTime)
	log.Info("[service] synchronised", "in", fmt.Sprintf("%dms", finishTime.Milliseconds()))

	return nil
}

func (c *Manager) deleteService(uid types.UID) error {
	// protect multiple calls
	c.mutex.Lock()
	defer c.mutex.Unlock()

	var updatedInstances []*instance.Instance
	var serviceInstance *instance.Instance
	found := false
	for x := range c.serviceInstances {
		log.Debug("service lookup", "target UID", uid, "found UID ", c.serviceInstances[x].ServiceSnapshot.UID)
		// Add the running services to the new array
		if c.serviceInstances[x].ServiceSnapshot.UID != uid {
			updatedInstances = append(updatedInstances, c.serviceInstances[x])
		} else {
			// Flip the found when we match
			found = true
			serviceInstance = c.serviceInstances[x]
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
		for _, vip := range fetchServiceAddresses(updatedInstances[x].ServiceSnapshot) { //updatedInstances[x].ServiceSnapshot.Spec.LoadBalancerIP {
			vipSet[vip] = nil
		}
	}
	for _, vip := range fetchServiceAddresses(serviceInstance.ServiceSnapshot) {
		if _, found := vipSet[vip]; found {
			shared = true
		}
	}
	if !shared {
		for x := range serviceInstance.Clusters {
			serviceInstance.Clusters[x].Stop()
		}
		if serviceInstance.IsDHCP {
			serviceInstance.DHCPClient.Stop()
			macvlan, err := netlink.LinkByName(serviceInstance.DHCPInterface)
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
				err := c.bgpServer.DelHost(cidrVip)
				if err != nil {
					return fmt.Errorf("[BGP] error deleting BGP host: %v", err)
				}
				log.Debug("[BGP] delete", "host", cidrVip)
			}
		}

		// We will need to tear down the egress
		if serviceInstance.ServiceSnapshot.Annotations[manager.Egress] == "true" {
			if serviceInstance.ServiceSnapshot.Annotations[manager.ActiveEndpoint] != "" {
				log.Info("egress re-write enabled", "service", serviceInstance.ServiceSnapshot.Name)
				err := manager.TeardownEgress(c.config, serviceInstance.ServiceSnapshot.Annotations[manager.ActiveEndpoint], serviceInstance.ServiceSnapshot.Spec.LoadBalancerIP, serviceInstance.ServiceSnapshot.Namespace, serviceInstance.ServiceSnapshot.Annotations)
				if err != nil {
					log.Error("egress teardown", "err", err)
				}
			}
		}
	}

	// Update the service array
	sm.serviceInstances = updatedInstances

	log.Info("Removed instance from manager", "uid", uid, "remaining advertised services", len(sm.serviceInstances))

	return nil
}

// Set up UPNP forwards for a service
// We first try to use the more modern Pinhole API introduced in UPNPv2 and fall back to UPNPv2 Port Forwarding if no forward was successful
func (sm *Manager) upnpMap(ctx context.Context, s *instance.Instance) {
	if !isUPNPEnabled(s.ServiceSnapshot) {
		// Skip services missing the annotation
		return
	}
	if !sm.config.upnp {
		log.Warn("[UPNP] Found kube-vip.io/forwardUPNP on service while UPNP forwarding is disabled in the kube-vip config. Not forwarding", "service", s.ServiceSnapshot.Name)
	}
	// If upnp is enabled then update the gateway/router with the address
	// TODO - check if this implementation for dualstack is correct

	gateways := upnp.GetGatewayClients(ctx)

	// Reset Gateway IPs to remove stale addresses
	s.UpnpGatewayIPs = make([]string, 0)

	for _, vip := range fetchServiceAddresses(s.ServiceSnapshot) {
		for _, port := range s.ServiceSnapshot.Spec.Ports {
			for _, gw := range gateways {
				log.Info("[UPNP] Adding map", "vip", vip, "port", port.Port, "service", s.ServiceSnapshot.Name, "gateway", gw.WANIPv6FirewallControlClient.Location)

				forwardSucessful := false
				if gw.WANIPv6FirewallControlClient != nil {
					pinholeID, pinholeErr := gw.WANIPv6FirewallControlClient.AddPinholeCtx(ctx, "0.0.0.0", uint16(port.Port), vip, uint16(port.Port), upnp.MapProtocolToIANA(string(port.Protocol)), 3600) //nolint  TODO
					if pinholeErr == nil {
						forwardSucessful = true
						log.Info("[UPNP] Service should be accessible externally", "port", port.Port, "pinhold ID", pinholeID)
					} else {
						//TODO: Cleanup
						log.Error("[UPNP] Unable to map port to gateway using Pinhole API", "err", pinholeErr.Error())
					}
				}
				// Fallback to PortForward
				if !forwardSucessful {
					portMappingErr := gw.ConnectionClient.AddPortMapping("0.0.0.0", uint16(port.Port), strings.ToUpper(string(port.Protocol)), uint16(port.Port), vip, true, s.ServiceSnapshot.Name, 3600) //nolint  TODO
					if portMappingErr == nil {
						log.Info("[UPNP] Service should be accessible externally", "port", port.Port)
						forwardSucessful = true
					} else {
						//TODO: Cleanup
						log.Error("[UPNP] Unable to map port to gateway using PortForward API", "err", portMappingErr.Error())
					}
				}

				if forwardSucessful {
					ip, err := gw.ConnectionClient.GetExternalIPAddress()
					if err == nil {
						s.upnpGatewayIPs = append(s.upnpGatewayIPs, ip)
					}
				}
			}
		}
	}

	// Remove duplicate IPs
	slices.Sort(s.upnpGatewayIPs)
	s.upnpGatewayIPs = slices.Compact(s.upnpGatewayIPs)
}

func (sm *Manager) updateStatus(i *Instance) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := sm.clientSet.CoreV1().Services(i.ServiceSnapshot.Namespace).Get(context.TODO(), i.ServiceSnapshot.Name, metav1.GetOptions{})
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
			currentServiceCopy.Annotations[vipHost] = sm.config.NodeName
		}
		if i.dhcpInterfaceHwaddr != "" || i.dhcpInterfaceIP != "" {
			currentServiceCopy.Annotations[hwAddrKey] = i.dhcpInterfaceHwaddr
			currentServiceCopy.Annotations[requestedIP] = i.dhcpInterfaceIP
		}

		if currentService.Annotations["development.kube-vip.io/synthetic-api-server-error-on-update"] == "true" {
			log.Error("(Synthetic error ) updating Spec", "service", i.ServiceSnapshot.Name, "err", err)
			return fmt.Errorf("(Synthetic) simulating api server errors")
		}

		if !cmp.Equal(currentService, currentServiceCopy) {
			currentService, err = sm.clientSet.CoreV1().Services(currentServiceCopy.Namespace).Update(context.TODO(), currentServiceCopy, metav1.UpdateOptions{})
			if err != nil {
				log.Error("updating Spec", "service", i.ServiceSnapshot.Name, "err", err)
				return err
			}
		}

		ports := make([]v1.PortStatus, 0, len(i.ServiceSnapshot.Spec.Ports))
		for _, port := range i.ServiceSnapshot.Spec.Ports {
			ports = append(ports, v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
			})
		}

		ingresses := []v1.LoadBalancerIngress{}

		for _, c := range i.VipConfigs {
			if !vip.IsIP(c.VIP) {
				ips, err := vip.LookupHost(c.VIP, sm.config.DNSMode)
				if err != nil {
					return err
				}
				for _, ip := range ips {
					i := v1.LoadBalancerIngress{
						IP:    ip,
						Ports: ports,
					}
					ingresses = append(ingresses, i)
				}
			} else {
				i := v1.LoadBalancerIngress{
					IP:    c.VIP,
					Ports: ports,
				}
				ingresses = append(ingresses, i)
			}
			if isUPNPEnabled(currentService) {
				for _, ip := range i.upnpGatewayIPs {
					i := v1.LoadBalancerIngress{
						IP:    ip,
						Ports: ports,
					}
					ingresses = append(ingresses, i)
				}
			}
		}
		if !cmp.Equal(currentService.Status.LoadBalancer.Ingress, ingresses) {
			currentService.Status.LoadBalancer.Ingress = ingresses
			_, err = sm.clientSet.CoreV1().Services(currentService.Namespace).UpdateStatus(context.TODO(), currentService, metav1.UpdateOptions{})
			if err != nil {
				log.Error("updating Service", "namespace", i.ServiceSnapshot.Namespace, "name", i.ServiceSnapshot.Name, "err", err)
				return err
			}
		}
		return nil
	})

	if retryErr != nil {
		log.Error("Failed to set Services", "err", retryErr)
		return retryErr
	}
	return nil
}

// fetchServiceAddresses tries to get the addresses from annotations
// kube-vip.io/loadbalancerIPs, then from spec.loadbalancerIP
func fetchServiceAddresses(s *v1.Service) []string {
	annotationAvailable := false
	if s.Annotations != nil {
		if v, annotationAvailable := s.Annotations[loadbalancerIPAnnotation]; annotationAvailable {
			ips := strings.Split(v, ",")
			var trimmedIPs []string
			for _, ip := range ips {
				trimmedIPs = append(trimmedIPs, strings.TrimSpace(ip))
			}
			return trimmedIPs
		}
	}

	lbStatusAddresses := []string{}
	if !annotationAvailable {
		if len(s.Status.LoadBalancer.Ingress) > 0 {
			for _, ingress := range s.Status.LoadBalancer.Ingress {
				lbStatusAddresses = append(lbStatusAddresses, ingress.IP)
			}
		}
	}

	lbIP := net.ParseIP(s.Spec.LoadBalancerIP)
	isLbIPv4 := vip.IsIPv4(s.Spec.LoadBalancerIP)

	if len(lbStatusAddresses) > 0 {
		for _, a := range lbStatusAddresses {
			if lbStatusIP := net.ParseIP(a); lbStatusIP != nil && lbIP != nil && vip.IsIPv4(a) == isLbIPv4 && !lbIP.Equal(lbStatusIP) {
				return []string{s.Spec.LoadBalancerIP}
			}
		}
		return lbStatusAddresses
	}

	if s.Spec.LoadBalancerIP != "" {
		return []string{s.Spec.LoadBalancerIP}
	}

	return []string{}
}

func isUPNPEnabled(s *v1.Service) bool {
	return metav1.HasAnnotation(s.ObjectMeta, upnpEnabled) && s.Annotations[upnpEnabled] == "true"
}

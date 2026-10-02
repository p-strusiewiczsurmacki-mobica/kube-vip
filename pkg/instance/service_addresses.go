package instance

import (
	"sort"

	v1 "k8s.io/api/core/v1"
)

// OrderedServiceAddresses returns Service VIPs ordered by creation time and
// stable identity, keeping shared-lease annotations deterministic.
func OrderedServiceAddresses(services []*v1.Service) []string {
	services = append([]*v1.Service(nil), services...)
	sort.SliceStable(services, func(first, second int) bool {
		firstService, secondService := services[first], services[second]
		if !firstService.CreationTimestamp.Equal(&secondService.CreationTimestamp) {
			return firstService.CreationTimestamp.Before(&secondService.CreationTimestamp)
		}
		if firstService.Namespace != secondService.Namespace {
			return firstService.Namespace < secondService.Namespace
		}
		if firstService.Name != secondService.Name {
			return firstService.Name < secondService.Name
		}
		return firstService.UID < secondService.UID
	})

	vips := make([]string, 0)
	for _, service := range services {
		addresses, _ := FetchServiceAddresses(service)
		vips = append(vips, addresses...)
	}
	return vips
}

package instance

import (
	"slices"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOrderedServiceAddressesUsesCreationTimeAndStableIdentity(t *testing.T) {
	older := metav1.NewTime(time.Unix(100, 0))
	newer := metav1.NewTime(time.Unix(200, 0))
	services := []*v1.Service{
		{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "default", UID: "new", CreationTimestamp: newer}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.30"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "default", UID: "second", CreationTimestamp: older}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.20"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "default", UID: "first", CreationTimestamp: older}, Spec: v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"}},
	}

	got := OrderedServiceAddresses(services)
	want := []string{"192.0.2.10", "192.0.2.20", "192.0.2.30"}
	if !slices.Equal(got, want) {
		t.Fatalf("OrderedServiceAddresses() = %v, want %v", got, want)
	}
}

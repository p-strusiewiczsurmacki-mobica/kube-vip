//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/kube-vip/kube-vip/testing/services/pkg/deployment"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var _ = Describe("kube-vip routing table mode", func() {
	if Mode == ModeBGP {
		var (
			logger                     log.Logger
			imagePath                  string
			k8sImagePath               string
			configPath                 string
			kubeVIPBGPManifestTemplate *template.Template
			tempDirPath                string
			v129                       bool
			localIPv4                  string
			localIPv6                  string
			containerIPv4              string
			containerIPv6              string
		)

		BeforeEach(func() {
			klog.SetOutput(GinkgoWriter)
			logger = e2e.TestLogger{}

			imagePath = os.Getenv("E2E_IMAGE_PATH")    // Path to kube-vip image
			configPath = os.Getenv("CONFIG_PATH")      // path to the api server config
			k8sImagePath = os.Getenv("K8S_IMAGE_PATH") // path to the kubernetes image (version for kind)
			if configPath == "" {
				configPath = "/etc/kubernetes/admin.conf"
			}
			_, v129 = os.LookupEnv("V129")
			curDir, err := os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			templateRoutingTablePath := filepath.Join(curDir, "kube-vip-bgp.yaml.tmpl")
			kubeVIPBGPManifestTemplate, err = template.New("kube-vip-bgp.yaml.tmpl").ParseFiles(templateRoutingTablePath)
			Expect(err).NotTo(HaveOccurred())

			tempDirPath, err = os.MkdirTemp("", "kube-vip-test")
			Expect(err).NotTo(HaveOccurred())
			localIPv4, err = deployment.GetLocalIPv4("br-36d13ecf5bde")
			Expect(err).ToNot(HaveOccurred())
			localIPv6, err = deployment.GetLocalIPv6("br-36d13ecf5bde")
			Expect(err).ToNot(HaveOccurred())
		})

		Describe("kube-vip IPv4 services routing table mode functionality", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				svcElection    bool
				ipFamily       []corev1.IPFamily

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(e2e.IPv4Family, SOffset.Get())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				manifestValues = &e2e.KubevipManifestValues{
					ControlPlaneVIP:    cpVIP,
					ImagePath:          imagePath,
					ConfigPath:         configPath,
					ControlPlaneEnable: "false",
					SvcEnable:          "true",
					SvcElectionEnable:  "false",
				}

				var err error
				svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol}

				clusterName, client = prepareCluster(tempDirPath, "bgp-svc-ipv4", k8sImagePath, v129, kubeVIPBGPManifestTemplate, logger, manifestValues, networking, nodesNumber)

				container := fmt.Sprintf("%s-control-plane", clusterName)

				By(localIPv4)
				By(localIPv6)
				By(clusterName)

				containerIPv4, containerIPv6, err = GetContainerIPs(container)
				Expect(err).ToNot(HaveOccurred())

				By(containerIPv4)
				By(containerIPv6)
			})

			AfterAll(func() {
				cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
			})

			DescribeTable("configures an IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv4Family, offset)
					testServiceRT(svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName, trafficPolicy, client, svcElection, ipFamily, 1)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv4Family, offset)
					testServiceRT(svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName, trafficPolicy, client, svcElection, ipFamily, 2)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		// Describe("kube-vip IPv6 services routing table mode functionality", Ordered, func() {
		// 	var (
		// 		cpVIP          string
		// 		clusterName    string
		// 		client         kubernetes.Interface
		// 		manifestValues *e2e.KubevipManifestValues
		// 		svcElection    bool
		// 		ipFamily       []corev1.IPFamily

		// 		nodesNumber = 1
		// 	)

		// 	BeforeAll(func() {
		// 		cpVIP = e2e.GenerateVIP(e2e.IPv6Family, SOffset.Get())

		// 		networking := &kindconfigv1alpha4.Networking{
		// 			IPFamily: kindconfigv1alpha4.IPv6Family,
		// 		}

		// 		manifestValues = &e2e.KubevipManifestValues{
		// 			ControlPlaneVIP:    cpVIP,
		// 			ImagePath:          imagePath,
		// 			ConfigPath:         configPath,
		// 			ControlPlaneEnable: "false",
		// 			SvcEnable:          "true",
		// 			SvcElectionEnable:  "false",
		// 		}

		// 		var err error
		// 		svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
		// 		Expect(err).ToNot(HaveOccurred())

		// 		ipFamily = []corev1.IPFamily{corev1.IPv6Protocol}

		// 		clusterName, client = prepareCluster(tempDirPath, "bgp-svc-ipv6", k8sImagePath, v129, kubeVIPBGPManifestTemplate, logger, manifestValues, networking, nodesNumber)
		// 	})

		// 	AfterAll(func() {
		// 		cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
		// 	})

		// 	DescribeTable("configures an IPv6 routes for services",
		// 		func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
		// 			lbAddress := e2e.GenerateVIP(e2e.IPv6Family, offset)
		// 			testServiceRT(svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName, trafficPolicy, client, svcElection, ipFamily, 1)
		// 		},
		// 		Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
		// 		Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
		// 	)

		// 	DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
		// 		func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
		// 			lbAddress := e2e.GenerateVIP(e2e.IPv6Family, offset)
		// 			testServiceRT(svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName, trafficPolicy, client, svcElection, ipFamily, 2)
		// 		},
		// 		Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
		// 		Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
		// 	)
		// })

		// Describe("kube-vip DualStack services routing table mode functionality - IPv4 primary", Ordered, func() {
		// 	var (
		// 		cpVIP          string
		// 		clusterName    string
		// 		client         kubernetes.Interface
		// 		manifestValues *e2e.KubevipManifestValues
		// 		svcElection    bool
		// 		ipFamily       []corev1.IPFamily

		// 		nodesNumber = 1
		// 	)

		// 	BeforeAll(func() {
		// 		cpVIP = e2e.GenerateDualStackVIP(SOffset.Get())

		// 		networking := &kindconfigv1alpha4.Networking{
		// 			IPFamily: kindconfigv1alpha4.DualStackFamily,
		// 		}

		// 		manifestValues = &e2e.KubevipManifestValues{
		// 			ControlPlaneVIP:      cpVIP,
		// 			ImagePath:            imagePath,
		// 			ConfigPath:           configPath,
		// 			ControlPlaneEnable:   "false",
		// 			SvcEnable:            "true",
		// 			SvcElectionEnable:    "false",
		// 			EnableEndpointslices: "true",
		// 		}

		// 		var err error
		// 		svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
		// 		Expect(err).ToNot(HaveOccurred())

		// 		ipFamily = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}

		// 		clusterName, client = prepareCluster(tempDirPath, "bgp-ds-svc-ipv4", k8sImagePath, v129, kubeVIPBGPManifestTemplate, logger, manifestValues, networking, nodesNumber)
		// 	})

		// 	AfterAll(func() {
		// 		cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
		// 	})

		// 	DescribeTable("configures an IPv4 and IPv6 routes for services",
		// 		func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
		// 			lbAddress := e2e.GenerateDualStackVIP(offset)
		// 			testServiceRT(svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName, trafficPolicy, client, svcElection, ipFamily, 1)
		// 		},
		// 		Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
		// 		Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
		// 	)

		// 	DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
		// 		func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
		// 			lbAddress := e2e.GenerateDualStackVIP(offset)
		// 			testServiceRT(svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName, trafficPolicy, client, svcElection, ipFamily, 2)
		// 		},
		// 		Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
		// 		Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
		// 	)
		// })

		// Describe("kube-vip DualStack services routing table mode functionality - IPv6 primary", Ordered, func() {
		// 	var (
		// 		cpVIP          string
		// 		clusterName    string
		// 		client         kubernetes.Interface
		// 		manifestValues *e2e.KubevipManifestValues
		// 		svcElection    bool
		// 		ipFamily       []corev1.IPFamily

		// 		nodesNumber = 1
		// 	)

		// 	BeforeAll(func() {
		// 		cpVIP = e2e.GenerateDualStackVIP(SOffset.Get())

		// 		networking := &kindconfigv1alpha4.Networking{
		// 			IPFamily:      kindconfigv1alpha4.DualStackFamily,
		// 			PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
		// 			ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
		// 		}

		// 		manifestValues = &e2e.KubevipManifestValues{
		// 			ControlPlaneVIP:      cpVIP,
		// 			ImagePath:            imagePath,
		// 			ConfigPath:           configPath,
		// 			ControlPlaneEnable:   "false",
		// 			SvcEnable:            "true",
		// 			SvcElectionEnable:    "false",
		// 			EnableEndpointslices: "true",
		// 		}

		// 		var err error
		// 		svcElection, err = strconv.ParseBool(manifestValues.SvcElectionEnable)
		// 		Expect(err).ToNot(HaveOccurred())

		// 		ipFamily = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}

		// 		clusterName, client = prepareCluster(tempDirPath, "bgp-ds-svc-ipv6", k8sImagePath, v129, kubeVIPBGPManifestTemplate, logger, manifestValues, networking, nodesNumber)
		// 	})

		// 	AfterAll(func() {
		// 		cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
		// 	})

		// 	DescribeTable("configures an IPv4 and IPv6 routes for services",
		// 		func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
		// 			lbAddress := e2e.GenerateDualStackVIP(offset)
		// 			testServiceRT(svcName, lbAddress, fmt.Sprintf("kubevip-%s", svcName), dsNamespace, clusterName, trafficPolicy, client, svcElection, ipFamily, 1)
		// 		},
		// 		Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
		// 		Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
		// 	)

		// 	DescribeTable("only removes route if it was referenced by multiple services and all of them were deleted",
		// 		func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
		// 			lbAddress := e2e.GenerateDualStackVIP(offset)
		// 			testServiceRT(svcName, lbAddress, "plndr-svcs-lock", "kube-system", clusterName, trafficPolicy, client, svcElection, ipFamily, 2)
		// 		},
		// 		Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
		// 		Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
		// 	)
		// })
	}
})

func testServiceBGP(svcName, lbAddress, leaseName, leaseNamespace, clusterName string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	client kubernetes.Interface, serviceElection bool, ipFamily []corev1.IPFamily, numberOfServices int) {
	lbAddresses := vip.Split(lbAddress)

	services := []string{}
	for i := range numberOfServices {
		services = append(services, fmt.Sprintf("%s-%d", svcName, i))
	}

	for _, svc := range services {
		createTestService(svc, dsNamespace, dsName, lbAddress,
			client, corev1.IPFamilyPolicyPreferDualStack, ipFamily, trafficPolicy)
	}

	var container string
	if serviceElection {
		container = e2e.GetLeaseHolder(leaseName, leaseNamespace, client)
	} else {
		container = fmt.Sprintf("%s-control-plane", clusterName)
	}

	for _, addr := range lbAddresses {
		e2e.CheckRoutePresence(addr, container, true)
	}

	for i := range numberOfServices {
		expected := i < numberOfServices-1
		err := client.CoreV1().Services(dsNamespace).Delete(context.TODO(), services[i], metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())

		for _, addr := range lbAddresses {
			e2e.CheckRoutePresence(addr, container, expected)
		}
	}
}

func GetContainerIPs(containerName string) (string, string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		panic(err)
	}
	containers, err := cli.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to list containers: %w", err)
	}

	for _, c := range containers {
		for _, n := range c.Names {
			if n[1:] == containerName {
				fmt.Println(n)
				for _, n := range c.NetworkSettings.Networks {
					return n.IPAddress, n.GlobalIPv6Address, nil
				}
			}
		}
	}

	return "", "", nil
}

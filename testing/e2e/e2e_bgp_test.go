//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	api "github.com/osrg/gobgp/v3/api"
)

var _ = Describe("kube-vip routing table mode", func() {
	if Mode == ModeBGP {
		var (
			logger                     log.Logger
			imagePath                  string
			k8sImagePath               string
			configPath                 string
			kubeVIPBGPManifestTemplate *template.Template
			goBGPConfigTemplate        *template.Template
			tempDirPath                string
			v129                       bool
			localIPv4                  string
			// localIPv6                  string
			containerIPv4 string
			// containerIPv6              string
			curDir string
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
			var err error
			curDir, err = os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			templateRoutingTablePath := filepath.Join(curDir, "kube-vip-bgp.yaml.tmpl")
			kubeVIPBGPManifestTemplate, err = template.New("kube-vip-bgp.yaml.tmpl").ParseFiles(templateRoutingTablePath)
			Expect(err).NotTo(HaveOccurred())

			tempDirPath, err = os.MkdirTemp("", "kube-vip-test")
			Expect(err).NotTo(HaveOccurred())
			localIPv4, err = deployment.GetLocalIPv4("br-36d13ecf5bde")
			Expect(err).ToNot(HaveOccurred())
			_, err = deployment.GetLocalIPv6("br-36d13ecf5bde")
			Expect(err).ToNot(HaveOccurred())
		})

		Describe("kube-vip IPv4 services routing table mode functionality", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				goBGPConfig    *e2e.GoBGPConfigValues
				ipFamily       []corev1.IPFamily

				bgpKill chan any

				nodesNumber = 1
			)

			BeforeAll(func() {
				cpVIP = e2e.GenerateVIP(e2e.IPv4Family, SOffset.Get())

				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				goBGPConfig = &e2e.GoBGPConfigValues{
					RouterID: localIPv4,
					AS:       65500,
					PeerAS:   65501,
				}

				manifestValues = &e2e.KubevipManifestValues{
					ControlPlaneVIP:    cpVIP,
					ImagePath:          imagePath,
					ConfigPath:         configPath,
					ControlPlaneEnable: "false",
					SvcEnable:          "true",
					SvcElectionEnable:  "false",
					GobgpConfig:        goBGPConfig,
				}

				var err error
				Expect(err).ToNot(HaveOccurred())

				ipFamily = []corev1.IPFamily{corev1.IPv4Protocol}

				clusterName, client = prepareCluster(tempDirPath, "bgp-svc-ipv4", k8sImagePath, v129, kubeVIPBGPManifestTemplate, logger, manifestValues, networking, nodesNumber)

				container := fmt.Sprintf("%s-control-plane", clusterName)

				containerIPv4, _, err = GetContainerIPs(container)
				Expect(err).ToNot(HaveOccurred())

				goBGPConfig.NeighborAddress = containerIPv4

				goBGPConfigPath := filepath.Join(filepath.Join(curDir, "bgp"), "config.toml.tmpl")
				goBGPConfigTemplate, err = template.New("config.toml.tmpl").ParseFiles(goBGPConfigPath)
				Expect(err).ToNot(HaveOccurred())

				goBGPConfigPath = filepath.Join(tempDirPath, "config.toml")

				f, err := os.OpenFile(goBGPConfigPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
				Expect(err).ToNot(HaveOccurred())
				defer f.Close()

				err = goBGPConfigTemplate.Execute(f, goBGPConfig)
				Expect(err).ToNot(HaveOccurred())

				bgpKill = make(chan any)

				go startGoBGP(goBGPConfigPath, bgpKill)

			})

			AfterAll(func() {
				close(bgpKill)
				cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv4Family, offset)
					ipv4UC := &api.Family{
						Afi:  api.Family_AFI_IP,
						Safi: api.Family_SAFI_UNICAST,
					}
					gobgpClient, err := newGoBGPClient(localIPv4, "50051")
					Expect(err).ToNot(HaveOccurred())
					testServiceBGP(svcName, lbAddress, trafficPolicy, client, ipFamily, 1, gobgpClient, ipv4UC)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					lbAddress := e2e.GenerateVIP(e2e.IPv4Family, offset)
					ipv4UC := &api.Family{
						Afi:  api.Family_AFI_IP,
						Safi: api.Family_SAFI_UNICAST,
					}
					gobgpClient, err := newGoBGPClient(localIPv4, "50051")
					Expect(err).ToNot(HaveOccurred())
					testServiceBGP(svcName, lbAddress, trafficPolicy, client, ipFamily, 2, gobgpClient, ipv4UC)
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

func testServiceBGP(svcName, lbAddress string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	client kubernetes.Interface, ipFamily []corev1.IPFamily, numberOfServices int, gobgpClient api.GobgpApiClient, gobgpFamily *api.Family) {
	lbAddresses := vip.Split(lbAddress)

	services := []string{}
	for i := range numberOfServices {
		services = append(services, fmt.Sprintf("%s-%d", svcName, i))
	}

	for _, svc := range services {
		createTestService(svc, dsNamespace, dsName, lbAddress,
			client, corev1.IPFamilyPolicyPreferDualStack, ipFamily, trafficPolicy)
	}

	for _, addr := range lbAddresses {
		paths := checkGoBGPPaths(context.Background(), gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
		Expect(strings.Contains(paths[0].Prefix, lbAddress)).To(BeTrue())
	}

	for i := range numberOfServices {
		err := client.CoreV1().Services(dsNamespace).Delete(context.TODO(), services[i], metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())
		if i < numberOfServices-1 {
			for _, addr := range lbAddresses {
				paths := checkGoBGPPaths(context.Background(), gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
				Expect(strings.Contains(paths[0].Prefix, lbAddress)).To(BeTrue())
			}
		}
	}

	for _, addr := range lbAddresses {
		checkGoBGPPaths(context.Background(), gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 0)
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

func newGoBGPClient(address, port string) (api.GobgpApiClient, error) {
	grpcOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	target := net.JoinHostPort(address, port)
	conn, err := grpc.NewClient(target, grpcOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to GoBGP server %q: %w", target, err)
	}

	return api.NewGobgpApiClient(conn), nil
}

func checkGoBGPPaths(ctx context.Context, client api.GobgpApiClient, family *api.Family, prefixes []*api.TableLookupPrefix, expectedPaths int) []*api.Destination {
	var paths []*api.Destination
	Eventually(func() error {
		var err error
		paths, err = getGoBGPPaths(ctx, client, family, prefixes)
		if err != nil {
			return err
		}
		if len(paths) != expectedPaths {
			return fmt.Errorf("expected %d paths, but found %d", expectedPaths, len(paths))
		}
		return nil
	}, "120s").ShouldNot(HaveOccurred())
	return paths
}

func getGoBGPPaths(ctx context.Context, client api.GobgpApiClient, family *api.Family, prefixes []*api.TableLookupPrefix) ([]*api.Destination, error) {
	pathCtx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	stream, err := client.ListPath(pathCtx, &api.ListPathRequest{
		TableType: api.TableType_GLOBAL,
		Family:    family,
		Name:      "",
		Prefixes:  prefixes,
		SortType:  api.ListPathRequest_PREFIX,
	})
	if err != nil {
		return nil, err
	}

	rib := make([]*api.Destination, 0)
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		rib = append(rib, r.Destination)
	}

	return rib, nil
}

func startGoBGP(config string, kill chan any) {
	By("starting GoBGP server")
	cmd := exec.Command("../../bin/gobgpd", "-f", config)
	go cmd.Run()
	<-kill
	By("stopping GoBGP server")
	err := cmd.Process.Kill()
	Expect(err).ToNot(HaveOccurred())
}

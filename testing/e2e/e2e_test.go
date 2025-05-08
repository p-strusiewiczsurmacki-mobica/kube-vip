//go:build e2e
// +build e2e

package e2e_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/onsi/gomega/gexec"

	kvcluster "github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/testing/e2e"
)

var _ = Describe("kube-vip ARP/NDP broadcast neighbor", func() {
	const (
		dsName    = "traefik-whoami"
		namespace = "default"
	)

	var (
		logger                              log.Logger
		imagePath                           string
		k8sImagePath                        string
		configPath                          string
		kubeVIPManifestTemplate             *template.Template
		kubeVIPHostnameManifestTemplate     *template.Template
		kubeVIPRoutingTableManifestTemplate *template.Template
		tempDirPath                         string
		v129                                bool
		configMtx                           sync.Mutex
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

		templatePath := filepath.Join(curDir, "kube-vip.yaml.tmpl")
		kubeVIPManifestTemplate, err = template.New("kube-vip.yaml.tmpl").ParseFiles(templatePath)
		Expect(err).NotTo(HaveOccurred())

		hostnameTemplatePath := filepath.Join(curDir, "kube-vip-hostname.yaml.tmpl")
		kubeVIPHostnameManifestTemplate, err = template.New("kube-vip-hostname.yaml.tmpl").ParseFiles(hostnameTemplatePath)
		Expect(err).NotTo(HaveOccurred())

		templateRoutingTablePath := filepath.Join(curDir, "kube-vip-routing-table.yaml.tmpl")
		kubeVIPRoutingTableManifestTemplate, err = template.New("kube-vip-routing-table.yaml.tmpl").ParseFiles(templateRoutingTablePath)
		Expect(err).NotTo(HaveOccurred())

		tempDirPath, err = os.MkdirTemp("", "kube-vip-test")
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("kube-vip IPv4 functionality, vip_leaderelection=true, svc_enable=true, svc_election=false", Ordered, func() {
		var (
			clusterConfig      kindconfigv1alpha4.Cluster
			ipv4VIP            string
			client             kubernetes.Interface
			clusterName        string
			stopClusterRemoval bool
		)

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-ipv4", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-ipv4.yaml")

			for i := 0; i < 3; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			ipv4VIP = e2e.GenerateVIP(e2e.IPv4Family, 5)

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP:   ipv4VIP,
				ImagePath:         imagePath,
				ConfigPath:        configPath,
				SvcEnable:         "true",
				SvcElectionEnable: "false",
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv4-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: ipv4VIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}

			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			client = createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("creating test daemonset"))
			createTestDS(dsName, namespace, client)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" || stopClusterRemoval {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(provider.Delete(clusterName, ""), "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("provides an IPv4 VIP address for the Kubernetes control plane nodes", func() {
			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv4 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(ipv4VIP, time.Duration(0), 20*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(ipv4VIP, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv4 VIP with little downtime"))
			// Allow at most 30 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(ipv4VIP, 3*time.Second, 60*time.Second)
		})

		DescribeTable("configures an IPv4 VIP address for service",
			func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateVIP(e2e.IPv4Family, offset)

				createTestService(svcName, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicySingleStack, []corev1.IPFamily{corev1.IPv4Protocol}, trafficPolicy)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, true, client, &stopClusterRemoval)

				assertConnection("http", lbAddress, "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svcName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, false, client, &stopClusterRemoval)
				assertConnectionError("http", lbAddress, "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc-cluster", uint(6), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc-local", uint(7), corev1.ServiceExternalTrafficPolicyLocal),
		)

		DescribeTable("only removes VIP address if it was referenced by multiple services and all of them were deleted",
			func(svc1Name string, svc2Name string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateVIP(e2e.IPv4Family, offset)

				createTestService(svc1Name, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicySingleStack, []corev1.IPFamily{corev1.IPv4Protocol}, trafficPolicy)
				createTestService(svc2Name, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicySingleStack, []corev1.IPFamily{corev1.IPv4Protocol}, trafficPolicy)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, true, client, &stopClusterRemoval)

				assertConnection("http", lbAddress, "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svc1Name, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				// sleep for 3 seconds so kube-vip will hopefully process deletion request
				time.Sleep(time.Second * 3)

				assertConnection("http", lbAddress, "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, true, client, &stopClusterRemoval)

				err = client.CoreV1().Services(namespace).Delete(context.TODO(), svc2Name, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, false, client, &stopClusterRemoval)

				assertConnectionError("http", lbAddress, "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc1-cluster", "test-svc2-cluster", uint(8), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc1-local", "test-svc2-local", uint(9), corev1.ServiceExternalTrafficPolicyLocal),
		)
	})

	Describe("kube-vip IPv4 functionality, vip_leaderelection=true, svc_enable=true, svc_election=true", Ordered, func() {
		var (
			clusterConfig      kindconfigv1alpha4.Cluster
			ipv4VIP            string
			client             kubernetes.Interface
			clusterName        string
			stopClusterRemoval bool
		)

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-svc-ipv4", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-ipv4.yaml")

			for i := 0; i < 3; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			ipv4VIP = e2e.GenerateVIP(e2e.IPv4Family, 10)

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP:   ipv4VIP,
				ImagePath:         imagePath,
				ConfigPath:        configPath,
				SvcEnable:         "true",
				SvcElectionEnable: "true",
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv4-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: ipv4VIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}

			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			client = createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("creating test daemonset"))
			createTestDS(dsName, namespace, client)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" || stopClusterRemoval {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(provider.Delete(clusterName, ""), "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("provides an IPv4 VIP address for the Kubernetes control plane nodes", func() {
			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv4 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(ipv4VIP, time.Duration(0), 20*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(ipv4VIP, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv4 VIP with little downtime"))
			// Allow at most 30 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(ipv4VIP, 3*time.Second, 60*time.Second)
		})

		DescribeTable("configures an IPv4 VIP address for service",
			func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateVIP(e2e.IPv4Family, offset)

				createTestService(svcName, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicySingleStack, []corev1.IPFamily{corev1.IPv4Protocol}, trafficPolicy)

				checkIPAddress(fmt.Sprintf("kubevip-%s", svcName), namespace, lbAddress, true, client, &stopClusterRemoval)

				assertConnection("http", lbAddress, "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				container := e2e.GetLeaseHolder(fmt.Sprintf("kubevip-%s", svcName), namespace, client)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svcName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				isPresent := e2e.CheckIPAddressPresence(lbAddress, container)
				Expect(isPresent).ToNot(BeNil())
				Expect(*isPresent).To(BeFalse())

				assertConnectionError("http", lbAddress, "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc-cluster", uint(11), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc-local", uint(12), corev1.ServiceExternalTrafficPolicyLocal),
		)
	})

	Describe("kube-vip IPv6 functionality, vip_leaderelection=true, svc_enable=true, svc_election=false", Ordered, func() {
		var (
			clusterConfig      kindconfigv1alpha4.Cluster
			ipv6VIP            string
			client             kubernetes.Interface
			clusterName        string
			stopClusterRemoval bool
		)

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-ipv6", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-ipv6.yaml")

			for i := 0; i < 3; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			ipv6VIP = e2e.GenerateVIP(e2e.IPv6Family, 13)

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP:   ipv6VIP,
				ImagePath:         imagePath,
				ConfigPath:        configPath,
				SvcEnable:         "true",
				SvcElectionEnable: "false",
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv6-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: ipv6VIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}

			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			client = createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("creating test daemonset"))
			createTestDS(dsName, namespace, client)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" || stopClusterRemoval {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(provider.Delete(clusterName, ""), "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("provides an IPv6 VIP address for the Kubernetes control plane nodes", func() {

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv6 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(ipv6VIP, time.Duration(0), 20*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(ipv6VIP, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv6 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(ipv6VIP, 3*time.Second, 60*time.Second)
		})

		DescribeTable("configures an IPv6 VIP address for service",
			func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateVIP(e2e.IPv6Family, offset)

				createTestService(svcName, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicySingleStack, []corev1.IPFamily{corev1.IPv6Protocol}, trafficPolicy)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, true, client, &stopClusterRemoval)

				assertConnection("http", lbAddress, "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svcName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, false, client, &stopClusterRemoval)
				assertConnectionError("http", lbAddress, "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc-cluster", uint(14), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc-local", uint(15), corev1.ServiceExternalTrafficPolicyLocal),
		)

		DescribeTable("only removes VIP address if it was referenced by multiple services and all of them were deleted",
			func(svc1Name string, svc2Name string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateVIP(e2e.IPv6Family, offset)

				createTestService(svc1Name, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicySingleStack, []corev1.IPFamily{corev1.IPv6Protocol}, trafficPolicy)
				createTestService(svc2Name, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicySingleStack, []corev1.IPFamily{corev1.IPv6Protocol}, trafficPolicy)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, true, client, &stopClusterRemoval)

				assertConnection("http", lbAddress, "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svc1Name, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				// sleep for 3 seconds so kube-vip will hopefully process deletion request
				time.Sleep(time.Second * 3)

				assertConnection("http", lbAddress, "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, true, client, &stopClusterRemoval)

				err = client.CoreV1().Services(namespace).Delete(context.TODO(), svc2Name, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddress, false, client, &stopClusterRemoval)
				assertConnectionError("http", lbAddress, "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc1-cluster", "test-svc2-cluster", uint(16), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc1-local", "test-svc2-local", uint(17), corev1.ServiceExternalTrafficPolicyLocal),
		)
	})

	Describe("kube-vip IPv6 functionality, vip_leaderelection=true, svc_enable=true, svc_election=true", Ordered, func() {
		var (
			clusterConfig      kindconfigv1alpha4.Cluster
			ipv6VIP            string
			client             kubernetes.Interface
			clusterName        string
			stopClusterRemoval bool
		)

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-svc-ipv6", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-ipv6.yaml")

			for i := 0; i < 3; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			ipv6VIP = e2e.GenerateVIP(e2e.IPv6Family, 18)

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP:   ipv6VIP,
				ImagePath:         imagePath,
				ConfigPath:        configPath,
				SvcEnable:         "true",
				SvcElectionEnable: "true",
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv6-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: ipv6VIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}

			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			client = createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("creating test daemonset"))
			createTestDS(dsName, namespace, client)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" || stopClusterRemoval {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(provider.Delete(clusterName, ""), "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("provides an IPv6 VIP address for the Kubernetes control plane nodes", func() {

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv6 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(ipv6VIP, time.Duration(0), 20*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(ipv6VIP, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv6 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(ipv6VIP, 3*time.Second, 60*time.Second)
		})

		DescribeTable("configures an IPv6 VIP address for service",
			func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateVIP(e2e.IPv6Family, offset)

				createTestService(svcName, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicySingleStack, []corev1.IPFamily{corev1.IPv6Protocol}, trafficPolicy)

				checkIPAddress(fmt.Sprintf("kubevip-%s", svcName), namespace, lbAddress, true, client, &stopClusterRemoval)

				assertConnection("http", lbAddress, "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				container := e2e.GetLeaseHolder(fmt.Sprintf("kubevip-%s", svcName), namespace, client)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svcName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				isPresent := e2e.CheckIPAddressPresence(lbAddress, container)
				Expect(isPresent).ToNot(BeNil())
				Expect(*isPresent).To(BeFalse())

				assertConnectionError("http", lbAddress, "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc-cluster", uint(19), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc-local", uint(20), corev1.ServiceExternalTrafficPolicyLocal),
		)
	})

	Describe("kube-vip DualStack functionality - IPv4 primary, vip_leaderelection=true, svc_enable=true, svc_election=false", Ordered, func() {
		var (
			clusterConfig      kindconfigv1alpha4.Cluster
			dualstackVIP       string
			clusterName        string
			client             kubernetes.Interface
			stopClusterRemoval bool
		)

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-ds-ipv4", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-dualstack.yaml")

			for i := 0; i < 3; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			dualstackVIP = e2e.GenerateDualStackVIP(21)

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP:      dualstackVIP,
				ImagePath:            imagePath,
				ConfigPath:           configPath,
				SvcEnable:            "true",
				EnableEndpointslices: "true",
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv6-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: dualstackVIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}

			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			client = createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("creating test daemonset"))
			createTestDS(dsName, namespace, client)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" || stopClusterRemoval {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(func() error {
				return provider.Delete(clusterName, "")
			}, "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("provides an DualStack VIP addresses for the Kubernetes control plane nodes", func() {
			vips := vip.Split(dualstackVIP)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv6 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(vips[1], time.Duration(0), 20*time.Second)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv4 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(vips[0], time.Duration(0), 20*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(vips[1], clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv6 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(vips[1], 3*time.Second, 60*time.Second)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv4 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(vips[0], 3*time.Second, 60*time.Second)
		})

		DescribeTable("configures an IPv4 and IPv6 VIP addresses for service",
			func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateDualStackVIP(offset)

				lbAddresses := strings.Split(lbAddress, ",")

				createTestService(svcName, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicyRequireDualStack, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, trafficPolicy)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], true, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], true, client, &stopClusterRemoval)

				assertConnection("http", lbAddresses[0], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)
				assertConnection("http", lbAddresses[1], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svcName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], false, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], false, client, &stopClusterRemoval)
				assertConnectionError("http", lbAddresses[0], "80", "", 3*time.Second)
				assertConnectionError("http", lbAddresses[1], "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc-cluster", uint(22), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc-local", uint(23), corev1.ServiceExternalTrafficPolicyLocal),
		)

		DescribeTable("only removes VIP address if it was referenced by multiple services and all of them were deleted",
			func(svc1Name string, svc2Name string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateDualStackVIP(offset)

				lbAddresses := strings.Split(lbAddress, ",")

				createTestService(svc1Name, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicyRequireDualStack, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, trafficPolicy)
				createTestService(svc2Name, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicyRequireDualStack, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, trafficPolicy)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], true, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], true, client, &stopClusterRemoval)

				assertConnection("http", lbAddresses[0], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)
				assertConnection("http", lbAddresses[1], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svc1Name, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				// sleep for 3 seconds so kube-vip will hopefully process deletion request
				time.Sleep(time.Second * 3)

				assertConnection("http", lbAddresses[0], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)
				assertConnection("http", lbAddresses[1], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], true, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], true, client, &stopClusterRemoval)

				err = client.CoreV1().Services(namespace).Delete(context.TODO(), svc2Name, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], false, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], false, client, &stopClusterRemoval)

				assertConnectionError("http", lbAddresses[0], "80", "", 3*time.Second)
				assertConnectionError("http", lbAddresses[1], "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc1-cluster", "test-svc2-cluster", uint(24), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc1-local", "test-svc2-local", uint(25), corev1.ServiceExternalTrafficPolicyLocal),
		)
	})

	Describe("kube-vip DualStack functionality - IPv4 primary, vip_leaderelection=true, svc_enable=true, svc_election=true", Ordered, func() {
		var (
			clusterConfig      kindconfigv1alpha4.Cluster
			dualstackVIP       string
			clusterName        string
			client             kubernetes.Interface
			stopClusterRemoval bool
		)

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-ds-svc-ipv4", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-dualstack.yaml")

			for i := 0; i < 3; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			dualstackVIP = e2e.GenerateDualStackVIP(26)

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP:      dualstackVIP,
				ImagePath:            imagePath,
				ConfigPath:           configPath,
				SvcEnable:            "true",
				EnableEndpointslices: "true",
				SvcElectionEnable:    "true",
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv6-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: dualstackVIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}

			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			client = createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("creating test daemonset"))
			createTestDS(dsName, namespace, client)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" || stopClusterRemoval {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(func() error {
				return provider.Delete(clusterName, "")
			}, "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("provides an DualStack VIP addresses for the Kubernetes control plane nodes", func() {
			vips := vip.Split(dualstackVIP)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv6 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(vips[1], time.Duration(0), 20*time.Second)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv4 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(vips[0], time.Duration(0), 20*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(vips[1], clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv6 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(vips[1], 3*time.Second, 60*time.Second)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv4 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(vips[0], 3*time.Second, 60*time.Second)
		})

		DescribeTable("configures an IPv6 VIP address for service",
			func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateDualStackVIP(offset)
				lbAddresses := strings.Split(lbAddress, ",")

				createTestService(svcName, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicyRequireDualStack, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, trafficPolicy)

				checkIPAddress(fmt.Sprintf("kubevip-%s", svcName), namespace, lbAddresses[0], true, client, &stopClusterRemoval)
				checkIPAddress(fmt.Sprintf("kubevip-%s", svcName), namespace, lbAddresses[1], true, client, &stopClusterRemoval)

				assertConnection("http", lbAddresses[0], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)
				assertConnection("http", lbAddresses[1], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				container := e2e.GetLeaseHolder(fmt.Sprintf("kubevip-%s", svcName), namespace, client)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svcName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				isPresent := e2e.CheckIPAddressPresence(lbAddresses[0], container)
				Expect(isPresent).ToNot(BeNil())
				Expect(*isPresent).To(BeFalse())

				isPresent = e2e.CheckIPAddressPresence(lbAddresses[1], container)
				Expect(isPresent).ToNot(BeNil())
				Expect(*isPresent).To(BeFalse())

				assertConnectionError("http", lbAddress, "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc-cluster", uint(27), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc-local", uint(28), corev1.ServiceExternalTrafficPolicyLocal),
		)
	})

	Describe("kube-vip DualStack functionality - IPv6 primary, vip_leaderelection=true, svc_enable=true, svc_election=false", Ordered, func() {
		var (
			clusterConfig      kindconfigv1alpha4.Cluster
			dualstackVIP       string
			clusterName        string
			client             kubernetes.Interface
			stopClusterRemoval bool
		)

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-ds-ipv6", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily:      kindconfigv1alpha4.DualStackFamily,
					PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
					ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-dualstack.yaml")

			for i := 0; i < 3; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			dualstackVIP = e2e.GenerateDualStackVIP(29)

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP:      dualstackVIP,
				ImagePath:            imagePath,
				ConfigPath:           configPath,
				SvcEnable:            "true",
				EnableEndpointslices: "true",
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv6-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: dualstackVIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}

			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			client = createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("creating test daemonset"))
			createTestDS(dsName, namespace, client)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" || stopClusterRemoval {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(func() error {
				err := provider.Delete(clusterName, "")
				if err != nil {
					By(withTimestamp(err.Error()))
				}
				return err
			}, "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("provides an DualStack VIP addresses for the Kubernetes control plane nodes", func() {
			vips := vip.Split(dualstackVIP)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv6 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(vips[1], time.Duration(0), 20*time.Second)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv4 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(vips[0], time.Duration(0), 20*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(vips[1], clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv6 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(vips[1], 3*time.Second, 60*time.Second)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv4 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(vips[0], 3*time.Second, 60*time.Second)
		})

		DescribeTable("configures an IPv4 and IPv6 VIP addresses for service",
			func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateDualStackVIP(offset)

				lbAddresses := strings.Split(lbAddress, ",")

				createTestService(svcName, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicyRequireDualStack, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, trafficPolicy)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], true, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], true, client, &stopClusterRemoval)

				assertConnection("http", lbAddresses[0], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)
				assertConnection("http", lbAddresses[1], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svcName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], false, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], false, client, &stopClusterRemoval)
				assertConnectionError("http", lbAddresses[0], "80", "", 3*time.Second)
				assertConnectionError("http", lbAddresses[1], "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc-cluster", uint(30), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc-local", uint(31), corev1.ServiceExternalTrafficPolicyLocal),
		)

		DescribeTable("only removes VIP address if it was referenced by multiple services and all of them were deleted",
			func(svc1Name string, svc2Name string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateDualStackVIP(offset)

				lbAddresses := strings.Split(lbAddress, ",")

				createTestService(svc1Name, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicyRequireDualStack, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, trafficPolicy)
				createTestService(svc2Name, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicyRequireDualStack, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, trafficPolicy)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], true, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], true, client, &stopClusterRemoval)

				assertConnection("http", lbAddresses[0], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)
				assertConnection("http", lbAddresses[1], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svc1Name, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				// sleep for 3 seconds so kube-vip will hopefully process deletion request
				time.Sleep(time.Second * 3)

				assertConnection("http", lbAddresses[0], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)
				assertConnection("http", lbAddresses[1], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], true, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], true, client, &stopClusterRemoval)

				err = client.CoreV1().Services(namespace).Delete(context.TODO(), svc2Name, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[0], false, client, &stopClusterRemoval)
				checkIPAddress("plndr-svcs-lock", "kube-system", lbAddresses[1], false, client, &stopClusterRemoval)

				assertConnectionError("http", lbAddresses[0], "80", "", 3*time.Second)
				assertConnectionError("http", lbAddresses[1], "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc1-cluster", "test-svc2-cluster", uint(32), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc1-local", "test-svc2-local", uint(33), corev1.ServiceExternalTrafficPolicyLocal),
		)
	})

	Describe("kube-vip DualStack functionality - IPv6 primary, vip_leaderelection=true, svc_enable=true, svc_election=true", Ordered, func() {
		var (
			clusterConfig      kindconfigv1alpha4.Cluster
			dualstackVIP       string
			clusterName        string
			client             kubernetes.Interface
			stopClusterRemoval bool
		)

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-ds-svc-ipv6", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily:      kindconfigv1alpha4.DualStackFamily,
					PodSubnet:     "fd00:10:244::/56,10.244.0.0/16",
					ServiceSubnet: "fd00:10:96::/112,10.96.0.0/16",
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-dualstack.yaml")

			for i := 0; i < 3; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			dualstackVIP = e2e.GenerateDualStackVIP(34)

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP:      dualstackVIP,
				ImagePath:            imagePath,
				ConfigPath:           configPath,
				SvcEnable:            "true",
				EnableEndpointslices: "true",
				SvcElectionEnable:    "true",
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv6-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: dualstackVIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}

			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			client = createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("creating test daemonset"))
			createTestDS(dsName, namespace, client)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" || stopClusterRemoval {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(func() error {
				return provider.Delete(clusterName, "")
			}, "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("provides an DualStack VIP addresses for the Kubernetes control plane nodes", func() {
			vips := vip.Split(dualstackVIP)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv6 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(vips[1], time.Duration(0), 20*time.Second)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv4 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(vips[0], time.Duration(0), 20*time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(vips[1], clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv6 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(vips[1], 3*time.Second, 60*time.Second)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv4 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(vips[0], 3*time.Second, 60*time.Second)
		})

		DescribeTable("configures an IPv6 VIP address for service",
			func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
				lbAddress := e2e.GenerateDualStackVIP(offset)
				lbAddresses := strings.Split(lbAddress, ",")

				createTestService(svcName, namespace, dsName, lbAddress,
					client, corev1.IPFamilyPolicyRequireDualStack, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, trafficPolicy)

				checkIPAddress(fmt.Sprintf("kubevip-%s", svcName), namespace, lbAddresses[0], true, client, &stopClusterRemoval)
				checkIPAddress(fmt.Sprintf("kubevip-%s", svcName), namespace, lbAddresses[1], true, client, &stopClusterRemoval)

				assertConnection("http", lbAddresses[0], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)
				assertConnection("http", lbAddresses[1], "80", "", 3*time.Second, 60*time.Second, &stopClusterRemoval)

				container := e2e.GetLeaseHolder(fmt.Sprintf("kubevip-%s", svcName), namespace, client)

				err := client.CoreV1().Services(namespace).Delete(context.TODO(), svcName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				isPresent := e2e.CheckIPAddressPresence(lbAddresses[0], container)
				Expect(isPresent).ToNot(BeNil())
				Expect(*isPresent).To(BeFalse())

				isPresent = e2e.CheckIPAddressPresence(lbAddresses[1], container)
				Expect(isPresent).ToNot(BeNil())
				Expect(*isPresent).To(BeFalse())

				assertConnectionError("http", lbAddress, "80", "", 3*time.Second)
			},
			Entry("with external traffic policy - cluster", "test-svc-cluster", uint(35), corev1.ServiceExternalTrafficPolicyCluster),
			Entry("with external traffic policy - local", "test-svc-local", uint(36), corev1.ServiceExternalTrafficPolicyLocal),
		)
	})

	Describe("kube-vip IPv4 functionality with legacy hostname", Ordered, func() {
		var (
			clusterConfig kindconfigv1alpha4.Cluster
			ipv4VIP       string
			clusterName   string
		)

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-ipv4-hostname", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-ipv4-hostname.yaml")

			for i := 0; i < 3; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			ipv4VIP = e2e.GenerateVIP(e2e.IPv4Family, 37)

			Expect(kubeVIPHostnameManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP: ipv4VIP,
				ImagePath:       imagePath,
				ConfigPath:      configPath,
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv4-hostname-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPHostnameManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: ipv4VIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(provider.Delete(clusterName, ""), "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("uses hostname fallback while providing an IPv4 VIP address for the Kubernetes control plane nodes", func() {
			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are accessible via the assigned IPv4 VIP"))
			// Allow enough time for control plane nodes to load the docker image and
			// use the default timeout for establishing a connection to the VIP
			assertControlPlaneIsRoutable(ipv4VIP, time.Duration(0), 20*time.Second)

			// wait for a bit
			By(withTimestamp("sitting for a few seconds to hopefully allow the roles to have been created in the cluster"))
			time.Sleep(30 * time.Second)

			By(withTimestamp("killing the leader Kubernetes control plane node to trigger a fail-over scenario"))
			killLeader(ipv4VIP, clusterName)

			By(withTimestamp("checking that the Kubernetes control plane nodes are still accessible via the assigned IPv4 VIP with little downtime"))
			// Allow at most 20 seconds of downtime when polling the control plane nodes
			assertControlPlaneIsRoutable(ipv4VIP, 3*time.Second, 60*time.Second)
		})
	})

	Describe("kube-vip IPv4 control-plane routing table mode functionality", Ordered, func() {
		var (
			clusterConfig kindconfigv1alpha4.Cluster
			ipv4VIP       string
			clusterName   string
		)

		numberOfCPNodes := 3

		BeforeAll(func() {
			clusterName = fmt.Sprintf("%s-rt-ipv4", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-ipv4.yaml")

			for i := 0; i < numberOfCPNodes; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			ipv4VIP = e2e.GenerateVIP(e2e.IPv4Family, 38)

			Expect(kubeVIPRoutingTableManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP: ipv4VIP,
				ImagePath:       imagePath,
				ConfigPath:      configPath,
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv4-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPRoutingTableManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: ipv4VIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(provider.Delete(clusterName, ""), "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("setups IPv4 address and route on control-plane node", func() {
			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)

			By(withTimestamp("sitting for a few seconds to hopefully allow kube-vip to start"))
			time.Sleep(30 * time.Second)

			for i := 1; i <= numberOfCPNodes; i++ {
				var container string
				if i > 1 {
					container = fmt.Sprintf("%s-control-plane%d", clusterName, i)
				} else {
					container = fmt.Sprintf("%s-control-plane", clusterName)
				}

				exists := e2e.CheckIPAddressPresence(ipv4VIP, container)
				Expect(*exists).To(BeTrue())
				rtExists := e2e.CheckRoutePresence(ipv4VIP, container)
				Expect(rtExists).To(BeTrue())
			}
		})
	})

	Describe("kube-vip IPv6 control-plane routing table mode functionality", Ordered, func() {
		var (
			clusterConfig kindconfigv1alpha4.Cluster
			ipv6VIP       string
			clusterName   string
		)

		numberOfCPNodes := 3

		BeforeAll(func() {

			clusterName = fmt.Sprintf("%s-rt-ipv6", filepath.Base(tempDirPath))

			clusterConfig = kindconfigv1alpha4.Cluster{
				Networking: kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				},
				Nodes: []kindconfigv1alpha4.Node{},
			}

			manifestPath := filepath.Join(tempDirPath, "kube-vip-ipv6.yaml")

			for i := 0; i < numberOfCPNodes; i++ {
				nodeConfig := kindconfigv1alpha4.Node{
					Role: kindconfigv1alpha4.ControlPlaneRole,
					ExtraMounts: []kindconfigv1alpha4.Mount{
						{
							HostPath:      manifestPath,
							ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
						},
					},
				}
				// Override the kind image version
				if k8sImagePath != "" {
					nodeConfig.Image = k8sImagePath
				}
				clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
			}

			manifestFile, err := os.Create(manifestPath)
			Expect(err).NotTo(HaveOccurred())

			defer manifestFile.Close()

			ipv6VIP = e2e.GenerateVIP(e2e.IPv6Family, 39)

			Expect(kubeVIPRoutingTableManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP: ipv6VIP,
				ImagePath:       imagePath,
				ConfigPath:      configPath,
			})).To(Succeed())

			if v129 {
				// create a seperate manifest
				manifestPath2 := filepath.Join(tempDirPath, "kube-vip-ipv6-first.yaml")

				// change the path of the mount to the new file
				clusterConfig.Nodes[0].ExtraMounts[0].HostPath = manifestPath2

				manifestFile2, err := os.Create(manifestPath2)
				Expect(err).NotTo(HaveOccurred())

				defer manifestFile2.Close()

				Expect(kubeVIPRoutingTableManifestTemplate.Execute(manifestFile2, e2e.KubevipManifestValues{
					ControlPlaneVIP: ipv6VIP,
					ImagePath:       imagePath,
					ConfigPath:      "/etc/kubernetes/super-admin.conf", // Change the kuberenetes file
				})).To(Succeed())
			}
		})

		AfterAll(func() {
			if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" {
				return
			}

			configMtx.Lock()
			defer configMtx.Unlock()

			provider := cluster.NewProvider(
				cluster.ProviderWithLogger(logger),
				cluster.ProviderWithDocker(),
			)

			Eventually(provider.Delete(clusterName, ""), "30s", "200ms").Should(Succeed())

			Expect(os.RemoveAll(tempDirPath)).To(Succeed())
		})

		It("setups IPv6 address and route on control-plane node", func() {
			By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
			createKindCluster(logger, &clusterConfig, clusterName)

			By(withTimestamp("loading local docker image to kind cluster"))
			e2e.LoadDockerImageToKind(logger, imagePath, clusterName)

			By(withTimestamp("sitting for a few seconds to hopefully allow kube-vip to start"))
			time.Sleep(30 * time.Second)

			for i := 1; i <= numberOfCPNodes; i++ {
				var container string
				if i > 1 {
					container = fmt.Sprintf("%s-control-plane%d", clusterName, i)
				} else {
					container = fmt.Sprintf("%s-control-plane", clusterName)
				}

				exists := e2e.CheckIPAddressPresence(ipv6VIP, container)
				Expect(*exists).To(BeTrue())
				rtExists := e2e.CheckRoutePresence(ipv6VIP, container)
				Expect(rtExists).To(BeTrue())
			}
		})
	})
})

func createKindCluster(logger log.Logger, config *v1alpha4.Cluster, clusterName string) kubernetes.Interface {
	provider := cluster.NewProvider(
		cluster.ProviderWithLogger(logger),
		cluster.ProviderWithDocker(),
	)
	format.UseStringerRepresentation = true // Otherwise error stacks have binary format.
	Expect(provider.Create(
		clusterName,
		cluster.CreateWithV1Alpha4Config(config),
		cluster.CreateWithRetain(os.Getenv("E2E_PRESERVE_CLUSTER") == "true"), // If create fails, we'll need the cluster alive to debug
	)).To(Succeed())

	kc, err := provider.KubeConfig(clusterName, false)
	Expect(err).ToNot(HaveOccurred())

	kubeconfigGetter := func() (*api.Config, error) {
		return clientcmd.Load([]byte(kc))
	}

	cfg, err := clientcmd.BuildConfigFromKubeconfigGetter("", kubeconfigGetter)
	if err != nil {
		panic(err.Error())
	}

	client, err := kubernetes.NewForConfig(cfg)
	Expect(err).ToNot(HaveOccurred())

	return client
}

// Assume the VIP is routable if status code is 200 or 500. Since etcd might glitch.
func assertControlPlaneIsRoutable(controlPlaneVIP string, transportTimeout, eventuallyTimeout time.Duration) {
	var stopClusterRemoval bool
	assertConnection("https", controlPlaneVIP, "6443", "livez", transportTimeout, eventuallyTimeout, &stopClusterRemoval)
}

// Assume connection to the provided address is possible
func assertConnection(protocol, ip, port, suffix string, transportTimeout, eventuallyTimeout time.Duration, stopClusterRemoval *bool) {
	if strings.Contains(ip, ":") {
		ip = fmt.Sprintf("[%s]", ip)
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // nolint
	}
	client := &http.Client{Transport: transport, Timeout: transportTimeout}

	code := 0

	defer func() {
		if code != http.StatusOK && code != http.StatusInternalServerError {
			By("Executing Defer assertConnection")
			*stopClusterRemoval = true
		}
	}()

	Eventually(func() int {
		resp, _ := client.Get(fmt.Sprintf("%s://%s:%s/%s", protocol, ip, port, suffix))
		if resp == nil {
			return -1
		}
		defer resp.Body.Close()
		code = resp.StatusCode
		return resp.StatusCode
	}, eventuallyTimeout).Should(BeElementOf([]int{http.StatusOK, http.StatusInternalServerError}), fmt.Sprintf("Failed to connect to %s", ip))
}

// Assume connection to the provided address is possible
func assertConnectionError(protocol, ip, port, suffix string, transportTimeout time.Duration) {
	if strings.Contains(ip, ":") {
		ip = fmt.Sprintf("[%s]", ip)
	}

	By("assertConnectionError: " + ip)

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // nolint
	}
	client := &http.Client{Transport: transport, Timeout: transportTimeout}

	Eventually(func() error {
		_, err := client.Get(fmt.Sprintf("%s://%s:%s/%s", protocol, ip, port, suffix))
		return err
	}, time.Second*30).Should(HaveOccurred())
}

func killLeader(leaderIPAddr string, clusterName string) {
	var leaderName string
	Eventually(func() string {
		leaderName = findLeader(leaderIPAddr, clusterName)
		return leaderName
	}, "600s").ShouldNot(BeEmpty())

	cmd := exec.Command(
		"docker", "kill", leaderName,
	)

	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session, "5s").Should(gexec.Exit(0))
}

func findLeader(leaderIPAddr string, clusterName string) string {
	dockerControlPlaneContainerNames := []string{
		fmt.Sprintf("%s-control-plane", clusterName),
		fmt.Sprintf("%s-control-plane2", clusterName),
		fmt.Sprintf("%s-control-plane3", clusterName),
	}
	var leaderName string
	for _, name := range dockerControlPlaneContainerNames {
		cmdOut := new(bytes.Buffer)
		cmd := exec.Command(
			"docker", "exec", name, "ip", "addr",
		)
		cmd.Stdout = cmdOut
		Eventually(cmd.Run(), "5s").Should(Succeed())

		if strings.Contains(cmdOut.String(), leaderIPAddr) {
			leaderName = name
			break
		}
	}
	return leaderName
}

func withTimestamp(text string) string {
	return fmt.Sprintf("%s: %s", time.Now(), text)
}

func createTestDS(name, namespace string, client kubernetes.Interface) {
	labels := make(map[string]string)
	labels["app"] = name
	d := v1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: v1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{
						{
							Key:      "node-role.kubernetes.io/control-plane",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node-role.kubernetes.io/master",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  fmt.Sprintf("%s-v4", name),
							Image: "ghcr.io/traefik/whoami:v1.11",
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 80,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "PORT",
									Value: "80",
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := client.AppsV1().DaemonSets(namespace).Create(context.TODO(), &d, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func createTestService(name, namespace, target, lbAddress string, client kubernetes.Interface, ipfPolicy corev1.IPFamilyPolicy, ipFamiles []corev1.IPFamily, externalPolicy corev1.ServiceExternalTrafficPolicy) {
	svcAnnotations := make(map[string]string)
	svcAnnotations[kvcluster.LoadbalancerIPAnnotation] = lbAddress

	labels := make(map[string]string)
	labels["app"] = target

	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: svcAnnotations,
		},
		Spec: corev1.ServiceSpec{
			IPFamilies:            ipFamiles,
			IPFamilyPolicy:        &ipfPolicy,
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: externalPolicy,
			Ports: []corev1.ServicePort{
				{
					Protocol: corev1.ProtocolTCP,
					Port:     80,
				},
			},
			Selector: labels,
		},
	}

	Eventually(func() error {
		_, err := client.CoreV1().Services(namespace).Create(context.TODO(), &s, metav1.CreateOptions{})
		return err
	}, time.Second*60, time.Second).Should(Succeed())
}

func checkIPAddress(name, namespace, lbAddress string, expected bool, client kubernetes.Interface, stopClusterRemoval *bool) {
	By("checkIPAddress: " + lbAddress)
	isPresent := false
	defer func() {
		By("Executing Defer checkIPAddress: " + lbAddress)
		if isPresent != expected {
			By("checkIPAddress - stopping cluster removal: " + lbAddress)
			*stopClusterRemoval = true
		}
	}()

	Eventually(func() *bool {
		value := e2e.CheckIPAddressPresenceByLease(name, namespace, lbAddress, client)
		if value != nil {
			isPresent = *value
		}
		return value
	}, time.Second*120, time.Second).ShouldNot(BeNil())
	matcher := BeFalse()

	if expected {
		matcher = BeTrue()
	}
	Expect(isPresent).To(matcher)
}

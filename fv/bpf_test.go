// Copyright (c) 2021 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build fvtests

package fv_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/projectcalico/libcalico-go/lib/apiconfig"
	api "github.com/projectcalico/libcalico-go/lib/apis/v3"
	client "github.com/projectcalico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/libcalico-go/lib/ipam"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/numorstring"
	options2 "github.com/projectcalico/libcalico-go/lib/options"

	"github.com/projectcalico/felix/bpf"
	"github.com/projectcalico/felix/bpf/conntrack"
	"github.com/projectcalico/felix/bpf/nat"
	. "github.com/projectcalico/felix/fv/connectivity"
	"github.com/projectcalico/felix/fv/containers"
	"github.com/projectcalico/felix/fv/infrastructure"
	"github.com/projectcalico/felix/fv/utils"
	"github.com/projectcalico/felix/fv/workload"
	"github.com/projectcalico/felix/timeshim"
)

// We run with and without connection-time load balancing for a couple of reasons:
// - We can only test the non-connection time NAT logic (and node ports) with it disabled.
// - Since the connection time program applies to the whole host, the different felix nodes actually share the
//   connection-time program.  This is a bit of a broken test but it's better than nothing since all felix nodes
//   should be programming the same NAT mappings.
var _ = describeBPFTests(withProto("tcp"), withConnTimeLoadBalancingEnabled(), withNonProtocolDependentTests())
var _ = describeBPFTests(withProto("udp"), withConnTimeLoadBalancingEnabled())
var _ = describeBPFTests(withProto("udp"), withConnTimeLoadBalancingEnabled(), withUDPUnConnected())
var _ = describeBPFTests(withProto("tcp"))
var _ = describeBPFTests(withProto("udp"))
var _ = describeBPFTests(withProto("udp"), withUDPUnConnected())
var _ = describeBPFTests(withProto("udp"), withUDPConnectedRecvMsg(), withConnTimeLoadBalancingEnabled())
var _ = describeBPFTests(withTunnel("ipip"), withProto("tcp"), withConnTimeLoadBalancingEnabled())
var _ = describeBPFTests(withTunnel("ipip"), withProto("udp"), withConnTimeLoadBalancingEnabled())
var _ = describeBPFTests(withTunnel("ipip"), withProto("tcp"))
var _ = describeBPFTests(withTunnel("ipip"), withProto("udp"))
var _ = describeBPFTests(withProto("tcp"), withDSR())
var _ = describeBPFTests(withProto("udp"), withDSR())
var _ = describeBPFTests(withTunnel("ipip"), withProto("tcp"), withDSR())
var _ = describeBPFTests(withTunnel("ipip"), withProto("udp"), withDSR())
var _ = describeBPFTests(withTunnel("wireguard"), withProto("tcp"))
var _ = describeBPFTests(withTunnel("wireguard"), withProto("tcp"), withConnTimeLoadBalancingEnabled())

// Run a stripe of tests with BPF logging disabled since the compiler tends to optimise the code differently
// with debug disabled and that can lead to verifier issues.
var _ = describeBPFTests(withProto("tcp"),
	withConnTimeLoadBalancingEnabled(),
	withBPFLogLevel("info"))

type bpfTestOptions struct {
	connTimeEnabled bool
	protocol        string
	udpUnConnected  bool
	bpfLogLevel     string
	tunnel          string
	dsr             bool
	udpConnRecvMsg  bool
	nonProtoTests   bool
}

type bpfTestOpt func(opts *bpfTestOptions)

func withProto(proto string) bpfTestOpt {
	return func(opts *bpfTestOptions) {
		opts.protocol = proto
	}
}

func withConnTimeLoadBalancingEnabled() bpfTestOpt {
	return func(opts *bpfTestOptions) {
		opts.connTimeEnabled = true
	}
}

func withNonProtocolDependentTests() bpfTestOpt {
	return func(opts *bpfTestOptions) {
		opts.nonProtoTests = true
	}
}

func withBPFLogLevel(level string) bpfTestOpt {
	return func(opts *bpfTestOptions) {
		opts.bpfLogLevel = level
	}
}

func withTunnel(tunnel string) bpfTestOpt {
	return func(opts *bpfTestOptions) {
		opts.tunnel = tunnel
	}
}

func withUDPUnConnected() bpfTestOpt {
	return func(opts *bpfTestOptions) {
		opts.udpUnConnected = true
	}
}

func withDSR() bpfTestOpt {
	return func(opts *bpfTestOptions) {
		opts.dsr = true
	}
}

func withUDPConnectedRecvMsg() bpfTestOpt {
	return func(opts *bpfTestOptions) {
		opts.udpConnRecvMsg = true
	}
}

const expectedRouteDump = `10.65.0.0/16: remote in-pool nat-out
10.65.0.2/32: local workload in-pool nat-out idx -
10.65.0.3/32: local workload in-pool nat-out idx -
10.65.0.4/32: local workload in-pool nat-out idx -
10.65.1.0/26: remote workload in-pool nat-out nh FELIX_1
10.65.2.0/26: remote workload in-pool nat-out nh FELIX_2
111.222.0.1/32: local host
111.222.1.1/32: remote host
111.222.2.1/32: remote host
FELIX_0/32: local host idx -
FELIX_1/32: remote host
FELIX_2/32: remote host`

const expectedRouteDumpWithTunnelAddr = `10.65.0.0/16: remote in-pool nat-out
10.65.0.1/32: local host
10.65.0.2/32: local workload in-pool nat-out idx -
10.65.0.3/32: local workload in-pool nat-out idx -
10.65.0.4/32: local workload in-pool nat-out idx -
10.65.1.0/26: remote workload in-pool nat-out nh FELIX_1
10.65.2.0/26: remote workload in-pool nat-out nh FELIX_2
111.222.0.1/32: local host
111.222.1.1/32: remote host
111.222.2.1/32: remote host
FELIX_0/32: local host idx -
FELIX_1/32: remote host
FELIX_2/32: remote host`

const extIP = "10.1.2.3"

func describeBPFTests(opts ...bpfTestOpt) bool {
	if os.Getenv("FELIX_FV_ENABLE_BPF") != "true" {
		// Non-BPF run.
		return true
	}

	testOpts := bpfTestOptions{
		bpfLogLevel: "debug",
		tunnel:      "none",
	}
	for _, o := range opts {
		o(&testOpts)
	}

	testIfTCP := testOpts.protocol == "tcp"

	protoExt := ""
	if testOpts.udpUnConnected {
		protoExt = "-unconnected"
	}
	if testOpts.udpConnRecvMsg {
		protoExt = "-conn-recvmsg"
	}

	desc := fmt.Sprintf("_BPF_ _BPF-SAFE_ BPF tests (%s%s, ct=%v, log=%s, tunnel=%s, dsr=%v)",
		testOpts.protocol, protoExt, testOpts.connTimeEnabled,
		testOpts.bpfLogLevel, testOpts.tunnel, testOpts.dsr,
	)
	return infrastructure.DatastoreDescribe(desc, []apiconfig.DatastoreType{apiconfig.Kubernetes}, func(getInfra infrastructure.InfraFactory) {
		var (
			infra              infrastructure.DatastoreInfra
			felixes            []*infrastructure.Felix
			calicoClient       client.Interface
			cc                 *Checker
			externalClient     *containers.Container
			deadWorkload       *workload.Workload
			options            infrastructure.TopologyOptions
			numericProto       uint8
			felixPanicExpected bool
		)

		switch testOpts.protocol {
		case "tcp":
			numericProto = 6
		case "udp":
			numericProto = 17
		default:
			Fail("bad protocol option")
		}

		BeforeEach(func() {
			felixPanicExpected = false
			infra = getInfra()

			cc = &Checker{
				CheckSNAT: true,
			}
			cc.Protocol = testOpts.protocol
			if testOpts.protocol == "udp" && testOpts.udpUnConnected {
				cc.Protocol += "-noconn"
			}
			if testOpts.protocol == "udp" && testOpts.udpConnRecvMsg {
				cc.Protocol += "-recvmsg"
			}

			options = infrastructure.DefaultTopologyOptions()
			options.FelixLogSeverity = "debug"
			options.NATOutgoingEnabled = true
			options.AutoHEPsEnabled = true
			// override IPIP being enabled by default
			options.IPIPEnabled = false
			options.IPIPRoutesEnabled = false
			switch testOpts.tunnel {
			case "none":
				// nothing
			case "ipip":
				options.IPIPEnabled = true
				options.IPIPRoutesEnabled = true
			case "wireguard":
				// Delay running Felix until Node resource has been created.
				options.DelayFelixStart = true
				options.TriggerDelayedFelixStart = true
				// Wireguard doesn't support IPv6, disable it.
				options.EnableIPv6 = false
				// Allocate tunnel address for Wireguard.
				options.WireguardEnabled = true
				// Enable Wireguard.
				options.ExtraEnvVars["FELIX_WIREGUARDENABLED"] = "true"
			default:
				Fail("bad tunnel option")
			}
			options.ExtraEnvVars["FELIX_BPFConnectTimeLoadBalancingEnabled"] = fmt.Sprint(testOpts.connTimeEnabled)
			options.ExtraEnvVars["FELIX_BPFLogLevel"] = fmt.Sprint(testOpts.bpfLogLevel)
			if testOpts.dsr {
				options.ExtraEnvVars["FELIX_BPFExternalServiceMode"] = "dsr"
			}
			options.ExternalIPs = true
			options.ExtraEnvVars["FELIX_BPFExtToServiceConnmark"] = "0x80"
		})

		JustAfterEach(func() {
			if CurrentGinkgoTestDescription().Failed {
				currBpfsvcs, currBpfeps := dumpNATmaps(felixes)

				for i, felix := range felixes {
					felix.Exec("iptables-save", "-c")
					felix.Exec("ip", "r")
					felix.Exec("ip", "route", "show", "cached")
					felix.Exec("calico-bpf", "ipsets", "dump")
					felix.Exec("calico-bpf", "routes", "dump")
					felix.Exec("calico-bpf", "nat", "dump")
					felix.Exec("calico-bpf", "conntrack", "dump")
					felix.Exec("calico-bpf", "arp", "dump")
					log.Infof("[%d]FrontendMap: %+v", i, currBpfsvcs[i])
					log.Infof("[%d]NATBackend: %+v", i, currBpfeps[i])
					log.Infof("[%d]SendRecvMap: %+v", i, dumpSendRecvMap(felix))
				}
				externalClient.Exec("ip", "route", "show", "cached")
			}
		})

		AfterEach(func() {
			log.Info("AfterEach starting")
			for _, f := range felixes {
				if !felixPanicExpected {
					f.Exec("calico-bpf", "connect-time", "clean")
				}
				f.Stop()
			}
			externalClient.Stop()
			log.Info("AfterEach done")
		})

		AfterEach(func() {
			infra.Stop()
		})

		createPolicy := func(policy *api.GlobalNetworkPolicy) *api.GlobalNetworkPolicy {
			log.WithField("policy", dumpResource(policy)).Info("Creating policy")
			policy, err := calicoClient.GlobalNetworkPolicies().Create(utils.Ctx, policy, utils.NoOptions)
			Expect(err).NotTo(HaveOccurred())
			return policy
		}

		updatePolicy := func(policy *api.GlobalNetworkPolicy) *api.GlobalNetworkPolicy {
			log.WithField("policy", dumpResource(policy)).Info("Updating policy")
			policy, err := calicoClient.GlobalNetworkPolicies().Update(utils.Ctx, policy, utils.NoOptions)
			Expect(err).NotTo(HaveOccurred())
			return policy
		}
		_ = updatePolicy

		Describe("with a single node and an allow-all policy", func() {
			var (
				hostW   *workload.Workload
				w       [2]*workload.Workload
				wepCopy [2]*api.WorkloadEndpoint
			)

			if !testOpts.connTimeEnabled {
				// These tests don't depend on NAT.
				return
			}

			if testOpts.tunnel != "none" {
				// Single node so tunnel doesn't matter.
				return
			}

			JustBeforeEach(func() {
				felixes, calicoClient = infrastructure.StartNNodeTopology(1, options, infra)

				hostW = workload.Run(
					felixes[0],
					"host",
					"default",
					felixes[0].IP, // Same IP as felix means "run in the host's namespace"
					"8055",
					testOpts.protocol)

				// Start a couple of workloads so we can check workload-to-workload and workload-to-host.
				for i := 0; i < 2; i++ {
					wIP := fmt.Sprintf("10.65.0.%d", i+2)
					w[i] = workload.Run(felixes[0], fmt.Sprintf("w%d", i), "default", wIP, "8055", testOpts.protocol)
					w[i].WorkloadEndpoint.Labels = map[string]string{"name": w[i].Name}
					// WEP gets clobbered when we add it to the datastore, take a copy so we can re-create the WEP.
					wepCopy[i] = w[i].WorkloadEndpoint
					w[i].ConfigureInInfra(infra)
				}

				err := infra.AddDefaultDeny()
				Expect(err).NotTo(HaveOccurred())

				pol := api.NewGlobalNetworkPolicy()
				pol.Namespace = "fv"
				pol.Name = "policy-1"
				pol.Spec.Ingress = []api.Rule{{Action: "Allow"}}
				pol.Spec.Egress = []api.Rule{{Action: "Allow"}}
				pol.Spec.Selector = "all()"

				pol = createPolicy(pol)
			})

			if testOpts.bpfLogLevel == "debug" && testOpts.protocol == "tcp" {
				Describe("with custom IptablesMarkMask", func() {
					BeforeEach(func() {
						// Disable core dumps, we know we're about to cause a panic.
						options.ExtraEnvVars["GOTRACEBACK"] = ""
						felixPanicExpected = true
					})

					It("0xffff000 not covering BPF bits should panic", func() {
						felixPanicExpected = true
						panicC := felixes[0].WatchStdoutFor(regexp.MustCompile("PANIC.*IptablesMarkMask doesn't cover bits that are used"))

						fc := api.NewFelixConfiguration()
						fc.Name = "default"
						mark := uint32(0x0ffff000)
						fc.Spec.IptablesMarkMask = &mark
						fc, err := calicoClient.FelixConfigurations().Create(context.Background(), fc, options2.SetOptions{})
						Expect(err).NotTo(HaveOccurred())

						Eventually(panicC, "5s", "100ms").Should(BeClosed())
					})

					It("0xfff00000 only covering BPF bits should panic", func() {
						panicC := felixes[0].WatchStdoutFor(regexp.MustCompile("PANIC.*Not enough mark bits available"))

						fc := api.NewFelixConfiguration()
						fc.Name = "default"
						mark := uint32(0xfff00000)
						fc.Spec.IptablesMarkMask = &mark
						fc, err := calicoClient.FelixConfigurations().Create(context.Background(), fc, options2.SetOptions{})
						Expect(err).NotTo(HaveOccurred())

						Eventually(panicC, "5s", "100ms").Should(BeClosed())
					})
				})
			}

			Describe("with DefaultEndpointToHostAction=DROP", func() {
				BeforeEach(func() {
					options.ExtraEnvVars["FELIX_DefaultEndpointToHostAction"] = "DROP"
				})
				It("should only allow traffic from workload to workload", func() {
					cc.ExpectSome(w[0], w[1])
					cc.ExpectSome(w[1], w[0])
					cc.ExpectNone(w[1], hostW)
					cc.ExpectSome(hostW, w[0])
					cc.CheckConnectivity()
				})
			})

			getMapIDByPath := func(felix *infrastructure.Felix, filename string) (int, error) {
				out, err := felix.ExecOutput("bpftool", "map", "show", "pinned", filename, "-j")
				if err != nil {
					return 0, err
				}
				var mapMeta struct {
					ID    int    `json:"id"`
					Error string `json:"error"`
				}
				err = json.Unmarshal([]byte(out), &mapMeta)
				if err != nil {
					return 0, err
				}
				if mapMeta.Error != "" {
					return 0, errors.New(mapMeta.Error)
				}
				return mapMeta.ID, nil
			}

			mustGetMapIDByPath := func(felix *infrastructure.Felix, filename string) int {
				var mapID int
				Eventually(func() error {
					var err error
					mapID, err = getMapIDByPath(felix, filename)
					return err
				}, "5s").ShouldNot(HaveOccurred())
				return mapID
			}

			Describe("with DefaultEndpointToHostAction=ACCEPT", func() {
				BeforeEach(func() {
					options.ExtraEnvVars["FELIX_DefaultEndpointToHostAction"] = "ACCEPT"
				})
				It("should allow traffic from workload to workload and to/from host", func() {
					cc.ExpectSome(w[0], w[1])
					cc.ExpectSome(w[1], w[0])
					cc.ExpectSome(w[1], hostW)
					cc.ExpectSome(hostW, w[0])
					cc.CheckConnectivity()
				})
			})

			if testOpts.protocol != "udp" { // No need to run these tests per-protocol.

				mapPath := conntrack.Map(&bpf.MapContext{}).Path()

				Describe("with map repinning enabled", func() {
					BeforeEach(func() {
						options.ExtraEnvVars["FELIX_DebugBPFMapRepinEnabled"] = "true"
					})

					It("should repin maps", func() {
						// Wait for the first felix to create its maps.
						mapID := mustGetMapIDByPath(felixes[0], mapPath)

						// Now, start a completely independent felix, which will get its own bpffs.  It should re-pin the
						// maps, picking up the ones from the first felix.
						extraFelix, _ := infrastructure.StartSingleNodeTopology(options, infra)
						defer extraFelix.Stop()

						secondMapID := mustGetMapIDByPath(extraFelix, mapPath)
						Expect(mapID).NotTo(BeNumerically("==", 0))
						Expect(mapID).To(BeNumerically("==", secondMapID))
					})
				})

				Describe("with map repinning disabled", func() {
					It("should repin maps", func() {
						// Wait for the first felix to create its maps.
						mapID := mustGetMapIDByPath(felixes[0], mapPath)

						// Now, start a completely independent felix, which will get its own bpffs.  It should make its own
						// maps.
						extraFelix, _ := infrastructure.StartSingleNodeTopology(options, infra)
						defer extraFelix.Stop()

						secondMapID := mustGetMapIDByPath(extraFelix, mapPath)
						Expect(mapID).NotTo(BeNumerically("==", 0))
						Expect(mapID).NotTo(BeNumerically("==", secondMapID))
					})
				})

				It("should clean up jump maps", func() {
					numJumpMaps := func() int {
						output, err := felixes[0].ExecOutput("sh", "-c", "find /sys/fs/bpf/tc -name cali_jump")
						Expect(err).NotTo(HaveOccurred())
						return strings.Count(output, "cali_jump")
					}

					expJumpMaps := func(numWorkloads int) int {
						numHostIfaces := 1
						expectedNumMaps := 2*numWorkloads + 2*numHostIfaces
						return expectedNumMaps
					}

					// Check start-of-day number of interfaces.
					Eventually(numJumpMaps, "15s", "200ms").Should(
						BeNumerically("==", expJumpMaps(len(w))),
						"Unexpected number of jump maps at start of day")

					// Remove a workload.
					w[0].RemoveFromInfra(infra)
					w[0].Stop()

					// Need a long timeout here because felix throttles cleanups.
					Eventually(numJumpMaps, "15s", "200ms").Should(
						BeNumerically("==", expJumpMaps(len(w)-1)),
						"Unexpected number of jump maps after removing workload")
				})

				It("should recover if the BPF programs are removed", func() {
					flapInterface := func() {
						By("Flapping interface")
						felixes[0].Exec("ip", "link", "set", "down", w[0].InterfaceName)
						felixes[0].Exec("ip", "link", "set", "up", w[0].InterfaceName)
					}

					recreateWEP := func() {
						By("Recreating WEP.")
						w[0].RemoveFromInfra(infra)
						w[0].WorkloadEndpoint = wepCopy[0]
						w[0].ConfigureInInfra(infra)
					}

					for _, trigger := range []func(){flapInterface, recreateWEP} {
						// Wait for initial programming to complete.
						cc.Expect(Some, w[0], w[1])
						cc.CheckConnectivity()
						cc.ResetExpectations()

						By("handling ingress program removal")
						felixes[0].Exec("tc", "filter", "del", "ingress", "dev", w[0].InterfaceName)

						// Removing the ingress program should break connectivity due to the lack of "seen" mark.
						cc.Expect(None, w[0], w[1])
						cc.CheckConnectivity()
						cc.ResetExpectations()

						// Trigger felix to recover.
						trigger()
						cc.Expect(Some, w[0], w[1])
						cc.CheckConnectivity()

						// Check the program is put back.
						Eventually(func() string {
							out, _ := felixes[0].ExecOutput("tc", "filter", "show", "ingress", "dev", w[0].InterfaceName)
							return out
						}, "5s", "200ms").Should(ContainSubstring("calico_from_workload_ep"))

						By("handling egress program removal")
						felixes[0].Exec("tc", "filter", "del", "egress", "dev", w[0].InterfaceName)
						// Removing the egress program doesn't stop traffic.

						// Trigger felix to recover.
						trigger()

						// Check the program is put back.
						Eventually(func() string {
							out, _ := felixes[0].ExecOutput("tc", "filter", "show", "egress", "dev", w[0].InterfaceName)
							return out
						}, "5s", "200ms").Should(ContainSubstring("calico_to_workload_ep"))
						cc.CheckConnectivity()

						By("Handling qdisc removal")
						felixes[0].Exec("tc", "qdisc", "delete", "dev", w[0].InterfaceName, "clsact")

						// Trigger felix to recover.
						trigger()

						// Check programs are put back.
						Eventually(func() string {
							out, _ := felixes[0].ExecOutput("tc", "filter", "show", "ingress", "dev", w[0].InterfaceName)
							return out
						}, "5s", "200ms").Should(ContainSubstring("calico_from_workload_ep"))
						Eventually(func() string {
							out, _ := felixes[0].ExecOutput("tc", "filter", "show", "egress", "dev", w[0].InterfaceName)
							return out
						}, "5s", "200ms").Should(ContainSubstring("calico_to_workload_ep"))
						cc.CheckConnectivity()
						cc.ResetExpectations()

						// Add a policy to block traffic.
						By("Adding deny policy")
						denyPol := api.NewGlobalNetworkPolicy()
						denyPol.Name = "policy-2"
						var one float64 = 1
						denyPol.Spec.Order = &one
						denyPol.Spec.Ingress = []api.Rule{{Action: "Deny"}}
						denyPol.Spec.Egress = []api.Rule{{Action: "Deny"}}
						denyPol.Spec.Selector = "all()"
						denyPol = createPolicy(denyPol)

						cc.Expect(None, w[0], w[1])
						cc.Expect(None, w[1], w[0])
						cc.CheckConnectivity()
						cc.ResetExpectations()

						By("Removing deny policy")
						_, err := calicoClient.GlobalNetworkPolicies().Delete(context.Background(), "policy-2", options2.DeleteOptions{})
						Expect(err).NotTo(HaveOccurred())

						cc.Expect(Some, w[0], w[1])
						cc.Expect(Some, w[1], w[0])
						cc.CheckConnectivity()
						cc.ResetExpectations()
					}
				})
			}

			if testOpts.nonProtoTests {
				// We can only test that felix _sets_ this because the flag is one-way and cannot be unset.
				It("should enable the kernel.unprivileged_bpf_disabled sysctl", func() {
					Eventually(func() string {
						out, err := felixes[0].ExecOutput("sysctl", "kernel.unprivileged_bpf_disabled")
						if err != nil {
							log.WithError(err).Error("Failed to run sysctl")
						}
						return out
					}).Should(ContainSubstring("kernel.unprivileged_bpf_disabled = 1"))
				})
			}
		})

		const numNodes = 3
		var (
			w     [numNodes][2]*workload.Workload
			hostW [numNodes]*workload.Workload
		)

		setupCluster := func() {
			felixes, calicoClient = infrastructure.StartNNodeTopology(numNodes, options, infra)

			addWorkload := func(run bool, ii, wi, port int, labels map[string]string) *workload.Workload {
				if labels == nil {
					labels = make(map[string]string)
				}

				wIP := fmt.Sprintf("10.65.%d.%d", ii, wi+2)
				wName := fmt.Sprintf("w%d%d", ii, wi)

				w := workload.New(felixes[ii], wName, "default",
					wIP, strconv.Itoa(port), testOpts.protocol)
				if run {
					w.Start()
				}

				labels["name"] = w.Name
				labels["workload"] = "regular"

				w.WorkloadEndpoint.Labels = labels
				w.ConfigureInInfra(infra)
				if options.UseIPPools {
					// Assign the workload's IP in IPAM, this will trigger calculation of routes.
					err := calicoClient.IPAM().AssignIP(context.Background(), ipam.AssignIPArgs{
						IP:       cnet.MustParseIP(wIP),
						HandleID: &w.Name,
						Attrs: map[string]string{
							ipam.AttributeNode: felixes[ii].Hostname,
						},
						Hostname: felixes[ii].Hostname,
					})
					Expect(err).NotTo(HaveOccurred())
				}

				return w
			}

			// Start a host networked workload on each host for connectivity checks.
			for ii := range felixes {
				// We tell each host-networked workload to open:
				// TODO: Copied from another test
				// - its normal (uninteresting) port, 8055
				// - port 2379, which is both an inbound and an outbound failsafe port
				// - port 22, which is an inbound failsafe port.
				// This allows us to test the interaction between do-not-track policy and failsafe
				// ports.
				hostW[ii] = workload.Run(
					felixes[ii],
					fmt.Sprintf("host%d", ii),
					"default",
					felixes[ii].IP, // Same IP as felix means "run in the host's namespace"
					"8055",
					testOpts.protocol)

				hostW[ii].WorkloadEndpoint.Labels = map[string]string{"name": hostW[ii].Name}
				hostW[ii].ConfigureInInfra(infra)

				// Two workloads on each host so we can check the same host and other host cases.
				w[ii][0] = addWorkload(true, ii, 0, 8055, map[string]string{"port": "8055"})
				w[ii][1] = addWorkload(true, ii, 1, 8056, nil)
			}

			// Create a workload on node 0 that does not run, but we can use it to set up paths
			deadWorkload = addWorkload(false, 0, 2, 8057, nil)

			// We will use this container to model an external client trying to connect into
			// workloads on a host.  Create a route in the container for the workload CIDR.
			// TODO: Copied from another test
			externalClient = containers.Run("external-client",
				containers.RunOpts{AutoRemove: true},
				"--privileged", // So that we can add routes inside the container.
				utils.Config.BusyboxImage,
				"/bin/sh", "-c", "sleep 1000")
			_ = externalClient

			err := infra.AddDefaultDeny()
			Expect(err).NotTo(HaveOccurred())
		}

		Describe(fmt.Sprintf("with a %d node cluster", numNodes), func() {
			BeforeEach(func() {
				setupCluster()
			})

			if testOpts.protocol == "udp" && testOpts.udpUnConnected {
				It("should have no connectivity to a pod before it is added to the datamodel", func() {
					// Above BeforeEach adds a default-deny but for this test we want policy to be open
					// so that it's only the lack of datastore configuration that blocks traffic.
					policy := api.NewNetworkPolicy()
					policy.Name = "allow-all"
					policy.Namespace = "default"
					one := float64(1)
					policy.Spec.Order = &one
					policy.Spec.Ingress = []api.Rule{{Action: api.Allow}}
					policy.Spec.Egress = []api.Rule{{Action: api.Allow}}
					policy.Spec.Selector = "all()"
					_, err := calicoClient.NetworkPolicies().Create(utils.Ctx, policy, utils.NoOptions)

					// The hardest path to secure with BPF is packets to the newly-added workload.  We can't block
					// the traffic with BPF until we have a BPF program in place so we rely on iptables catch-alls.

					// Set up a workload but do not add it to the datastore.
					dpOnlyWorkload := workload.New(felixes[1], "w-dp", "default", "10.65.1.5", "8057", testOpts.protocol)
					err = dpOnlyWorkload.Start()
					Expect(err).NotTo(HaveOccurred())
					felixes[1].Exec("ip", "route", "add", dpOnlyWorkload.IP, "dev", dpOnlyWorkload.InterfaceName, "scope", "link")

					// Attach tcpdump to the workload so we can verify that we don't see any packets at all.  We need
					// to verify ingress and egress separately since a round-trip test would be blocked by either.
					tcpdump := dpOnlyWorkload.AttachTCPDump()
					tcpdump.SetLogEnabled(true)
					pattern := fmt.Sprintf(`IP .* %s\.8057: UDP`, dpOnlyWorkload.IP)
					tcpdump.AddMatcher("UDP-8057", regexp.MustCompile(pattern))
					tcpdump.Start()
					defer tcpdump.Stop()

					// Send packets in the background.
					var wg sync.WaitGroup
					wg.Add(1)
					ctx, cancelFn := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancelFn()
					go func() {
						defer wg.Done()
						defer GinkgoRecover()
						for {
							if ctx.Err() != nil {
								return
							}
							_, err = w[1][0].RunCmd("/pktgen", w[1][0].IP, dpOnlyWorkload.IP, "udp",
								"--port-src", "30444", "--port-dst", "8057")
							Expect(err).NotTo(HaveOccurred())
							time.Sleep(100 * (time.Millisecond))
						}
					}()
					defer wg.Wait()

					Consistently(tcpdump.MatchCountFn("UDP-8057"), "5s", "200ms").Should(
						BeNumerically("==", 0),
						"Traffic to the workload should be blocked before datastore is configured")

					dpOnlyWorkload.ConfigureInInfra(infra)

					Eventually(tcpdump.MatchCountFn("UDP-8057"), "5s", "200ms").Should(
						BeNumerically(">", 0),
						"Traffic to the workload should be allowed after datastore is configured")
				})
			}

			It("should have correct routes", func() {
				expectedRoutes := expectedRouteDump
				if felixes[0].ExpectedIPIPTunnelAddr != "" || felixes[0].ExpectedVXLANTunnelAddr != "" || felixes[0].ExpectedWireguardTunnelAddr != "" {
					expectedRoutes = expectedRouteDumpWithTunnelAddr
				}
				dumpRoutes := func() string {
					out, err := felixes[0].ExecOutput("calico-bpf", "routes", "dump")
					if err != nil {
						return fmt.Sprint(err)
					}

					lines := strings.Split(out, "\n")
					var filteredLines []string
					idxRE := regexp.MustCompile(`idx \d+`)
					for _, l := range lines {
						l = strings.TrimLeft(l, " ")
						if len(l) == 0 {
							continue
						}
						l = strings.ReplaceAll(l, felixes[0].IP, "FELIX_0")
						l = strings.ReplaceAll(l, felixes[1].IP, "FELIX_1")
						l = strings.ReplaceAll(l, felixes[2].IP, "FELIX_2")
						l = idxRE.ReplaceAllLiteralString(l, "idx -")
						filteredLines = append(filteredLines, l)
					}
					sort.Strings(filteredLines)
					return strings.Join(filteredLines, "\n")
				}
				Eventually(dumpRoutes).Should(Equal(expectedRoutes), dumpRoutes)
			})

			It("should only allow traffic from the local host by default", func() {
				// Same host, other workload.
				cc.ExpectNone(w[0][0], w[0][1])
				cc.ExpectNone(w[0][1], w[0][0])
				// Workloads on other host.
				cc.ExpectNone(w[0][0], w[1][0])
				cc.ExpectNone(w[1][0], w[0][0])
				// Hosts.
				cc.ExpectSome(felixes[0], w[0][0])
				cc.ExpectNone(felixes[1], w[0][0])
				cc.CheckConnectivity()
			})

			It("should allow host -> host", func() {
				// XXX as long as there is no HEP policy
				// using hostW as a sink
				cc.Expect(Some, felixes[0], hostW[1])
				cc.Expect(Some, felixes[1], hostW[0])
				cc.CheckConnectivity()
			})

			Context("with a policy allowing ingress to w[0][0] from all regular workloads", func() {
				var (
					pol       *api.GlobalNetworkPolicy
					k8sClient *kubernetes.Clientset
				)

				BeforeEach(func() {
					pol = api.NewGlobalNetworkPolicy()
					pol.Namespace = "fv"
					pol.Name = "policy-1"
					pol.Spec.Ingress = []api.Rule{
						{
							Action: "Allow",
							Source: api.EntityRule{
								Selector: "workload=='regular'",
							},
						},
					}
					pol.Spec.Egress = []api.Rule{
						{
							Action: "Allow",
							Source: api.EntityRule{
								Selector: "workload=='regular'",
							},
						},
					}
					pol.Spec.Selector = "workload=='regular'"

					pol = createPolicy(pol)

					k8sClient = infra.(*infrastructure.K8sDatastoreInfra).K8sClient
					_ = k8sClient
				})

				It("should handle NAT outgoing", func() {
					By("SNATting outgoing traffic with the flag set")
					cc.ExpectSNAT(w[0][0], felixes[0].IP, hostW[1])
					cc.CheckConnectivity()

					if testOpts.tunnel == "none" {
						By("Leaving traffic alone with the flag clear")
						pool, err := calicoClient.IPPools().Get(context.TODO(), "test-pool", options2.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						pool.Spec.NATOutgoing = false
						pool, err = calicoClient.IPPools().Update(context.TODO(), pool, options2.SetOptions{})
						Expect(err).NotTo(HaveOccurred())
						cc.ResetExpectations()
						cc.ExpectSNAT(w[0][0], w[0][0].IP, hostW[1])
						cc.CheckConnectivity()

						By("SNATting again with the flag set")
						pool.Spec.NATOutgoing = true
						pool, err = calicoClient.IPPools().Update(context.TODO(), pool, options2.SetOptions{})
						Expect(err).NotTo(HaveOccurred())
						cc.ResetExpectations()
						cc.ExpectSNAT(w[0][0], felixes[0].IP, hostW[1])
						cc.CheckConnectivity()
					}
				})

				It("connectivity from all workloads via workload 0's main IP", func() {
					cc.ExpectSome(w[0][1], w[0][0])
					cc.ExpectSome(w[1][0], w[0][0])
					cc.ExpectSome(w[1][1], w[0][0])
					cc.CheckConnectivity()
				})

				// Test doesn't use services so ignore the runs with those turned on.
				if testOpts.protocol == "tcp" && !testOpts.connTimeEnabled && !testOpts.dsr {
					It("should not be able to spoof TCP", func() {
						// Make sure the workload is up and has configured its routes.
						By("Having basic connectivity")
						cc.Expect(Some, w[0][0], w[1][0])
						cc.CheckConnectivity()

						// Add a second interface to the workload, this will allow us to adjust the routes
						// inside the workload to move connections from one interface to the other.
						By("Having basic connectivity after setting up the spoof interface")
						w[0][0].AddSpoofInterface()
						// Check that the route manipulation succeeded.
						cc.CheckConnectivity()
						cc.ResetExpectations()

						// PHASE 1: basic single-shot connectivity checks to check that the test infra
						// is basically doing what we want.  I.e. if felix and the workload disagree on
						// interface then new connections get dropped.

						// Switch routes to use the spoofed interface, should fail.
						By("Workload using spoof0, felix expecting eth0, should fail")
						w[0][0].UseSpoofInterface(true)
						cc.Expect(None, w[0][0], w[1][0])
						cc.CheckConnectivity()
						cc.ResetExpectations()

						By("Workload using spoof0, felix expecting spoof0, should succeed")
						w[0][0].RemoveFromInfra(infra)
						w[0][0].ConfigureInInfraAsSpoofInterface(infra)
						cc.Expect(Some, w[0][0], w[1][0])
						cc.CheckConnectivity()
						cc.ResetExpectations()

						By("Both back to eth0, should succeed")
						w[0][0].RemoveSpoofWEPFromInfra(infra)
						w[0][0].ConfigureInInfra(infra)
						w[0][0].UseSpoofInterface(false)
						cc.Expect(Some, w[0][0], w[1][0])
						cc.CheckConnectivity()
						cc.ResetExpectations()

						// PHASE 2: keep a connection up and move it from one interface to the other using the pod's
						// routes.  To the host this looks like one workload is spoofing the other.
						By("Starting permanent connection")
						pc := w[0][0].StartPersistentConnection(w[1][0].IP, 8055, workload.PersistentConnectionOpts{
							MonitorConnectivity: true,
						})
						defer pc.Stop()

						expectPongs := func() {
							EventuallyWithOffset(1, pc.SinceLastPong, "5s").Should(
								BeNumerically("<", time.Second),
								"Expected to see pong responses on the connection but didn't receive any")
							log.Info("Pongs received within last 1s")
						}
						expectNoPongs := func() {
							EventuallyWithOffset(1, pc.SinceLastPong, "5s").Should(
								BeNumerically(">", time.Second),
								"Expected to see pong responses stop but continued to receive them")
							log.Info("No pongs received for >1s")
						}

						// Simulate a second WEP for the spoof interface.
						w[0][0].ConfigureOtherWEPInInfraAsSpoofInterface(infra)

						// Should get some pongs to start with...
						By("Should get pongs to start with")
						expectPongs()

						// Switch the route, should start dropping packets.
						w[0][0].UseSpoofInterface(true)
						By("Should no longer get pongs when using the spoof interface")
						expectNoPongs()

						// Switch the route back, should work.
						w[0][0].UseSpoofInterface(false)
						By("Should get pongs again after switching back")
						expectPongs()

						// Switch the route, should start dropping packets.
						w[0][0].UseSpoofInterface(true)
						By("Should no longer get pongs when using the spoof interface")
						expectNoPongs()

						// Move WEP to spoof interface
						w[0][0].RemoveFromInfra(infra)
						w[0][0].RemoveSpoofWEPFromInfra(infra)
						w[0][0].ConfigureInInfraAsSpoofInterface(infra)
						By("Should get pongs again after switching WEP to spoof iface")
						expectPongs()
					})
				}

				// Test doesn't use services so ignore the runs with those turned on.
				if testOpts.protocol == "udp" && !testOpts.connTimeEnabled && !testOpts.dsr {
					It("should not be able to spoof UDP", func() {
						By("allowing any traffic", func() {
							pol.Spec.Ingress = []api.Rule{
								{
									Action: "Allow",
									Source: api.EntityRule{
										Nets: []string{
											"0.0.0.0/0",
										},
									},
								},
							}
							pol = updatePolicy(pol)

							cc.ExpectSome(w[1][0], w[0][0])
							cc.ExpectSome(w[1][1], w[0][0])
							cc.CheckConnectivity()
						})

						By("testing that packet sent by another workload is dropped", func() {
							tcpdump := w[0][0].AttachTCPDump()
							tcpdump.SetLogEnabled(true)
							matcher := fmt.Sprintf("IP %s\\.30444 > %s\\.30444: UDP", w[1][0].IP, w[0][0].IP)
							tcpdump.AddMatcher("UDP-30444", regexp.MustCompile(matcher))
							tcpdump.Start(testOpts.protocol, "port", "30444", "or", "port", "30445")
							defer tcpdump.Stop()

							// send a packet from the correct workload to create a conntrack entry
							_, err := w[1][0].RunCmd("/pktgen", w[1][0].IP, w[0][0].IP, "udp",
								"--port-src", "30444", "--port-dst", "30444")
							Expect(err).NotTo(HaveOccurred())

							// We must eventually see the packet at the target
							Eventually(func() int { return tcpdump.MatchCount("UDP-30444") }).
								Should(BeNumerically("==", 1), matcher)

							// Send a spoofed packet from a different pod. Since we hit the
							// conntrack we would not do the WEP only RPF check.
							_, err = w[1][1].RunCmd("/pktgen", w[1][0].IP, w[0][0].IP, "udp",
								"--port-src", "30444", "--port-dst", "30444")
							Expect(err).NotTo(HaveOccurred())

							// Since the packet will get dropped, we would not see it at the dest.
							// So we send another good packet from the spoofing workload, that we
							// will see at the dest.
							matcher2 := fmt.Sprintf("IP %s\\.30445 > %s\\.30445: UDP", w[1][1].IP, w[0][0].IP)
							tcpdump.AddMatcher("UDP-30445", regexp.MustCompile(matcher2))

							_, err = w[1][1].RunCmd("/pktgen", w[1][1].IP, w[0][0].IP, "udp",
								"--port-src", "30445", "--port-dst", "30445")
							Expect(err).NotTo(HaveOccurred())

							// Wait for the good packet from the bad workload
							Eventually(func() int { return tcpdump.MatchCount("UDP-30445") }).
								Should(BeNumerically("==", 1), matcher2)

							// Check that we have not seen the spoofed packet. If there was not
							// packet reordering, which in our setup is guaranteed not to happen,
							// we know that the spoofed packet was dropped.
							Expect(tcpdump.MatchCount("UDP-30444")).To(BeNumerically("==", 1), matcher)
						})

						var eth20, eth30 *workload.Workload

						defer func() {
							if eth20 != nil {
								eth20.Stop()
							}
							if eth30 != nil {
								eth30.Stop()
							}
						}()

						// Now, set up a topology that mimics two host NICs by creating one workload per fake NIC.
						// We then move a route between the two NICs to pretend that there's a workload behind
						// one or other of them.
						//
						//      eth20 = workload used as a NIC
						//         - eth20 ------ movable fake workload 10.65.15.15
						//       192.168.20.1
						//       /
						//    10.0.0.20
						// Felix
						//    10.0.0.30
						//       \
						//       192.168.30.1
						//         - eth30 ------ movable fake workload 10.65.15.15
						//      eth30 = workload used as a NIC
						//
						fakeWorkloadIP := "10.65.15.15"

						By("setting up node's fake external ifaces", func() {
							// We name the ifaces ethXY since such ifaces are
							// treated by felix as external to the node
							//
							// Using a test-workload creates the namespaces and the
							// interfaces to emulate the host NICs

							eth20 = &workload.Workload{
								Name:          "eth20",
								C:             felixes[1].Container,
								IP:            "192.168.20.1",
								Ports:         "57005", // 0xdead
								Protocol:      testOpts.protocol,
								InterfaceName: "eth20",
							}
							err := eth20.Start()
							Expect(err).NotTo(HaveOccurred())

							// assign address to eth20 and add route to the .20 network
							felixes[1].Exec("ip", "route", "add", "192.168.20.0/24", "dev", "eth20")
							felixes[1].Exec("ip", "addr", "add", "10.0.0.20/32", "dev", "eth20")
							_, err = eth20.RunCmd("ip", "route", "add", "10.0.0.20/32", "dev", "eth0")
							Expect(err).NotTo(HaveOccurred())
							// Add a route to the test workload to the fake external
							// client emulated by the test-workload
							_, err = eth20.RunCmd("ip", "route", "add", w[1][1].IP+"/32", "via", "10.0.0.20")
							Expect(err).NotTo(HaveOccurred())

							eth30 = &workload.Workload{
								Name:          "eth30",
								C:             felixes[1].Container,
								IP:            "192.168.30.1",
								Ports:         "57005", // 0xdead
								Protocol:      testOpts.protocol,
								InterfaceName: "eth30",
							}
							err = eth30.Start()
							Expect(err).NotTo(HaveOccurred())

							// assign address to eth30 and add route to the .30 network
							felixes[1].Exec("ip", "route", "add", "192.168.30.0/24", "dev", "eth30")
							felixes[1].Exec("ip", "addr", "add", "10.0.0.30/32", "dev", "eth30")
							_, err = eth30.RunCmd("ip", "route", "add", "10.0.0.30/32", "dev", "eth0")
							Expect(err).NotTo(HaveOccurred())
							// Add a route to the test workload to the fake external
							// client emulated by the test-workload
							_, err = eth30.RunCmd("ip", "route", "add", w[1][1].IP+"/32", "via", "10.0.0.30")
							Expect(err).NotTo(HaveOccurred())

							// Make sure that networking with the .20 and .30 networks works
							cc.ResetExpectations()
							cc.ExpectSome(w[1][1], TargetIP(eth20.IP), 0xdead)
							cc.ExpectSome(w[1][1], TargetIP(eth30.IP), 0xdead)
							cc.CheckConnectivity()
						})

						By("testing that external traffic updates the RPF check if routing changes", func() {
							// set the route to the fake workload to .20 network
							felixes[1].Exec("ip", "route", "add", fakeWorkloadIP+"/32", "dev", "eth20")

							tcpdump := w[1][1].AttachTCPDump()
							tcpdump.SetLogEnabled(true)
							matcher := fmt.Sprintf("IP %s\\.30446 > %s\\.30446: UDP", fakeWorkloadIP, w[1][1].IP)
							tcpdump.AddMatcher("UDP-30446", regexp.MustCompile(matcher))
							tcpdump.Start()
							defer tcpdump.Stop()

							_, err := eth20.RunCmd("/pktgen", fakeWorkloadIP, w[1][1].IP, "udp",
								"--port-src", "30446", "--port-dst", "30446")
							Expect(err).NotTo(HaveOccurred())

							// Expect to receive the packet from the .20 as the routing is correct
							Eventually(func() int { return tcpdump.MatchCount("UDP-30446") }).
								Should(BeNumerically("==", 1), matcher)

							ctBefore := dumpCTMap(felixes[1])

							k := conntrack.NewKey(17, net.ParseIP(w[1][1].IP).To4(), 30446,
								net.ParseIP(fakeWorkloadIP).To4(), 30446)
							Expect(ctBefore).To(HaveKey(k))

							// XXX Since the same code is used to do the drop of spoofed
							// packet between pods, we do not repeat it here as it is not 100%
							// bulletproof.
							//
							// We should perhaps compare the iptables counter and see if the
							// packet was dropped by the RPF check.

							// Change the routing to be from the .30
							felixes[1].Exec("ip", "route", "del", fakeWorkloadIP+"/32", "dev", "eth20")
							felixes[1].Exec("ip", "route", "add", fakeWorkloadIP+"/32", "dev", "eth30")

							_, err = eth30.RunCmd("/pktgen", fakeWorkloadIP, w[1][1].IP, "udp",
								"--port-src", "30446", "--port-dst", "30446")
							Expect(err).NotTo(HaveOccurred())

							// Expect the packet from the .30 to make it through as RPF will
							// allow it and we will update the expected interface
							Eventually(func() int { return tcpdump.MatchCount("UDP-30446") }).
								Should(BeNumerically("==", 2), matcher)

							ctAfter := dumpCTMap(felixes[1])
							Expect(ctAfter).To(HaveKey(k))

							// Ifindex must have changed
							// B2A because of IPA > IPB - deterministic
							Expect(ctBefore[k].Data().B2A.Ifindex).NotTo(BeNumerically("==", 0))
							Expect(ctAfter[k].Data().B2A.Ifindex).NotTo(BeNumerically("==", 0))
							Expect(ctBefore[k].Data().B2A.Ifindex).
								NotTo(BeNumerically("==", ctAfter[k].Data().B2A.Ifindex))
						})
					})
				}

				Describe("Test Load balancer service with external IP", func() {
					if testOpts.connTimeEnabled {
						// FIXME externalClient also does conntime balancing
						return
					}

					srcIPRange := []string{}
					externalIP := []string{extIP}
					testSvcName := "test-lb-service-extip"
					tgtPort := 8055
					var testSvc *v1.Service
					var ip []string
					var port uint16
					BeforeEach(func() {
						externalClient.EnsureBinary("test-connection")
						externalClient.Exec("ip", "route", "add", extIP, "via", felixes[0].IP)
						testSvc = k8sCreateLBServiceWithEndPoints(k8sClient, testSvcName, "10.101.0.10", w[0][0], 80, tgtPort,
							testOpts.protocol, externalIP, srcIPRange)
						// when we point Load Balancer to a node in GCE it adds local routes to the external IP on the hosts.
						// Similarity add local routes for externalIP on felixes[0], felixes[1]
						felixes[1].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
						felixes[0].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
						ip = testSvc.Spec.ExternalIPs
						port = uint16(testSvc.Spec.Ports[0].Port)
						pol.Spec.Ingress = []api.Rule{
							{
								Action: "Allow",
								Source: api.EntityRule{
									Nets: []string{
										externalClient.IP + "/32",
										w[0][1].IP + "/32",
										w[1][0].IP + "/32",
										w[1][1].IP + "/32",
									},
								},
							},
						}
						pol = updatePolicy(pol)
					})

					It("should have connectivity from workloads[1][0],[1][1], [0][1] and external client via external IP to workload 0", func() {
						cc.ExpectSome(w[1][0], TargetIP(ip[0]), port)
						cc.ExpectSome(w[1][1], TargetIP(ip[0]), port)
						cc.ExpectSome(w[0][1], TargetIP(ip[0]), port)
						cc.ExpectSome(externalClient, TargetIP(ip[0]), port)
						cc.CheckConnectivity()
					})

					It("should handle temporary overlap of external IPs", func() {
						By("Having connectivity to external IP initially")
						cc.ExpectSome(externalClient, TargetIP(ip[0]), port)
						cc.CheckConnectivity()

						By("Adding second service with same external IP")
						testSvc = k8sCreateLBServiceWithEndPoints(k8sClient, testSvcName+"-2", "10.101.0.11", w[0][0], 80, tgtPort,
							testOpts.protocol, externalIP, srcIPRange)

						By("Deleting first service")
						err := k8sClient.CoreV1().Services(testSvc.ObjectMeta.Namespace).Delete(context.Background(), testSvcName, metav1.DeleteOptions{})
						Expect(err).NotTo(HaveOccurred())

						By("Sleeping")
						time.Sleep(20 * time.Second)
						By("And still having connectivity...")
						cc.ExpectSome(externalClient, TargetIP(ip[0]), port)
						cc.CheckConnectivity()
					})
				})

				Context("Test load balancer service with src ranges", func() {
					var testSvc *v1.Service
					tgtPort := 8055
					externalIP := []string{extIP}
					srcIPRange := []string{"10.65.1.3/24"}
					testSvcName := "test-lb-service-extip"
					var ip []string
					var port uint16
					BeforeEach(func() {
						testSvc = k8sCreateLBServiceWithEndPoints(k8sClient, testSvcName, "10.101.0.10", w[0][0], 80, tgtPort,
							testOpts.protocol, externalIP, srcIPRange)
						felixes[1].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
						felixes[0].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
						ip = testSvc.Spec.ExternalIPs
						port = uint16(testSvc.Spec.Ports[0].Port)
					})
					It("should have connectivity from workloads[1][0],[1][1] via external IP to workload 0", func() {
						cc.ExpectSome(w[1][0], TargetIP(ip[0]), port)
						cc.ExpectSome(w[1][1], TargetIP(ip[0]), port)
						cc.ExpectNone(w[0][1], TargetIP(ip[0]), port)
						cc.CheckConnectivity()
					})
				})

				Context("Test load balancer service with no backend", func() {
					if testOpts.connTimeEnabled || testOpts.udpUnConnected {
						// Skip UDP unconnected, connectime load balancing cases as externalClient also does conntime balancing
						return
					}

					var testSvc *v1.Service
					tgtPort := 8055
					externalIP := []string{extIP}
					srcIPRange := []string{}
					testSvcName := "test-lb-service-extip"
					var port uint16
					var ip []string

					BeforeEach(func() {
						externalClient.EnsureBinary("test-connection")
						externalClient.Exec("ip", "route", "add", extIP, "via", felixes[0].IP)
						// create a service workload as nil, so that the service has no backend
						testSvc = k8sCreateLBServiceWithEndPoints(k8sClient, testSvcName, "10.101.0.10", nil, 80, tgtPort,
							testOpts.protocol, externalIP, srcIPRange)
						felixes[1].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
						felixes[0].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
						ip = testSvc.Spec.ExternalIPs
						port = uint16(testSvc.Spec.Ports[0].Port)
						pol.Spec.Ingress = []api.Rule{
							{
								Action: "Allow",
								Source: api.EntityRule{
									Nets: []string{
										externalClient.IP + "/32",
									},
								},
							},
						}
						pol = updatePolicy(pol)
					})
					It("should not have connectivity from external client, and return connection refused", func() {
						cc.Expect(None, externalClient, TargetIP(ip[0]),
							ExpectWithPorts(port),
							ExpectNoneWithError("connection refused"),
						)
						cc.CheckConnectivity()
					})
				})

				Describe("Test load balancer service with external Client,src ranges", func() {
					if testOpts.connTimeEnabled {
						// FIXME externalClient also does conntime balancing
						return
					}

					var testSvc *v1.Service
					tgtPort := 8055
					externalIP := []string{extIP}
					testSvcName := "test-lb-service-extip"
					var ip []string
					var port uint16
					var srcIPRange []string
					BeforeEach(func() {
						externalClient.Exec("ip", "route", "add", extIP, "via", felixes[0].IP)
						externalClient.EnsureBinary("test-connection")
						pol.Spec.Ingress = []api.Rule{
							{
								Action: "Allow",
								Source: api.EntityRule{
									Nets: []string{
										externalClient.IP + "/32",
									},
								},
							},
						}
						pol = updatePolicy(pol)
						felixes[1].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
						felixes[0].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
						srcIPRange = []string{"10.65.1.3/24"}
					})
					Context("Test LB-service with external Client's IP not in src range", func() {
						BeforeEach(func() {
							testSvc = k8sCreateLBServiceWithEndPoints(k8sClient, testSvcName, "10.101.0.10", w[0][0], 80, tgtPort,
								testOpts.protocol, externalIP, srcIPRange)
							ip = testSvc.Spec.ExternalIPs
							port = uint16(testSvc.Spec.Ports[0].Port)
						})
						It("should not have connectivity from external Client via external IP to workload 0", func() {
							cc.ExpectNone(externalClient, TargetIP(ip[0]), port)
							cc.CheckConnectivity()
						})
					})
					Context("Test LB-service with external Client's IP in src range", func() {
						BeforeEach(func() {
							srcIPRange = []string{externalClient.IP + "/32"}
							testSvc = k8sCreateLBServiceWithEndPoints(k8sClient, testSvcName, "10.101.0.10", w[0][0], 80, tgtPort,
								testOpts.protocol, externalIP, srcIPRange)
							ip = testSvc.Spec.ExternalIPs
							port = uint16(testSvc.Spec.Ports[0].Port)
						})
						It("should have connectivity from external Client via external IP to workload 0", func() {
							cc.ExpectSome(externalClient, TargetIP(ip[0]), port)
							cc.CheckConnectivity()
						})
					})
				})

				Context("Test Service type transitions", func() {
					if testOpts.protocol != "tcp" {
						// Skip tests for UDP, UDP-Unconnected
						return
					}

					var (
						testSvc          *v1.Service
						testSvcNamespace string
					)
					clusterIP := "10.101.0.10"
					testSvcName := "test-service"
					tgtPort := 8055
					externalIP := []string{extIP}

					// Create a service of type clusterIP
					BeforeEach(func() {
						testSvc = k8sService(testSvcName, clusterIP, w[0][0], 80, tgtPort, 0, testOpts.protocol)
						testSvcNamespace = testSvc.ObjectMeta.Namespace
						_, err := k8sClient.CoreV1().Services(testSvcNamespace).Create(context.Background(), testSvc, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
						Eventually(k8sGetEpsForServiceFunc(k8sClient, testSvc), "10s").Should(HaveLen(1),
							"Service endpoints didn't get created? Is controller-manager happy?")
						felixes[1].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
						felixes[0].Exec("ip", "route", "add", "local", extIP, "dev", "eth0")
					})

					It("should have connectivity from all workloads via a service to workload 0", func() {
						ip := testSvc.Spec.ClusterIP
						port := uint16(testSvc.Spec.Ports[0].Port)

						cc.ExpectSome(w[0][1], TargetIP(ip), port)
						cc.ExpectSome(w[1][0], TargetIP(ip), port)
						cc.ExpectSome(w[1][1], TargetIP(ip), port)
						cc.CheckConnectivity()
					})

					/* Below Context handles the following transitions.
					   Cluster IP -> External IP
					   External IP -> LoadBalancer
					   External IP -> NodePort
					   External IP -> Cluster IP
					*/
					Context("change service from cluster IP to external IP", func() {
						var testSvcWithExtIP *v1.Service
						BeforeEach(func() {
							testSvcWithExtIP = k8sServiceWithExtIP(testSvcName, clusterIP, w[0][0], 80, tgtPort, 0, testOpts.protocol, externalIP)
							k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcWithExtIP)
						})

						It("should have connectivity from all workloads via external IP to workload 0", func() {
							ip := testSvcWithExtIP.Spec.ExternalIPs
							port := uint16(testSvcWithExtIP.Spec.Ports[0].Port)
							cc.ExpectSome(w[1][0], TargetIP(ip[0]), port)
							cc.ExpectSome(w[0][1], TargetIP(ip[0]), port)
							cc.ExpectSome(w[1][1], TargetIP(ip[0]), port)
							cc.CheckConnectivity()
						})
						Context("change service type from external IP to LoadBalancer", func() {
							srcIPRange := []string{}
							var testSvcLB *v1.Service
							BeforeEach(func() {
								testSvcLB = k8sLBService(testSvcName, "10.101.0.10", w[0][0].Name, 80, tgtPort, testOpts.protocol,
									externalIP, srcIPRange)
								k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcLB)
							})
							It("should have connectivity from workload 0 to service via external IP", func() {
								ip := testSvcLB.Spec.ExternalIPs
								port := uint16(testSvcLB.Spec.Ports[0].Port)
								cc.ExpectSome(w[1][0], TargetIP(ip[0]), port)
								cc.ExpectSome(w[1][1], TargetIP(ip[0]), port)
								cc.ExpectSome(w[0][1], TargetIP(ip[0]), port)
								cc.CheckConnectivity()
							})
						})

						Context("change Service type from external IP to nodeport", func() {
							var testSvcNodePort *v1.Service
							npPort := uint16(30333)
							BeforeEach(func() {
								testSvcNodePort = k8sService(testSvcName, "10.101.0.10", w[0][0], 80, tgtPort, int32(npPort), testOpts.protocol)
								k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcNodePort)
							})
							It("should have connectivity via the node port to workload 0", func() {
								node1IP := felixes[1].IP
								cc.ExpectSome(w[0][1], TargetIP(node1IP), npPort)
								cc.ExpectSome(w[1][0], TargetIP(node1IP), npPort)
								cc.ExpectSome(w[1][1], TargetIP(node1IP), npPort)

								ip := testSvcWithExtIP.Spec.ExternalIPs
								port := uint16(testSvcWithExtIP.Spec.Ports[0].Port)
								cc.ExpectNone(w[1][0], TargetIP(ip[0]), port)
								cc.ExpectNone(w[0][1], TargetIP(ip[0]), port)
								cc.ExpectNone(w[1][1], TargetIP(ip[0]), port)
								cc.CheckConnectivity()
							})
						})
						Context("change service from external IP to cluster IP", func() {
							var testSvcWithoutExtIP *v1.Service
							BeforeEach(func() {
								testSvcWithoutExtIP = k8sService(testSvcName, "10.101.0.10", w[0][0], 80, tgtPort, 0, testOpts.protocol)
								k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcWithoutExtIP)
							})
							It("should not have connectivity to workload 0 via external IP", func() {
								ip := testSvcWithExtIP.Spec.ExternalIPs
								port := uint16(testSvcWithExtIP.Spec.Ports[0].Port)
								cc.ExpectNone(w[1][0], TargetIP(ip[0]), port)
								cc.ExpectNone(w[1][1], TargetIP(ip[0]), port)
								cc.ExpectNone(w[0][1], TargetIP(ip[0]), port)

								clusterIP = testSvcWithoutExtIP.Spec.ClusterIP
								cc.ExpectSome(w[0][1], TargetIP(clusterIP), port)
								cc.ExpectSome(w[1][0], TargetIP(clusterIP), port)
								cc.ExpectSome(w[1][1], TargetIP(clusterIP), port)
								cc.CheckConnectivity()
							})
						})
					})

					/* Below Context handles the following transitions.
					   Cluster IP -> LoadBalancer
					   LoadBalancer -> External IP
					   LoadBalancer -> NodePort
					   LoadBalancer -> Cluster IP
					*/
					Context("change service type to LoadBalancer", func() {
						srcIPRange := []string{}
						var testSvcLB *v1.Service
						BeforeEach(func() {
							testSvcLB = k8sLBService(testSvcName, "10.101.0.10", w[0][0].Name, 80, tgtPort, testOpts.protocol,
								externalIP, srcIPRange)
							k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcLB)
						})
						It("should have connectivity from workload 0 to service via external IP", func() {
							ip := testSvcLB.Spec.ExternalIPs
							port := uint16(testSvcLB.Spec.Ports[0].Port)
							cc.ExpectSome(w[1][0], TargetIP(ip[0]), port)
							cc.ExpectSome(w[1][1], TargetIP(ip[0]), port)
							cc.ExpectSome(w[0][1], TargetIP(ip[0]), port)
							cc.CheckConnectivity()
						})

						Context("change service from Loadbalancer to external IP", func() {
							var testSvcWithExtIP *v1.Service
							BeforeEach(func() {
								testSvcWithExtIP = k8sServiceWithExtIP(testSvcName, clusterIP, w[0][0], 80, tgtPort, 0, testOpts.protocol, externalIP)
								k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcWithExtIP)
							})

							It("should have connectivity from all workloads via external IP to workload 0", func() {
								ip := testSvcWithExtIP.Spec.ExternalIPs
								port := uint16(testSvcWithExtIP.Spec.Ports[0].Port)
								cc.ExpectSome(w[1][0], TargetIP(ip[0]), port)
								cc.ExpectSome(w[0][1], TargetIP(ip[0]), port)
								cc.ExpectSome(w[1][1], TargetIP(ip[0]), port)
								cc.CheckConnectivity()
							})
						})

						Context("change Service type from Loadbalancer to nodeport", func() {
							var testSvcNodePort *v1.Service
							npPort := uint16(30333)
							BeforeEach(func() {
								testSvcNodePort = k8sService(testSvcName, "10.101.0.10", w[0][0], 80, tgtPort, int32(npPort), testOpts.protocol)
								k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcNodePort)
							})
							It("should have connectivity via the node port to workload 0 and not via external IP", func() {
								ip := testSvcLB.Spec.ExternalIPs
								port := uint16(testSvcLB.Spec.Ports[0].Port)
								cc.ExpectNone(w[1][0], TargetIP(ip[0]), port)
								cc.ExpectNone(w[1][1], TargetIP(ip[0]), port)
								cc.ExpectNone(w[0][1], TargetIP(ip[0]), port)
								node1IP := felixes[1].IP
								cc.ExpectSome(w[0][1], TargetIP(node1IP), npPort)
								cc.ExpectSome(w[1][0], TargetIP(node1IP), npPort)
								cc.ExpectSome(w[1][1], TargetIP(node1IP), npPort)
								cc.CheckConnectivity()
							})
						})
						Context("Change service type from LoadBalancer to cluster IP", func() {
							var testSvcClusterIP *v1.Service
							BeforeEach(func() {
								testSvcClusterIP = k8sService(testSvcName, "10.101.0.10", w[0][0], 80, tgtPort, 0, testOpts.protocol)
								k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcClusterIP)
							})
							It("should have connectivity to workload 0 via cluster IP and not external IP", func() {
								ip := testSvcLB.Spec.ExternalIPs
								port := uint16(testSvcLB.Spec.Ports[0].Port)
								cc.ExpectNone(w[1][0], TargetIP(ip[0]), port)
								cc.ExpectNone(w[1][1], TargetIP(ip[0]), port)
								cc.ExpectNone(w[0][1], TargetIP(ip[0]), port)

								clusterIP = testSvcClusterIP.Spec.ClusterIP

								cc.ExpectSome(w[0][1], TargetIP(clusterIP), port)
								cc.ExpectSome(w[1][0], TargetIP(clusterIP), port)
								cc.ExpectSome(w[1][1], TargetIP(clusterIP), port)
								cc.CheckConnectivity()
							})

						})
					})

					/* Below Context handles the following transitions.
					   Cluster IP -> NodePort
					   NodePort -> External IP
					   NodePort -> LoadBalancer
					   NodePort -> Cluster IP
					*/
					Context("change Service type to nodeport", func() {
						var testSvcNodePort *v1.Service
						npPort := uint16(30333)
						BeforeEach(func() {
							testSvcNodePort = k8sService(testSvcName, "10.101.0.10", w[0][0], 80, tgtPort, int32(npPort), testOpts.protocol)
							k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcNodePort)
						})
						It("should have connectivity via the node port to workload 0", func() {
							node1IP := felixes[1].IP
							node1IPExt := felixes[1].ExternalIP
							cc.ExpectSome(w[0][1], TargetIP(node1IP), npPort)
							cc.ExpectSome(w[1][0], TargetIP(node1IP), npPort)
							cc.ExpectSome(w[1][1], TargetIP(node1IP), npPort)
							cc.ExpectSome(w[0][1], TargetIP(node1IPExt), npPort)
							cc.ExpectSome(w[1][0], TargetIP(node1IPExt), npPort)
							cc.ExpectSome(w[1][1], TargetIP(node1IPExt), npPort)
							cc.CheckConnectivity()
						})

						Context("change service type from nodeport to external IP", func() {
							var testSvcWithExtIP *v1.Service
							BeforeEach(func() {
								testSvcWithExtIP = k8sServiceWithExtIP(testSvcName, clusterIP, w[0][0], 80, tgtPort, 0, testOpts.protocol, externalIP)
								k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcWithExtIP)
							})
							It("should have connectivity via external IP to workload 0 and not node port", func() {
								ip := testSvcWithExtIP.Spec.ExternalIPs
								port := uint16(testSvcWithExtIP.Spec.Ports[0].Port)
								cc.ExpectSome(w[1][0], TargetIP(ip[0]), port)
								cc.ExpectSome(w[0][1], TargetIP(ip[0]), port)
								cc.ExpectSome(w[1][1], TargetIP(ip[0]), port)

								node1IP := felixes[1].IP
								cc.ExpectNone(w[0][1], TargetIP(node1IP), npPort)
								cc.ExpectNone(w[1][0], TargetIP(node1IP), npPort)
								cc.ExpectNone(w[1][1], TargetIP(node1IP), npPort)
								cc.CheckConnectivity()
							})
						})
						Context("change service type from nodeport to LoadBalancer", func() {
							srcIPRange := []string{}
							var testSvcLB *v1.Service
							BeforeEach(func() {
								testSvcLB = k8sLBService(testSvcName, "10.101.0.10", w[0][0].Name, 80, tgtPort, testOpts.protocol,
									externalIP, srcIPRange)
								k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcLB)
							})
							It("should have connectivity from workload 0 to service via external IP and not via nodeport", func() {
								node1IP := felixes[1].IP
								cc.ExpectNone(w[0][1], TargetIP(node1IP), npPort)
								cc.ExpectNone(w[1][0], TargetIP(node1IP), npPort)
								cc.ExpectNone(w[1][1], TargetIP(node1IP), npPort)

								ip := testSvcLB.Spec.ExternalIPs
								port := uint16(testSvcLB.Spec.Ports[0].Port)
								cc.ExpectSome(w[1][0], TargetIP(ip[0]), port)
								cc.ExpectSome(w[1][1], TargetIP(ip[0]), port)
								cc.ExpectSome(w[0][1], TargetIP(ip[0]), port)
								cc.CheckConnectivity()
							})
						})
						Context("Change service type from nodeport to cluster IP", func() {
							var testSvcClusterIP *v1.Service
							BeforeEach(func() {
								testSvcClusterIP = k8sService(testSvcName, "10.101.0.10", w[0][0], 80, tgtPort, 0, testOpts.protocol)
								k8sUpdateService(k8sClient, testSvcNamespace, testSvcName, testSvc, testSvcClusterIP)
							})
							It("should have connectivity to workload 0 via cluster IP and not via nodeport", func() {
								node1IP := felixes[1].IP
								cc.ExpectNone(w[0][1], TargetIP(node1IP), npPort)
								cc.ExpectNone(w[1][0], TargetIP(node1IP), npPort)
								cc.ExpectNone(w[1][1], TargetIP(node1IP), npPort)

								clusterIP = testSvcClusterIP.Spec.ClusterIP
								port := uint16(testSvcClusterIP.Spec.Ports[0].Port)
								cc.ExpectSome(w[0][1], TargetIP(clusterIP), port)
								cc.ExpectSome(w[1][0], TargetIP(clusterIP), port)
								cc.ExpectSome(w[1][1], TargetIP(clusterIP), port)
								cc.CheckConnectivity()
							})

						})

					})
				})
				Context("with test-service configured 10.101.0.10:80 -> w[0][0].IP:8055", func() {
					var (
						testSvc          *v1.Service
						testSvcNamespace string
					)

					testSvcName := "test-service"
					tgtPort := 8055

					BeforeEach(func() {
						testSvc = k8sService(testSvcName, "10.101.0.10", w[0][0], 80, tgtPort, 0, testOpts.protocol)
						testSvcNamespace = testSvc.ObjectMeta.Namespace
						_, err := k8sClient.CoreV1().Services(testSvcNamespace).Create(context.Background(), testSvc, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
						Eventually(k8sGetEpsForServiceFunc(k8sClient, testSvc), "10s").Should(HaveLen(1),
							"Service endpoints didn't get created? Is controller-manager happy?")
					})

					It("should have connectivity from all workloads via a service to workload 0", func() {
						ip := testSvc.Spec.ClusterIP
						port := uint16(testSvc.Spec.Ports[0].Port)

						cc.ExpectSome(w[0][1], TargetIP(ip), port)
						cc.ExpectSome(w[1][0], TargetIP(ip), port)
						cc.ExpectSome(w[1][1], TargetIP(ip), port)
						cc.CheckConnectivity()
					})

					if testOpts.connTimeEnabled {
						It("workload should have connectivity to self via a service", func() {
							ip := testSvc.Spec.ClusterIP
							port := uint16(testSvc.Spec.Ports[0].Port)

							cc.ExpectSome(w[0][0], TargetIP(ip), port)
							cc.CheckConnectivity()
						})

						It("should only have connectivity from the local host via a service to workload 0", func() {
							// Local host is always white-listed (for kubelet health checks).
							ip := testSvc.Spec.ClusterIP
							port := uint16(testSvc.Spec.Ports[0].Port)

							cc.ExpectSome(felixes[0], TargetIP(ip), port)
							cc.ExpectNone(felixes[1], TargetIP(ip), port)
							cc.CheckConnectivity()
						})
					} else {
						It("should not have connectivity from the local host via a service to workload 0", func() {
							// Local host is always white-listed (for kubelet health checks).
							ip := testSvc.Spec.ClusterIP
							port := uint16(testSvc.Spec.Ports[0].Port)

							cc.ExpectNone(felixes[0], TargetIP(ip), port)
							cc.ExpectNone(felixes[1], TargetIP(ip), port)
							cc.CheckConnectivity()
						})
					}

					if testOpts.connTimeEnabled {
						Describe("after updating the policy to allow traffic from hosts", func() {
							BeforeEach(func() {
								pol.Spec.Ingress = []api.Rule{
									{
										Action: "Allow",
										Source: api.EntityRule{
											Selector: "ep-type == 'host'",
										},
									},
								}
								pol = updatePolicy(pol)
							})

							It("should have connectivity from the hosts via a service to workload 0", func() {
								ip := testSvc.Spec.ClusterIP
								port := uint16(testSvc.Spec.Ports[0].Port)

								cc.ExpectSome(felixes[0], TargetIP(ip), port)
								cc.ExpectSome(felixes[1], TargetIP(ip), port)
								cc.ExpectNone(w[0][1], TargetIP(ip), port)
								cc.ExpectNone(w[1][0], TargetIP(ip), port)
								cc.CheckConnectivity()
							})
						})
					}

					It("should create sane conntrack entries and clean them up", func() {
						By("Generating some traffic")
						ip := testSvc.Spec.ClusterIP
						port := uint16(testSvc.Spec.Ports[0].Port)

						cc.ExpectSome(w[0][1], TargetIP(ip), port)
						cc.ExpectSome(w[1][0], TargetIP(ip), port)
						cc.CheckConnectivity()

						By("Checking timestamps on conntrack entries are sane")
						// This test verifies that we correctly interpret conntrack entry timestamps by reading them back
						// and checking that they're (a) in the past and (b) sensibly recent.
						ctDump, err := felixes[0].ExecOutput("calico-bpf", "conntrack", "dump")
						Expect(err).NotTo(HaveOccurred())
						re := regexp.MustCompile(`LastSeen:\s*(\d+)`)
						matches := re.FindAllStringSubmatch(ctDump, -1)
						Expect(matches).ToNot(BeEmpty(), "didn't find any conntrack entries")
						for _, match := range matches {
							lastSeenNanos, err := strconv.ParseInt(match[1], 10, 64)
							Expect(err).NotTo(HaveOccurred())
							nowNanos := bpf.KTimeNanos()
							age := time.Duration(nowNanos - lastSeenNanos)
							Expect(age).To(BeNumerically(">", 0))
							Expect(age).To(BeNumerically("<", 60*time.Second))
						}

						By("Checking conntrack entries are cleaned up")
						// We have UTs that check that all kinds of entries eventually get cleaned up.  This
						// test is mainly to check that the cleanup code actually runs and is able to actually delete
						// entries.
						numWl0ConntrackEntries := func() int {
							ctDump, err := felixes[0].ExecOutput("calico-bpf", "conntrack", "dump")
							Expect(err).NotTo(HaveOccurred())
							return strings.Count(ctDump, w[0][0].IP)
						}

						startingCTEntries := numWl0ConntrackEntries()
						Expect(startingCTEntries).To(BeNumerically(">", 0))

						// TODO reduce timeouts just for this test.
						Eventually(numWl0ConntrackEntries, "180s", "5s").Should(BeNumerically("<", startingCTEntries))
					})

					Context("with test-service port updated", func() {

						var (
							testSvcUpdated      *v1.Service
							natBackBeforeUpdate []nat.BackendMapMem
							natBeforeUpdate     []nat.MapMem
						)

						BeforeEach(func() {
							ip := testSvc.Spec.ClusterIP
							portOld := uint16(testSvc.Spec.Ports[0].Port)
							ipv4 := net.ParseIP(ip)
							oldK := nat.NewNATKey(ipv4, portOld, numericProto)

							// Wait for the NAT maps to converge...
							log.Info("Waiting for NAT maps to converge...")
							startTime := time.Now()
							for {
								if time.Since(startTime) > 5*time.Second {
									Fail("NAT maps failed to converge")
								}
								natBeforeUpdate, natBackBeforeUpdate = dumpNATmaps(felixes)
								for i, m := range natBeforeUpdate {
									if natV, ok := m[oldK]; !ok {
										goto retry
									} else {
										bckCnt := natV.Count()
										if bckCnt != 1 {
											log.Debugf("Expected single backend, not %d; retrying...", bckCnt)
											goto retry
										}
										bckID := natV.ID()
										bckK := nat.NewNATBackendKey(bckID, 0)
										if _, ok := natBackBeforeUpdate[i][bckK]; !ok {
											log.Debugf("Backend not found %v; retrying...", bckK)
											goto retry
										}
									}
								}

								log.Infof("NAT maps converge took %v", time.Since(startTime))
								break
							retry:
								time.Sleep(100 * time.Millisecond)
								log.Info("NAT maps converge retry")
							}
							log.Info("NAT maps converged.")

							testSvcUpdated = k8sService(testSvcName, "10.101.0.10", w[0][0], 88, 8055, 0, testOpts.protocol)

							svc, err := k8sClient.CoreV1().
								Services(testSvcNamespace).
								Get(context.Background(), testSvcName, metav1.GetOptions{})

							testSvcUpdated.ObjectMeta.ResourceVersion = svc.ObjectMeta.ResourceVersion

							_, err = k8sClient.CoreV1().Services(testSvcNamespace).Update(context.Background(), testSvcUpdated, metav1.UpdateOptions{})
							Expect(err).NotTo(HaveOccurred())
							Eventually(k8sGetEpsForServiceFunc(k8sClient, testSvc), "10s").Should(HaveLen(1),
								"Service endpoints didn't get created? Is controller-manager happy?")
						})

						It("should have connectivity from all workloads via the new port", func() {
							ip := testSvcUpdated.Spec.ClusterIP
							port := uint16(testSvcUpdated.Spec.Ports[0].Port)

							cc.ExpectSome(w[0][1], TargetIP(ip), port)
							cc.ExpectSome(w[1][0], TargetIP(ip), port)
							cc.ExpectSome(w[1][1], TargetIP(ip), port)
							cc.CheckConnectivity()
						})

						It("should not have connectivity from all workloads via the old port", func() {
							ip := testSvc.Spec.ClusterIP
							port := uint16(testSvc.Spec.Ports[0].Port)

							cc.ExpectNone(w[0][1], TargetIP(ip), port)
							cc.ExpectNone(w[1][0], TargetIP(ip), port)
							cc.ExpectNone(w[1][1], TargetIP(ip), port)
							cc.CheckConnectivity()

							natmaps, natbacks := dumpNATmaps(felixes)
							ipv4 := net.ParseIP(ip)
							portOld := uint16(testSvc.Spec.Ports[0].Port)
							oldK := nat.NewNATKey(ipv4, portOld, numericProto)
							portNew := uint16(testSvcUpdated.Spec.Ports[0].Port)
							natK := nat.NewNATKey(ipv4, portNew, numericProto)

							for i := range felixes {
								Expect(natmaps[i]).To(HaveKey(natK))
								Expect(natmaps[i]).NotTo(HaveKey(nat.NewNATKey(ipv4, portOld, numericProto)))

								Expect(natBeforeUpdate[i]).To(HaveKey(oldK))
								oldV := natBeforeUpdate[i][oldK]

								natV := natmaps[i][natK]
								bckCnt := natV.Count()
								bckID := natV.ID()

								log.WithField("backCnt", bckCnt).Debug("Backend count.")
								for ord := uint32(0); ord < uint32(bckCnt); ord++ {
									bckK := nat.NewNATBackendKey(bckID, ord)
									oldBckK := nat.NewNATBackendKey(oldV.ID(), ord)
									Expect(natbacks[i]).To(HaveKey(bckK))
									Expect(natBackBeforeUpdate[i]).To(HaveKey(oldBckK))
									Expect(natBackBeforeUpdate[i][oldBckK]).To(Equal(natbacks[i][bckK]))
								}

							}
						})

						It("after removing service, should not have connectivity from workloads via a service to workload 0", func() {
							ip := testSvcUpdated.Spec.ClusterIP
							port := uint16(testSvcUpdated.Spec.Ports[0].Port)
							natK := nat.NewNATKey(net.ParseIP(ip), port, numericProto)
							var prevBpfsvcs []nat.MapMem
							Eventually(func() bool {
								prevBpfsvcs, _ = dumpNATmaps(felixes)
								for _, m := range prevBpfsvcs {
									if _, ok := m[natK]; !ok {
										return false
									}
								}
								return true
							}, "5s").Should(BeTrue(), "service NAT key didn't show up")

							err := k8sClient.CoreV1().
								Services(testSvcNamespace).
								Delete(context.Background(), testSvcName, metav1.DeleteOptions{})
							Expect(err).NotTo(HaveOccurred())
							Eventually(k8sGetEpsForServiceFunc(k8sClient, testSvc), "10s").Should(HaveLen(0))

							cc.ExpectNone(w[0][1], TargetIP(ip), port)
							cc.ExpectNone(w[1][0], TargetIP(ip), port)
							cc.ExpectNone(w[1][1], TargetIP(ip), port)
							cc.CheckConnectivity()

							for i, f := range felixes {
								natV := prevBpfsvcs[i][natK]
								bckCnt := natV.Count()
								bckID := natV.ID()

								Eventually(func() bool {
									svcs := dumpNATMap(f)
									eps := dumpEPMap(f)

									if _, ok := svcs[natK]; ok {
										return false
									}

									for ord := uint32(0); ord < uint32(bckCnt); ord++ {
										bckK := nat.NewNATBackendKey(bckID, ord)
										if _, ok := eps[bckK]; ok {
											return false
										}
									}

									return true
								}, "5s").Should(BeTrue(), "service NAT key wasn't removed correctly")
							}
						})
					})
				})

				Context("with test-service configured 10.101.0.10:80 -> w[*][0].IP:8055 and affinity", func() {
					var (
						testSvc          *v1.Service
						testSvcNamespace string
					)

					testSvcName := "test-service"

					BeforeEach(func() {
						testSvc = k8sService(testSvcName, "10.101.0.10", w[0][0], 80, 8055, 0, testOpts.protocol)
						testSvcNamespace = testSvc.ObjectMeta.Namespace
						// select all pods with port 8055
						testSvc.Spec.Selector = map[string]string{"port": "8055"}
						testSvc.Spec.SessionAffinity = "ClientIP"
						_, err := k8sClient.CoreV1().Services(testSvcNamespace).Create(context.Background(), testSvc, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
						Eventually(k8sGetEpsForServiceFunc(k8sClient, testSvc), "10s").Should(HaveLen(1),
							"Service endpoints didn't get created? Is controller-manager happy?")
					})

					// FIXME we can only do the test with regular NAT as
					// cgroup shares one random affinity map
					if !testOpts.connTimeEnabled {
						It("should have connectivity from a workload to a service with multiple backends", func() {

							ip := testSvc.Spec.ClusterIP
							port := uint16(testSvc.Spec.Ports[0].Port)

							cc.ExpectSome(w[1][1], TargetIP(ip), port)
							cc.ExpectSome(w[1][1], TargetIP(ip), port)
							cc.ExpectSome(w[1][1], TargetIP(ip), port)
							cc.CheckConnectivity()

							aff := dumpAffMap(felixes[1])
							Expect(aff).To(HaveLen(1))

							var mkey nat.AffinityKey
							var mVal nat.AffinityValue
							// get the only key
							for k, v := range aff {
								mkey = k
								mVal = v
							}

							Eventually(func() nat.AffinityValue {
								// Remove the affinity entry to emulate timer
								// expiring / no prior affinity.
								m := nat.AffinityMap(&bpf.MapContext{})
								cmd, err := bpf.MapDeleteKeyCmd(m, mkey.AsBytes())
								Expect(err).NotTo(HaveOccurred())
								err = felixes[1].ExecMayFail(cmd...)
								Expect(err).NotTo(HaveOccurred())

								aff = dumpAffMap(felixes[1])
								Expect(aff).To(HaveLen(0))

								cc.CheckConnectivity()

								aff := dumpAffMap(felixes[1])
								Expect(aff).To(HaveLen(1))

								return aff[mkey]
							}, 60*time.Second, time.Second).ShouldNot(Equal(mVal))
						})
					}
				})

				npPort := uint16(30333)

				nodePortsTest := func(localOnly bool) {
					var (
						testSvc          *v1.Service
						testSvcNamespace string
					)

					testSvcName := "test-service"

					BeforeEach(func() {
						k8sClient := infra.(*infrastructure.K8sDatastoreInfra).K8sClient
						testSvc = k8sService(testSvcName, "10.101.0.10",
							w[0][0], 80, 8055, int32(npPort), testOpts.protocol)
						if localOnly {
							testSvc.Spec.ExternalTrafficPolicy = "Local"
						}
						testSvcNamespace = testSvc.ObjectMeta.Namespace
						_, err := k8sClient.CoreV1().Services(testSvcNamespace).Create(context.Background(), testSvc, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
						Eventually(k8sGetEpsForServiceFunc(k8sClient, testSvc), "10s").Should(HaveLen(1),
							"Service endpoints didn't get created? Is controller-manager happy?")
					})

					It("should have connectivity from all workloads via a service to workload 0", func() {
						clusterIP := testSvc.Spec.ClusterIP
						port := uint16(testSvc.Spec.Ports[0].Port)

						cc.ExpectSome(w[0][1], TargetIP(clusterIP), port)
						cc.ExpectSome(w[1][0], TargetIP(clusterIP), port)
						cc.ExpectSome(w[1][1], TargetIP(clusterIP), port)
						cc.CheckConnectivity()
					})

					if localOnly {
						It("should not have connectivity from all workloads via a nodeport to non-local workload 0", func() {
							node0IP := felixes[0].IP
							node1IP := felixes[1].IP
							// Via remote nodeport, should fail.
							cc.ExpectNone(w[0][1], TargetIP(node1IP), npPort)
							cc.ExpectNone(w[1][0], TargetIP(node1IP), npPort)
							cc.ExpectNone(w[1][1], TargetIP(node1IP), npPort)
							// Include a check that goes via the local nodeport to make sure the dataplane has converged.
							cc.ExpectSome(w[0][1], TargetIP(node0IP), npPort)
							cc.CheckConnectivity()
						})
					} else {
						It("should have connectivity from all workloads via a nodeport to workload 0", func() {
							node0IP := felixes[0].IP
							node1IP := felixes[1].IP

							cc.ExpectSome(w[0][1], TargetIP(node0IP), npPort)
							cc.ExpectSome(w[1][0], TargetIP(node0IP), npPort)
							cc.ExpectSome(w[1][1], TargetIP(node0IP), npPort)

							cc.ExpectSome(w[0][1], TargetIP(node1IP), npPort)
							cc.ExpectSome(w[1][0], TargetIP(node1IP), npPort)
							cc.ExpectSome(w[1][1], TargetIP(node1IP), npPort)

							cc.CheckConnectivity()
						})

						if testOpts.connTimeEnabled {
							Describe("with policy enabling ingress to w[0][0] from host endpoints", func() {
								BeforeEach(func() {
									pol = api.NewGlobalNetworkPolicy()
									pol.Namespace = "fv"
									pol.Name = "policy-host-eps"
									pol.Spec.Ingress = []api.Rule{
										{
											Action: "Allow",
											Source: api.EntityRule{
												Selector: "ep-type=='host'",
											},
										},
									}
									w00Slector := fmt.Sprintf("name=='%s'", w[0][0].Name)
									pol.Spec.Selector = w00Slector

									pol = createPolicy(pol)
								})

								It("should have connectivity from all host-networked workloads to workload 0", func() {
									node0IP := felixes[0].IP
									node1IP := felixes[1].IP

									hostW0SrcIP := ExpectWithSrcIPs(node0IP)
									hostW1SrcIP := ExpectWithSrcIPs(node1IP)

									switch testOpts.tunnel {
									case "ipip":
										hostW0SrcIP = ExpectWithSrcIPs(felixes[0].ExpectedIPIPTunnelAddr)
										hostW1SrcIP = ExpectWithSrcIPs(felixes[1].ExpectedIPIPTunnelAddr)
									case "wireguard":
										hostW1SrcIP = ExpectWithSrcIPs(felixes[0].ExpectedWireguardTunnelAddr)
										hostW1SrcIP = ExpectWithSrcIPs(felixes[1].ExpectedWireguardTunnelAddr)
									}

									ports := ExpectWithPorts(npPort)

									// Also try host networked pods, both on a local and remote node.
									// N.B. it cannot work without the connect time balancer
									cc.Expect(Some, hostW[0], TargetIP(node0IP), ports, hostW0SrcIP)
									cc.Expect(Some, hostW[1], TargetIP(node0IP), ports, hostW1SrcIP)
									cc.Expect(Some, hostW[0], TargetIP(node1IP), ports, hostW0SrcIP)
									cc.Expect(Some, hostW[1], TargetIP(node1IP), ports, hostW1SrcIP)

									cc.CheckConnectivity()
								})
							})
						}

					}

					if !localOnly {
						It("should have connectivity from a workload via a nodeport on another node to workload 0", func() {
							ip := felixes[1].IP

							cc.ExpectSome(w[2][1], TargetIP(ip), npPort)
							cc.CheckConnectivity()

						})
					}

					if testOpts.connTimeEnabled {
						It("workload should have connectivity to self via local/remote node", func() {
							cc.ExpectSome(w[0][0], TargetIP(felixes[1].IP), npPort)
							cc.ExpectSome(w[0][0], TargetIP(felixes[0].IP), npPort)
							cc.CheckConnectivity()
						})
					}

					It("should not have connectivity from external to w[0] via local/remote node", func() {
						cc.ExpectNone(externalClient, TargetIP(felixes[1].IP), npPort)
						cc.ExpectNone(externalClient, TargetIP(felixes[0].IP), npPort)
						// Include a check that goes via the local nodeport to make sure the dataplane has converged.
						cc.ExpectSome(w[0][1], TargetIP(felixes[0].IP), npPort)
						cc.CheckConnectivity()
					})

					Describe("after updating the policy to allow traffic from externalClient", func() {
						BeforeEach(func() {
							pol.Spec.Ingress = []api.Rule{
								{
									Action: "Allow",
									Source: api.EntityRule{
										Nets: []string{
											externalClient.IP + "/32",
										},
									},
								},
							}
							pol = updatePolicy(pol)
						})

						if localOnly && !testOpts.connTimeEnabled {
							It("should not have connectivity from external to w[0] via node1->node0 fwd", func() {
								cc.ExpectNone(externalClient, TargetIP(felixes[1].IP), npPort)
								// Include a check that goes via the nodeport with a local backing pod to make sure the dataplane has converged.
								cc.ExpectSome(externalClient, TargetIP(felixes[0].IP), npPort)
								cc.CheckConnectivity()
							})
						} else if !testOpts.connTimeEnabled {
							It("should have connectivity from external to w[0] via node1->node0 fwd", func() {
								By("checking the connectivity and thus populating the  neigh table", func() {
									cc.ExpectSome(externalClient, TargetIP(felixes[1].IP), npPort)
									cc.CheckConnectivity()
								})

								// The test does not make sense in DSR mode as the neigh
								// table is not used on the return path.
								if !testOpts.dsr {
									var srcMAC, dstMAC string

									By("making sure that neigh table is populated", func() {
										out, err := felixes[0].ExecOutput("calico-bpf", "arp", "dump")
										Expect(err).NotTo(HaveOccurred())

										arpRegexp := regexp.MustCompile(fmt.Sprintf(".*%s : (.*) -> (.*)", felixes[1].IP))

										lines := strings.Split(out, "\n")
										for _, l := range lines {
											if strings.Contains(l, felixes[1].IP) {
												MACs := arpRegexp.FindStringSubmatch(l)
												Expect(MACs).To(HaveLen(3))
												srcMAC = MACs[1]
												dstMAC = MACs[2]
											}
										}

										Expect(srcMAC).NotTo(Equal(""))
										Expect(dstMAC).NotTo(Equal(""))
									})

									// Since local-host networking ignores L2 addresses, we
									// need to make sure by other means that they are set
									// correctly.
									By("making sure that return VXLAN has the right MACs using tcpdump", func() {
										tcpdump := felixes[0].AttachTCPDump("eth0")
										tcpdump.SetLogEnabled(true)
										tcpdump.AddMatcher("MACs", regexp.MustCompile(fmt.Sprintf("%s > %s", srcMAC, dstMAC)))
										tcpdump.Start("-e", "udp", "and", "src", felixes[0].IP, "and", "port", "4789")
										defer tcpdump.Stop()

										cc.ExpectSome(externalClient, TargetIP(felixes[1].IP), npPort)
										cc.CheckConnectivity()

										Eventually(func() int { return tcpdump.MatchCount("MACs") }).
											Should(BeNumerically(">", 0), "MACs do not match")
									})
								}
							})

							_ = testIfTCP && It("should survive conntrack cleanup sweep", func() {
								By("checking the connectivity and thus syncing with service creation", func() {
									cc.ExpectSome(externalClient, TargetIP(felixes[1].IP), npPort)
									cc.CheckConnectivity()
								})

								By("monitoring a persistent connection", func() {
									pc := &PersistentConnection{
										Runtime:             externalClient,
										RuntimeName:         externalClient.Name,
										IP:                  felixes[1].IP,
										Port:                int(npPort),
										Protocol:            testOpts.protocol,
										MonitorConnectivity: true,
									}

									err := pc.Start()
									Expect(err).NotTo(HaveOccurred())
									defer pc.Stop()

									EventuallyWithOffset(1, pc.PongCount, "5s").Should(
										BeNumerically(">", 0),
										"Expected to see pong responses on the connection but didn't receive any")
									log.Info("Pongs received within last 1s")

									// We make sure that at least one iteration of the conntrack
									// cleanup executes and we periodically monitor the connection if
									// it is alive by checking that the number of PONGs keeps
									// increasing.
									start := time.Now()
									prevCount := pc.PongCount()
									for time.Since(start) < 2*conntrack.ScanPeriod {
										time.Sleep(time.Second)
										newCount := pc.PongCount()
										Expect(prevCount).Should(
											BeNumerically("<", newCount),
											"No new pongs since the last iteration. Connection broken?",
										)
										prevCount = newCount
									}
								})
							})

							if !testOpts.dsr {
								// When DSR is enabled, we need to have away how to pass the
								// original traffic back.
								//
								// felixes[0].Exec("ip", "route", "add", "192.168.20.0/24", "via", felixes[1].IP)
								//
								// This does not work since the other node would treat it as
								// DNAT due to the existing CT entries and NodePort traffix
								// otherwise :-/

								It("should have connectivity from external to w[0] via node1IP2 -> nodeIP1 -> node0 fwd", func() {
									// 192.168.20.1              +----------|---------+
									//      |                    |          |         |
									//      v                    |          |         V
									//    eth20                 eth0        |       eth0
									//  10.0.0.20:30333 --> felixes[1].IP   |   felixes[0].IP
									//                                      |        |
									//                                      |        V
									//                                      |     caliXYZ
									//                                      |    w[0][0].IP:8055
									//                                      |
									//                node1                 |      node0

									var eth20 *workload.Workload

									defer func() {
										if eth20 != nil {
											eth20.Stop()
										}
									}()

									By("setting up node's fake external iface", func() {
										// We name the iface eth20 since such ifaces are
										// treated by felix as external to the node
										//
										// Using a test-workload creates the namespaces and the
										// interfaces to emulate the host NICs

										eth20 = &workload.Workload{
											Name:          "eth20",
											C:             felixes[1].Container,
											IP:            "192.168.20.1",
											Ports:         "57005", // 0xdead
											Protocol:      testOpts.protocol,
											InterfaceName: "eth20",
										}
										eth20.Start()

										// assign address to eth20 and add route to the .20 network
										felixes[1].Exec("ip", "route", "add", "192.168.20.0/24", "dev", "eth20")
										felixes[1].Exec("ip", "addr", "add", "10.0.0.20/32", "dev", "eth20")
										_, err := eth20.RunCmd("ip", "route", "add", "10.0.0.20/32", "dev", "eth0")
										Expect(err).NotTo(HaveOccurred())
										// Add a route to felix[1] to be able to reach the nodeport
										_, err = eth20.RunCmd("ip", "route", "add", felixes[1].IP+"/32", "via", "10.0.0.20")
										Expect(err).NotTo(HaveOccurred())
										// This multi-NIC scenario works only if the kernel's RPF check
										// is not strict so we need to override it for the test and must
										// be set properly when product is deployed. We reply on
										// iptables to do require check for us.
										felixes[1].Exec("sysctl", "-w", "net.ipv4.conf.eth0.rp_filter=2")
										felixes[1].Exec("sysctl", "-w", "net.ipv4.conf.eth20.rp_filter=2")
									})

									By("setting up routes to .20 net on dest node to trigger RPF check", func() {
										// set up a dummy interface just for the routing purpose
										felixes[0].Exec("ip", "link", "add", "dummy1", "type", "dummy")
										felixes[0].Exec("ip", "link", "set", "dummy1", "up")
										// set up route to the .20 net through the dummy iface. This
										// makes the .20 a universaly reachable external world from the
										// internal/private eth0 network
										felixes[0].Exec("ip", "route", "add", "192.168.20.0/24", "dev", "dummy1")
										// This multi-NIC scenario works only if the kernel's RPF check
										// is not strict so we need to override it for the test and must
										// be set properly when product is deployed. We reply on
										// iptables to do require check for us.
										felixes[0].Exec("sysctl", "-w", "net.ipv4.conf.eth0.rp_filter=2")
										felixes[0].Exec("sysctl", "-w", "net.ipv4.conf.dummy1.rp_filter=2")
									})

									By("Allowing traffic from the eth20 network", func() {
										pol.Spec.Ingress = []api.Rule{
											{
												Action: "Allow",
												Source: api.EntityRule{
													Nets: []string{
														eth20.IP + "/32",
													},
												},
											},
										}
										pol = updatePolicy(pol)
									})

									By("Checking that there is connectivity from eth20 network", func() {

										cc.ExpectSome(eth20, TargetIP(felixes[1].IP), npPort)
										cc.CheckConnectivity()
									})
								})
							}

							if testOpts.protocol == "tcp" {

								const (
									npEncapOverhead = 50
									hostIfaceMTU    = 1500
									podIfaceMTU     = 1450
									sendLen         = hostIfaceMTU
									recvLen         = podIfaceMTU - npEncapOverhead
								)

								Context("with TCP, tx/rx close to MTU size on NP via node1->node0 ", func() {

									negative := ""
									adjusteMTU := podIfaceMTU - npEncapOverhead
									if testOpts.dsr {
										negative = "not "
										adjusteMTU = 0
									}

									It("should "+negative+"adjust MTU on workload side", func() {
										// force non-GSO packets when workload replies
										_, err := w[0][0].RunCmd("ethtool", "-K", "eth0", "gso", "off")
										Expect(err).NotTo(HaveOccurred())
										_, err = w[0][0].RunCmd("ethtool", "-K", "eth0", "tso", "off")
										Expect(err).NotTo(HaveOccurred())

										pmtu, err := w[0][0].PathMTU(externalClient.IP)
										Expect(err).NotTo(HaveOccurred())
										Expect(pmtu).To(Equal(0)) // nothing specific for this path yet

										cc.Expect(Some, externalClient, TargetIP(felixes[1].IP),
											ExpectWithPorts(npPort),
											ExpectWithSendLen(sendLen),
											ExpectWithRecvLen(recvLen),
											ExpectWithClientAdjustedMTU(hostIfaceMTU, hostIfaceMTU),
										)
										cc.CheckConnectivity()

										pmtu, err = w[0][0].PathMTU(externalClient.IP)
										Expect(err).NotTo(HaveOccurred())
										Expect(pmtu).To(Equal(adjusteMTU))
									})

									It("should not adjust MTU on client side if GRO off on nodes", func() {
										// force non-GSO packets on node ingress
										err := felixes[1].ExecMayFail("ethtool", "-K", "eth0", "gro", "off")
										Expect(err).NotTo(HaveOccurred())

										cc.Expect(Some, externalClient, TargetIP(felixes[1].IP),
											ExpectWithPorts(npPort),
											ExpectWithSendLen(sendLen),
											ExpectWithRecvLen(recvLen),
											ExpectWithClientAdjustedMTU(hostIfaceMTU, hostIfaceMTU),
										)
										cc.CheckConnectivity()
									})
								})
							}
						}

						if !testOpts.connTimeEnabled {
							It("should have connectivity from external to w[0] via node0", func() {
								log.WithFields(log.Fields{
									"externalClientIP": externalClient.IP,
									"nodePortIP":       felixes[1].IP,
								}).Infof("external->nodeport connection")

								cc.ExpectSome(externalClient, TargetIP(felixes[0].IP), npPort)
								cc.CheckConnectivity()
							})
						}
					})
				}

				Context("with test-service being a nodeport @ "+strconv.Itoa(int(npPort)), func() {
					nodePortsTest(false)
				})

				// FIXME connect time shares the same NAT table and it is a lottery which one it gets
				if !testOpts.connTimeEnabled {
					Context("with test-service being a nodeport @ "+strconv.Itoa(int(npPort))+
						" ExternalTrafficPolicy=local", func() {
						nodePortsTest(true)
					})
				}

				Context("with icmp blocked from workloads, external client", func() {
					var (
						testSvc          *v1.Service
						testSvcNamespace string
					)

					testSvcName := "test-service"

					BeforeEach(func() {
						icmpProto := numorstring.ProtocolFromString("icmp")
						pol.Spec.Ingress = []api.Rule{
							{
								Action: "Allow",
								Source: api.EntityRule{
									Nets: []string{"0.0.0.0/0"},
								},
							},
						}
						pol.Spec.Egress = []api.Rule{
							{
								Action: "Allow",
								Source: api.EntityRule{
									Nets: []string{"0.0.0.0/0"},
								},
							},
							{
								Action:   "Deny",
								Protocol: &icmpProto,
							},
						}
						pol = updatePolicy(pol)
					})

					var tgtPort int
					var tgtWorkload *workload.Workload

					JustBeforeEach(func() {
						k8sClient := infra.(*infrastructure.K8sDatastoreInfra).K8sClient
						testSvc = k8sService(testSvcName, "10.101.0.10",
							tgtWorkload, 80, tgtPort, int32(npPort), testOpts.protocol)
						testSvcNamespace = testSvc.ObjectMeta.Namespace
						_, err := k8sClient.CoreV1().Services(testSvcNamespace).Create(context.Background(), testSvc, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
						Eventually(k8sGetEpsForServiceFunc(k8sClient, testSvc), "10s").Should(HaveLen(1),
							"Service endpoints didn't get created? Is controller-manager happy?")

						// sync with NAT table being applied
						natFtKey := nat.NewNATKey(net.ParseIP(felixes[1].IP), npPort, numericProto)
						Eventually(func() bool {
							m := dumpNATMap(felixes[1])
							v, ok := m[natFtKey]
							return ok && v.Count() > 0
						}, 5*time.Second).Should(BeTrue())

						// Sync with policy
						cc.ExpectSome(w[1][0], w[0][0])
						cc.CheckConnectivity()
					})

					Describe("with dead workload", func() {
						if testOpts.connTimeEnabled {
							// FIXME externalClient also does conntime balancing
							return
						}

						BeforeEach(func() {
							tgtPort = 8057
							tgtWorkload = deadWorkload
						})

						It("should get host unreachable from nodeport via node1->node0 fwd", func() {
							err := felixes[0].ExecMayFail("ip", "route", "add", "unreachable", deadWorkload.IP)
							Expect(err).NotTo(HaveOccurred())

							tcpdump := externalClient.AttachTCPDump("any")
							tcpdump.SetLogEnabled(true)
							matcher := fmt.Sprintf("IP %s > %s: ICMP host %s unreachable",
								felixes[1].IP, externalClient.IP, felixes[1].IP)
							tcpdump.AddMatcher("ICMP", regexp.MustCompile(matcher))
							tcpdump.Start(testOpts.protocol, "port", strconv.Itoa(int(npPort)), "or", "icmp")
							defer tcpdump.Stop()

							cc.ExpectNone(externalClient, TargetIP(felixes[1].IP), npPort)
							cc.CheckConnectivity()

							Eventually(func() int { return tcpdump.MatchCount("ICMP") }).
								Should(BeNumerically(">", 0), matcher)
						})
					})

					Describe("with wrong target port", func() {
						// TCP would send RST instead of ICMP, it is enough to test one way of
						// triggering the ICMP message
						if testOpts.protocol != "udp" {
							return
						}

						BeforeEach(func() {
							tgtPort = 0xdead
							tgtWorkload = w[0][0]
						})

						if !testOpts.connTimeEnabled {
							It("should get port unreachable via node1->node0 fwd", func() {
								tcpdump := externalClient.AttachTCPDump("any")
								tcpdump.SetLogEnabled(true)
								matcher := fmt.Sprintf("IP %s > %s: ICMP %s udp port %d unreachable",
									felixes[1].IP, externalClient.IP, felixes[1].IP, npPort)
								tcpdump.AddMatcher("ICMP", regexp.MustCompile(matcher))
								tcpdump.Start(testOpts.protocol, "port", strconv.Itoa(int(npPort)), "or", "icmp")
								defer tcpdump.Stop()

								cc.ExpectNone(externalClient, TargetIP(felixes[1].IP), npPort)
								cc.CheckConnectivity()
								Eventually(func() int { return tcpdump.MatchCount("ICMP") }).
									Should(BeNumerically(">", 0), matcher)
							})
						}

						It("should get port unreachable workload to workload", func() {
							tcpdump := w[1][1].AttachTCPDump()
							tcpdump.SetLogEnabled(true)
							matcher := fmt.Sprintf("IP %s > %s: ICMP %s udp port %d unreachable",
								tgtWorkload.IP, w[1][1].IP, tgtWorkload.IP, tgtPort)
							tcpdump.AddMatcher("ICMP", regexp.MustCompile(matcher))
							tcpdump.Start(testOpts.protocol, "port", strconv.Itoa(tgtPort), "or", "icmp")
							defer tcpdump.Stop()

							cc.ExpectNone(w[1][1], TargetIP(tgtWorkload.IP), uint16(tgtPort))
							cc.CheckConnectivity()
							Eventually(func() int { return tcpdump.MatchCount("ICMP") }).
								Should(BeNumerically(">", 0), matcher)
						})

						It("should get port unreachable workload to workload through NP", func() {
							tcpdump := w[1][1].AttachTCPDump()
							tcpdump.SetLogEnabled(true)

							var matcher string

							if testOpts.connTimeEnabled {
								matcher = fmt.Sprintf("IP %s > %s: ICMP %s udp port %d unreachable",
									tgtWorkload.IP, w[1][1].IP, w[0][0].IP, tgtPort)
								tcpdump.AddMatcher("ICMP", regexp.MustCompile(matcher))
								tcpdump.Start(testOpts.protocol, "port", strconv.Itoa(tgtPort), "or", "icmp")
							} else {
								matcher = fmt.Sprintf("IP %s > %s: ICMP %s udp port %d unreachable",
									tgtWorkload.IP, w[1][1].IP, felixes[1].IP, npPort)
								tcpdump.AddMatcher("ICMP", regexp.MustCompile(matcher))
								tcpdump.Start(testOpts.protocol, "port", strconv.Itoa(int(npPort)), "or", "icmp")
							}
							defer tcpdump.Stop()

							cc.ExpectNone(w[1][1], TargetIP(felixes[1].IP), npPort)
							cc.CheckConnectivity()
							Eventually(func() int { return tcpdump.MatchCount("ICMP") }).
								Should(BeNumerically(">", 0), matcher)
						})
					})
				})

				Context("with CT tables full", func() {
					It("should still allow host -> host", func() {
						// XXX as long as there is no HEP policy
						// using hostW as a sink

						By("waiting for everything to come up", func() {
							cc.Expect(Some, felixes[0], hostW[1])
							cc.Expect(Some, felixes[1], hostW[0])
							cc.CheckConnectivity()
						})

						By("filling up the CT tables", func() {
							srcIP := net.IPv4(123, 123, 123, 123)
							dstIP := net.IPv4(121, 121, 121, 121)

							now := time.Duration(timeshim.RealTime().KTimeNanos())
							leg := conntrack.Leg{SynSeen: true, AckSeen: true, Opener: true}
							val := conntrack.NewValueNormal(now, now, 0, leg, leg)
							val64 := base64.StdEncoding.EncodeToString(val[:])

							key := conntrack.NewKey(6 /* TCP */, srcIP, 0, dstIP, 0)
							key64 := base64.StdEncoding.EncodeToString(key[:])

							_, err := felixes[0].ExecCombinedOutput("calico-bpf", "conntrack", "fill", key64, val64)
							Expect(err).NotTo(HaveOccurred())
						})

						By("checking host-host connectivity works", func() {
							cc.ResetExpectations()
							cc.Expect(Some, felixes[0], hostW[1])
							cc.Expect(Some, felixes[1], hostW[0])
							cc.CheckConnectivity()
						})

						By("checking pod-pod connectivity fails", func() {
							cc.ResetExpectations()
							cc.Expect(None, w[0][1], w[0][0])
							cc.Expect(None, w[1][0], w[0][0])
							cc.Expect(None, w[1][1], w[0][0])
							cc.CheckConnectivity()
						})

						By("cleaning up the CT maps", func() {
							_, err := felixes[0].ExecOutput("calico-bpf", "conntrack", "clean")
							Expect(err).NotTo(HaveOccurred())
						})

						By("checking pod-pod connectivity works again", func() {
							cc.ResetExpectations()
							cc.Expect(Some, w[0][1], w[0][0])
							cc.Expect(Some, w[1][0], w[0][0])
							cc.Expect(Some, w[1][1], w[0][0])
							cc.CheckConnectivity()
						})
					})
				})
			})
		})

		Describe("with BPF disabled to begin with", func() {
			var pc *PersistentConnection

			BeforeEach(func() {
				options.TestManagesBPF = true
				setupCluster()

				// Default to Allow...
				pol := api.NewGlobalNetworkPolicy()
				pol.Namespace = "fv"
				pol.Name = "policy-1"
				pol.Spec.Ingress = []api.Rule{{Action: "Allow"}}
				pol.Spec.Egress = []api.Rule{{Action: "Allow"}}
				pol.Spec.Selector = "all()"
				pol = createPolicy(pol)

				pc = nil
			})

			AfterEach(func() {
				if pc != nil {
					pc.Stop()
				}
			})

			enableBPF := func() {
				By("Enabling BPF")
				// Some tests start with a felix config pre-created, try to update it...
				fc, err := calicoClient.FelixConfigurations().Get(context.Background(), "default", options2.GetOptions{})
				bpfEnabled := true
				if err == nil {
					fc.Spec.BPFEnabled = &bpfEnabled
					_, err := calicoClient.FelixConfigurations().Update(context.Background(), fc, options2.SetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return
				}

				// Fall back on creating it...
				fc = api.NewFelixConfiguration()
				fc.Name = "default"
				fc.Spec.BPFEnabled = &bpfEnabled
				fc, err = calicoClient.FelixConfigurations().Create(context.Background(), fc, options2.SetOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Wait for BPF to be active.
				numTCProgramsOnEth0 := func() (total int) {
					for _, f := range felixes {
						total += f.NumTCBPFProgs("eth0")
					}
					return
				}
				Eventually(numTCProgramsOnEth0, "10s").Should(Equal(len(felixes) * 2))
			}

			expectPongs := func() {
				EventuallyWithOffset(1, pc.SinceLastPong, "5s").Should(
					BeNumerically("<", time.Second),
					"Expected to see pong responses on the connection but didn't receive any")
				log.Info("Pongs received within last 1s")
			}

			if testOpts.protocol == "tcp" && testOpts.dsr {
				verifyConnectivityWhileEnablingBPF := func(from, to *workload.Workload) {
					By("Starting persistent connection")
					pc = from.StartPersistentConnection(to.IP, 8055, workload.PersistentConnectionOpts{
						MonitorConnectivity: true,
					})

					By("having initial connectivity", expectPongs)
					By("enabling BPF mode", enableBPF) // Waits for BPF programs to be installed
					time.Sleep(2 * time.Second)        // pongs time out after 1s, make sure we look for fresh pongs.
					By("still having connectivity on the existing connection", expectPongs)
				}

				It("should keep a connection up between hosts when BPF is enabled", func() {
					verifyConnectivityWhileEnablingBPF(hostW[0], hostW[1])
				})

				It("should keep a connection up between workloads on different hosts when BPF is enabled", func() {
					verifyConnectivityWhileEnablingBPF(w[0][0], w[1][0])
				})

				It("should keep a connection up between hosts and remote workloads when BPF is enabled", func() {
					verifyConnectivityWhileEnablingBPF(hostW[0], w[1][0])
				})

				It("should keep a connection up between hosts and local workloads when BPF is enabled", func() {
					verifyConnectivityWhileEnablingBPF(hostW[0], w[0][0])
				})
			}
		})

		Describe("3rd party CNI", func() {
			// We do not use tunnel in such environments, no need to test.
			if testOpts.tunnel != "none" {
				return
			}

			BeforeEach(func() {
				// To mimic 3rd party CNI, we do not install IPPools and set the source to
				// learn routes to WorkloadIPs as IPAM/CNI is not going to provide either.
				options.UseIPPools = false
				options.ExtraEnvVars["FELIX_ROUTESOURCE"] = "WorkloadIPs"
				setupCluster()
			})

			Describe("CNI installs NAT outgoing iptable rules", func() {
				var extWorkload *workload.Workload
				BeforeEach(func() {
					c := containers.Run("external-workload",
						containers.RunOpts{AutoRemove: true},
						"--privileged", // So that we can add routes inside the container.
						utils.Config.BusyboxImage,
						"/bin/sh", "-c", "sleep 1000")

					extWorkload = &workload.Workload{
						C:        c,
						Name:     "ext-workload",
						Ports:    "4321",
						Protocol: testOpts.protocol,
						IP:       c.IP,
					}

					err := extWorkload.Start()
					Expect(err).NotTo(HaveOccurred())

					for _, felix := range felixes {
						felix.Exec("iptables", "-t", "nat", "-A", "POSTROUTING", "-d", extWorkload.IP, "-j", "MASQUERADE")
					}
				})

				It("should have connectivity to external workload", func() {
					By("allowing any traffic", func() {
						pol := api.NewGlobalNetworkPolicy()
						pol.Namespace = "fv"
						pol.Name = "policy-1"
						pol.Spec.Ingress = []api.Rule{{Action: "Allow"}}
						pol.Spec.Egress = []api.Rule{{Action: "Allow"}}
						pol.Spec.Selector = "all()"

						pol = createPolicy(pol)

						cc.ExpectSome(w[1][0], w[0][0])
						cc.ExpectSome(w[1][1], w[0][0])
						cc.CheckConnectivity()
						cc.ResetExpectations()
					})

					By("checking connectivity to the external workload", func() {
						cc.Expect(Some, w[0][0], extWorkload, ExpectWithPorts(4321), ExpectWithSrcIPs(felixes[0].IP))
						cc.Expect(Some, w[1][0], extWorkload, ExpectWithPorts(4321), ExpectWithSrcIPs(felixes[1].IP))
						cc.CheckConnectivity()
					})
				})

				AfterEach(func() {
					extWorkload.Stop()
				})
			})
		})
	})
}

func typeMetaV1(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{
		Kind:       kind,
		APIVersion: "v1",
	}
}

func objectMetaV1(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: "default",
	}
}

func dumpNATmaps(felixes []*infrastructure.Felix) ([]nat.MapMem, []nat.BackendMapMem) {
	bpfsvcs := make([]nat.MapMem, len(felixes))
	bpfeps := make([]nat.BackendMapMem, len(felixes))

	// Felixes are independent, we can dump the maps  concurrently
	var wg sync.WaitGroup

	for i := range felixes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer GinkgoRecover()
			bpfsvcs[i], bpfeps[i] = dumpNATMaps(felixes[i])
		}(i)
	}

	wg.Wait()

	return bpfsvcs, bpfeps
}

func dumpNATMaps(felix *infrastructure.Felix) (nat.MapMem, nat.BackendMapMem) {
	return dumpNATMap(felix), dumpEPMap(felix)
}

func dumpBPFMap(felix *infrastructure.Felix, m bpf.Map, iter bpf.IterCallback) {
	// Wait for the map to exist before trying to access it.  Otherwise, we
	// might fail a test that was retrying this dump anyway.
	Eventually(func() bool {
		return felix.FileExists(m.Path())
	}).Should(BeTrue(), fmt.Sprintf("dumpBPFMap: map %s didn't show up inside container", m.Path()))
	cmd, err := bpf.DumpMapCmd(m)
	Expect(err).NotTo(HaveOccurred(), "Failed to get BPF map dump command: "+m.Path())
	log.WithField("cmd", cmd).Debug("dumpBPFMap")
	out, err := felix.ExecOutput(cmd...)
	Expect(err).NotTo(HaveOccurred(), "Failed to get dump BPF map: "+m.Path())
	err = bpf.IterMapCmdOutput([]byte(out), iter)
	Expect(err).NotTo(HaveOccurred(), "Failed to parse BPF map dump: "+m.Path())
}

func dumpNATMap(felix *infrastructure.Felix) nat.MapMem {
	bm := nat.FrontendMap(&bpf.MapContext{})
	m := make(nat.MapMem)
	dumpBPFMap(felix, bm, nat.MapMemIter(m))
	return m
}

func dumpEPMap(felix *infrastructure.Felix) nat.BackendMapMem {
	bm := nat.BackendMap(&bpf.MapContext{})
	m := make(nat.BackendMapMem)
	dumpBPFMap(felix, bm, nat.BackendMapMemIter(m))
	return m
}

func dumpAffMap(felix *infrastructure.Felix) nat.AffinityMapMem {
	bm := nat.AffinityMap(&bpf.MapContext{})
	m := make(nat.AffinityMapMem)
	dumpBPFMap(felix, bm, nat.AffinityMapMemIter(m))
	return m
}

func dumpCTMap(felix *infrastructure.Felix) conntrack.MapMem {
	bm := conntrack.Map(&bpf.MapContext{})
	m := make(conntrack.MapMem)
	dumpBPFMap(felix, bm, conntrack.MapMemIter(m))
	return m
}

func dumpSendRecvMap(felix *infrastructure.Felix) nat.SendRecvMsgMapMem {
	bm := nat.SendRecvMsgMap(&bpf.MapContext{})
	m := make(nat.SendRecvMsgMapMem)
	dumpBPFMap(felix, bm, nat.SendRecvMsgMapMemIter(m))
	return m
}

func k8sService(name, clusterIP string, w *workload.Workload, port,
	tgtPort int, nodePort int32, protocol string) *v1.Service {
	k8sProto := v1.ProtocolTCP
	if protocol == "udp" {
		k8sProto = v1.ProtocolUDP
	}

	svcType := v1.ServiceTypeClusterIP
	if nodePort != 0 {
		svcType = v1.ServiceTypeNodePort
	}

	return &v1.Service{
		TypeMeta:   typeMetaV1("Service"),
		ObjectMeta: objectMetaV1(name),
		Spec: v1.ServiceSpec{
			ClusterIP: clusterIP,
			Type:      svcType,
			Selector: map[string]string{
				"name": w.Name,
			},
			Ports: []v1.ServicePort{
				{
					Protocol:   k8sProto,
					Port:       int32(port),
					NodePort:   nodePort,
					Name:       fmt.Sprintf("port-%d", tgtPort),
					TargetPort: intstr.FromInt(tgtPort),
				},
			},
		},
	}
}

func k8sLBService(name, clusterIP string, wname string, port,
	tgtPort int, protocol string, externalIPs, srcRange []string) *v1.Service {
	k8sProto := v1.ProtocolTCP
	if protocol == "udp" {
		k8sProto = v1.ProtocolUDP
	}

	svcType := v1.ServiceTypeLoadBalancer
	return &v1.Service{
		TypeMeta:   typeMetaV1("Service"),
		ObjectMeta: objectMetaV1(name),
		Spec: v1.ServiceSpec{
			ClusterIP:                clusterIP,
			Type:                     svcType,
			LoadBalancerSourceRanges: srcRange,
			ExternalIPs:              externalIPs,
			Selector: map[string]string{
				"name": wname,
			},
			Ports: []v1.ServicePort{
				{
					Protocol:   k8sProto,
					Port:       int32(port),
					Name:       fmt.Sprintf("port-%d", tgtPort),
					TargetPort: intstr.FromInt(tgtPort),
				},
			},
		},
	}
}

func k8sServiceWithExtIP(name, clusterIP string, w *workload.Workload, port,
	tgtPort int, nodePort int32, protocol string, externalIPs []string) *v1.Service {
	k8sProto := v1.ProtocolTCP
	if protocol == "udp" {
		k8sProto = v1.ProtocolUDP
	}

	svcType := v1.ServiceTypeClusterIP
	if nodePort != 0 {
		svcType = v1.ServiceTypeNodePort
	}
	return &v1.Service{
		TypeMeta:   typeMetaV1("Service"),
		ObjectMeta: objectMetaV1(name),
		Spec: v1.ServiceSpec{
			ClusterIP:   clusterIP,
			Type:        svcType,
			ExternalIPs: externalIPs,
			Selector: map[string]string{
				"name": w.Name,
			},
			Ports: []v1.ServicePort{
				{
					Protocol:   k8sProto,
					Port:       int32(port),
					NodePort:   nodePort,
					Name:       fmt.Sprintf("port-%d", tgtPort),
					TargetPort: intstr.FromInt(tgtPort),
				},
			},
		},
	}
}

func k8sGetEpsForService(k8s kubernetes.Interface, svc *v1.Service) []v1.EndpointSubset {
	ep, _ := k8s.CoreV1().
		Endpoints(svc.ObjectMeta.Namespace).
		Get(context.Background(), svc.ObjectMeta.Name, metav1.GetOptions{})
	log.WithField("endpoints",
		spew.Sprint(ep)).Infof("Got endpoints for %s", svc.ObjectMeta.Name)
	return ep.Subsets
}

func k8sGetEpsForServiceFunc(k8s kubernetes.Interface, svc *v1.Service) func() []v1.EndpointSubset {
	return func() []v1.EndpointSubset {
		return k8sGetEpsForService(k8s, svc)
	}
}

func k8sUpdateService(k8sClient kubernetes.Interface, nameSpace, svcName string, oldsvc, newsvc *v1.Service) {
	svc, err := k8sClient.CoreV1().
		Services(nameSpace).
		Get(context.Background(), svcName, metav1.GetOptions{})
	newsvc.ObjectMeta.ResourceVersion = svc.ObjectMeta.ResourceVersion
	_, err = k8sClient.CoreV1().Services(nameSpace).Update(context.Background(), newsvc, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
	Eventually(k8sGetEpsForServiceFunc(k8sClient, oldsvc), "10s").Should(HaveLen(1),
		"Service endpoints didn't get created? Is controller-manager happy?")
}

func k8sCreateLBServiceWithEndPoints(k8sClient kubernetes.Interface, name, clusterIP string, w *workload.Workload, port,
	tgtPort int, protocol string, externalIPs, srcRange []string) *v1.Service {
	var (
		testSvc          *v1.Service
		testSvcNamespace string
		epslen           int
	)
	if w != nil {
		testSvc = k8sLBService(name, clusterIP, w.Name, 80, tgtPort, protocol, externalIPs, srcRange)
		epslen = 1
	} else {
		testSvc = k8sLBService(name, clusterIP, "nobackend", 80, tgtPort, protocol, externalIPs, srcRange)
		epslen = 0
	}
	testSvcNamespace = testSvc.ObjectMeta.Namespace
	_, err := k8sClient.CoreV1().Services(testSvcNamespace).Create(context.Background(), testSvc, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	Eventually(k8sGetEpsForServiceFunc(k8sClient, testSvc), "10s").Should(HaveLen(epslen),
		"Service endpoints didn't get created? Is controller-manager happy?")
	return testSvc
}

package tests

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openshift-kni/eco-goinfra/pkg/mco"
	"github.com/openshift-kni/eco-goinfra/pkg/namespace"
	"github.com/openshift-kni/eco-goinfra/pkg/nodes"
	"github.com/openshift-kni/eco-goinfra/pkg/nto" //nolint:misspell
	"github.com/openshift-kni/eco-goinfra/pkg/reportxml"
	"github.com/openshift-kni/eco-gotests/tests/cnf/ran/internal/cluster"
	"github.com/openshift-kni/eco-gotests/tests/cnf/ran/internal/raninittools"
	"github.com/openshift-kni/eco-gotests/tests/cnf/ran/powermanagement/internal/helper"
	"github.com/openshift-kni/eco-gotests/tests/cnf/ran/powermanagement/internal/tsparams"
	performancev2 "github.com/openshift/cluster-node-tuning-operator/pkg/apis/performanceprofile/v2"
	"github.com/openshift/cluster-node-tuning-operator/pkg/performanceprofile/controller/performanceprofile/components"
	mcov1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/cpuset"
)

var _ = Describe("Per-core runtime power states tuning", Label(tsparams.LabelPowerSaveTestCases), Ordered, func() {
	var (
		nodeList                []*nodes.Builder
		nodeName                string
		perfProfile             *nto.Builder
		originalPerfProfileSpec performancev2.PerformanceProfileSpec
		err                     error
	)

	BeforeAll(func() {
		nodeList, err = nodes.List(raninittools.Spoke1APIClient)
		Expect(err).ToNot(HaveOccurred(), "Failed to get nodes")
		Expect(len(nodeList)).To(Equal(1), "Currently only SNO clusters are supported")

		nodeName = nodeList[0].Object.Name
		perfProfile, err = helper.GetPerformanceProfileWithCPUSet()
		Expect(err).ToNot(HaveOccurred(), "Failed to get performance profile")

		originalPerfProfileSpec = perfProfile.Object.Spec

		By("Creating the privileged pod namespace")
		_, err = namespace.NewBuilder(raninittools.Spoke1APIClient, tsparams.PrivPodNamespace).Create()
		Expect(err).ToNot(HaveOccurred(), "Failed to create the privileged pod namespace")
	})

	AfterAll(func() {
		By("Deleting the privileged pod namespace")
		err = namespace.NewBuilder(raninittools.Spoke1APIClient, tsparams.PrivPodNamespace).
			DeleteAndWait(tsparams.PowerSaveTimeout)
		Expect(err).ToNot(HaveOccurred(), "Failed to delete priv pod namespace")

		perfProfile, err = helper.GetPerformanceProfileWithCPUSet()
		Expect(err).ToNot(HaveOccurred(), "Failed to get performance profile")

		if reflect.DeepEqual(perfProfile.Object.Spec, originalPerfProfileSpec) {
			glog.V(tsparams.LogLevel).Info("Performance profile did not change, exiting")

			return
		}

		By("Restoring performance profile to original specs")
		perfProfile.Definition.Spec = originalPerfProfileSpec

		_, err = perfProfile.Update(true)
		Expect(err).ToNot(HaveOccurred())
		mcp, err := mco.Pull(raninittools.Spoke1APIClient, "master")
		Expect(err).ToNot(HaveOccurred(), "Failed to get machineconfigpool")

		err = mcp.WaitToBeInCondition(mcov1.MachineConfigPoolUpdating, corev1.ConditionTrue, 2*tsparams.PowerSaveTimeout)
		Expect(err).ToNot(HaveOccurred(), "Failed to wait for machineconfigpool to be updating")

		err = mcp.WaitForUpdate(3 * tsparams.PowerSaveTimeout)
		Expect(err).ToNot(HaveOccurred(), "Failed to wait for machineconfigpool to be updated")
	})

	// 54571 - Install SNO node with standard DU profile that does not include WorkloadHints
	It("Verifies expected kernel parameters with no workload hints specified in PerformanceProfile",
		reportxml.ID("54571"), func() {
			workloadHints := perfProfile.Definition.Spec.WorkloadHints
			if workloadHints != nil {
				Skip("WorkloadHints already present in perfProfile.Spec")
			}

			By("Checking for expected kernel parameters")
			cmdline, err := cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, "cat /proc/cmdline")
			Expect(err).ToNot(HaveOccurred(), "Failed to cat /proc/cmdline")

			// Expected default set of kernel parameters when no WorkloadHints are specified in PerformanceProfile
			requiredKernelParms := []string{
				"nohz_full=[0-9,-]+",
				"tsc=nowatchdog",
				"nosoftlockup",
				"nmi_watchdog=0",
				"mce=off",
				"skew_tick=1",
				"intel_pstate=disable",
			}
			for _, parameter := range requiredKernelParms {
				By(fmt.Sprintf("Checking /proc/cmdline for %s", parameter))
				rePattern := regexp.MustCompile(parameter)
				Expect(rePattern.FindStringIndex(cmdline)).
					ToNot(BeNil(), "Kernel parameter %s is missing from cmdline", parameter)
			}
		})

	// 54572 - Enable powersave at node level and then enable performance at node level
	It("Enables powersave at node level and then enable performance at node level", reportxml.ID("54572"), func() {
		By("Patching the performance profile with the workload hints")
		err := helper.SetPowerModeAndWaitForMcpUpdate(perfProfile, *nodeList[0], true, false, true)
		Expect(err).ToNot(HaveOccurred(), "Failed to set power mode")

		cmdline, err := cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, "cat /proc/cmdline")
		Expect(err).ToNot(HaveOccurred(), "Failed to cat /proc/cmdline")
		Expect(cmdline).
			To(ContainSubstring("intel_pstate=passive"), "Kernel parameter intel_pstate=passive missing from /proc/cmdline")
		Expect(cmdline).
			ToNot(ContainSubstring("intel_pstate=disable"), "Kernel parameter intel_pstate=disable found on /proc/cmdline")
	})

	// 54574 - Enable powersave at node level and then enable high performance at node level, check power
	// consumption with no workload pods.
	It("Enable powersave, and then enable high performance at node level, check power consumption with no workload pods.",
		reportxml.ID("54574"), func() {
			testPodAnnotations := map[string]string{
				"cpu-load-balancing.crio.io": "disable",
				"cpu-quota.crio.io":          "disable",
				"irq-load-balancing.crio.io": "disable",
				"cpu-c-states.crio.io":       "disable",
				"cpu-freq-governor.crio.io":  "performance",
			}

			cpuLimit := resource.MustParse("2")
			memLimit := resource.MustParse("100Mi")

			By("Define test pod")
			testpod, err := helper.DefineQoSTestPod(
				tsparams.PrivPodNamespace, nodeName, cpuLimit.String(), cpuLimit.String(), memLimit.String(), memLimit.String())
			Expect(err).ToNot(HaveOccurred(), "Failed to define test pod")

			testpod.Definition.Annotations = testPodAnnotations
			runtimeClass := fmt.Sprintf("%s-%s", components.ComponentNamePrefix, perfProfile.Definition.Name)
			testpod.Definition.Spec.RuntimeClassName = &runtimeClass

			DeferCleanup(func() {
				// Delete the test pod if it's still around when the function returns, like in a test case failure.
				if testpod.Exists() {
					By("Delete pod in case of a failure")
					_, err = testpod.DeleteAndWait(tsparams.PowerSaveTimeout)
					Expect(err).ToNot(HaveOccurred(), "Failed to delete test pod in case of failure")
				}
			})

			By("Create test pod")
			testpod, err = testpod.CreateAndWaitUntilRunning(tsparams.PowerSaveTimeout)
			Expect(err).ToNot(HaveOccurred(), "Failed to create pod")
			Expect(testpod.Object.Status.QOSClass).To(Equal(corev1.PodQOSGuaranteed),
				"Test pod does not have QoS class of Guaranteed")

			cpusetOutput, err := testpod.ExecCommand([]string{"sh", `-c`, "taskset -c -p $$ | cut -d: -f2"})
			Expect(err).ToNot(HaveOccurred(), "Failed to get cpuset")

			By("Verify powersetting of cpus used by the pod")
			trimmedOutput := strings.Trim(cpusetOutput.String(), " \r\n")
			cpusUsed, err := cpuset.Parse(trimmedOutput)
			Expect(err).ToNot(HaveOccurred(), "Failed to parse cpuset output")

			targetCpus := cpusUsed.List()
			checkCPUGovernorsAndResumeLatency(targetCpus, "n/a", "performance")

			By("Verify the rest of cpus have default power setting")
			allCpus := nodeList[0].Object.Status.Capacity.Cpu()
			cpus, err := cpuset.Parse(fmt.Sprintf("0-%d", allCpus.Value()-1))
			Expect(err).ToNot(HaveOccurred(), "Failed to parse cpuset")

			otherCPUs := cpus.Difference(cpusUsed)
			// Verify cpus not assigned to the pod have default power settings.
			checkCPUGovernorsAndResumeLatency(otherCPUs.List(), "0", "performance")

			By("Delete the pod")
			_, err = testpod.DeleteAndWait(tsparams.PowerSaveTimeout)
			Expect(err).ToNot(HaveOccurred(), "Failed to delete test pod")

			By("Verify after pod was deleted cpus assigned to container have default powersave settings")
			checkCPUGovernorsAndResumeLatency(targetCpus, "0", "performance")
		})

	Context("Collect power usage metrics", Ordered, func() {
		var (
			samplingInterval time.Duration
			powerState       string
		)

		BeforeAll(func() {
			if raninittools.BMCClient == nil {
				Skip("Collecting power usage metrics requires the BMC configuration be set.")
			}

			samplingInterval, err = time.ParseDuration(raninittools.RANConfig.MetricSamplingInterval)
			Expect(err).ToNot(HaveOccurred(), "Failed to parse metric sampling interval")

			// Determine power state to be used as a tag for the metric
			powerState, err = helper.GetPowerState(perfProfile)
			Expect(err).ToNot(HaveOccurred(), "Failed to get power state for the performance profile")
		})

		It("Checks power usage for 'noworkload' scenario", func() {
			duration, err := time.ParseDuration(raninittools.RANConfig.NoWorkloadDuration)
			Expect(err).ToNot(HaveOccurred(), "Failed to parse no workload duration")

			compMap, err := helper.CollectPowerMetricsWithNoWorkload(duration, samplingInterval, powerState)
			Expect(err).ToNot(HaveOccurred(), "Failed to collect power metrics with no workload")

			// Persist power usage metric to ginkgo report for further processing in pipeline.
			for metricName, metricValue := range compMap {
				GinkgoWriter.Printf("%s: %s\n", metricName, metricValue)
			}
		})

		It("Checks power usage for 'steadyworkload' scenario", func() {
			duration, err := time.ParseDuration(raninittools.RANConfig.WorkloadDuration)
			Expect(err).ToNot(HaveOccurred(), "Failed to parse steady workload duration")

			compMap, err := helper.CollectPowerMetricsWithSteadyWorkload(
				duration, samplingInterval, powerState, perfProfile, nodeName)
			Expect(err).ToNot(HaveOccurred(), "Failed to collect power metrics with steady workload")

			// Persist power usage metric to ginkgo report for further processing in pipeline.
			for metricName, metricValue := range compMap {
				GinkgoWriter.Printf("%s: %s\n", metricName, metricValue)
			}
		})
	})

	Context("Reserved Core Frequency Tuning Test", func() {
		var (
			desiredReservedCoreFreq = performancev2.CPUfrequency(2500004)
			desiredIsolatedCoreFreq = performancev2.CPUfrequency(2200004)
			isolatedCPUNumber       = 2
			ReservedCPUNumber       = 0
		)

		It("sets frequency of reserved and isolated CPU cores", Label("ReservedCoreFreqTuningTest"), func() { //REMOVE LABEL- for testing only
			By("patch performance profile to set core frequency to coreFrequency")
			err := helper.SetCPUFreqAndWaitForMcpUpdate(perfProfile, *nodeList[0], &desiredIsolatedCoreFreq, &desiredReservedCoreFreq)
			Expect(err).ToNot(HaveOccurred(), "Failed to set CPU Freq")

			By("Get modified isolated core frequency")
			spokeCommand := fmt.Sprintf("cat /sys/devices/system/cpu/cpufreq/policy%v/scaling_max_freq", isolatedCPUNumber)
			consoleOut, err := cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, spokeCommand)
			Expect(err).ToNot(HaveOccurred(), "Failed to cat /sys/devices/system/cpu/cpufreq/policy%s/scaling_max_freq")

			By("Compare current isolated core freq to desired isolated core freq")
			currIsolatedCoreFreq, err := strconv.Atoi(strings.TrimSuffix(consoleOut, "\n"))
			Expect(err).ToNot(HaveOccurred(), "strconv.ParseInt Failed")
			Expect(currIsolatedCoreFreq).Should(Equal(int(desiredIsolatedCoreFreq)), "Isolated CPU Frequency does not match expected frequency")

			By("Get current reserved core frequency")
			spokeCommand = fmt.Sprintf("cat /sys/devices/system/cpu/cpufreq/policy%v/scaling_max_freq", ReservedCPUNumber)
			consoleOut, err = cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, spokeCommand)
			Expect(err).ToNot(HaveOccurred(), "Failed to cat /sys/devices/system/cpu/cpufreq/policy%s/scaling_max_freq")

			By("Compare current reserved core freq to desired reserved core freq")
			currReservedCoreFreq, err := strconv.Atoi(strings.TrimSuffix(consoleOut, "\n"))
			Expect(err).ToNot(HaveOccurred(), "strconv.ParseInt Failed")
			Expect(currReservedCoreFreq).Should(Equal(int(desiredReservedCoreFreq)), "Reserved CPU Frequency does not match expected frequency")

		})
	})
})

// checkCPUGovernorsAndResumeLatency checks power and latency settings of the cpus.
func checkCPUGovernorsAndResumeLatency(cpus []int, pmQos, governor string) {
	for _, cpu := range cpus {
		command := fmt.Sprintf("sleep 0.01; cat /sys/devices/system/cpu/cpu%d/power/pm_qos_resume_latency_us | cat -", cpu)

		var output string
		for len(output) == 0 {
			value, err := cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, command)
			Expect(err).ToNot(HaveOccurred(), "Error executing command %s", command)

			output = strings.Trim(value, "\r\n")
		}
		Expect(output).To(Equal(pmQos))

		command = fmt.Sprintf("sleep 0.01; cat /sys/devices/system/cpu/cpu%d/cpufreq/scaling_governor | cat -", cpu)

		output = ""
		for len(output) == 0 {
			value, err := cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, command)
			Expect(err).ToNot(HaveOccurred(), "Error executing command %s", command)

			output = strings.Trim(value, "\r\n")
		}
		Expect(output).To(Equal(governor))
	}
}

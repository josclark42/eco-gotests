package tests

import (
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openshift-kni/eco-goinfra/pkg/namespace"
	"github.com/openshift-kni/eco-goinfra/pkg/nodes"
	"github.com/openshift-kni/eco-goinfra/pkg/nto" //nolint:misspell
	"github.com/openshift-kni/eco-gotests/tests/cnf/ran/internal/cluster"
	"github.com/openshift-kni/eco-gotests/tests/cnf/ran/internal/raninittools"
	"github.com/openshift-kni/eco-gotests/tests/cnf/ran/powermanagement/internal/helper"
	"github.com/openshift-kni/eco-gotests/tests/cnf/ran/powermanagement/internal/tsparams"
	performancev2 "github.com/openshift/cluster-node-tuning-operator/pkg/apis/performanceprofile/v2"
	"log"
	"strconv"
	"strings"
)

var _ = Describe("Per-core runtime power states tuning", Label(tsparams.LabelPowerSaveTestCases), Ordered, func() {
	var (
		nodeList    []*nodes.Builder
		perfProfile *nto.Builder
		err         error
	)

	BeforeAll(func() {
		nodeList, err = nodes.List(raninittools.Spoke1APIClient)
		Expect(err).ToNot(HaveOccurred(), "Failed to get nodes")
		Expect(len(nodeList)).To(Equal(1), "Currently only SNO clusters are supported")

		perfProfile, err = helper.GetPerformanceProfileWithCPUSet()
		Expect(err).ToNot(HaveOccurred(), "Failed to get performance profile")

		By("Creating the privileged pod namespace")
		_, err = namespace.NewBuilder(raninittools.Spoke1APIClient, tsparams.PrivPodNamespace).Create()
		Expect(err).ToNot(HaveOccurred(), "Failed to create the privileged pod namespace")
	})

	AfterAll(func() {
		By("Deleting the privileged pod namespace")
		err = namespace.NewBuilder(raninittools.Spoke1APIClient, tsparams.PrivPodNamespace).
			DeleteAndWait(tsparams.PowerSaveTimeout)
		Expect(err).ToNot(HaveOccurred(), "Failed to delete priv pod namespace")

	})

	FContext("Reserved Core Frequency Tuning Test", func() {
		var (
			desiredReservedCoreFreq = performancev2.CPUfrequency(2500002)
			desiredIsolatedCoreFreq = performancev2.CPUfrequency(2200002)
			originalIsolatedCPUFreq performancev2.CPUfrequency
			originalReservedCPUFreq performancev2.CPUfrequency
			isolatedCPUNumber       = 2
			ReservedCPUNumber       = 0
		)

		It("tests changing reserved and isolated CPU frequencies", func() {
			By("get original isolated core frequency")
			spokeCommand := fmt.Sprintf("cat /sys/devices/system/cpu/cpufreq/policy%v/scaling_max_freq |cat -",
				isolatedCPUNumber)
			consoleOut, err := cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, spokeCommand)
			Expect(err).ToNot(HaveOccurred(), "Failed to %s", spokeCommand)
			freqAsInt, err := strconv.Atoi(strings.Trim(consoleOut, "\r\n"))
			originalIsolatedCPUFreq = performancev2.CPUfrequency(freqAsInt)
			log.Println("ORIGINAL: %v", freqAsInt) //DEBUG

			By("get original reserved core frequency")
			spokeCommand = fmt.Sprintf("cat /sys/devices/system/cpu/cpufreq/policy%v/scaling_max_freq |cat -",
				ReservedCPUNumber)
			consoleOut, err = cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, spokeCommand)
			Expect(err).ToNot(HaveOccurred(), "Failed to %s", spokeCommand)
			freqAsInt, err = strconv.Atoi(strings.Trim(consoleOut, "\r\n"))
			originalReservedCPUFreq = performancev2.CPUfrequency(freqAsInt)

			By("patch performance profile to set core frequencies")
			err = helper.SetCPUFreqAndWaitForMcpUpdate(perfProfile, *nodeList[0],
				&desiredIsolatedCoreFreq, &desiredReservedCoreFreq)
			Expect(err).ToNot(HaveOccurred(), "Failed to set CPU Freq")

			By("Get modified isolated core frequency")
			spokeCommand = fmt.Sprintf("cat /sys/devices/system/cpu/cpufreq/policy%v/scaling_max_freq |cat -",
				isolatedCPUNumber)
			consoleOut, err = cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, spokeCommand)
			Expect(err).ToNot(HaveOccurred(), "Failed to %s", spokeCommand)
			log.Println("NEW: %v", consoleOut) //DEBUG

			By("Compare current isolated core freq to desired isolated core freq")
			currIsolatedCoreFreq, err := strconv.Atoi(strings.Trim(consoleOut, "\r\n "))
			Expect(err).ToNot(HaveOccurred(), "strconv.Atoi Failed")
			Expect(currIsolatedCoreFreq).To(Equal(int(desiredIsolatedCoreFreq)),
				"Isolated CPU Frequency does not match expected frequency")

			By("Get current reserved core frequency")
			spokeCommand = fmt.Sprintf("cat /sys/devices/system/cpu/cpufreq/policy%v/scaling_max_freq |cat -",
				ReservedCPUNumber)
			consoleOut, err = cluster.ExecCommandOnSNO(raninittools.Spoke1APIClient, 3, spokeCommand)
			Expect(err).ToNot(HaveOccurred(), "Failed to %s", spokeCommand)

			By("Compare current reserved core freq to desired reserved core freq")
			currReservedCoreFreq, err := strconv.Atoi(strings.Trim(consoleOut, "\r\n "))
			Expect(err).ToNot(HaveOccurred(), "strconv.Atoi Failed")
			Expect(currReservedCoreFreq).To(Equal(int(desiredReservedCoreFreq)),
				"Reserved CPU Frequency does not match expected frequency")

			By("Reverts the CPU frequencies to the original setting")
			err = helper.SetCPUFreqAndWaitForMcpUpdate(perfProfile, *nodeList[0],
				&originalIsolatedCPUFreq, &originalReservedCPUFreq)
			Expect(err).ToNot(HaveOccurred(), "Failed to set CPU Freq")

		})
	})
})

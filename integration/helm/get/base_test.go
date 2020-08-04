package get_test

import (
	"fmt"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/werf/werf/pkg/testing/utils"
)

var _ = Describe("helm get-something", func() {
	envName := "test"

	BeforeEach(func() {
		utils.CopyIn(utils.FixturePath("base"), testDirPath)
		stubs.SetEnv("WERF_ENV", envName)
	})

	It("should receive release name (default scheme)", func() {
		output := utils.SucceedCommandOutputString(
			testDirPath,
			werfBinPath,
			"helm", "get-release",
		)

		Ω(output).Should(ContainSubstring(utils.ProjectName() + "-" + envName))
	})

	It("should receive namespace name (default scheme)", func() {
		output := utils.SucceedCommandOutputString(
			testDirPath,
			werfBinPath,
			"helm", "get-namespace",
		)

		Ω(output).Should(ContainSubstring(utils.ProjectName() + "-" + envName))
	})

	It("should receive namespace name (default scheme)", func() {
		output := utils.SucceedCommandOutputString(
			testDirPath,
			werfBinPath,
			"helm", "get-namespace",
		)

		Ω(output).Should(ContainSubstring(utils.ProjectName() + "-" + envName))
	})

	It("should receive autogenerated values", func() {
		output := utils.SucceedCommandOutputString(
			testDirPath,
			werfBinPath,
			"helm", "get-autogenerated-values",
		)

		for _, substrFormat := range []string{
			"env: %[2]s",
			"namespace: %[1]s-%[2]s",
			"name: %[1]s",
		} {
			Ω(output).Should(ContainSubstring(fmt.Sprintf(substrFormat, utils.ProjectName(), envName)))
		}
	})
})
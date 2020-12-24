package mount_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	"github.com/werf/werf/integration/utils"
	"github.com/werf/werf/integration/utils/docker"
)

type entry struct {
	fixturePath                       string
	expectedFirstBuildOutputMatchers  []types.GomegaMatcher
	expectedSecondBuildOutputMatchers []types.GomegaMatcher
}

var itBody = func(e entry) {
	utils.CopyIn(e.fixturePath, SuiteData.TestDirPath)

	SuiteData.Stubs.SetEnv("FROM_CACHE_VERSION", "1")

	output := utils.SucceedCommandOutputString(
		SuiteData.TestDirPath,
		SuiteData.WerfBinPath,
		"build",
	)

	for _, match := range e.expectedFirstBuildOutputMatchers {
		Ω(output).Should(match)
	}

	SuiteData.Stubs.SetEnv("FROM_CACHE_VERSION", "2")

	output = utils.SucceedCommandOutputString(
		SuiteData.TestDirPath,
		SuiteData.WerfBinPath,
		"build",
	)

	for _, match := range e.expectedSecondBuildOutputMatchers {
		Ω(output).Should(match)
	}

	docker.RunSucceedContainerCommandWithStapel(SuiteData.WerfBinPath, SuiteData.TestDirPath, []string{}, []string{"[[ -z \"$(ls -A /mount)\" ]]"})
}

var _ = BeforeEach(func() {
	SuiteData.Stubs.SetEnv("WERF_LOOSE_GITERMINISM", "1")
})

var _ = DescribeTable("base (non-deterministic)", itBody,
	Entry("tmp_dir", entry{
		fixturePath: utils.FixturePath("tmp_dir"),
		expectedFirstBuildOutputMatchers: []types.GomegaMatcher{
			ContainSubstring("Result number is 2"),
		},
		expectedSecondBuildOutputMatchers: []types.GomegaMatcher{
			ContainSubstring("Result number is 2"),
		},
	}),
	Entry("build_dir", entry{
		fixturePath: utils.FixturePath("build_dir"),
		expectedFirstBuildOutputMatchers: []types.GomegaMatcher{
			ContainSubstring("Result number is 2"),
		},
		expectedSecondBuildOutputMatchers: []types.GomegaMatcher{
			ContainSubstring("Result number is 4"),
		},
	}),
	Entry("from_path", entry{
		fixturePath: utils.FixturePath("from_path"),
		expectedFirstBuildOutputMatchers: []types.GomegaMatcher{
			ContainSubstring("Result number is 4"),
		},
		expectedSecondBuildOutputMatchers: []types.GomegaMatcher{
			ContainSubstring("Result number is 6"),
		},
	}))

package ffmpeg

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFixMisplacedSEI_BrokenFiles(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	run(`
		cat <<- 'EOF1' > input-sei-order.out
		Access Unit Delimiter
		Slice Header
		Supplemental Enhancement Information
		Access Unit Delimiter
		EOF1

		cat <<- 'EOF2' > fixed-sei-order.out
		Access Unit Delimiter
		Supplemental Enhancement Information
		Slice Header
		Access Unit Delimiter
		EOF2

		# missing-dts.ts has a SEI pre-prended, check that's preserved
		cat <<- 'EOF3' > leading-sei.out
		Supplemental Enhancement Information
		Access Unit Delimiter
		Slice Header
		Supplemental Enhancement Information
		EOF3

		# missing-dts.ts has a SEI pre-prended, check that's preserved
		cat <<- 'EOF3' > fixed-leading-sei.out
		Supplemental Enhancement Information
		Access Unit Delimiter
		Supplemental Enhancement Information
		Slice Header
		EOF3

	`)

	for _, name := range []string{"skip_1.ts", "skip_3.ts", "missing-dts.ts"} {
		t.Run(name, func(t *testing.T) {
			input := dataFilePath(t, name)
			if "missing-dts.ts" == name {
				checkNALSequence(t, run, input, "leading-sei.out")
			} else {
				checkNALSequence(t, run, input, "input-sei-order.out")
			}

			inputData, err := ioutil.ReadFile(input)
			require.NoError(t, err)

			fixedPath, err := FixMisplacedSEI(input)
			require.NoError(t, err)
			require.NotEqual(t, input, fixedPath, "expected fix-up to trigger")
			defer os.Remove(fixedPath)

			fixedData, err := ioutil.ReadFile(fixedPath)
			require.NoError(t, err)
			require.Equal(t, len(inputData), len(fixedData), "fix-up must preserve byte size")
			if "missing-dts.ts" == name {
				checkNALSequence(t, run, fixedPath, "fixed-leading-sei.out")
			} else {
				checkNALSequence(t, run, fixedPath, "fixed-sei-order.out")
			}
		})
	}
}

func TestFixMisplacedSEI_NoChanges(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	run(`
    # normally SEI comes before any picture data
		cat <<- 'EOF1' > vertical-sei-order.out
		Access Unit Delimiter
		Supplemental Enhancement Information
		Slice Header
		Access Unit Delimiter
		EOF1

		# this sample should NOT have any SEI
		! ffmpeg -hide_banner -i "$1/../data/portrait.ts" -c copy -bsf:v trace_headers -f null - 2>&1 | grep "Supplemental Enhancement Information"
	`)

	checkNALSequence(t, run, dataFilePath(t, "vertical-sample.ts"), "vertical-sei-order.out")

	for _, name := range []string{"vertical-sample.ts", "portrait.ts", "broken-h264-parser.ts"} {
		t.Run(name, func(t *testing.T) {
			input := dataFilePath(t, name)
			fixedPath, err := FixMisplacedSEI(input)
			require.NoError(t, err)
			require.Equal(t, input, fixedPath, "known-good sample should pass through unchanged")
		})
	}
}

func dataFilePath(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return path.Join(wd, "..", "data", name)
}

func checkNALSequence(t *testing.T, run func(cmd string) bool, inputPath, expectedPath string) {
	t.Helper()
	cmd := fmt.Sprintf(`
		ffmpeg -hide_banner -i "%s" -c copy -bsf:v trace_headers -f null - 2>&1 | grep -e 'Access Unit\|Slice Header\|Supplement' | head -4 > pre.raw
		sed -E 's/^\[[^]]+\] //' pre.raw > pre.out
		diff -u %s pre.out
	`, inputPath, expectedPath)
	require.True(t, run(cmd), "NAL ordering check failed for %s", inputPath)
}

package ffmpeg

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_LogCtx(t *testing.T) {
	InitFFmpegWithLogLevel(FFLogInfo)

	file, err := ioutil.TempFile("", "TestLogCtx")
	if err != nil {
		t.Fatalf("Tempfile failed: %v", err)
	}
	fd := file.Fd()
	// redirect ffmpeg log output to file
	log_init(fd)
	var wg sync.WaitGroup
	st := func(i int) {
		logCtx := fmt.Sprintf("thread=th%d", i)
		go func() {
			s := time.Now()
			log_test(logCtx)
			fmt.Printf("Log test thread %d took %s\n", i, time.Since(s))
			wg.Done()
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go st(i)
	}
	wg.Wait()
	log_deinit()
	cont, err := ioutil.ReadFile(file.Name())
	require.NoError(t, err)
	fileStrings := strings.Split(string(cont), "\n")
	// check that different go-routines print logs with own contexts
	assert.Contains(t, fileStrings, "thread=th1 context logging test second=2")
	assert.Contains(t, fileStrings, "thread=th2 context logging test second=2")
	defer os.Remove(file.Name())
}

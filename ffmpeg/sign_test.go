package ffmpeg

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/m3u8"
	"github.com/olekukonko/tablewriter"
)

func parseVideoProfiles(inp string) []VideoProfile {
	profs := make([]VideoProfile, 0)
	presets := strings.Split(inp, ",")
	for _, v := range presets {
		if p, ok := VideoProfileLookup[strings.TrimSpace(v)]; ok {
			profs = append(profs, p)
		}
	}
	return profs
}
func TranscodingWithSign(outPrefix string, bSign bool, bNvidia bool) {
	in := "bbb/source.m3u8"
	accel := Software
	if bNvidia {
		accel = Nvidia
	}
	dir := path.Dir(in)
	devices := []string{"0"}

	profilestring := "P720p60fps16x9,P360p30fps16x9,P720p30fps16x9"
	profiles := parseVideoProfiles(profilestring)

	f, err := os.Open(in)
	if err != nil {
		glog.Fatal("Couldn't open input manifest: ", err)
	}
	p, _, err := m3u8.DecodeFrom(bufio.NewReader(f), true)
	if err != nil {
		glog.Fatal("Couldn't decode input manifest: ", err)
	}
	pl, ok := p.(*m3u8.MediaPlaylist)
	if !ok {
		glog.Fatalf("Expecting media playlist in the input %s", in)
	}

	var wg sync.WaitGroup

	concurrentSessions := 1
	segCount := 0
	realTimeSegCount := 0
	srcDur := 0.0
	var mu sync.Mutex
	transcodeDur := 0.0
	for i := 0; i < concurrentSessions; i++ {
		wg.Add(1)
		go func(k int, wg *sync.WaitGroup) {
			tc := NewTranscoder()
			for j, v := range pl.Segments {
				if v == nil {
					continue
				}
				u := path.Join(dir, v.URI)
				in := &TranscodeOptionsIn{
					Fname: u,
					Accel: accel,
				}
				if Software != accel {
					in.Device = devices[k%len(devices)]
				}
				profs2opts := func(profs []VideoProfile) []TranscodeOptions {
					opts := []TranscodeOptions{}
					for n, p := range profs {

						oname := fmt.Sprintf("%s_%s_%d_%d_%d.ts", outPrefix, p.Name, n, k, j)
						muxer := "mpegts"

						o := TranscodeOptions{
							Oname:        oname,
							Profile:      p,
							Accel:        accel,
							AudioEncoder: ComponentOptions{Name: "copy"},
							Muxer:        ComponentOptions{Name: muxer},
						}
						if bSign {
							o.CalcSign = true
						}
						opts = append(opts, o)
					}
					return opts
				}
				out := profs2opts(profiles)
				t := time.Now()
				_, err := tc.Transcode(in, out)
				end := time.Now()
				if err != nil {
					glog.Fatalf("Transcoding failed for session %d segment %d: %v", k, j, err)
				}

				fmt.Printf("%s,%d,%d,%0.4v,%0.4v\n", end.Format("2006-01-02 15:04:05.9999"), k, j, v.Duration, end.Sub(t).Seconds())

				segTxDur := end.Sub(t).Seconds()
				mu.Lock()
				transcodeDur += segTxDur
				srcDur += v.Duration
				segCount++
				if segTxDur <= v.Duration {
					realTimeSegCount += 1
				}
				mu.Unlock()
			}
			tc.StopTranscoder()
			wg.Done()
		}(i, &wg)
		time.Sleep(2300 * time.Millisecond) // wait for at least one segment before moving on to the next session
	}
	wg.Wait()
	if segCount == 0 || srcDur == 0.0 {
		glog.Fatal("Input manifest has no segments or total duration is 0s")
	}
	statsTable := tablewriter.NewWriter(os.Stderr)
	signmode := "Sign Mode"
	if bSign == false {
		signmode = "No Sign Mode"
	}
	devmod := "Software"
	if bNvidia {
		devmod = "Nvidia"
	}
	stats := [][]string{
		{"Sign Status", fmt.Sprintf("%v - %v", signmode, devmod)},
		{"Total Segs Transcoded", fmt.Sprintf("%v", segCount)},
		{"Real-Time Segs Transcoded", fmt.Sprintf("%v", realTimeSegCount)},
		{"* Real-Time Segs Ratio *", fmt.Sprintf("%0.4v", float64(realTimeSegCount)/float64(segCount))},
		{"Total Source Duration", fmt.Sprintf("%vs", srcDur)},
		{"Total Transcoding Duration", fmt.Sprintf("%vs", transcodeDur)},
		{"* Real-Time Duration Ratio *", fmt.Sprintf("%0.4v", transcodeDur/srcDur)},
	}
	statsTable.SetAlignment(tablewriter.ALIGN_LEFT)
	statsTable.SetCenterSeparator("*")
	statsTable.SetColumnSeparator("|")
	statsTable.AppendBulk(stats)
	statsTable.Render()
}

func TestSign_SWTranscoding(t *testing.T) {
	_, outdir := setupTest(t)
	defer os.RemoveAll(outdir)
	outPrefix := outdir + "/"
	TranscodingWithSign(outPrefix, true, false)
}
func TestSignNo_SWTranscoding(t *testing.T) {
	_, outdir := setupTest(t)
	defer os.RemoveAll(outdir)
	outPrefix := outdir + "/"
	TranscodingWithSign(outPrefix, false, false)
}
func TestSign_NVTranscoding(t *testing.T) {
	_, outdir := setupTest(t)
	defer os.RemoveAll(outdir)
	outPrefix := outdir + "/"
	TranscodingWithSign(outPrefix, true, true)
}
func TestSignNo_NVTranscoding(t *testing.T) {
	_, outdir := setupTest(t)
	defer os.RemoveAll(outdir)
	outPrefix := outdir + "/"
	TranscodingWithSign(outPrefix, false, true)
}

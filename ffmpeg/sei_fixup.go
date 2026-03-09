package ffmpeg

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/livepeer/joy4/format/ts/tsio"
)

const (
	tsPacketSize        = 188
	invalidPID   uint16 = 0x1fff
)

type byteRange struct {
	start int
	end   int
}

type nalInfo struct {
	start int
	end   int
	typ   uint8
}

// FixMisplacedSEI rewrites a TS segment into a temp file when SEI NAL units are
// found after VCL NAL units within an access unit. If no fix is needed, it
// returns the original input path.
func FixMisplacedSEI(inputPath string) (fixedPath string, err error) {
	data, err := ioutil.ReadFile(inputPath)
	if err != nil {
		return "", err
	}
	fixedData, changed := fixSEIOrder(data)
	if !changed {
		return inputPath, nil
	}

	dir := filepath.Dir(inputPath)
	tmp, err := ioutil.TempFile(dir, "sei-fixup-*.ts")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(fixedData); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

func fixSEIOrder(data []byte) ([]byte, bool) {
	if len(data) < tsPacketSize {
		return data, false
	}
	videoPID := findVideoPID(data)
	if videoPID == invalidPID {
		return data, false
	}

	result := make([]byte, len(data))
	copy(result, data)

	var allPayload []byteRange
	inVideoPES := false
	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
		pid, start, _, hdrlen, err := tsio.ParseTSHeader(pkt)
		if err != nil || hdrlen >= tsPacketSize {
			continue
		}
		if pid != videoPID {
			continue
		}
		payloadStart := off + hdrlen
		payloadEnd := off + tsPacketSize

		if start {
			inVideoPES = false
			payload := pkt[hdrlen:]
			if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
				continue
			}
			pesHdrLen, streamid, _, _, _, err := tsio.ParsePESHeader(payload)
			if err != nil || streamid < 0xe0 || streamid > 0xef {
				continue
			}
			payloadStart += pesHdrLen
			if payloadStart > payloadEnd {
				// Header spans this packet; skip payload bytes from this packet.
				continue
			}
			inVideoPES = true
		}

		if !inVideoPES {
			continue
		}
		if payloadStart < payloadEnd {
			allPayload = append(allPayload, byteRange{start: payloadStart, end: payloadEnd})
		}
	}
	if len(allPayload) == 0 {
		return result, false
	}
	return result, fixPES(result, result, allPayload)
}

func findVideoPID(data []byte) uint16 {
	pmtPID := invalidPID
	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
		pid, start, _, hdrlen, err := tsio.ParseTSHeader(pkt)
		if err != nil || !start || hdrlen >= tsPacketSize {
			continue
		}
		if pid != tsio.PAT_PID {
			continue
		}
		payload := pkt[hdrlen:]
		tableid, _, psihdrlen, datalen, err := tsio.ParsePSI(payload)
		if err != nil || tableid != tsio.TableIdPAT {
			continue
		}
		end := psihdrlen + datalen
		if end > len(payload) || datalen <= 0 {
			continue
		}
		var pat tsio.PAT
		if _, err := pat.Unmarshal(payload[psihdrlen:end]); err != nil {
			continue
		}
		for _, e := range pat.Entries {
			if e.ProgramMapPID != 0 {
				pmtPID = e.ProgramMapPID
				break
			}
		}
		if pmtPID != invalidPID {
			break
		}
	}

	if pmtPID != invalidPID {
		for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
			pkt := data[off : off+tsPacketSize]
			pid, start, _, hdrlen, err := tsio.ParseTSHeader(pkt)
			if err != nil || !start || hdrlen >= tsPacketSize || pid != pmtPID {
				continue
			}
			payload := pkt[hdrlen:]
			tableid, _, psihdrlen, datalen, err := tsio.ParsePSI(payload)
			if err != nil || tableid != tsio.TableIdPMT {
				continue
			}
			end := psihdrlen + datalen
			if end > len(payload) || datalen <= 0 {
				continue
			}
			var pmt tsio.PMT
			if _, err := pmt.Unmarshal(payload[psihdrlen:end]); err != nil {
				continue
			}
			for _, es := range pmt.ElementaryStreamInfos {
				if es.StreamType == tsio.ElementaryStreamTypeH264 {
					return es.ElementaryPID
				}
			}
		}
	}

	// Fallback for truncated segments that may not include PAT/PMT.
	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
		pid, start, _, hdrlen, err := tsio.ParseTSHeader(pkt)
		if err != nil || !start || hdrlen >= tsPacketSize {
			continue
		}
		payload := pkt[hdrlen:]
		if len(payload) >= 4 && payload[0] == 0 && payload[1] == 0 && payload[2] == 1 {
			if payload[3] >= 0xe0 && payload[3] <= 0xef {
				return pid
			}
		}
	}
	return invalidPID
}

func fixPES(orig, result []byte, ranges []byteRange) bool {
	total := 0
	for _, r := range ranges {
		if r.end > r.start {
			total += r.end - r.start
		}
	}
	if total == 0 {
		return false
	}

	es := make([]byte, 0, total)
	for _, r := range ranges {
		if r.end <= r.start || r.start < 0 || r.end > len(orig) {
			return false
		}
		es = append(es, orig[r.start:r.end]...)
	}
	nals := scanNALs(es)
	if len(nals) == 0 {
		return false
	}

	leading := es[:nals[0].start]
	reordered := make([]byte, 0, len(es))
	reordered = append(reordered, leading...)

	var changed bool
	appendSegment := func(seg []nalInfo) {
		if len(seg) == 0 {
			return
		}
		firstVCL := -1
		for i, n := range seg {
			if n.typ >= 1 && n.typ <= 5 {
				firstVCL = i
				break
			}
		}
		if firstVCL < 0 {
			for _, n := range seg {
				reordered = append(reordered, es[n.start:n.end]...)
			}
			return
		}
		misplacedSEI := false
		for i := firstVCL + 1; i < len(seg); i++ {
			if seg[i].typ == 6 {
				misplacedSEI = true
				break
			}
		}
		if !misplacedSEI {
			for _, n := range seg {
				reordered = append(reordered, es[n.start:n.end]...)
			}
			return
		}

		changed = true
		for i := 0; i < firstVCL; i++ {
			n := seg[i]
			reordered = append(reordered, es[n.start:n.end]...)
		}
		for i := firstVCL; i < len(seg); i++ {
			n := seg[i]
			if n.typ == 6 {
				reordered = append(reordered, es[n.start:n.end]...)
			}
		}
		for i := firstVCL; i < len(seg); i++ {
			n := seg[i]
			if n.typ != 6 {
				reordered = append(reordered, es[n.start:n.end]...)
			}
		}
	}

	segStart := 0
	for i := 0; i <= len(nals); i++ {
		segmentBoundary := i == len(nals) || (i > segStart && nals[i].typ == 9)
		if !segmentBoundary {
			continue
		}
		appendSegment(nals[segStart:i])
		segStart = i
	}
	if !changed {
		return false
	}
	if len(reordered) != len(es) {
		return false
	}

	pos := 0
	for _, r := range ranges {
		n := r.end - r.start
		if n <= 0 {
			continue
		}
		copy(result[r.start:r.end], reordered[pos:pos+n])
		pos += n
	}
	return pos == len(reordered)
}

func scanNALs(es []byte) []nalInfo {
	var nals []nalInfo
	for pos := 0; pos < len(es); {
		start, scLen := findStartCode(es, pos)
		if start < 0 {
			break
		}
		nextStart, _ := findStartCode(es, start+scLen)
		end := len(es)
		if nextStart >= 0 {
			end = nextStart
		}
		if start+scLen < end {
			nals = append(nals, nalInfo{
				start: start,
				end:   end,
				typ:   es[start+scLen] & 0x1f,
			})
		}
		if nextStart < 0 {
			break
		}
		pos = nextStart
	}
	return nals
}

func findStartCode(b []byte, from int) (int, int) {
	for i := from; i+3 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 {
			continue
		}
		if b[i+2] == 1 {
			return i, 3
		}
		if i+4 < len(b) && b[i+2] == 0 && b[i+3] == 1 {
			return i, 4
		}
	}
	return -1, 0
}

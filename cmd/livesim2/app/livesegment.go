package app

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// LiveSegments generates a live media segment for asset dependent on configuration.
func LiveSegment(vodFS fs.FS, a *asset, cfg *ResponseConfig, segmentPart string, nowMS int) ([]byte, error) {
	seg, segRef, err := findMediaSegment(vodFS, a, cfg, segmentPart, nowMS)
	if err != nil {
		return nil, fmt.Errorf("findSegment: %w", err)
	}
	timeShift := segRef.newTime - seg.Segments[0].Fragments[0].Moof.Traf.Tfdt.BaseMediaDecodeTime()
	for _, frag := range seg.Segments[0].Fragments {
		frag.Moof.Mfhd.SequenceNumber = segRef.newNr
		oldTime := frag.Moof.Traf.Tfdt.BaseMediaDecodeTime()
		frag.Moof.Traf.Tfdt.SetBaseMediaDecodeTime(oldTime + timeShift)
	}

	out := make([]byte, seg.Size())
	sw := bits.NewFixedSliceWriterFromSlice(out)
	err = seg.EncodeSW(sw)
	if err != nil {
		return nil, fmt.Errorf("mp4Encode: %w", err)
	}
	return sw.Bytes(), nil
}

type segRef struct {
	rep       *RepData
	origTime  uint64
	newTime   uint64
	origNr    uint32
	newNr     uint32
	origDur   uint32
	newDur    uint32
	timescale uint32
}

// findSegRefFromTime finds the proper segment if time is OK. Otherwise error message like TooEarly or Gone
func findSegRefFromTime(a *asset, rep *RepData, time uint64, cfg *ResponseConfig, nowMS int) (segRef, error) {
	mediaRef := cfg.StartTimeS * rep.MediaTimescale // TODO. Add period offsets
	now := nowMS * rep.MediaTimescale / 1000
	nowRel := now - mediaRef
	wrapDur := a.LoopDurMS * rep.MediaTimescale / 1000
	nrWraps := int(time) / wrapDur
	wrapTime := nrWraps * wrapDur
	timeAfterWrap := int(time) - wrapTime
	idx := rep.findSegmentIndexFromTime(uint64(timeAfterWrap))
	if idx == len(rep.segments) {
		return segRef{}, fmt.Errorf("no matching segment")
	}
	seg := rep.segments[idx]
	if seg.startTime != uint64(timeAfterWrap) {
		return segRef{}, fmt.Errorf("segment time mismatch %d <-> %d", timeAfterWrap, seg.startTime)
	}

	// Check interval validity
	// Valid interval [nowRel-cfg.tsbd, nowRel) where end-time must be used
	segAvailTime := int(seg.endTime) + wrapTime
	if cfg.AvailabilityTimeOffsetS != nil {
		segAvailTime -= int(*cfg.AvailabilityTimeOffsetS * float64(rep.MediaTimescale))
	}
	if segAvailTime > nowRel {
		return segRef{}, newErrTooEarly((segAvailTime - nowRel) * 1000 / rep.MediaTimescale)
	}
	if segAvailTime < nowRel-(*cfg.TimeShiftBufferDepthS+timeShiftBufferDepthMarginS)*rep.MediaTimescale {
		return segRef{}, errGone
	}

	// Default startNr is 1, but can be overriddenby actual value set in cfg.
	outNrOffset := 1
	if cfg.StartNr != nil {
		outNrOffset = *cfg.StartNr
	}

	return segRef{
		rep:       rep,
		origTime:  seg.startTime,
		newTime:   time,
		origNr:    seg.nr,
		newNr:     uint32(outNrOffset + idx + nrWraps*len(rep.segments)),
		origDur:   uint32(seg.endTime - seg.startTime),
		newDur:    uint32(seg.endTime - seg.startTime),
		timescale: uint32(rep.MediaTimescale),
	}, nil
}

func findSegRefFromNr(a *asset, rep *RepData, nr uint32, cfg *ResponseConfig, nowMS int) (segRef, error) {
	wrapLen := len(rep.segments)
	startNr := 1
	if cfg.StartNr != nil {
		startNr = *cfg.StartNr
	}
	nrWraps := (int(nr) - startNr) / wrapLen
	relNr := int(nr) - nrWraps*wrapLen
	wrapDur := a.LoopDurMS * rep.MediaTimescale / 1000
	wrapTime := nrWraps * wrapDur
	seg := rep.segments[relNr]
	segTime := wrapTime + int(seg.startTime)

	mediaRef := cfg.StartTimeS * rep.MediaTimescale // TODO. Add period offsets
	now := nowMS * rep.MediaTimescale / 1000
	nowRel := now - mediaRef

	// Check interval validity
	// Valid interval [nowRel-cfg.tsbd, nowRel) where end-time must be used
	segAvailTime := int(seg.endTime) + wrapTime
	if cfg.AvailabilityTimeOffsetS != nil {
		segAvailTime -= int(*cfg.AvailabilityTimeOffsetS * float64(rep.MediaTimescale))
	}
	if segAvailTime > nowRel {
		return segRef{}, newErrTooEarly((segAvailTime - nowRel) * 1000 / rep.MediaTimescale)
	}
	if segAvailTime < nowRel-(*cfg.TimeShiftBufferDepthS+timeShiftBufferDepthMarginS)*rep.MediaTimescale {
		return segRef{}, errGone
	}

	return segRef{
		rep:       rep,
		origTime:  seg.startTime,
		newTime:   uint64(segTime),
		origNr:    seg.nr,
		newNr:     nr,
		origDur:   uint32(seg.endTime - seg.startTime),
		newDur:    uint32(seg.endTime - seg.startTime),
		timescale: uint32(rep.MediaTimescale),
	}, nil
}

func writeInitSegment(w http.ResponseWriter, vodFS fs.FS, a *asset, segmentPart string) (bool, error) {
	for _, rep := range a.Reps {
		if segmentPart == rep.initURI {
			w.Header().Set("Content-Length", strconv.Itoa(len(rep.initBytes)))
			w.Header().Set("Content-Type", "video/mp4") // TODO. Make better depending on extension
			_, err := w.Write(rep.initBytes)
			if err != nil {
				log.Error().Err(err).Msg("writing response")
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

func writeLiveSegment(w http.ResponseWriter, cfg *ResponseConfig, vodFS fs.FS, a *asset, segmentPart string, nowMS int) error {
	data, err := LiveSegment(vodFS, a, cfg, segmentPart, nowMS)
	if err != nil {
		return fmt.Errorf("convertToLive: %w", err)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Type", "video/mp4") // TODO. Make better depending on extension
	_, err = w.Write(data)
	if err != nil {
		log.Error().Err(err).Msg("writing response")
		return err
	}
	return nil
}

func findMediaSegment(vodFS fs.FS, a *asset, cfg *ResponseConfig, segmentPart string, nowMS int) (seg *mp4.File, segRef segRef, err error) {
	for _, rep := range a.Reps {
		mParts := rep.mediaRegexp.FindStringSubmatch(segmentPart)
		if mParts == nil {
			continue
		}
		if len(mParts) != 2 {
			return nil, segRef, fmt.Errorf("bad segment match")
		}
		idNr, err := strconv.Atoi(mParts[1])
		if err != nil {
			return nil, segRef, err
		}

		switch cfg.liveMPDType() {
		case segmentNumber, timeLineNumber:
			nr := uint32(idNr)
			segRef, err = findSegRefFromNr(a, rep, nr, cfg, nowMS)
		case timeLineTime:
			time := uint64(idNr)
			segRef, err = findSegRefFromTime(a, rep, time, cfg, nowMS)
		default:
			return nil, segRef, fmt.Errorf("unknown liveMPDtype")
		}
		if err != nil {
			return nil, segRef, err
		}
		segPath := path.Join(a.AssetPath, replaceTimeAndNr(rep.mediaURI, segRef.origTime, segRef.origNr))
		data, err := fs.ReadFile(vodFS, segPath)
		if err != nil {
			return nil, segRef, fmt.Errorf("read segment: %w", err)
		}
		sr := bits.NewFixedSliceReader(data)
		seg, err = mp4.DecodeFileSR(sr)
		if err != nil {
			return nil, segRef, fmt.Errorf("mp4Decode: %w", err)
		}
		break // seg found
	}
	if seg == nil {
		return nil, segRef, errNotFound
	}
	return seg, segRef, nil
}

// writeChunkedSegment splits a segment into chunks and send them as they become available timewise.
//
// nowMS servers as referens for the current time and can be set to any value. Media time will
// be incremented with respect to nowMS.
func writeChunkedSegment(ctx context.Context, w http.ResponseWriter, log *zerolog.Logger, cfg *ResponseConfig,
	vodFS fs.FS, a *asset, segmentPart string, nowMS int) error {

	// Need init segment
	seg, segRef, err := findMediaSegment(vodFS, a, cfg, segmentPart, nowMS)
	if err != nil {
		return fmt.Errorf("findSegment: %w", err)
	}
	// Some part of the segment should be available, so we need to deliver that part and then return the
	// rest as time passes.
	// In general, we should extract all the samples and build a new one with the right fragment duration.
	// That fragment/chunk duration is segment_duration-availabilityTimeOffset.
	chunkDur := (a.SegmentDurMS - int(*cfg.AvailabilityTimeOffsetS*1000)) * int(segRef.timescale) / 1000
	chunks, err := chunkSegment(segRef.rep.initSeg, seg, segRef, chunkDur)
	if err != nil {
		return fmt.Errorf("chunkSegment: %w", err)
	}
	fmt.Printf("nr segments is %d\n", len(chunks))
	startUnixMS := unixMS()
	chunkAvailTime := int(segRef.newTime) + cfg.StartTimeS*int(segRef.timescale)
	for _, chk := range chunks {
		chunkAvailTime += int(chk.dur)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		chunkAvailMS := chunkAvailTime * 1000 / int(segRef.timescale)
		if chunkAvailMS < nowMS {
			err = writeChunk(w, chk)
			if err != nil {
				return fmt.Errorf("writeChunk: %w", err)
			}
			continue
		}
		nowUpdateMS := unixMS() - startUnixMS + nowMS
		if chunkAvailMS < nowUpdateMS {
			err = writeChunk(w, chk)
			if err != nil {
				return fmt.Errorf("writeChunk: %w", err)
			}
			continue
		}
		sleepMS := chunkAvailMS - nowUpdateMS
		time.Sleep(time.Duration(sleepMS * 1_000_000))
		err = writeChunk(w, chk)
		if err != nil {
			return fmt.Errorf("writeChunk: %w", err)
		}
	}
	return nil
}

func unixMS() int {
	return int(time.Now().UnixMilli())
}

type chunk struct {
	styp *mp4.StypBox
	frag *mp4.Fragment
	dur  uint64 // in media timescale
}

func createChunk(styp *mp4.StypBox, trackID, seqNr uint32) chunk {
	frag, _ := mp4.CreateFragment(seqNr, trackID)
	return chunk{
		styp: styp,
		frag: frag,
		dur:  0,
	}
}

// chunkSegment splits a segment into chunks of specified duration.
// The first chunk gets an styp box if one is available in the incoming segment.
func chunkSegment(init *mp4.InitSegment, seg *mp4.File, segRef segRef, chunkDur int) ([]chunk, error) {
	if len(seg.Segments) != 1 {
		return nil, fmt.Errorf("not 1 but %d segments", len(seg.Segments))
	}
	trex := init.Moov.Mvex.Trex
	s := seg.Segments[0]
	fs := make([]mp4.FullSample, 0, 32)
	for _, f := range s.Fragments {
		ff, err := f.GetFullSamples(trex)
		if err != nil {
			return nil, err
		}
		fs = append(fs, ff...)
	}
	chunks := make([]chunk, 0, segRef.newDur/uint32(chunkDur))
	trackID := init.Moov.Trak.Tkhd.TrackID
	ch := createChunk(s.Styp, trackID, segRef.newNr)
	chunkNr := 1
	var accChunkDur uint32 = 0
	var totalDur int = 0
	sampleDecodeTime := segRef.newTime
	var thisChunkDur uint32 = 0
	for i := range fs {
		fs[i].DecodeTime = sampleDecodeTime
		ch.frag.AddFullSample(fs[i])
		dur := fs[i].Dur
		sampleDecodeTime += uint64(dur)
		accChunkDur += dur
		thisChunkDur += dur
		totalDur += int(dur)
		if totalDur >= chunkDur*chunkNr {
			ch.dur = uint64(thisChunkDur)
			chunks = append(chunks, ch)
			ch = createChunk(nil, trackID, segRef.newNr)
			thisChunkDur = 0
			chunkNr++
		}
	}
	if thisChunkDur > 0 {
		ch.dur = uint64(chunkDur)
		chunks = append(chunks, ch)
	}

	return chunks, nil
}

func writeChunk(w http.ResponseWriter, chk chunk) error {
	if chk.styp != nil {
		err := chk.styp.Encode(w)
		if err != nil {
			return err
		}
	}
	err := chk.frag.Encode(w)
	if err != nil {
		return err
	}
	flusher := w.(http.Flusher)
	flusher.Flush()
	return nil
}
// Copyright 2023, DASH-Industry Forum. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package app

import (
	"fmt"
	"math"
	"strings"

	m "github.com/Eyevinn/dash-mpd/mpd"
)

type wrapTimes struct {
	startWraps  int
	startWrapMS int
	startTimeMS int
	startRelMS  int
	nowMS       int
	nowWraps    int
	nowWrapMS   int
	nowRelMS    int
}

func calcWrapTimes(a *asset, cfg *ResponseConfig, nowMS int, tsbd m.Duration) wrapTimes {
	wt := wrapTimes{nowMS: nowMS}
	wt.startTimeMS = nowMS - int(tsbd)/1_000_000
	startTimeMS := cfg.StartTimeS * 1000
	if wt.startTimeMS < startTimeMS {
		wt.startTimeMS = startTimeMS
	}
	wt.startWraps = (wt.startTimeMS - startTimeMS) / a.LoopDurMS
	wt.startWrapMS = wt.startWraps*a.LoopDurMS + startTimeMS
	wt.startRelMS = wt.startTimeMS - wt.startWrapMS

	wt.nowWraps = (nowMS - startTimeMS) / a.LoopDurMS
	wt.nowWrapMS = wt.nowWraps*a.LoopDurMS + startTimeMS
	wt.nowRelMS = nowMS - wt.nowWrapMS

	return wt
}

// LiveMPD generates a dynamic configured MPD for a VoD asset.
func LiveMPD(a *asset, mpdName string, cfg *ResponseConfig, nowMS int) (*m.MPD, error) {
	mpd, err := a.getVodMPD(mpdName)
	if err != nil {
		return nil, err
	}
	mpd.Type = Ptr("dynamic")
	mpd.MediaPresentationDuration = nil
	mpd.AvailabilityStartTime = m.ConvertToDateTime(float64(cfg.StartTimeS))
	mpd.MinimumUpdatePeriod = Ptr(m.Duration(a.SegmentDurMS * 1_000_000))
	if cfg.MinimumUpdatePeriodS != nil {
		mpd.MinimumUpdatePeriod = m.Seconds2DurPtr(*cfg.MinimumUpdatePeriodS)
	}
	if cfg.SuggestedPresentationDelayS != nil {
		mpd.SuggestedPresentationDelay = m.Seconds2DurPtr(*cfg.SuggestedPresentationDelayS)
	}
	if cfg.TimeShiftBufferDepthS != nil {
		mpd.TimeShiftBufferDepth = m.Seconds2DurPtr(*cfg.TimeShiftBufferDepthS)
	}
	if cfg.AddLocationFlag {
		var strBuf strings.Builder
		//strBuf.WriteString("http://localhost:8888")
		for i := 1; i < len(cfg.URLParts); i++ {
			strBuf.WriteString("/")
			switch {
			case strings.HasPrefix(cfg.URLParts[i], "startrel_"):
				strBuf.WriteString(fmt.Sprintf("start_%d", cfg.StartTimeS))
			case strings.HasPrefix(cfg.URLParts[i], "stoprel_"):
				strBuf.WriteString(fmt.Sprintf("stop_%d", *cfg.StopTimeS))
			default:
				strBuf.WriteString(cfg.URLParts[i])
			}
		}
		mpd.Location = []m.AnyURI{m.AnyURI(strBuf.String())}
	}

	if cfg.getAvailabilityTimeOffsetS() > 0 {
		if !cfg.AvailabilityTimeCompleteFlag {
			if cfg.LatencyTargetMS == nil {
				return nil, fmt.Errorf("latencyTargetMS (ltgt) not set")
			}
			latencyTargetMS := uint32(*cfg.LatencyTargetMS)
			mpd.ServiceDescription = createServiceDescription(latencyTargetMS)
		}
	}

	addUTCTimings(mpd, cfg)

	afterStop := false
	endTimeMS := nowMS
	if cfg.StopTimeS != nil {
		stopTimeMS := *cfg.StopTimeS * 1000
		if stopTimeMS < nowMS {
			endTimeMS = stopTimeMS
			afterStop = true
		}
	}

	wTimes := calcWrapTimes(a, cfg, endTimeMS, *mpd.TimeShiftBufferDepth)

	period := mpd.Periods[0]
	period.Duration = nil
	period.Id = "P0"
	period.Start = Ptr(m.Duration(0))

	for i, as := range period.AdaptationSets {
		se, err := calcSegmentEntriesForAdaptationSet(cfg, a, as, wTimes)
		if err != nil {
			return nil, err
		}
		switch cfg.liveMPDType() {
		case timeLineTime:
			err := adjustAdaptationSetForTimelineTime(cfg, se, as)
			if err != nil {
				return nil, fmt.Errorf("adjustASForTimelineTime: %w", err)
			}
			if i == 0 {
				mpd.PublishTime = m.ConvertToDateTime(calcPublishTime(cfg, se.lsi))
			}
		case timeLineNumber:
			err := adjustAdaptationSetForTimelineNr(cfg, se, as)
			if err != nil {
				return nil, fmt.Errorf("adjustASForTimelineNr: %w", err)
			}
			if i == 0 {
				mpd.PublishTime = m.ConvertToDateTime(calcPublishTime(cfg, se.lsi))
			}
		case segmentNumber:
			err := adjustAdaptationSetForSegmentNumber(cfg, a, as, wTimes)
			if err != nil {
				return nil, fmt.Errorf("adjustASForSegmentNumber: %w", err)
			}
			mpd.PublishTime = mpd.AvailabilityStartTime
		default:
			return nil, fmt.Errorf("unknown mpd type")
		}
	}
	if len(cfg.TimeSubsStpp) > 0 {
		err = addTimeSubsStpp(cfg, a, period)
		if err != nil {
			return nil, fmt.Errorf("addTimeSubsStpp: %w", err)
		}
	}
	if cfg.PeriodsPerHour == nil {
		if afterStop {
			mpdDurS := *cfg.StopTimeS - cfg.StartTimeS
			makeMPDStatic(mpd, mpdDurS)
		}
		return mpd, nil
	}

	// Split into multiple periods
	err = splitPeriod(mpd, a, cfg, wTimes)
	if err != nil {
		return nil, fmt.Errorf("splitPeriods: %w", err)
	}

	if afterStop {
		mpdDurS := *cfg.StopTimeS - cfg.StartTimeS
		makeMPDStatic(mpd, mpdDurS)
	}

	return mpd, nil
}

func makeMPDStatic(mpd *m.MPD, mpdDurS int) {
	mpd.Type = Ptr(m.StaticMPDType)
	mpd.TimeShiftBufferDepth = nil
	mpd.MinimumUpdatePeriod = nil
	mpd.SuggestedPresentationDelay = nil
	mpd.MediaPresentationDuration = m.Seconds2DurPtr(mpdDurS)
}

// splitPeriod splits the single-period MPD into multiple periods given cfg.PeriodsPerHour
func splitPeriod(mpd *m.MPD, a *asset, cfg *ResponseConfig, wTimes wrapTimes) error {
	if len(mpd.Periods) != 1 {
		return fmt.Errorf("not exactly one period in the MPD")
	}
	if cfg.PeriodsPerHour == nil {
		return nil
	}
	periodDur := 3600 / *cfg.PeriodsPerHour
	if periodDur*1000%a.SegmentDurMS != 0 {
		return fmt.Errorf("period duration %ds not a multiple of segment duration %dms", periodDur, a.SegmentDurMS)
	}

	startPeriodNr := wTimes.startTimeMS / (periodDur * 1000)
	endPeriodNr := wTimes.nowMS / (periodDur * 1000)
	inPeriod := mpd.Periods[0]
	nrPeriods := endPeriodNr - startPeriodNr + 1
	periods := make([]*m.Period, 0, nrPeriods)
	for pNr := startPeriodNr; pNr <= endPeriodNr; pNr++ {
		p := inPeriod.Clone()
		p.Id = fmt.Sprintf("P%d", pNr)
		p.Start = m.Seconds2DurPtr(pNr * periodDur)
		for aNr, as := range p.AdaptationSets {
			inAS := inPeriod.AdaptationSets[aNr]
			timeScale := int(as.SegmentTemplate.GetTimescale())
			pto := Ptr(uint64(pNr * periodDur * timeScale))
			switch cfg.liveMPDType() {
			case segmentNumber:
				as.SegmentTemplate.PresentationTimeOffset = pto
				segDur := int(*as.SegmentTemplate.Duration)
				startNr := uint32(pNr * periodDur * timeScale / segDur)
				as.SegmentTemplate.StartNumber = Ptr(startNr)
			case timeLineTime:
				as.SegmentTemplate.PresentationTimeOffset = pto
				inS := inAS.SegmentTemplate.SegmentTimeline.S
				periodStart, periodEnd := uint64(pNr*periodDur), uint64((pNr+1)*periodDur)
				as.SegmentTemplate.SegmentTimeline.S, _ = reduceS(inS, nil, timeScale, periodStart, periodEnd)
			case timeLineNumber:
				as.SegmentTemplate.PresentationTimeOffset = pto
				inS := inAS.SegmentTemplate.SegmentTimeline.S
				startNr := inAS.SegmentTemplate.StartNumber
				periodStart, periodEnd := uint64(pNr*periodDur), uint64((pNr+1)*periodDur)
				as.SegmentTemplate.SegmentTimeline.S, as.SegmentTemplate.StartNumber = reduceS(inS, startNr, timeScale, periodStart, periodEnd)
			default:
				return fmt.Errorf("unknown mpd type")
			}
		}
		periods = append(periods, p)
	}
	mpd.Periods = nil
	for _, p := range periods {
		mpd.AppendPeriod(p)
	}
	return nil
}

func reduceS(entries []*m.S, startNr *uint32, timescale int, periodStartS, periodEndS uint64) ([]*m.S, *uint32) {
	var t uint64
	pStart := periodStartS * uint64(timescale)
	pEnd := periodEndS * uint64(timescale)
	nr := uint32(0)
	if startNr != nil {
		nr = *startNr
	}
	outStartNr := nr
	newS := make([]*m.S, 0, len(entries))
	var currS *m.S
	for _, e := range entries {
		if e.T != nil {
			t = *e.T
		}
		d := e.D
		for i := 0; i <= e.R; i++ {
			if t < pStart {
				t += d
				nr++
				continue
			}
			if t >= pEnd {
				return newS, &nr
			}
			if currS == nil {
				currS = &m.S{
					T: Ptr(t),
					D: d,
				}
				outStartNr = nr
				newS = append(newS, currS)
			} else {
				if d == currS.D {
					currS.R++
				} else {
					currS = &m.S{
						T: Ptr(t),
						D: d,
					}
					newS = append(newS, currS)
				}
			}
			t += d
		}
	}
	return newS, &outStartNr
}

// createServiceDescription creates a fixed service description for low-latency
func createServiceDescription(latencyTargetMS uint32) []*m.ServiceDescriptionType {
	minLatency := latencyTargetMS * 3 / 4
	maxLatency := latencyTargetMS * 2
	return []*m.ServiceDescriptionType{
		{
			Id: 0,
			Latencies: []*m.LatencyType{
				{
					ReferenceId: 0,
					Max:         Ptr(maxLatency),
					Min:         Ptr(minLatency),
					Target:      Ptr(latencyTargetMS),
				},
			},
			PlaybackRates: []*m.PlaybackRateType{
				{
					Max: 1.04,
					Min: 0.96,
				},
			},
		},
	}
}

func createProducerReferenceTimes(startTimeS int) []*m.ProducerReferenceTimeType {
	return []*m.ProducerReferenceTimeType{
		{
			Id:               0,
			PresentationTime: 0,
			Type:             "encoder",
			WallClockTime:    string(m.ConvertToDateTime(float64(startTimeS))),
			UTCTiming: &m.DescriptorType{
				SchemeIdUri: "urn:mpeg:dash:utc:http-iso:2014",
				Value:       "http://time.akamai.com/?iso",
			},
		},
	}
}

type segEntries struct {
	entries        []*m.S
	lsi            lastSegInfo
	startNr        int
	mediaTimescale uint32
}

func calcSegmentEntriesForAdaptationSet(cfg *ResponseConfig, a *asset, as *m.AdaptationSetType, wt wrapTimes) (segEntries, error) {
	se := segEntries{}
	if as.SegmentTemplate == nil {
		return se, fmt.Errorf("no SegmentTemplate in AdaptationSet")
	}
	ato := cfg.getAvailabilityTimeOffsetS()
	if cfg.liveMPDType() != segmentNumber {
		if ato == math.Inf(+1) {
			return se, ErrAtoInfTimeline
		}
	}
	if ato != 0 {
		as.SegmentTemplate.AvailabilityTimeOffset = m.FloatInf64(ato)
	}
	if !cfg.AvailabilityTimeCompleteFlag {
		as.SegmentTemplate.AvailabilityTimeComplete = Ptr(false)
		if cfg.getAvailabilityTimeOffsetS() > 0 {
			as.SegmentTemplate.AvailabilityTimeOffset = m.FloatInf64(cfg.getAvailabilityTimeOffsetS())
			as.ProducerReferenceTimes = createProducerReferenceTimes(cfg.StartTimeS)
		}
	}
	atoMS := int(1000 * ato)
	r := as.Representations[0] // Assume that any representation will be fine
	se.mediaTimescale = uint32(a.Reps[r.Id].MediaTimescale)
	se.entries, se.lsi, se.startNr = a.generateTimelineEntries(r.Id, wt, atoMS)
	return se, nil
}

func adjustAdaptationSetForTimelineTime(cfg *ResponseConfig, se segEntries, as *m.AdaptationSetType) error {
	if as.SegmentTemplate.SegmentTimeline == nil {
		as.SegmentTemplate.SegmentTimeline = &m.SegmentTimelineType{}
	}
	as.SegmentTemplate.StartNumber = nil
	as.SegmentTemplate.Duration = nil
	as.SegmentTemplate.Media = strings.Replace(as.SegmentTemplate.Media, "$Number$", "$Time$", -1)
	as.SegmentTemplate.Timescale = Ptr(se.mediaTimescale)
	as.SegmentTemplate.SegmentTimeline.S = se.entries
	return nil
}

func adjustAdaptationSetForTimelineNr(cfg *ResponseConfig, se segEntries, as *m.AdaptationSetType) error {
	if as.SegmentTemplate.SegmentTimeline == nil {
		as.SegmentTemplate.SegmentTimeline = &m.SegmentTimelineType{}
	}
	as.SegmentTemplate.StartNumber = nil
	as.SegmentTemplate.Duration = nil
	as.SegmentTemplate.Media = strings.Replace(as.SegmentTemplate.Media, "$Time$", "$Number$", -1)
	as.SegmentTemplate.Timescale = Ptr(se.mediaTimescale)
	as.SegmentTemplate.SegmentTimeline.S = se.entries

	if se.startNr >= 0 {
		as.SegmentTemplate.StartNumber = Ptr(uint32(se.startNr))
	}
	return nil
}

func adjustAdaptationSetForSegmentNumber(cfg *ResponseConfig, a *asset, as *m.AdaptationSetType, wt wrapTimes) error {
	if as.SegmentTemplate.Duration == nil {
		r0 := as.Representations[0]
		rep0 := a.Reps[r0.Id]
		dur := rep0.duration() / len(rep0.segments)
		timeScale := rep0.MediaTimescale
		as.SegmentTemplate.Duration = Ptr(uint32(dur))
		as.SegmentTemplate.Timescale = Ptr(uint32(timeScale))
	}
	as.SegmentTemplate.SegmentTimeline = nil
	if cfg.StartNr != nil {
		startNr := Ptr(uint32(*cfg.StartNr))
		as.SegmentTemplate.StartNumber = startNr
	}
	as.SegmentTemplate.Media = strings.Replace(as.SegmentTemplate.Media, "$Time$", "$Number$", -1)
	return nil
}

func addTimeSubsStpp(cfg *ResponseConfig, a *asset, period *m.Period) error {
	var vAS *m.AdaptationSetType
	for _, as := range period.AdaptationSets {
		if as.ContentType == "video" {
			vAS = as
			break
		}
	}
	if vAS == nil {
		return fmt.Errorf("no video adaptation set found")
	}
	segDurMS := a.SegmentDurMS
	typicalStppSegSizeBits := 2000 * 8 // 2kB
	vST := vAS.SegmentTemplate
	for i, lang := range cfg.TimeSubsStpp {
		rep := m.NewRepresentation()
		rep.Id = SUBS_STPP_PREFIX + lang
		rep.Bandwidth = uint32(typicalStppSegSizeBits*1000) / uint32(segDurMS)
		rep.StartWithSAP = 1
		st := m.NewSegmentTemplate()
		st.Initialization = "$RepresentationID$/init.mp4"
		st.Media = "$RepresentationID$/$Number$.m4s"
		st.SetTimescale(1000)

		if vST.Duration != nil {
			st.Duration = Ptr(*vST.Duration * 1000 / vST.GetTimescale())
		}
		if vST.StartNumber != nil {
			st.StartNumber = vST.StartNumber
		}
		as := m.NewAdaptationSet()
		as.Id = Ptr(uint32(100 + i))
		as.Lang = lang
		as.ContentType = "text"
		as.MimeType = "application/mp4"
		as.SegmentAlignment = true
		as.Codecs = "stpp"
		as.Roles = append(as.Roles,
			&m.DescriptorType{SchemeIdUri: "urn:mpeg:dash:role:2011", Value: "subtitle"})
		as.SegmentTemplate = st
		as.AppendRepresentation(rep)
		period.AppendAdaptationSet(as)
	}
	return nil
}

// calcPublishTime calculates the last time there was a change in the manifest in seconds.
func calcPublishTime(cfg *ResponseConfig, lsi lastSegInfo) float64 {
	switch cfg.liveMPDType() {
	case segmentNumber:
		// For single-period case, nothing change after startTime
		return float64(cfg.StartTimeS)
	case timeLineTime, timeLineNumber:
		// Here we need the availabilityTime of the last segment
		return lastSegAvailTimeS(cfg, lsi)
	default:
		panic("liveMPD type not yet implemented")
	}
}

// lastSegAvailTimeS returns the availabilityTime of the last segment,
// including the availabilityTimeOffset.
func lastSegAvailTimeS(cfg *ResponseConfig, lsi lastSegInfo) float64 {
	ast := float64(cfg.StartTimeS)
	if lsi.nr < 0 {
		return ast
	}
	availTime := lsi.availabilityTime(cfg.AvailabilityTimeOffsetS) + ast
	if availTime < ast {
		return ast
	}
	return availTime
}

// addUTCTimings adds the UTCTiming elements to the MPD.
func addUTCTimings(mpd *m.MPD, cfg *ResponseConfig) {
	if len(cfg.UTCTimingMethods) == 0 {
		// default if none is set. Use HTTP with ms precision.
		mpd.UTCTimings = []*m.DescriptorType{
			{
				SchemeIdUri: "urn:mpeg:dash:utc:http-iso:2014",
				Value:       UtcTimingHttpServerMS,
			},
		}
		return
	}
	for _, utcTiming := range cfg.UTCTimingMethods {
		var ut *m.DescriptorType
		switch utcTiming {
		case UtcTimingDirect:
			ut = &m.DescriptorType{
				SchemeIdUri: "urn:mpeg:dash:utc:direct:2014",
				Value:       string(mpd.PublishTime),
			}
		case UtcTimingNtp:
			ut = &m.DescriptorType{
				SchemeIdUri: "urn:mpeg:dash:utc:ntp:2014",
				Value:       UtcTimingNtpServer,
			}
		case UtcTimingSntp:
			ut = &m.DescriptorType{
				SchemeIdUri: "urn:mpeg:dash:utc:sntp:2014",
				Value:       UtcTimingSntpServer,
			}
		case UtcTimingHttpXSDate:
			ut = &m.DescriptorType{
				SchemeIdUri: "urn:mpeg:dash:utc:http-xsdate:2014",
				Value:       UtcTimingHttpServer,
			}
		case UtcTimingHttpISO:
			ut = &m.DescriptorType{
				SchemeIdUri: "urn:mpeg:dash:utc:http-iso:2014",
				Value:       UtcTimingHttpServerMS,
			}
		case UtcTimingNone:
			cfg.UTCTimingMethods = nil
			return // no UTCTiming elements
		}
		mpd.UTCTimings = append(mpd.UTCTimings, ut)
	}
}

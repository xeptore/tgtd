package mpd

import (
	"encoding/xml"
	"fmt"
	"io"

	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/sliceutil"
)

type MPD struct {
	XMLName                   xml.Name `xml:"MPD"`
	Profiles                  string   `xml:"profiles,attr"`
	Type                      string   `xml:"type,attr"`
	MinBufferTime             string   `xml:"minBufferTime,attr"`
	MediaPresentationDuration string   `xml:"mediaPresentationDuration,attr"`
	Period                    Period   `xml:"Period"`
}

func (m *MPD) flawP() flaw.P {
	return flaw.P{
		"profiles":                    m.Profiles,
		"type":                        m.Type,
		"min_buffer_time":             m.MinBufferTime,
		"media_presentation_duration": m.MediaPresentationDuration,
		"period":                      m.Period.flawP(),
	}
}

type Period struct {
	ID            string        `xml:"id,attr"`
	AdaptationSet AdaptationSet `xml:"AdaptationSet"`
}

func (p Period) flawP() flaw.P {
	return flaw.P{
		"id":             p.ID,
		"adaptation_set": p.AdaptationSet.flawP(),
	}
}

type AdaptationSet struct {
	ID               string         `xml:"id,attr"`
	ContentType      string         `xml:"contentType,attr"`
	MimeType         string         `xml:"mimeType,attr"`
	SegmentAlignment bool           `xml:"segmentAlignment,attr"`
	Representation   Representation `xml:"Representation"`
}

func (a AdaptationSet) flawP() flaw.P {
	return flaw.P{
		"id":                a.ID,
		"content_type":      a.ContentType,
		"mime_type":         a.MimeType,
		"segment_alignment": a.SegmentAlignment,
		"representation":    a.Representation.flawP(),
	}
}

type Representation struct {
	ID                string          `xml:"id,attr"`
	Codecs            string          `xml:"codecs,attr"`
	Bandwidth         int             `xml:"bandwidth,attr"`
	AudioSamplingRate int             `xml:"audioSamplingRate,attr"`
	SegmentTemplate   SegmentTemplate `xml:"SegmentTemplate"`
}

func (r Representation) flawP() flaw.P {
	return flaw.P{
		"id":                  r.ID,
		"codecs":              r.Codecs,
		"bandwidth":           r.Bandwidth,
		"audio_sampling_rate": r.AudioSamplingRate,
		"segment_template":    r.SegmentTemplate.flawP(),
	}
}

type SegmentTemplate struct {
	Timescale       int             `xml:"timescale,attr"`
	Initialization  string          `xml:"initialization,attr"`
	Media           string          `xml:"media,attr"`
	StartNumber     int             `xml:"startNumber,attr"`
	SegmentTimeline SegmentTimeline `xml:"SegmentTimeline"`
}

func (s SegmentTemplate) flawP() flaw.P {
	return flaw.P{
		"timescale":        s.Timescale,
		"initialization":   s.Initialization,
		"media":            s.Media,
		"start_number":     s.StartNumber,
		"segment_timeline": s.SegmentTimeline.flawP(),
	}
}

type SegmentTimeline struct {
	S []S `xml:"S"`
}

func (s SegmentTimeline) flawP() flaw.P {
	return flaw.P{
		"s": sliceutil.Map(s.S, func(s S) flaw.P { return s.flawP() }),
	}
}

type S struct {
	D int `xml:"d,attr"`
	R int `xml:"r,attr,omitempty"`
}

func (s S) flawP() flaw.P {
	return flaw.P{
		"d": s.D,
		"r": s.R,
	}
}

type StreamInfo struct {
	Codec    string
	MimeType string
	Parts    Parts
}

func (si StreamInfo) FlawP() flaw.P {
	return flaw.P{
		"codec": si.Codec,
		"parts": si.Parts.FlawP(),
	}
}

type Parts struct {
	InitializationURLTemplate string
	Count                     int
}

func (p Parts) FlawP() flaw.P {
	return flaw.P{
		"initialization_url_template": p.InitializationURLTemplate,
		"count":                       p.Count,
	}
}

func (m *MPD) parts() (*Parts, error) {
	contentType := m.Period.AdaptationSet.ContentType
	if contentType != "audio" {
		return nil, flaw.From(fmt.Errorf("unexpected content type: %s", contentType))
	}

	partsCount := 2
	for _, s := range m.Period.AdaptationSet.Representation.SegmentTemplate.SegmentTimeline.S {
		if s.R != 0 {
			partsCount += s.R
		} else {
			partsCount++
		}
	}
	return &Parts{
		InitializationURLTemplate: m.Period.AdaptationSet.Representation.SegmentTemplate.Media,
		Count:                     partsCount,
	}, nil
}

func ParseStreamInfo(r io.Reader) (*StreamInfo, error) {
	var mpd MPD
	dec := xml.NewDecoder(r)
	dec.Strict = true
	if err := dec.Decode(&mpd); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to parse MPD: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"parsed_mpd": mpd.flawP()}

	parts, err := mpd.parts()
	if nil != err {
		return nil, flaw.From(fmt.Errorf("failed to get parts: %v", err)).Append(flawP)
	}

	return &StreamInfo{
		Codec:    mpd.Period.AdaptationSet.Representation.Codecs,
		MimeType: mpd.Period.AdaptationSet.MimeType,
		Parts:    *parts,
	}, nil
}

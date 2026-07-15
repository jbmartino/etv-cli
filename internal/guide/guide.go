// Package guide parses ErsatzTV's XMLTV guide into a per-channel, now/next view.
//
// It exists so "what does the server think is on right now" is one command, which is exactly the
// question when the stream and Plex's cached guide disagree: ErsatzTV's own guide is the truth, and
// this reads it straight from the source.
package guide

import (
	"encoding/xml"
	"sort"
	"strings"
	"time"
)

// xmltvTime is the layout XMLTV uses for start/stop, e.g. "20260714232600 -0400".
const xmltvTime = "20060102150405 -0700"

type Programme struct {
	Start time.Time
	Stop  time.Time
	Title string
	Sub   string
}

type Channel struct {
	// Number and Name are split from the XMLTV display-name, which ErsatzTV writes as "5 Disney".
	Number     string
	Name       string
	Programmes []Programme
}

type xmlTV struct {
	Channels   []xmlChannel   `xml:"channel"`
	Programmes []xmlProgramme `xml:"programme"`
}

type xmlChannel struct {
	ID          string   `xml:"id,attr"`
	DisplayName []string `xml:"display-name"`
}

type xmlProgramme struct {
	Channel  string `xml:"channel,attr"`
	Start    string `xml:"start,attr"`
	Stop     string `xml:"stop,attr"`
	Title    string `xml:"title"`
	SubTitle string `xml:"sub-title"`
}

// Parse reads an XMLTV document into channels, each with its programmes sorted by start time. The
// channel order in the document is preserved, so channels stay in lineup order.
func Parse(data []byte) ([]Channel, error) {
	var doc xmlTV
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}

	byID := map[string]*Channel{}
	var order []string
	for _, c := range doc.Channels {
		num, name := splitDisplayName(firstNonEmpty(c.DisplayName))
		byID[c.ID] = &Channel{Number: num, Name: name}
		order = append(order, c.ID)
	}

	for _, p := range doc.Programmes {
		ch := byID[p.Channel]
		if ch == nil {
			continue
		}
		start, err := time.Parse(xmltvTime, p.Start)
		if err != nil {
			continue
		}
		stop, _ := time.Parse(xmltvTime, p.Stop)
		ch.Programmes = append(ch.Programmes, Programme{Start: start, Stop: stop, Title: p.Title, Sub: p.SubTitle})
	}

	out := make([]Channel, 0, len(order))
	for _, id := range order {
		ch := byID[id]
		sort.Slice(ch.Programmes, func(i, j int) bool { return ch.Programmes[i].Start.Before(ch.Programmes[j].Start) })
		out = append(out, *ch)
	}
	return out, nil
}

// NowNext returns the programme airing at t and the one that follows it. When nothing is airing at t
// (a gap, or t is before the guide starts), now is nil and next is the first upcoming programme.
func (c Channel) NowNext(t time.Time) (now, next *Programme) {
	for i := range c.Programmes {
		p := &c.Programmes[i]
		if !p.Start.After(t) && t.Before(p.Stop) {
			if i+1 < len(c.Programmes) {
				return p, &c.Programmes[i+1]
			}
			return p, nil
		}
	}
	for i := range c.Programmes {
		if c.Programmes[i].Start.After(t) {
			return nil, &c.Programmes[i]
		}
	}
	return nil, nil
}

func splitDisplayName(s string) (num, name string) {
	parts := strings.SplitN(strings.TrimSpace(s), " ", 2)
	if len(parts) == 2 && isNumber(parts[0]) {
		return parts[0], parts[1]
	}
	return "", s
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

func firstNonEmpty(ss []string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// Package export reads a live ErsatzTV instance and writes a manifest directory that apply can push
// back, so a rebuilt server can be restored from files. It is the inverse of apply: apply pushes the
// manifest up, export pulls the server down.
//
// Export writes only what it can faithfully round-trip. A channel whose playout is not a single
// managed Sequential schedule cannot be expressed in the manifest, so it is reported and skipped
// rather than written as something that would not come back the same.
package export

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jbmartino/etv-cli/internal/etv"
	"github.com/jbmartino/etv-cli/internal/manifest"
	"gopkg.in/yaml.v3"
)

type Options struct {
	Dir   string
	Force bool
	Out   func(format string, args ...any)
}

func Export(c *etv.Client, opts Options) error {
	say := opts.Out
	if say == nil {
		say = func(string, ...any) {}
	}
	dir := opts.Dir
	if dir == "" {
		dir = "."
	}

	manifestPath := filepath.Join(dir, "etv.yaml")
	if !opts.Force {
		if _, err := os.Stat(manifestPath); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite", manifestPath)
		}
	}

	channels, err := c.Channels()
	if err != nil {
		return err
	}
	playouts, err := c.Playouts()
	if err != nil {
		return err
	}
	schedules, err := c.Schedules()
	if err != nil {
		return err
	}

	profileName, err := profileNames(c)
	if err != nil {
		return err
	}
	watermarkName, err := refNames(c.Watermarks)
	if err != nil {
		return err
	}
	fillerName, err := refNames(c.Fillers)
	if err != nil {
		return err
	}

	channelNameByID := map[int]string{}
	for _, ch := range channels {
		channelNameByID[ch.ID] = ch.Name
	}
	scheduleNameByPath := map[string]string{}
	for _, s := range schedules {
		scheduleNameByPath[normPath(s.Path)] = s.Name
	}

	// Which managed Sequential schedule (if any) each channel runs. A channel not in this map cannot
	// be represented in the manifest and is skipped with a reason.
	seqSchedule := map[string]string{}
	skipReason := map[string]string{}
	for _, p := range playouts {
		if p.ScheduleKind != "Sequential" {
			skipReason[p.ChannelName] = fmt.Sprintf("its playout is %s, not Sequential", p.ScheduleKind)
			continue
		}
		if name, ok := scheduleNameByPath[normPath(p.ScheduleFile)]; ok {
			seqSchedule[p.ChannelName] = name
		} else {
			skipReason[p.ChannelName] = "its playout points at a schedule the server does not manage by name"
		}
	}

	var outChannels []manifest.Channel
	usedSchedules := map[string]bool{}

	for _, ch := range channels {
		schedName, ok := seqSchedule[ch.Name]
		if !ok {
			reason := skipReason[ch.Name]
			if reason == "" {
				reason = "it has no playout"
			}
			say("skip channel %q: %s", ch.Name, reason)
			continue
		}

		detail, err := c.Channel(ch.ID)
		if err != nil {
			return err
		}

		mc := manifest.Channel{
			Name:                       ch.Name,
			Schedule:                   schedName,
			Number:                     ptr(detail.Number),
			Group:                      ptr(detail.Group),
			StreamSelectorMode:         ptr(detail.StreamSelectorMode),
			StreamingMode:              ptr(detail.StreamingMode),
			StreamingEngine:            ptr(detail.StreamingEngine),
			NextEngineTextSubtitleMode: ptr(detail.NextEngineTextSubtitleMode),
			TranscodeMode:              ptr(detail.TranscodeMode),
			IdleBehavior:               ptr(detail.IdleBehavior),
			PlayoutSource:              ptr(detail.PlayoutSource),
			PlayoutMode:                ptr(detail.PlayoutMode),
			SubtitleMode:               ptr(detail.SubtitleMode),
			MusicVideoCreditsMode:      ptr(detail.MusicVideoCreditsMode),
			SongVideoMode:              ptr(detail.SongVideoMode),
			SlugSeconds:                detail.SlugSeconds,
			PlayoutOffset:              detail.PlayoutOffset,
			Enabled:                    ptr(detail.IsEnabled),
			ShowInEpg:                  ptr(detail.ShowInEpg),
		}

		// Strings are emitted only when non-empty; the server's default for these is the empty string,
		// so an omitted one restores to the same value.
		mc.Categories = nonEmpty(detail.Categories)
		mc.StreamSelector = nonEmpty(detail.StreamSelector)
		mc.PreferredAudioTitle = nonEmpty(detail.PreferredAudioTitle)
		mc.MusicVideoCreditsTemplate = nonEmpty(detail.MusicVideoCreditsTemplate)
		if detail.PreferredAudioLanguage != nil {
			mc.PreferredAudioLanguage = nonEmpty(*detail.PreferredAudioLanguage)
		}
		if detail.PreferredSubtitleLanguage != nil {
			mc.PreferredSubtitleLanguage = nonEmpty(*detail.PreferredSubtitleLanguage)
		}

		// References are stored by id but written by name, so the manifest survives a rebuilt server.
		if detail.FFmpegProfileID != 0 {
			name, ok := profileName[detail.FFmpegProfileID]
			if !ok {
				return fmt.Errorf("channel %q uses ffmpeg profile id %d, which the server did not list",
					ch.Name, detail.FFmpegProfileID)
			}
			mc.FFmpegProfile = ptr(name)
		}
		if detail.WatermarkID != nil {
			name, ok := watermarkName[*detail.WatermarkID]
			if !ok {
				return fmt.Errorf("channel %q uses watermark id %d, which the server did not list",
					ch.Name, *detail.WatermarkID)
			}
			mc.Watermark = ptr(name)
		}
		if detail.FallbackFillerID != nil {
			name, ok := fillerName[*detail.FallbackFillerID]
			if !ok {
				return fmt.Errorf("channel %q uses filler id %d, which the server did not list",
					ch.Name, *detail.FallbackFillerID)
			}
			mc.Filler = ptr(name)
		}
		if detail.MirrorSourceChannelID != nil {
			name, ok := channelNameByID[*detail.MirrorSourceChannelID]
			if !ok {
				return fmt.Errorf("channel %q mirrors channel id %d, which the server did not list",
					ch.Name, *detail.MirrorSourceChannelID)
			}
			mc.MirrorSourceChannel = ptr(name)
		}

		if ch.LogoPath != "" {
			rel, err := writeLogo(c, dir, ch)
			if err != nil {
				return err
			}
			mc.Logo = ptr(rel)
		}

		usedSchedules[schedName] = true
		outChannels = append(outChannels, mc)
	}

	if len(outChannels) == 0 {
		return fmt.Errorf("no channels with a managed Sequential playout to export")
	}

	if err := os.MkdirAll(filepath.Join(dir, "schedules"), 0o755); err != nil {
		return err
	}
	for name := range usedSchedules {
		content, err := c.GetSchedule(name)
		if err != nil {
			return err
		}
		if content == "" {
			return fmt.Errorf("schedule %q returned no content", name)
		}
		if err := os.WriteFile(filepath.Join(dir, "schedules", name+".seq.yaml"), []byte(content), 0o644); err != nil {
			return err
		}
	}

	collections, err := c.Collections()
	if err != nil {
		return err
	}
	var outCollections []manifest.Collection
	for _, col := range collections {
		items, err := c.CollectionItems(col.ID)
		if err != nil {
			return err
		}
		var shows []string
		for _, it := range items {
			shows = append(shows, it.Name)
		}
		sort.Strings(shows)
		outCollections = append(outCollections, manifest.Collection{Name: col.Name, Shows: shows})
	}

	man := manifest.Manifest{
		SchedulesDir: "schedules",
		Collections:  outCollections,
		Channels:     outChannels,
	}
	blob, err := yaml.Marshal(man)
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, blob, 0o644); err != nil {
		return err
	}

	say("exported %d channel(s), %d schedule(s), %d collection(s) to %s",
		len(outChannels), len(usedSchedules), len(outCollections), manifestPath)
	return nil
}

// writeLogo downloads a channel's logo and writes it under logos/, returning the manifest-relative
// path. The extension comes from the response content type, so an extensionless hash becomes g4.png.
func writeLogo(c *etv.Client, dir string, ch etv.Channel) (string, error) {
	image, contentType, err := c.GetLogo(ch.LogoPath)
	if err != nil {
		return "", err
	}
	name := slugify(ch.Name) + etv.LogoExtension(contentType)
	if err := os.MkdirAll(filepath.Join(dir, "logos"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "logos", name), image, 0o644); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join("logos", name)), nil
}

func profileNames(c *etv.Client) (map[int]string, error) {
	profiles, err := c.FFmpegProfiles()
	if err != nil {
		return nil, err
	}
	m := map[int]string{}
	for _, p := range profiles {
		m[p.ID] = p.Name
	}
	return m, nil
}

func refNames(list func() ([]etv.NamedRef, error)) (map[int]string, error) {
	items, err := list()
	if err != nil {
		return nil, err
	}
	m := map[int]string{}
	for _, it := range items {
		m[it.ID] = it.Name
	}
	return m, nil
}

func ptr[T any](v T) *T { return &v }

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// normPath compares server-reported paths across OSes: the server may be Windows or Linux, so
// separators and case are normalized rather than compared byte for byte.
func normPath(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
}

// slugify turns a channel name into a safe file stem for its logo: lowercase, non-alphanumerics
// collapsed to single dashes, trimmed.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "logo"
	}
	return out
}

// Package manifest describes the channels etv manages.
//
// The manifest plus the schedule files are what you keep in version control and push to the server.
// Nothing here reads the server; apply is what pushes the two up.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Manifest struct {
	// SchedulesDir holds the *.seq.yaml files, relative to the manifest.
	SchedulesDir string       `yaml:"schedulesDir,omitempty"`
	Collections  []Collection `yaml:"collections"`
	Channels     []Channel    `yaml:"channels"`

	dir string
}

type Collection struct {
	Name string `yaml:"name"`
	// Shows are matched against the titles returned by GET /api/media/shows. The server reports
	// them as "South Park (1997)", so a bare title matches too.
	Shows []string `yaml:"shows"`
}

type Channel struct {
	Name string `yaml:"name"`
	// Schedule is the schedule name, which is also the file stem: "mtv" -> mtv.seq.yaml.
	Schedule string `yaml:"schedule"`
	// Logo is an image path relative to the manifest. Three states, like every other optional field:
	// omitted means the manifest does not manage the logo and apply leaves whatever is there; an
	// empty string means the channel should have no logo, and apply removes it; a path means set it.
	Logo *string `yaml:"logo,omitempty"`

	// Settings below are optional, and unset means unmanaged: apply leaves whatever the server has.
	// The manifest only owns what it actually mentions, so adopting an existing channel does not
	// require writing down every field just to avoid resetting it. Pointers, not zero values,
	// because "" and false are both legitimate things to want.
	//
	// FFmpegProfile, Watermark, Filler and MirrorSourceChannel are references: the server stores
	// them by numeric id, but ids do not survive a rebuilt server, so the manifest names them and
	// apply resolves the name to an id. A name that does not exist on the server is an error.
	Number                     *string  `yaml:"number,omitempty"`
	Group                      *string  `yaml:"group,omitempty"`
	Categories                 *string  `yaml:"categories,omitempty"`
	FFmpegProfile              *string  `yaml:"ffmpegProfile,omitempty"`
	SlugSeconds                *float64 `yaml:"slugSeconds,omitempty"`
	StreamSelectorMode         *string  `yaml:"streamSelectorMode,omitempty"`
	StreamSelector             *string  `yaml:"streamSelector,omitempty"`
	StreamingMode              *string  `yaml:"streamingMode,omitempty"`
	StreamingEngine            *string  `yaml:"streamingEngine,omitempty"`
	NextEngineTextSubtitleMode *string  `yaml:"nextEngineTextSubtitleMode,omitempty"`
	TranscodeMode              *string  `yaml:"transcodeMode,omitempty"`
	IdleBehavior               *string  `yaml:"idleBehavior,omitempty"`
	PlayoutSource              *string  `yaml:"playoutSource,omitempty"`
	PlayoutMode                *string  `yaml:"playoutMode,omitempty"`
	PlayoutOffset              *string  `yaml:"playoutOffset,omitempty"`
	MirrorSourceChannel        *string  `yaml:"mirrorSourceChannel,omitempty"`
	PreferredAudioLanguage     *string  `yaml:"preferredAudioLanguage,omitempty"`
	PreferredAudioTitle        *string  `yaml:"preferredAudioTitle,omitempty"`
	PreferredSubtitleLanguage  *string  `yaml:"preferredSubtitleLanguage,omitempty"`
	SubtitleMode               *string  `yaml:"subtitleMode,omitempty"`
	MusicVideoCreditsMode      *string  `yaml:"musicVideoCreditsMode,omitempty"`
	MusicVideoCreditsTemplate  *string  `yaml:"musicVideoCreditsTemplate,omitempty"`
	SongVideoMode              *string  `yaml:"songVideoMode,omitempty"`
	Watermark                  *string  `yaml:"watermark,omitempty"`
	Filler                     *string  `yaml:"filler,omitempty"`
	Enabled                    *bool    `yaml:"enabled,omitempty"`
	ShowInEpg                  *bool    `yaml:"showInEpg,omitempty"`
}

func Load(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	m.dir = filepath.Dir(path)

	if m.SchedulesDir == "" {
		m.SchedulesDir = "schedules"
	}
	if !filepath.IsAbs(m.SchedulesDir) {
		m.SchedulesDir = filepath.Join(filepath.Dir(path), m.SchedulesDir)
	}

	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if len(m.Channels) == 0 {
		return fmt.Errorf("manifest has no channels")
	}

	seen := map[string]bool{}
	seenNumber := map[string]string{}
	for _, ch := range m.Channels {
		if strings.TrimSpace(ch.Name) == "" {
			return fmt.Errorf("channel with no name")
		}
		if strings.TrimSpace(ch.Schedule) == "" {
			return fmt.Errorf("channel %q has no schedule", ch.Name)
		}
		if seen[ch.Name] {
			return fmt.Errorf("channel %q listed twice", ch.Name)
		}
		seen[ch.Name] = true

		// Numbers are how apply recognizes a channel it has renamed, and the server requires them to
		// be unique anyway, so two channels claiming one number can only end badly.
		if ch.Number != nil {
			if other, dup := seenNumber[*ch.Number]; dup {
				return fmt.Errorf("channels %q and %q both claim number %s", other, ch.Name, *ch.Number)
			}
			seenNumber[*ch.Number] = ch.Name
		}

		if _, err := os.Stat(m.ScheduleFile(ch.Schedule)); err != nil {
			return fmt.Errorf("channel %q references schedule %q, but %s does not exist",
				ch.Name, ch.Schedule, m.ScheduleFile(ch.Schedule))
		}

		if logo, managed := m.LogoFile(ch); managed && logo != "" {
			if _, err := os.Stat(logo); err != nil {
				return fmt.Errorf("channel %q references logo %s, which does not exist", ch.Name, logo)
			}
		}
	}
	return nil
}

// LogoFile resolves a channel's logo.
//
// managed is false when the manifest says nothing about the logo, in which case apply must leave the
// channel's current logo alone. When managed is true, an empty path means the channel should have no
// logo at all, and apply removes whatever is there.
func (m *Manifest) LogoFile(ch Channel) (path string, managed bool) {
	if ch.Logo == nil {
		return "", false
	}
	if *ch.Logo == "" {
		return "", true
	}
	if filepath.IsAbs(*ch.Logo) {
		return *ch.Logo, true
	}
	return filepath.Join(m.dir, *ch.Logo), true
}

// ScheduleFile is the local path of a schedule by name.
func (m *Manifest) ScheduleFile(name string) string {
	return filepath.Join(m.SchedulesDir, name+".seq.yaml")
}

func (m *Manifest) ReadSchedule(name string) (string, error) {
	raw, err := os.ReadFile(m.ScheduleFile(name))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

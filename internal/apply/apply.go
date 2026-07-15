// Package apply pushes a manifest to a live ErsatzTV instance over its HTTP API.
//
// It is idempotent: applying an unchanged manifest changes nothing and rebuilds nothing, which is
// what makes it safe to run from a git hook or CI on every push.
//
// Two rules keep it honest:
//
// Never a silent no-op. If the manifest asks for something apply cannot do, it says so or fails.
// A tool that quietly does nothing is worse than one that errors, because you believe it.
//
// Never a silent deletion. The manifest owns what it names, and only what it names. Anything else
// on the server is reported as unmanaged and left alone, because a partial manifest is a normal
// thing to have and it must not be able to take channels off the air.
package apply

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jbmartino/etv-cli/internal/etv"
	"github.com/jbmartino/etv-cli/internal/manifest"
	"github.com/jbmartino/etv-cli/internal/validate"
)

type Options struct {
	DryRun bool
	Out    func(format string, args ...any)
}

type Result struct {
	Changed int
	// Unmanaged is what exists on the server but is not in the manifest. Reported, never deleted.
	Unmanaged []string
	Errors    []error
}

func Apply(c *etv.Client, m *manifest.Manifest, opts Options) (*Result, error) {
	res := &Result{}
	say := opts.Out
	if say == nil {
		say = func(string, ...any) {}
	}

	prefix := ""
	if opts.DryRun {
		prefix = "[dry-run] "
	}

	// 1. Validate every schedule locally first. The server would reject a bad one anyway, but
	//    failing before we mutate anything means a typo cannot leave the instance half-applied.
	for _, ch := range m.Channels {
		yaml, err := m.ReadSchedule(ch.Schedule)
		if err != nil {
			return nil, err
		}
		if errs := validate.Schedule(yaml); len(errs) > 0 {
			return nil, fmt.Errorf("schedule %q is invalid:\n  - %s", ch.Schedule, strings.Join(errs, "\n  - "))
		}
	}
	say("validated %d schedules", len(m.Channels))

	// 2. Channels. These come first because everything else hangs off them.
	channelID, err := reconcileChannels(c, m, opts, res, say, prefix)
	if err != nil {
		return nil, err
	}

	// 3. Collections.
	if err := reconcileCollections(c, m, opts, res, say, prefix); err != nil {
		return nil, err
	}

	// 4. Schedules: upload only what actually differs, so an unchanged apply rebuilds nothing.
	changedSchedule, managedPath, err := reconcileSchedules(c, m, opts, res, say, prefix)
	if err != nil {
		return nil, err
	}

	// 5. Playouts: a channel must have exactly one Sequential playout pointing at its schedule.
	if err := reconcilePlayouts(c, m, opts, res, say, prefix, channelID, changedSchedule, managedPath); err != nil {
		return nil, err
	}

	// 6. Whatever is on the server that the manifest never mentioned.
	if err := reportUnmanaged(c, m, res); err != nil {
		return nil, err
	}

	return res, nil
}

// reconcileChannels creates missing channels and updates the settings the manifest actually
// specifies. It returns the id of every channel in the manifest; a channel that would have been
// created during a dry run has no id yet, and is absent from the map.
func reconcileChannels(
	c *etv.Client,
	m *manifest.Manifest,
	opts Options,
	res *Result,
	say func(string, ...any),
	prefix string,
) (map[string]int, error) {
	channels, err := c.Channels()
	if err != nil {
		return nil, err
	}
	existing := map[string]etv.Channel{}
	byNumber := map[string]etv.Channel{}
	for _, ch := range channels {
		existing[ch.Name] = ch
		byNumber[ch.Number] = ch
	}

	// Reference lists (ffmpeg profiles, watermarks, fillers) are named by the manifest and resolved
	// to ids per channel. Each list is fetched only if some channel actually names one of its kind.
	profileID := map[string]int{}
	watermarkID := map[string]int{}
	fillerID := map[string]int{}
	var needProfile, needWatermark, needFiller bool
	for _, want := range m.Channels {
		needProfile = needProfile || want.FFmpegProfile != nil
		needWatermark = needWatermark || want.Watermark != nil
		needFiller = needFiller || want.Filler != nil
	}
	if needProfile {
		profiles, err := c.FFmpegProfiles()
		if err != nil {
			return nil, err
		}
		for _, p := range profiles {
			profileID[p.Name] = p.ID
		}
	}
	if needWatermark {
		watermarks, err := c.Watermarks()
		if err != nil {
			return nil, err
		}
		for _, w := range watermarks {
			watermarkID[w.Name] = w.ID
		}
	}
	if needFiller {
		fillers, err := c.Fillers()
		if err != nil {
			return nil, err
		}
		for _, fp := range fillers {
			fillerID[fp.Name] = fp.ID
		}
	}

	ids := map[string]int{}
	for _, want := range m.Channels {
		refs, err := resolveRefs(want, profileID, watermarkID, fillerID, existing, byNumber)
		if err != nil {
			return nil, err
		}

		// A channel is matched by name, or by its number when the manifest gives one. Without the
		// number fallback, renaming a channel in the manifest would silently create a second channel
		// rather than rename the one you meant.
		ch, ok := existing[want.Name]
		if !ok && want.Number != nil {
			ch, ok = byNumber[*want.Number]
			if ok {
				say("%srename channel %q -> %q (number %s)", prefix, ch.Name, want.Name, *want.Number)
			}
		}

		if !ok {
			say("%screate channel %q", prefix, want.Name)
			res.Changed++
			if opts.DryRun {
				// No id to hang a logo or a playout off, so the rest of this channel's plan is
				// reported by the steps below as if it were new, which it is.
				continue
			}
			id, err := c.CreateChannel(createChannelFields(want, refs))
			if err != nil {
				return nil, err
			}
			ids[want.Name] = id
			if err := setLogo(c, m, want, "", id, opts, res, say, prefix); err != nil {
				return nil, err
			}
			continue
		}

		ids[want.Name] = ch.ID

		detail, err := c.Channel(ch.ID)
		if err != nil {
			return nil, err
		}
		if changes := channelChanges(want, detail, refs); len(changes) > 0 {
			say("%supdate channel %q (%s)", prefix, want.Name, strings.Join(sortedKeys(changes), ", "))
			res.Changed++
			if !opts.DryRun {
				if err := c.UpdateChannel(ch.ID, changes); err != nil {
					return nil, err
				}
			}
		}

		if err := setLogo(c, m, want, ch.LogoPath, ch.ID, opts, res, say, prefix); err != nil {
			return nil, err
		}
	}

	return ids, nil
}

// setLogo converges the channel's logo. Artwork is content-addressed (the path is the uppercase MD5),
// so hashing the local file tells us whether the channel already has this exact image. Without that
// check every apply would re-upload every logo, and apply would never be a no-op.
//
// An omitted logo is unmanaged and left alone. An explicitly empty one is removed, which used to be a
// silent no-op: deleting the logo line from the manifest changed nothing and said nothing.
func setLogo(
	c *etv.Client,
	m *manifest.Manifest,
	want manifest.Channel,
	haveHash string,
	channelID int,
	opts Options,
	res *Result,
	say func(string, ...any),
	prefix string,
) error {
	logoPath, managed := m.LogoFile(want)
	if !managed {
		return nil
	}

	if logoPath == "" {
		if haveHash == "" {
			return nil
		}
		say("%sremove logo from %q", prefix, want.Name)
		res.Changed++
		if opts.DryRun {
			return nil
		}
		// an empty logo path is what the server treats as "delete this channel's logo"
		return c.UpdateChannel(channelID, map[string]any{"logoPath": ""})
	}

	image, err := os.ReadFile(logoPath)
	if err != nil {
		return err
	}
	contentType, ok := etv.LogoContentType(logoPath)
	if !ok {
		return fmt.Errorf("channel %q: unsupported logo type %s (use png, jpg, gif or webp)",
			want.Name, filepath.Ext(logoPath))
	}

	if etv.LogoHash(image) == haveHash {
		return nil
	}

	say("%sset logo for %q (%s)", prefix, want.Name, filepath.Base(logoPath))
	res.Changed++
	if opts.DryRun {
		return nil
	}
	return c.SetChannelLogo(channelID, contentType, image)
}

// channelRefs holds the resolved ids for a channel's named references. A zero profileID means the
// manifest does not manage the ffmpeg profile; a nil pointer means the manifest does not manage that
// reference at all, so apply leaves whatever the server has.
type channelRefs struct {
	profileID   int
	watermarkID *int
	fillerID    *int
	mirrorID    *int
}

// resolveRefs turns a channel's named references into ids. A name that does not exist on the server
// is an error, not a silent skip, so a typo cannot quietly leave a reference unmanaged.
func resolveRefs(
	want manifest.Channel,
	profileID, watermarkID, fillerID map[string]int,
	existing, byNumber map[string]etv.Channel,
) (channelRefs, error) {
	var refs channelRefs

	if want.FFmpegProfile != nil {
		id, ok := profileID[*want.FFmpegProfile]
		if !ok {
			return refs, fmt.Errorf("channel %q wants ffmpeg profile %q, which does not exist on the server (have: %s)",
				want.Name, *want.FFmpegProfile, strings.Join(sortedKeys(profileID), ", "))
		}
		refs.profileID = id
	}
	if want.Watermark != nil {
		id, ok := watermarkID[*want.Watermark]
		if !ok {
			return refs, fmt.Errorf("channel %q wants watermark %q, which does not exist on the server (have: %s)",
				want.Name, *want.Watermark, strings.Join(sortedKeys(watermarkID), ", "))
		}
		refs.watermarkID = &id
	}
	if want.Filler != nil {
		id, ok := fillerID[*want.Filler]
		if !ok {
			return refs, fmt.Errorf("channel %q wants filler %q, which does not exist on the server (have: %s)",
				want.Name, *want.Filler, strings.Join(sortedKeys(fillerID), ", "))
		}
		refs.fillerID = &id
	}
	if want.MirrorSourceChannel != nil {
		src, ok := existing[*want.MirrorSourceChannel]
		if !ok {
			src, ok = byNumber[*want.MirrorSourceChannel]
		}
		if !ok {
			return refs, fmt.Errorf("channel %q mirrors %q, which is not a channel on the server",
				want.Name, *want.MirrorSourceChannel)
		}
		id := src.ID
		refs.mirrorID = &id
	}

	return refs, nil
}

// createChannelFields is the body for a new channel. Anything the manifest does not specify is left
// out entirely, so the server applies its own default rather than a default invented here.
func createChannelFields(want manifest.Channel, refs channelRefs) map[string]any {
	fields := map[string]any{"name": want.Name}
	putStr(fields, "number", want.Number)
	putStr(fields, "group", want.Group)
	putStr(fields, "categories", want.Categories)
	if refs.profileID != 0 {
		fields["ffmpegProfileId"] = refs.profileID
	}
	if want.SlugSeconds != nil {
		fields["slugSeconds"] = *want.SlugSeconds
	}
	putStr(fields, "streamSelectorMode", want.StreamSelectorMode)
	putStr(fields, "streamSelector", want.StreamSelector)
	putStr(fields, "streamingMode", want.StreamingMode)
	putStr(fields, "streamingEngine", want.StreamingEngine)
	putStr(fields, "nextEngineTextSubtitleMode", want.NextEngineTextSubtitleMode)
	putStr(fields, "transcodeMode", want.TranscodeMode)
	putStr(fields, "idleBehavior", want.IdleBehavior)
	putStr(fields, "playoutSource", want.PlayoutSource)
	putStr(fields, "playoutMode", want.PlayoutMode)
	putStr(fields, "playoutOffset", want.PlayoutOffset)
	if refs.mirrorID != nil {
		fields["mirrorSourceChannelId"] = *refs.mirrorID
	}
	putStr(fields, "preferredAudioLanguageCode", want.PreferredAudioLanguage)
	putStr(fields, "preferredAudioTitle", want.PreferredAudioTitle)
	putStr(fields, "preferredSubtitleLanguageCode", want.PreferredSubtitleLanguage)
	putStr(fields, "subtitleMode", want.SubtitleMode)
	putStr(fields, "musicVideoCreditsMode", want.MusicVideoCreditsMode)
	putStr(fields, "musicVideoCreditsTemplate", want.MusicVideoCreditsTemplate)
	putStr(fields, "songVideoMode", want.SongVideoMode)
	if refs.watermarkID != nil {
		fields["watermarkId"] = *refs.watermarkID
	}
	if refs.fillerID != nil {
		fields["fallbackFillerId"] = *refs.fillerID
	}
	if want.Enabled != nil {
		fields["isEnabled"] = *want.Enabled
	}
	if want.ShowInEpg != nil {
		fields["showInEpg"] = *want.ShowInEpg
	}
	return fields
}

// channelChanges is the subset of fields that actually differ. Sending only these is what keeps a
// partial update from resetting everything the manifest does not mention. Enum-valued fields are
// compared case-insensitively because the server may echo a different casing than the manifest uses.
func channelChanges(want manifest.Channel, have etv.ChannelDetail, refs channelRefs) map[string]any {
	changes := map[string]any{}

	// differs only when the channel was matched by number, i.e. it is being renamed
	if want.Name != have.Name {
		changes["name"] = want.Name
	}
	changeStr(changes, "number", want.Number, have.Number)
	changeStr(changes, "group", want.Group, have.Group)
	changeStr(changes, "categories", want.Categories, have.Categories)
	if refs.profileID != 0 && refs.profileID != have.FFmpegProfileID {
		changes["ffmpegProfileId"] = refs.profileID
	}
	if want.SlugSeconds != nil && !sameFloat(want.SlugSeconds, have.SlugSeconds) {
		changes["slugSeconds"] = *want.SlugSeconds
	}
	changeFold(changes, "streamSelectorMode", want.StreamSelectorMode, have.StreamSelectorMode)
	changeStr(changes, "streamSelector", want.StreamSelector, have.StreamSelector)
	changeFold(changes, "streamingMode", want.StreamingMode, have.StreamingMode)
	changeFold(changes, "streamingEngine", want.StreamingEngine, have.StreamingEngine)
	changeFold(changes, "nextEngineTextSubtitleMode", want.NextEngineTextSubtitleMode, have.NextEngineTextSubtitleMode)
	changeFold(changes, "transcodeMode", want.TranscodeMode, have.TranscodeMode)
	changeFold(changes, "idleBehavior", want.IdleBehavior, have.IdleBehavior)
	changeFold(changes, "playoutSource", want.PlayoutSource, have.PlayoutSource)
	changeFold(changes, "playoutMode", want.PlayoutMode, have.PlayoutMode)
	changeStr(changes, "playoutOffset", want.PlayoutOffset, deref(have.PlayoutOffset))
	if refs.mirrorID != nil && !sameInt(refs.mirrorID, have.MirrorSourceChannelID) {
		changes["mirrorSourceChannelId"] = *refs.mirrorID
	}
	changeStr(changes, "preferredAudioLanguageCode", want.PreferredAudioLanguage, deref(have.PreferredAudioLanguage))
	changeStr(changes, "preferredAudioTitle", want.PreferredAudioTitle, have.PreferredAudioTitle)
	changeStr(changes, "preferredSubtitleLanguageCode", want.PreferredSubtitleLanguage, deref(have.PreferredSubtitleLanguage))
	changeFold(changes, "subtitleMode", want.SubtitleMode, have.SubtitleMode)
	changeFold(changes, "musicVideoCreditsMode", want.MusicVideoCreditsMode, have.MusicVideoCreditsMode)
	changeStr(changes, "musicVideoCreditsTemplate", want.MusicVideoCreditsTemplate, have.MusicVideoCreditsTemplate)
	changeFold(changes, "songVideoMode", want.SongVideoMode, have.SongVideoMode)
	if refs.watermarkID != nil && !sameInt(refs.watermarkID, have.WatermarkID) {
		changes["watermarkId"] = *refs.watermarkID
	}
	if refs.fillerID != nil && !sameInt(refs.fillerID, have.FallbackFillerID) {
		changes["fallbackFillerId"] = *refs.fillerID
	}
	if want.Enabled != nil && *want.Enabled != have.IsEnabled {
		changes["isEnabled"] = *want.Enabled
	}
	if want.ShowInEpg != nil && *want.ShowInEpg != have.ShowInEpg {
		changes["showInEpg"] = *want.ShowInEpg
	}

	return changes
}

// putStr sets fields[key] to *v when the manifest manages that field (v is non-nil).
func putStr(fields map[string]any, key string, v *string) {
	if v != nil {
		fields[key] = *v
	}
}

// changeStr records a change when the manifest manages the field (want is non-nil) and it differs.
func changeStr(changes map[string]any, key string, want *string, have string) {
	if want != nil && *want != have {
		changes[key] = *want
	}
}

// changeFold is changeStr for enum-valued fields, comparing case-insensitively.
func changeFold(changes map[string]any, key string, want *string, have string) {
	if want != nil && !strings.EqualFold(*want, have) {
		changes[key] = *want
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// reconcileCollections converges membership in both directions. Creating a collection but never
// updating it was the one place this tool used to lie: adding a show to an existing collection
// printed nothing, changed nothing, and exited 0.
func reconcileCollections(
	c *etv.Client,
	m *manifest.Manifest,
	opts Options,
	res *Result,
	say func(string, ...any),
	prefix string,
) error {
	if len(m.Collections) == 0 {
		return nil
	}

	existing, err := c.Collections()
	if err != nil {
		return err
	}
	byName := map[string]etv.Collection{}
	for _, col := range existing {
		byName[col.Name] = col
	}

	shows, err := c.Shows()
	if err != nil {
		return err
	}

	for _, want := range m.Collections {
		ids, missing := resolveShows(shows, want.Shows)
		if len(missing) > 0 {
			return fmt.Errorf("collection %q references shows not in the library: %s",
				want.Name, strings.Join(missing, ", "))
		}

		col, exists := byName[want.Name]
		if !exists {
			say("%screate collection %q with %d shows", prefix, want.Name, len(ids))
			res.Changed++
			if opts.DryRun {
				continue
			}
			created, err := c.CreateCollection(want.Name)
			if err != nil {
				return err
			}
			if len(ids) > 0 {
				if err := c.AddShowsToCollection(created.ID, ids); err != nil {
					return err
				}
			}
			continue
		}

		have, err := c.CollectionItems(col.ID)
		if err != nil {
			return err
		}

		add, remove, removeNames := membershipDiff(ids, have)

		var addNames []string
		for _, id := range add {
			addNames = append(addNames, showName(shows, id))
		}

		if len(add) > 0 {
			say("%sadd to collection %q: %s", prefix, want.Name, strings.Join(addNames, ", "))
			res.Changed++
			if !opts.DryRun {
				if err := c.AddShowsToCollection(col.ID, add); err != nil {
					return err
				}
			}
		}

		// A removal is a real deletion, but it is a deletion *within* something the manifest owns,
		// so it is part of converging that collection rather than pruning an unmanaged thing.
		if len(remove) > 0 {
			say("%sremove from collection %q: %s", prefix, want.Name, strings.Join(removeNames, ", "))
			res.Changed++
			if !opts.DryRun {
				if err := c.RemoveShowsFromCollection(col.ID, remove); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// membershipDiff is what has to change for a collection to hold exactly the wanted items: what to add,
// and what to take out. Removals are named because the plan has to be able to say what it is about to
// take out of a collection.
func membershipDiff(want []int, have []etv.CollectionItem) (add []int, remove []int, removeNames []string) {
	wanted := map[int]bool{}
	for _, id := range want {
		wanted[id] = true
	}
	present := map[int]bool{}
	for _, item := range have {
		present[item.MediaItemID] = true
	}

	for _, id := range want {
		if !present[id] {
			add = append(add, id)
		}
	}
	for _, item := range have {
		if !wanted[item.MediaItemID] {
			remove = append(remove, item.MediaItemID)
			removeNames = append(removeNames, item.Name)
		}
	}
	return add, remove, removeNames
}

func reconcileSchedules(
	c *etv.Client,
	m *manifest.Manifest,
	opts Options,
	res *Result,
	say func(string, ...any),
	prefix string,
) (map[string]bool, map[string]string, error) {
	remote, err := c.Schedules()
	if err != nil {
		return nil, nil, err
	}
	haveSchedule := map[string]bool{}
	// managedPath is where the server keeps each schedule it manages. A playout must point at
	// exactly this path, otherwise it is still referencing some file we do not control (for example
	// an absolute path typed into the UI) and the manifest is not actually the source of truth.
	managedPath := map[string]string{}
	for _, s := range remote {
		haveSchedule[s.Name] = true
		managedPath[s.Name] = s.Path
	}

	changedSchedule := map[string]bool{}
	for _, ch := range m.Channels {
		local, err := m.ReadSchedule(ch.Schedule)
		if err != nil {
			return nil, nil, err
		}

		if haveSchedule[ch.Schedule] {
			current, err := c.GetSchedule(ch.Schedule)
			if err != nil {
				return nil, nil, err
			}
			if normalize(current) == normalize(local) {
				continue
			}
		}

		say("%supload schedule %q", prefix, ch.Schedule)
		changedSchedule[ch.Schedule] = true
		res.Changed++

		if opts.DryRun {
			continue
		}

		uploaded, err := c.PutSchedule(ch.Schedule, local)
		if err != nil {
			return nil, nil, err
		}
		managedPath[ch.Schedule] = uploaded.Path
	}

	return changedSchedule, managedPath, nil
}

func reconcilePlayouts(
	c *etv.Client,
	m *manifest.Manifest,
	opts Options,
	res *Result,
	say func(string, ...any),
	prefix string,
	channelID map[string]int,
	changedSchedule map[string]bool,
	managedPath map[string]string,
) error {
	playouts, err := c.Playouts()
	if err != nil {
		return err
	}
	playoutByChannel := map[string]etv.Playout{}
	playoutByNumber := map[string]etv.Playout{}
	for _, p := range playouts {
		playoutByChannel[p.ChannelName] = p
		playoutByNumber[p.ChannelNumber] = p
	}

	for _, want := range m.Channels {
		id, known := channelID[want.Name]
		if !known {
			// Only possible during a dry run against a channel that does not exist yet.
			say("%screate Sequential playout for %q -> %q", prefix, want.Name, want.Schedule)
			res.Changed++
			continue
		}

		// By number as well as name, because during a dry run of a rename the server still has the
		// playout under the old channel name, and reporting "create a playout" there would be a lie.
		current, hasPlayout := playoutByChannel[want.Name]
		if !hasPlayout && want.Number != nil {
			current, hasPlayout = playoutByNumber[*want.Number]
		}

		// Managed means: Sequential, and pointing at the schedule the server manages under this
		// name. A playout pointing at some other path that happens to share the file name is NOT
		// managed, and gets rebuilt against the managed one.
		managed := hasPlayout &&
			current.ScheduleKind == "Sequential" &&
			samePath(current.ScheduleFile, managedPath[want.Schedule])

		if managed {
			// Only rebuild if the schedule content actually changed underneath it.
			if changedSchedule[want.Schedule] {
				say("%srebuild %q (schedule changed)", prefix, want.Name)
				res.Changed++
				if !opts.DryRun {
					if err := c.BuildPlayout(current.ID, "reset"); err != nil {
						return err
					}
				}
			}
			continue
		}

		switch {
		case hasPlayout && current.ScheduleKind == "Sequential":
			say("%srepoint %q at the managed schedule %q (was %s)",
				prefix, want.Name, want.Schedule, current.ScheduleFile)
		case hasPlayout:
			say("%sreplace playout for %q (%s -> Sequential %q)", prefix, want.Name, current.ScheduleKind, want.Schedule)
		default:
			say("%screate Sequential playout for %q -> %q", prefix, want.Name, want.Schedule)
		}
		res.Changed++
		if opts.DryRun {
			continue
		}

		if hasPlayout {
			if err := c.DeletePlayout(current.ID); err != nil {
				return err
			}
		}
		newID, err := c.CreateSequentialPlayout(id, want.Schedule)
		if err != nil {
			return err
		}
		if err := c.BuildPlayout(newID, "reset"); err != nil {
			return err
		}
	}

	return nil
}

// reportUnmanaged lists what exists on the server but is not in the manifest. It never deletes: a
// partial manifest is a normal state, and a reconciler that pruned by default would be one typo away
// from taking every channel off the air. Saying it out loud is most of the value; acting on it is
// the user's call.
func reportUnmanaged(c *etv.Client, m *manifest.Manifest, res *Result) error {
	wantChannel := map[string]bool{}
	wantSchedule := map[string]bool{}
	for _, ch := range m.Channels {
		wantChannel[ch.Name] = true
		wantSchedule[ch.Schedule] = true
	}
	wantCollection := map[string]bool{}
	for _, col := range m.Collections {
		wantCollection[col.Name] = true
	}

	channels, err := c.Channels()
	if err != nil {
		return err
	}
	for _, ch := range channels {
		if !wantChannel[ch.Name] {
			res.Unmanaged = append(res.Unmanaged, fmt.Sprintf("channel %q (number %s)", ch.Name, ch.Number))
		}
	}

	collections, err := c.Collections()
	if err != nil {
		return err
	}
	for _, col := range collections {
		if !wantCollection[col.Name] {
			res.Unmanaged = append(res.Unmanaged, fmt.Sprintf("collection %q", col.Name))
		}
	}

	schedules, err := c.Schedules()
	if err != nil {
		return err
	}
	for _, s := range schedules {
		if !wantSchedule[s.Name] {
			res.Unmanaged = append(res.Unmanaged, fmt.Sprintf("schedule %q", s.Name))
		}
	}

	sort.Strings(res.Unmanaged)
	return nil
}

// samePath compares server-reported paths. The server may be Windows or Linux, so separators and
// case are normalized rather than compared byte for byte.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, "\\", "/"))
	}
	return norm(a) == norm(b)
}

func normalize(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func showName(shows []etv.Show, id int) string {
	for _, s := range shows {
		if s.MediaItemID == id {
			return s.Name
		}
	}
	return fmt.Sprintf("#%d", id)
}

// resolveShows matches manifest titles against the library. The server reports "South Park (1997)",
// so an exact match is tried first and a bare title second.
func resolveShows(shows []etv.Show, want []string) (ids []int, missing []string) {
	exact := map[string]int{}
	bare := map[string][]int{}
	for _, s := range shows {
		exact[s.Name] = s.MediaItemID
		b := s.Name
		if i := strings.LastIndex(b, " ("); i > 0 {
			b = b[:i]
		}
		bare[b] = append(bare[b], s.MediaItemID)
	}

	for _, w := range want {
		if id, ok := exact[w]; ok {
			ids = append(ids, id)
			continue
		}
		if got := bare[w]; len(got) == 1 {
			ids = append(ids, got[0])
			continue
		}
		missing = append(missing, w)
	}
	sort.Ints(ids)
	return ids, missing
}

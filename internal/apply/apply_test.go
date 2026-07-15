package apply

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/jbmartino/etv-cli/internal/etv"
	"github.com/jbmartino/etv-cli/internal/manifest"
)

const schedule = `content:
  - collection: "G4"
    key: "G4"
    order: shuffle

playout:
  - count: 1
    content: "G4"

  - repeat: true
`

var logoBytes = []byte("not-really-a-png")

// writeManifest lays out a manifest on disk and loads it.
func writeManifest(t *testing.T, body string) *manifest.Manifest {
	t.Helper()
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "schedules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "logos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schedules", "g4.seq.yaml"), []byte(schedule), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logos", "g4.png"), logoBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "etv.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return m
}

func run(t *testing.T, c *etv.Client, m *manifest.Manifest) *Result {
	t.Helper()
	res, err := Apply(c, m, Options{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return res
}

// A channel that already exists, configured the way the live ones are: MPEG-TS, not the create default.
func seedG4(f *fakeETV) *fakeChannel {
	return f.AddChannel(fakeChannel{
		Number:          "5",
		Name:            "G4",
		Group:           "Group",
		FFmpegProfileID: 2,
		StreamingMode:   "TransportStream",
		StreamingEngine: "Legacy",
		TranscodeMode:   "OnDemand",
		IdleBehavior:    "KeepRunning",
		IsEnabled:       true,
		ShowInEpg:       true,
		LogoHash:        hashOf(logoBytes),
	})
}

const g4Manifest = `
schedulesDir: schedules
collections:
  - name: G4
    shows: [South Park]
channels:
  - { name: G4, schedule: g4, logo: logos/g4.png }
`

// The headline guarantee: touching one field must not reset the rest of the channel.
func TestPartialUpdateDoesNotClobber(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	f.AddCollection("G4", etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"})
	f.AddSchedule("g4", schedule)
	f.AddPlayout(ch.ID, schedulePath("g4"))
	c := f.Start(t)

	m := writeManifest(t, `
schedulesDir: schedules
collections:
  - name: G4
    shows: [South Park]
channels:
  - { name: G4, schedule: g4, logo: logos/g4.png, group: RetroTV }
`)

	res := run(t, c, m)

	if res.Changed != 1 {
		t.Fatalf("want 1 change, got %d", res.Changed)
	}
	if len(f.ChannelPuts) != 1 {
		t.Fatalf("want 1 PUT, got %d: %v", len(f.ChannelPuts), f.ChannelPuts)
	}

	// the body must carry the group and NOTHING else
	put := f.ChannelPuts[0]
	if len(put) != 1 || put["group"] != "RetroTV" {
		t.Fatalf("PUT body should be exactly {group: RetroTV}, got %v", put)
	}

	// and the server's other settings must have survived
	if ch.StreamingMode != "TransportStream" {
		t.Errorf("streamingMode clobbered: %s", ch.StreamingMode)
	}
	if ch.IdleBehavior != "KeepRunning" {
		t.Errorf("idleBehavior clobbered: %s", ch.IdleBehavior)
	}
	if ch.FFmpegProfileID != 2 {
		t.Errorf("ffmpegProfile clobbered: %d", ch.FFmpegProfileID)
	}
	if ch.LogoHash == "" {
		t.Error("logo was deleted")
	}
}

func TestUnchangedManifestIsANoOp(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	f.AddCollection("G4", etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"})
	f.AddSchedule("g4", schedule)
	f.AddPlayout(ch.ID, schedulePath("g4"))
	c := f.Start(t)

	res := run(t, c, writeManifest(t, g4Manifest))

	if res.Changed != 0 {
		t.Fatalf("want 0 changes, got %d", res.Changed)
	}
	if len(f.ChannelPuts) != 0 || len(f.LogoUploads) != 0 || len(f.SchedulePuts) != 0 || len(f.Builds) != 0 {
		t.Fatalf("a no-op apply touched the server: puts=%v logos=%v schedules=%v builds=%v",
			f.ChannelPuts, f.LogoUploads, f.SchedulePuts, f.Builds)
	}
}

// Adding a show to an existing collection used to print nothing, change nothing and exit 0.
func TestCollectionMembershipIsAdded(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	col := f.AddCollection("G4", etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"})
	f.AddSchedule("g4", schedule)
	f.AddPlayout(ch.ID, schedulePath("g4"))
	c := f.Start(t)

	m := writeManifest(t, `
schedulesDir: schedules
collections:
  - name: G4
    shows: [South Park, CatDog]
channels:
  - { name: G4, schedule: g4, logo: logos/g4.png }
`)

	res := run(t, c, m)

	if res.Changed != 1 {
		t.Fatalf("want 1 change, got %d", res.Changed)
	}
	if !slices.Equal(f.ItemsAdded[col.ID], []int{11}) {
		t.Fatalf("want CatDog (11) added, got %v", f.ItemsAdded[col.ID])
	}
	if len(col.Items) != 2 {
		t.Fatalf("collection should now hold 2 shows, holds %d", len(col.Items))
	}
}

func TestCollectionMembershipIsRemoved(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	col := f.AddCollection("G4",
		etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"},
		etv.CollectionItem{MediaItemID: 11, Kind: "Show", Name: "CatDog (1998)"})
	f.AddSchedule("g4", schedule)
	f.AddPlayout(ch.ID, schedulePath("g4"))
	c := f.Start(t)

	res := run(t, c, writeManifest(t, g4Manifest))

	if res.Changed != 1 {
		t.Fatalf("want 1 change, got %d", res.Changed)
	}
	if !slices.Equal(f.ItemsRemoved[col.ID], []int{11}) {
		t.Fatalf("want CatDog (11) removed, got %v", f.ItemsRemoved[col.ID])
	}
	if len(col.Items) != 1 || col.Items[0].MediaItemID != 10 {
		t.Fatalf("collection should hold only South Park, holds %v", col.Items)
	}
}

// An omitted logo is unmanaged: apply must not touch it.
func TestOmittedLogoIsLeftAlone(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	f.AddCollection("G4", etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"})
	f.AddSchedule("g4", schedule)
	f.AddPlayout(ch.ID, schedulePath("g4"))
	c := f.Start(t)

	m := writeManifest(t, `
schedulesDir: schedules
collections:
  - name: G4
    shows: [South Park]
channels:
  - { name: G4, schedule: g4 }
`)

	res := run(t, c, m)

	if res.Changed != 0 {
		t.Fatalf("want 0 changes, got %d", res.Changed)
	}
	if ch.LogoHash == "" {
		t.Error("an unmanaged logo was deleted")
	}
}

// An explicitly empty logo means the channel should have none. This used to be a silent no-op.
func TestEmptyLogoRemovesIt(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	f.AddCollection("G4", etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"})
	f.AddSchedule("g4", schedule)
	f.AddPlayout(ch.ID, schedulePath("g4"))
	c := f.Start(t)

	m := writeManifest(t, `
schedulesDir: schedules
collections:
  - name: G4
    shows: [South Park]
channels:
  - { name: G4, schedule: g4, logo: "" }
`)

	res := run(t, c, m)

	if res.Changed != 1 {
		t.Fatalf("want 1 change, got %d", res.Changed)
	}
	if len(f.ChannelPuts) != 1 || f.ChannelPuts[0]["logoPath"] != "" {
		t.Fatalf("want a PUT clearing logoPath, got %v", f.ChannelPuts)
	}
	if ch.LogoHash != "" {
		t.Error("logo was not removed")
	}
}

// Renaming in the manifest must rename the channel, not quietly create a second one.
func TestRenameIsMatchedByNumber(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	f.AddCollection("G4", etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"})
	f.AddSchedule("g4", schedule)
	f.AddPlayout(ch.ID, schedulePath("g4"))
	c := f.Start(t)

	m := writeManifest(t, `
schedulesDir: schedules
collections:
  - name: G4
    shows: [South Park]
channels:
  - { name: G4 Classic, schedule: g4, logo: logos/g4.png, number: "5" }
`)

	res := run(t, c, m)

	if len(f.CreatedChans) != 0 {
		t.Fatalf("a rename created a new channel: %v", f.CreatedChans)
	}
	if len(f.ChannelPuts) != 1 || f.ChannelPuts[0]["name"] != "G4 Classic" {
		t.Fatalf("want a PUT renaming the channel, got %v", f.ChannelPuts)
	}
	if ch.Name != "G4 Classic" {
		t.Fatalf("channel not renamed, is %q", ch.Name)
	}
	if res.Changed != 1 {
		t.Fatalf("want 1 change, got %d", res.Changed)
	}
	// the existing playout is still the channel's, so it must not be torn down and rebuilt
	if len(f.Deletes) != 0 {
		t.Fatalf("a rename deleted something: %v", f.Deletes)
	}
}

// A partial manifest must report what it does not own, and must never delete it.
func TestUnmanagedThingsAreReportedNotDeleted(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	f.AddCollection("G4", etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"})
	f.AddSchedule("g4", schedule)
	f.AddPlayout(ch.ID, schedulePath("g4"))

	// things the manifest says nothing about
	mtv := f.AddChannel(fakeChannel{Number: "13", Name: "MTV", StreamingMode: "TransportStream", IsEnabled: true})
	f.AddCollection("MTV")
	f.AddSchedule("mtv", schedule)
	mtvPlayout := f.AddPlayout(mtv.ID, schedulePath("mtv"))
	c := f.Start(t)

	res := run(t, c, writeManifest(t, g4Manifest))

	if res.Changed != 0 {
		t.Fatalf("want 0 changes, got %d", res.Changed)
	}
	want := []string{`channel "MTV" (number 13)`, `collection "MTV"`, `schedule "mtv"`}
	if !slices.Equal(res.Unmanaged, want) {
		t.Fatalf("want unmanaged %v, got %v", want, res.Unmanaged)
	}
	if len(f.Deletes) != 0 {
		t.Fatalf("apply deleted unmanaged things: %v", f.Deletes)
	}
	if f.Channel("MTV") == nil {
		t.Error("the MTV channel was deleted")
	}
	if _, ok := f.playouts[mtvPlayout.ID]; !ok {
		t.Error("the MTV playout was deleted")
	}
}

func TestCreatesEverythingOnAnEmptyServer(t *testing.T) {
	f := newFakeETV()
	c := f.Start(t)

	m := writeManifest(t, `
schedulesDir: schedules
collections:
  - name: G4
    shows: [South Park]
channels:
  - { name: G4, schedule: g4, logo: logos/g4.png, number: "5", ffmpegProfile: nvenc }
`)

	run(t, c, m)

	ch := f.Channel("G4")
	if ch == nil {
		t.Fatal("channel was not created")
	}
	if ch.Number != "5" || ch.FFmpegProfileID != 2 {
		t.Errorf("channel created with wrong fields: %+v", ch)
	}
	if ch.LogoHash != hashOf(logoBytes) {
		t.Error("logo was not uploaded")
	}
	col := f.Collection("G4")
	if col == nil || len(col.Items) != 1 {
		t.Fatalf("collection not created with its shows: %+v", col)
	}
	if _, ok := f.schedules["g4"]; !ok {
		t.Error("schedule was not uploaded")
	}
	if len(f.playouts) != 1 {
		t.Errorf("want 1 playout, got %d", len(f.playouts))
	}
	if len(f.Builds) != 1 {
		t.Errorf("the new playout should have been built once, got %v", f.Builds)
	}

	// and doing it again changes nothing
	res := run(t, c, m)
	if res.Changed != 0 {
		t.Fatalf("second apply was not a no-op: %d changes", res.Changed)
	}
}

// A schedule edit must rebuild that channel, and only that channel.
func TestChangedScheduleRebuildsThePlayout(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	f.AddCollection("G4", etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"})
	f.AddSchedule("g4", "content:\n  - collection: \"old\"\n")
	p := f.AddPlayout(ch.ID, schedulePath("g4"))
	c := f.Start(t)

	res := run(t, c, writeManifest(t, g4Manifest))

	if res.Changed != 2 { // upload + rebuild
		t.Fatalf("want 2 changes, got %d", res.Changed)
	}
	if !slices.Equal(f.SchedulePuts, []string{"g4"}) {
		t.Fatalf("want the schedule uploaded, got %v", f.SchedulePuts)
	}
	if !slices.Equal(f.Builds, []int{p.ID}) {
		t.Fatalf("want playout %d rebuilt, got %v", p.ID, f.Builds)
	}
	if len(f.Deletes) != 0 {
		t.Fatalf("a schedule edit tore down the playout: %v", f.Deletes)
	}
}

// A playout pointing at a file we do not manage is not ours, and gets repointed.
func TestUnmanagedPlayoutIsRepointed(t *testing.T) {
	f := newFakeETV()
	ch := seedG4(f)
	f.AddCollection("G4", etv.CollectionItem{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"})
	f.AddSchedule("g4", schedule)
	old := f.AddPlayout(ch.ID, `C:\retrotv\schedules\g4.seq.yaml`) // hand-placed, not the managed path
	c := f.Start(t)

	res := run(t, c, writeManifest(t, g4Manifest))

	if res.Changed != 1 {
		t.Fatalf("want 1 change, got %d", res.Changed)
	}
	if !slices.Contains(f.Deletes, "playout:"+strconv.Itoa(old.ID)) {
		t.Fatalf("the old playout should have been deleted, deletes: %v", f.Deletes)
	}
	if len(f.playouts) != 1 {
		t.Fatalf("want exactly 1 playout, got %d", len(f.playouts))
	}
	for _, p := range f.playouts {
		if p.ScheduleFile != schedulePath("g4") {
			t.Errorf("playout still points at %s", p.ScheduleFile)
		}
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	f := newFakeETV()
	c := f.Start(t)

	m := writeManifest(t, `
schedulesDir: schedules
collections:
  - name: G4
    shows: [South Park]
channels:
  - { name: G4, schedule: g4, logo: logos/g4.png }
`)

	res, err := Apply(c, m, Options{DryRun: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Changed == 0 {
		t.Fatal("dry run reported no changes against an empty server")
	}
	if len(f.channels) != 0 || len(f.collections) != 0 || len(f.playouts) != 0 || len(f.schedules) != 0 {
		t.Fatal("dry run mutated the server")
	}
}

func TestMissingShowIsALoudError(t *testing.T) {
	f := newFakeETV()
	c := f.Start(t)

	m := writeManifest(t, `
schedulesDir: schedules
collections:
  - name: G4
    shows: [Nonexistent Show]
channels:
  - { name: G4, schedule: g4 }
`)

	if _, err := Apply(c, m, Options{}); err == nil {
		t.Fatal("want an error for a show that is not in the library")
	}
}

func TestUnknownFFmpegProfileIsALoudError(t *testing.T) {
	f := newFakeETV()
	c := f.Start(t)

	m := writeManifest(t, `
schedulesDir: schedules
channels:
  - { name: G4, schedule: g4, ffmpegProfile: does-not-exist }
`)

	if _, err := Apply(c, m, Options{}); err == nil {
		t.Fatal("want an error for an ffmpeg profile that does not exist")
	}
}

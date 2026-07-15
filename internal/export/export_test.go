package export

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbmartino/etv-cli/internal/etv"
	"github.com/jbmartino/etv-cli/internal/manifest"
)

const schedPath = `C:\config\schedules\g4.seq.yaml`

const scheduleContent = `content:
  - collection: "G4"
    key: "G4"
    order: shuffle

playout:
  - count: 1
    content: "G4"

  - repeat: true
`

func iptr(n int) *int { return &n }

// fakeServer serves the read endpoints export uses. G4 has a Sequential playout and a full set of
// references; Source has no playout, so export must skip it while still resolving it as G4's mirror.
func fakeServer() http.Handler {
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("GET /api/channels", func(w http.ResponseWriter, _ *http.Request) {
		j(w, []etv.Channel{
			{ID: 1, Number: "5", Name: "G4", LogoPath: "ABC123"},
			{ID: 2, Number: "1", Name: "Source"},
		})
	})
	mux.HandleFunc("GET /api/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		j(w, etv.ChannelDetail{
			ID: 1, Number: "5", Name: "G4", Group: "RetroTV", Categories: "retro",
			FFmpegProfileID:            2,
			StreamSelectorMode:         "Default",
			StreamingMode:              "TransportStreamHybrid",
			StreamingEngine:            "Legacy",
			NextEngineTextSubtitleMode: "Burn",
			TranscodeMode:              "OnDemand",
			IdleBehavior:               "StopOnDisconnect",
			PlayoutSource:              "Generated",
			PlayoutMode:                "Continuous",
			SubtitleMode:               "None",
			MusicVideoCreditsMode:      "None",
			SongVideoMode:              "Default",
			WatermarkID:                iptr(1),
			FallbackFillerID:           iptr(1),
			MirrorSourceChannelID:      iptr(2),
			IsEnabled:                  true,
			ShowInEpg:                  true,
		})
	})
	mux.HandleFunc("GET /api/playouts", func(w http.ResponseWriter, _ *http.Request) {
		j(w, []etv.Playout{
			{ID: 1, ScheduleKind: "Sequential", ChannelName: "G4", ChannelNumber: "5", ScheduleFile: schedPath},
		})
	})
	mux.HandleFunc("GET /api/schedules", func(w http.ResponseWriter, _ *http.Request) {
		j(w, []etv.Schedule{{Name: "g4", Path: schedPath}})
	})
	mux.HandleFunc("GET /api/schedules/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != "g4" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(scheduleContent))
	})
	mux.HandleFunc("GET /api/ffmpeg/profiles", func(w http.ResponseWriter, _ *http.Request) {
		j(w, []etv.FFmpegProfile{{ID: 2, Name: "nvenc"}, {ID: 1, Name: "default"}})
	})
	mux.HandleFunc("GET /api/watermarks", func(w http.ResponseWriter, _ *http.Request) {
		j(w, []etv.NamedRef{{ID: 1, Name: "corner-bug"}})
	})
	mux.HandleFunc("GET /api/fillers", func(w http.ResponseWriter, _ *http.Request) {
		j(w, []etv.NamedRef{{ID: 1, Name: "commercials"}})
	})
	mux.HandleFunc("GET /api/collections", func(w http.ResponseWriter, _ *http.Request) {
		j(w, []etv.Collection{{ID: 1, Name: "G4"}})
	})
	mux.HandleFunc("GET /api/collections/{id}/items", func(w http.ResponseWriter, _ *http.Request) {
		j(w, []etv.CollectionItem{{MediaItemID: 10, Kind: "Show", Name: "South Park (1997)"}})
	})
	mux.HandleFunc("GET /iptv/logos/{path}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("not-really-a-png"))
	})

	return mux
}

func TestExportProducesALoadableManifest(t *testing.T) {
	srv := httptest.NewServer(fakeServer())
	defer srv.Close()
	c := etv.New(srv.URL, "k")
	dir := t.TempDir()

	var out []string
	err := Export(c, Options{Dir: dir, Out: func(f string, a ...any) {
		out = append(out, fmt.Sprintf(f, a...))
	}})
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	// The strongest round-trip assertion: what export wrote must load as a valid manifest, including
	// the schedule and logo files it references existing on disk.
	m, err := manifest.Load(filepath.Join(dir, "etv.yaml"))
	if err != nil {
		t.Fatalf("exported manifest does not load: %v", err)
	}

	if len(m.Channels) != 1 {
		t.Fatalf("want exactly 1 exported channel (Source has no playout), got %d", len(m.Channels))
	}
	ch := m.Channels[0]
	if ch.Name != "G4" || ch.Schedule != "g4" {
		t.Fatalf("channel = %q schedule = %q", ch.Name, ch.Schedule)
	}
	if got := deref(ch.FFmpegProfile); got != "nvenc" {
		t.Errorf("ffmpegProfile = %q, want nvenc (id 2 resolved to name)", got)
	}
	if got := deref(ch.Watermark); got != "corner-bug" {
		t.Errorf("watermark = %q, want corner-bug", got)
	}
	if got := deref(ch.Filler); got != "commercials" {
		t.Errorf("filler = %q, want commercials", got)
	}
	if got := deref(ch.MirrorSourceChannel); got != "Source" {
		t.Errorf("mirrorSourceChannel = %q, want Source (id 2 resolved to name)", got)
	}
	if got := deref(ch.StreamingMode); got != "TransportStreamHybrid" {
		t.Errorf("streamingMode = %q", got)
	}
	if got := deref(ch.Logo); got != "logos/g4.png" {
		t.Errorf("logo = %q, want logos/g4.png", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "logos", "g4.png")); err != nil {
		t.Errorf("logo file not written: %v", err)
	}

	if len(m.Collections) != 1 || m.Collections[0].Name != "G4" ||
		len(m.Collections[0].Shows) != 1 || m.Collections[0].Shows[0] != "South Park (1997)" {
		t.Errorf("collections = %+v", m.Collections)
	}

	// Source must have been reported as skipped, not silently dropped.
	if !contains(out, "Source") {
		t.Errorf("Source should have been reported as skipped, output was: %v", out)
	}
}

func TestExportRefusesToOverwriteWithoutForce(t *testing.T) {
	srv := httptest.NewServer(fakeServer())
	defer srv.Close()
	c := etv.New(srv.URL, "k")
	dir := t.TempDir()

	if err := Export(c, Options{Dir: dir}); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if err := Export(c, Options{Dir: dir}); err == nil {
		t.Fatal("a second export without --force should refuse to overwrite etv.yaml")
	}
	if err := Export(c, Options{Dir: dir, Force: true}); err != nil {
		t.Fatalf("export with force should overwrite: %v", err)
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func contains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

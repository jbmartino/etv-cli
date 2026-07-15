package apply

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jbmartino/etv-cli/internal/etv"
)

// fakeETV is an in-memory ErsatzTV. It exists so the reconciler can be tested end to end, including
// the exact request bodies it sends, without a server, a database or a network.
//
// It defaults new channels the way the real server does, which is the whole point: a test that let
// PUT send a partial body against a fake with no defaults would prove nothing about the bug this
// tool exists to avoid.
type fakeETV struct {
	mu sync.Mutex

	channels    map[int]*fakeChannel
	collections map[int]*fakeCollection
	playouts    map[int]*fakePlayout
	schedules   map[string]string
	shows       []etv.Show
	profiles    []etv.FFmpegProfile
	watermarks  []etv.NamedRef
	fillers     []etv.NamedRef

	nextChannelID, nextCollectionID, nextPlayoutID int

	// recorded traffic, so a test can assert what was sent and what was never sent
	ChannelPuts  []map[string]any
	LogoUploads  []int
	Builds       []int
	Deletes      []string
	CreatedChans []map[string]any
	ItemsAdded   map[int][]int
	ItemsRemoved map[int][]int
	SchedulePuts []string
}

type fakeChannel struct {
	ID                         int
	Number                     string
	Name                       string
	Group                      string
	Categories                 string
	FFmpegProfileID            int
	SlugSeconds                *float64
	StreamSelectorMode         string
	StreamSelector             string
	StreamingMode              string
	StreamingEngine            string
	NextEngineTextSubtitleMode string
	TranscodeMode              string
	IdleBehavior               string
	PlayoutSource              string
	PlayoutMode                string
	MirrorSourceChannelID      *int
	PlayoutOffset              *string
	PreferredAudioLanguage     *string
	PreferredAudioTitle        string
	PreferredSubtitleLanguage  *string
	SubtitleMode               string
	MusicVideoCreditsMode      string
	MusicVideoCreditsTemplate  string
	SongVideoMode              string
	WatermarkID                *int
	FallbackFillerID           *int
	IsEnabled                  bool
	ShowInEpg                  bool
	LogoHash                   string
}

type fakeCollection struct {
	ID    int
	Name  string
	Items []etv.CollectionItem
}

type fakePlayout struct {
	ID           int
	Kind         string
	ChannelID    int
	ScheduleFile string
}

func schedulePath(name string) string {
	return `C:\config\schedules\` + name + ".seq.yaml"
}

func hashOf(b []byte) string {
	sum := md5.Sum(b)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func newFakeETV() *fakeETV {
	return &fakeETV{
		channels:         map[int]*fakeChannel{},
		collections:      map[int]*fakeCollection{},
		playouts:         map[int]*fakePlayout{},
		schedules:        map[string]string{},
		ItemsAdded:       map[int][]int{},
		ItemsRemoved:     map[int][]int{},
		nextChannelID:    1,
		nextCollectionID: 1,
		nextPlayoutID:    1,
		profiles:         []etv.FFmpegProfile{{ID: 1, Name: "default"}, {ID: 2, Name: "nvenc"}},
		watermarks:       []etv.NamedRef{{ID: 1, Name: "corner-bug"}, {ID: 2, Name: "big-logo"}},
		fillers:          []etv.NamedRef{{ID: 1, Name: "commercials"}, {ID: 2, Name: "bumpers"}},
		shows: []etv.Show{
			{MediaItemID: 10, Name: "South Park (1997)"},
			{MediaItemID: 11, Name: "CatDog (1998)"},
			{MediaItemID: 12, Name: "Hey Arnold! (1996)"},
		},
	}
}

// AddChannel seeds a channel that already exists on the server.
func (f *fakeETV) AddChannel(ch fakeChannel) *fakeChannel {
	if ch.ID == 0 {
		ch.ID = f.nextChannelID
	}
	if ch.ID >= f.nextChannelID {
		f.nextChannelID = ch.ID + 1
	}
	f.channels[ch.ID] = &ch
	return &ch
}

func (f *fakeETV) AddCollection(name string, items ...etv.CollectionItem) *fakeCollection {
	col := &fakeCollection{ID: f.nextCollectionID, Name: name, Items: items}
	f.nextCollectionID++
	f.collections[col.ID] = col
	return col
}

func (f *fakeETV) AddSchedule(name, content string) {
	f.schedules[name] = content
}

func (f *fakeETV) AddPlayout(channelID int, scheduleFile string) *fakePlayout {
	p := &fakePlayout{ID: f.nextPlayoutID, Kind: "Sequential", ChannelID: channelID, ScheduleFile: scheduleFile}
	f.nextPlayoutID++
	f.playouts[p.ID] = p
	return p
}

func (f *fakeETV) Channel(name string) *fakeChannel {
	for _, ch := range f.channels {
		if ch.Name == name {
			return ch
		}
	}
	return nil
}

func (f *fakeETV) Collection(name string) *fakeCollection {
	for _, col := range f.collections {
		if col.Name == name {
			return col
		}
	}
	return nil
}

// Start serves the fake and returns a client pointed at it.
func (f *fakeETV) Start(t *testing.T) *etv.Client {
	t.Helper()
	srv := httptest.NewServer(f.routes())
	t.Cleanup(srv.Close)
	return etv.New(srv.URL, "test-key")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func (f *fakeETV) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"appVersion": "test"})
	})

	mux.HandleFunc("GET /api/ffmpeg/profiles", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, f.profiles)
	})

	mux.HandleFunc("GET /api/watermarks", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, f.watermarks)
	})

	mux.HandleFunc("GET /api/fillers", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, f.fillers)
	})

	mux.HandleFunc("GET /api/channels", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []etv.Channel{}
		for _, ch := range f.sortedChannels() {
			out = append(out, etv.Channel{
				ID:            ch.ID,
				Number:        ch.Number,
				Name:          ch.Name,
				FFmpegProfile: f.profileName(ch.FFmpegProfileID),
				StreamingMode: "MPEG-TS", // display string, exactly like the real list endpoint
				LogoPath:      ch.LogoHash,
			})
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /api/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ch, ok := f.channels[atoi(r.PathValue("id"))]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, etv.ChannelDetail{
			ID:                         ch.ID,
			Number:                     ch.Number,
			Name:                       ch.Name,
			Group:                      ch.Group,
			Categories:                 ch.Categories,
			FFmpegProfileID:            ch.FFmpegProfileID,
			SlugSeconds:                ch.SlugSeconds,
			StreamSelectorMode:         ch.StreamSelectorMode,
			StreamSelector:             ch.StreamSelector,
			StreamingMode:              ch.StreamingMode,
			StreamingEngine:            ch.StreamingEngine,
			NextEngineTextSubtitleMode: ch.NextEngineTextSubtitleMode,
			TranscodeMode:              ch.TranscodeMode,
			IdleBehavior:               ch.IdleBehavior,
			PlayoutSource:              ch.PlayoutSource,
			PlayoutMode:                ch.PlayoutMode,
			MirrorSourceChannelID:      ch.MirrorSourceChannelID,
			PlayoutOffset:              ch.PlayoutOffset,
			PreferredAudioLanguage:     ch.PreferredAudioLanguage,
			PreferredAudioTitle:        ch.PreferredAudioTitle,
			PreferredSubtitleLanguage:  ch.PreferredSubtitleLanguage,
			SubtitleMode:               ch.SubtitleMode,
			MusicVideoCreditsMode:      ch.MusicVideoCreditsMode,
			MusicVideoCreditsTemplate:  ch.MusicVideoCreditsTemplate,
			SongVideoMode:              ch.SongVideoMode,
			WatermarkID:                ch.WatermarkID,
			FallbackFillerID:           ch.FallbackFillerID,
			IsEnabled:                  ch.IsEnabled,
			ShowInEpg:                  ch.ShowInEpg,
		})
	})

	mux.HandleFunc("POST /api/channels", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body := map[string]any{}
		if err := readJSON(r, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.CreatedChans = append(f.CreatedChans, body)

		// the server's create defaults, which is what a partial update used to reset a channel to
		ch := &fakeChannel{
			ID:              f.nextChannelID,
			Number:          "1",
			Group:           "ErsatzTV",
			FFmpegProfileID: 1,
			StreamingMode:   "TransportStreamHybrid",
			StreamingEngine: "Legacy",
			TranscodeMode:   "OnDemand",
			IdleBehavior:    "StopOnDisconnect",
			IsEnabled:       true,
			ShowInEpg:       true,
		}
		f.nextChannelID++
		applyChannelFields(ch, body)
		f.channels[ch.ID] = ch
		writeJSON(w, map[string]any{"channelId": ch.ID})
	})

	mux.HandleFunc("PUT /api/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ch, ok := f.channels[atoi(r.PathValue("id"))]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := map[string]any{}
		if err := readJSON(r, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.ChannelPuts = append(f.ChannelPuts, body)

		// A merge, like the real one: only what is present in the body is touched. Anything the
		// caller omitted keeps its current value.
		applyChannelFields(ch, body)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("PUT /api/channels/{id}/logo", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ch, ok := f.channels[atoi(r.PathValue("id"))]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		ch.LogoHash = hashOf(raw)
		f.LogoUploads = append(f.LogoUploads, ch.ID)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /api/collections", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []etv.Collection{}
		for _, col := range f.collections {
			out = append(out, etv.Collection{ID: col.ID, Name: col.Name})
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("POST /api/collections", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			Name string `json:"name"`
		}
		if err := readJSON(r, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		col := &fakeCollection{ID: f.nextCollectionID, Name: body.Name}
		f.nextCollectionID++
		f.collections[col.ID] = col
		writeJSON(w, etv.Collection{ID: col.ID, Name: col.Name})
	})

	mux.HandleFunc("GET /api/collections/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		col, ok := f.collections[atoi(r.PathValue("id"))]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		items := col.Items
		if items == nil {
			items = []etv.CollectionItem{}
		}
		writeJSON(w, items)
	})

	mux.HandleFunc("POST /api/collections/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := atoi(r.PathValue("id"))
		col, ok := f.collections[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			ShowIDs []int `json:"showIds"`
		}
		if err := readJSON(r, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.ItemsAdded[id] = append(f.ItemsAdded[id], body.ShowIDs...)
		for _, showID := range body.ShowIDs {
			col.Items = append(col.Items, etv.CollectionItem{
				MediaItemID: showID,
				Kind:        "Show",
				Name:        f.showName(showID),
			})
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("DELETE /api/collections/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := atoi(r.PathValue("id"))
		col, ok := f.collections[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			MediaItemIDs []int `json:"mediaItemIds"`
		}
		if err := readJSON(r, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.ItemsRemoved[id] = append(f.ItemsRemoved[id], body.MediaItemIDs...)
		gone := map[int]bool{}
		for _, mediaItemID := range body.MediaItemIDs {
			gone[mediaItemID] = true
		}
		var kept []etv.CollectionItem
		for _, item := range col.Items {
			if !gone[item.MediaItemID] {
				kept = append(kept, item)
			}
		}
		col.Items = kept
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /api/media/shows", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, f.shows)
	})

	mux.HandleFunc("GET /api/schedules", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []etv.Schedule{}
		for name := range f.schedules {
			out = append(out, etv.Schedule{Name: name, Path: schedulePath(name)})
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /api/schedules/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		content, ok := f.schedules[r.PathValue("name")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(content))
	})

	mux.HandleFunc("PUT /api/schedules/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		raw, _ := io.ReadAll(r.Body)
		f.schedules[name] = string(raw)
		f.SchedulePuts = append(f.SchedulePuts, name)
		writeJSON(w, etv.Schedule{Name: name, Path: schedulePath(name)})
	})

	mux.HandleFunc("DELETE /api/schedules/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.Deletes = append(f.Deletes, "schedule:"+r.PathValue("name"))
		delete(f.schedules, r.PathValue("name"))
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /api/playouts", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []etv.Playout{}
		for _, p := range f.playouts {
			ch := f.channels[p.ChannelID]
			if ch == nil {
				continue
			}
			out = append(out, etv.Playout{
				ID:            p.ID,
				ScheduleKind:  p.Kind,
				ChannelNumber: ch.Number,
				ChannelName:   ch.Name,
				ScheduleFile:  p.ScheduleFile,
			})
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("POST /api/playouts/sequential", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			ChannelID    int    `json:"channelId"`
			ScheduleName string `json:"scheduleName"`
		}
		if err := readJSON(r, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p := &fakePlayout{
			ID:           f.nextPlayoutID,
			Kind:         "Sequential",
			ChannelID:    body.ChannelID,
			ScheduleFile: schedulePath(body.ScheduleName),
		}
		f.nextPlayoutID++
		f.playouts[p.ID] = p
		writeJSON(w, map[string]any{"playoutId": p.ID})
	})

	mux.HandleFunc("POST /api/playouts/{id}/build", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.Builds = append(f.Builds, atoi(r.PathValue("id")))
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("DELETE /api/playouts/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := atoi(r.PathValue("id"))
		f.Deletes = append(f.Deletes, "playout:"+strconv.Itoa(id))
		delete(f.playouts, id)
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

// applyChannelFields is the fake's merge: present keys win, absent keys are left alone.
func applyChannelFields(ch *fakeChannel, body map[string]any) {
	for k, v := range body {
		switch k {
		case "name":
			ch.Name = v.(string)
		case "number":
			ch.Number = v.(string)
		case "group":
			ch.Group = v.(string)
		case "categories":
			ch.Categories = v.(string)
		case "ffmpegProfileId":
			ch.FFmpegProfileID = int(v.(float64))
		case "slugSeconds":
			s := v.(float64)
			ch.SlugSeconds = &s
		case "streamSelectorMode":
			ch.StreamSelectorMode = v.(string)
		case "streamSelector":
			ch.StreamSelector = v.(string)
		case "streamingMode":
			ch.StreamingMode = v.(string)
		case "streamingEngine":
			ch.StreamingEngine = v.(string)
		case "nextEngineTextSubtitleMode":
			ch.NextEngineTextSubtitleMode = v.(string)
		case "transcodeMode":
			ch.TranscodeMode = v.(string)
		case "idleBehavior":
			ch.IdleBehavior = v.(string)
		case "playoutSource":
			ch.PlayoutSource = v.(string)
		case "playoutMode":
			ch.PlayoutMode = v.(string)
		case "playoutOffset":
			s := v.(string)
			ch.PlayoutOffset = &s
		case "mirrorSourceChannelId":
			n := int(v.(float64))
			ch.MirrorSourceChannelID = &n
		case "preferredAudioLanguageCode":
			s := v.(string)
			ch.PreferredAudioLanguage = &s
		case "preferredAudioTitle":
			ch.PreferredAudioTitle = v.(string)
		case "preferredSubtitleLanguageCode":
			s := v.(string)
			ch.PreferredSubtitleLanguage = &s
		case "subtitleMode":
			ch.SubtitleMode = v.(string)
		case "musicVideoCreditsMode":
			ch.MusicVideoCreditsMode = v.(string)
		case "musicVideoCreditsTemplate":
			ch.MusicVideoCreditsTemplate = v.(string)
		case "songVideoMode":
			ch.SongVideoMode = v.(string)
		case "watermarkId":
			n := int(v.(float64))
			ch.WatermarkID = &n
		case "fallbackFillerId":
			n := int(v.(float64))
			ch.FallbackFillerID = &n
		case "isEnabled":
			ch.IsEnabled = v.(bool)
		case "showInEpg":
			ch.ShowInEpg = v.(bool)
		case "logoPath":
			if v.(string) == "" {
				ch.LogoHash = ""
			}
		}
	}
}

func (f *fakeETV) sortedChannels() []*fakeChannel {
	out := make([]*fakeChannel, 0, len(f.channels))
	for i := 1; i < f.nextChannelID; i++ {
		if ch, ok := f.channels[i]; ok {
			out = append(out, ch)
		}
	}
	return out
}

func (f *fakeETV) profileName(id int) string {
	for _, p := range f.profiles {
		if p.ID == id {
			return p.Name
		}
	}
	return ""
}

func (f *fakeETV) showName(id int) string {
	for _, s := range f.shows {
		if s.MediaItemID == id {
			return s.Name
		}
	}
	return fmt.Sprintf("#%d", id)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

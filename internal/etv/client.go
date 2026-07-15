// Package etv is a client for the ErsatzTV management API.
//
// Everything here is plain HTTP against a base URL. There is deliberately no SSH, no scp, no
// volume mount and no filesystem access to the server: that is what lets the same binary drive a
// Windows desktop, a Docker container or a cluster deployment without knowing which it is.
package etv

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

type Channel struct {
	ID            int    `json:"id"`
	Number        string `json:"number"`
	Name          string `json:"name"`
	FFmpegProfile string `json:"ffmpegProfile"`
	StreamingMode string `json:"streamingMode"`
	// LogoPath is the uppercase MD5 of the image content, so a client can hash its local file and
	// tell whether the channel already has that exact logo.
	LogoPath string `json:"logoPath"`
}

// ChannelDetail is GET /api/channels/{id}, which returns every updatable field. The list endpoint
// does not: it reports ffmpegProfile and streamingMode as display strings ("MPEG-TS"), which cannot
// be fed back in. Anything nullable is omitted from the response entirely, hence the pointers.
type ChannelDetail struct {
	ID                         int      `json:"id"`
	Number                     string   `json:"number"`
	Name                       string   `json:"name"`
	Group                      string   `json:"group"`
	Categories                 string   `json:"categories"`
	FFmpegProfileID            int      `json:"ffmpegProfileId"`
	SlugSeconds                *float64 `json:"slugSeconds"`
	StreamSelectorMode         string   `json:"streamSelectorMode"`
	StreamSelector             string   `json:"streamSelector"`
	StreamingMode              string   `json:"streamingMode"`
	StreamingEngine            string   `json:"streamingEngine"`
	NextEngineTextSubtitleMode string   `json:"nextEngineTextSubtitleMode"`
	TranscodeMode              string   `json:"transcodeMode"`
	IdleBehavior               string   `json:"idleBehavior"`
	PlayoutSource              string   `json:"playoutSource"`
	PlayoutMode                string   `json:"playoutMode"`
	MirrorSourceChannelID      *int     `json:"mirrorSourceChannelId"`
	PlayoutOffset              *string  `json:"playoutOffset"`
	PreferredAudioLanguage     *string  `json:"preferredAudioLanguageCode"`
	PreferredAudioTitle        string   `json:"preferredAudioTitle"`
	PreferredSubtitleLanguage  *string  `json:"preferredSubtitleLanguageCode"`
	SubtitleMode               string   `json:"subtitleMode"`
	MusicVideoCreditsMode      string   `json:"musicVideoCreditsMode"`
	MusicVideoCreditsTemplate  string   `json:"musicVideoCreditsTemplate"`
	SongVideoMode              string   `json:"songVideoMode"`
	WatermarkID                *int     `json:"watermarkId"`
	FallbackFillerID           *int     `json:"fallbackFillerId"`
	IsEnabled                  bool     `json:"isEnabled"`
	ShowInEpg                  bool     `json:"showInEpg"`
}

type Collection struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// NamedRef is the id-and-name shape of GET /api/watermarks and GET /api/fillers. A channel stores a
// watermark or filler by numeric id, which is not portable across a rebuilt server, so the manifest
// references them by name and resolves the name to an id through these lists.
type NamedRef struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// CollectionItem is one member of a collection. A show is reported as a show, not expanded into its
// episodes, so MediaItemID is the id that add and remove operate on.
type CollectionItem struct {
	MediaItemID int    `json:"mediaItemId"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
}

type FFmpegProfile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Playout struct {
	ID               int    `json:"id"`
	ScheduleKind     string `json:"scheduleKind"`
	ChannelNumber    string `json:"channelNumber"`
	ChannelName      string `json:"channelName"`
	ScheduleFile     string `json:"scheduleFile"`
	LastBuildSuccess *bool  `json:"lastBuildSuccess"`
}

type Schedule struct {
	Name            string    `json:"name"`
	Path            string    `json:"path"`
	SizeBytes       int64     `json:"sizeBytes"`
	LastModifiedUTC time.Time `json:"lastModifiedUtc"`
}

type Show struct {
	MediaItemID int    `json:"mediaItemId"`
	Name        string `json:"name"`
}

// APIError carries the server's status and body. ErsatzTV returns ProblemDetails and
// ValidationProblemDetails, and the body is where the actual reason lives, so it must not be
// swallowed: a schema violation on PUT /api/schedules is reported there and nowhere else.
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 500 {
		body = body[:500] + "..."
	}
	if body == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, body)
}

func (c *Client) do(method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if c.APIKey != "" {
		req.Header.Set("X-Api-Key", c.APIKey)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Method: method, Path: path, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) postJSON(path string, in, out any) error {
	buf, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(http.MethodPost, path, "application/json", bytes.NewReader(buf), out)
}

func (c *Client) Version() (map[string]any, error) {
	var v map[string]any
	err := c.do(http.MethodGet, "/api/version", "", nil, &v)
	return v, err
}

func (c *Client) Channels() ([]Channel, error) {
	var v []Channel
	err := c.do(http.MethodGet, "/api/channels", "", nil, &v)
	return v, err
}

func (c *Client) Channel(id int) (ChannelDetail, error) {
	var v ChannelDetail
	err := c.do(http.MethodGet, fmt.Sprintf("/api/channels/%d", id), "", nil, &v)
	return v, err
}

func (c *Client) CreateChannel(fields map[string]any) (int, error) {
	var v struct {
		ChannelID int `json:"channelId"`
	}
	err := c.postJSON("/api/channels", fields, &v)
	return v.ChannelID, err
}

// UpdateChannel sends only the fields it is given. The server leaves anything omitted alone, so a
// partial update cannot reset streaming mode, transcode mode, the watermark or the logo the way a
// full replacement would.
func (c *Client) UpdateChannel(id int, fields map[string]any) error {
	buf, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return c.do(http.MethodPut, fmt.Sprintf("/api/channels/%d", id), "application/json", bytes.NewReader(buf), nil)
}

func (c *Client) FFmpegProfiles() ([]FFmpegProfile, error) {
	var v []FFmpegProfile
	err := c.do(http.MethodGet, "/api/ffmpeg/profiles", "", nil, &v)
	return v, err
}

func (c *Client) Watermarks() ([]NamedRef, error) {
	var v []NamedRef
	err := c.do(http.MethodGet, "/api/watermarks", "", nil, &v)
	return v, err
}

func (c *Client) Fillers() ([]NamedRef, error) {
	var v []NamedRef
	err := c.do(http.MethodGet, "/api/fillers", "", nil, &v)
	return v, err
}

func (c *Client) Collections() ([]Collection, error) {
	var v []Collection
	err := c.do(http.MethodGet, "/api/collections", "", nil, &v)
	return v, err
}

// CollectionItems is a collection's membership. Without it a client can add items but can never tell
// whether it needs to, which is what made reconciling a collection impossible.
func (c *Client) CollectionItems(id int) ([]CollectionItem, error) {
	var v []CollectionItem
	err := c.do(http.MethodGet, fmt.Sprintf("/api/collections/%d/items", id), "", nil, &v)
	return v, err
}

func (c *Client) RemoveShowsFromCollection(id int, mediaItemIDs []int) error {
	buf, err := json.Marshal(map[string][]int{"mediaItemIds": mediaItemIDs})
	if err != nil {
		return err
	}
	return c.do(
		http.MethodDelete,
		fmt.Sprintf("/api/collections/%d/items", id),
		"application/json",
		bytes.NewReader(buf),
		nil,
	)
}

func (c *Client) CreateCollection(name string) (Collection, error) {
	var v Collection
	err := c.postJSON("/api/collections", map[string]string{"name": name}, &v)
	return v, err
}

func (c *Client) AddShowsToCollection(id int, showIDs []int) error {
	return c.postJSON(fmt.Sprintf("/api/collections/%d/items", id), map[string][]int{"showIds": showIDs}, nil)
}

func (c *Client) Shows() ([]Show, error) {
	var v []Show
	err := c.do(http.MethodGet, "/api/media/shows", "", nil, &v)
	return v, err
}

func (c *Client) Playouts() ([]Playout, error) {
	var v []Playout
	err := c.do(http.MethodGet, "/api/playouts", "", nil, &v)
	return v, err
}

func (c *Client) DeletePlayout(id int) error {
	return c.do(http.MethodDelete, fmt.Sprintf("/api/playouts/%d", id), "", nil, nil)
}

// CreateSequentialPlayout references the schedule by name. The server resolves it inside its own
// schedules folder, so the client never learns or cares where that is.
func (c *Client) CreateSequentialPlayout(channelID int, scheduleName string) (int, error) {
	var v struct {
		PlayoutID int `json:"playoutId"`
	}
	err := c.postJSON("/api/playouts/sequential", map[string]any{
		"channelId":    channelID,
		"scheduleName": scheduleName,
	}, &v)
	return v.PlayoutID, err
}

func (c *Client) BuildPlayout(id int, mode string) error {
	return c.do(http.MethodPost, fmt.Sprintf("/api/playouts/%d/build?mode=%s", id, mode), "", nil, nil)
}

func (c *Client) Schedules() ([]Schedule, error) {
	var v []Schedule
	err := c.do(http.MethodGet, "/api/schedules", "", nil, &v)
	return v, err
}

func (c *Client) GetSchedule(name string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/api/schedules/"+url.PathEscape(name), nil)
	if err != nil {
		return "", err
	}
	if c.APIKey != "" {
		req.Header.Set("X-Api-Key", c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &APIError{Status: resp.StatusCode, Method: "GET", Path: "/api/schedules/" + name, Body: string(raw)}
	}
	return string(raw), nil
}

// PutSchedule uploads schedule content. The server validates it against the same schema the playout
// builder uses, so a bad schedule is rejected here rather than failing later at build time.
func (c *Client) PutSchedule(name, yaml string) (Schedule, error) {
	var v Schedule
	err := c.do(
		http.MethodPut,
		"/api/schedules/"+url.PathEscape(name),
		"text/plain",
		strings.NewReader(yaml),
		&v,
	)
	return v, err
}

func (c *Client) DeleteSchedule(name string) error {
	return c.do(http.MethodDelete, "/api/schedules/"+url.PathEscape(name), "", nil, nil)
}

// GetLogo downloads a channel's logo by its path (the uppercase MD5 the API reports as logoPath).
// Logos are served from /iptv/logos rather than under /api, and the response Content-Type is what
// names the file on export.
func (c *Client) GetLogo(logoPath string) (image []byte, contentType string, err error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/iptv/logos/"+url.PathEscape(logoPath), nil)
	if err != nil {
		return nil, "", err
	}
	if c.APIKey != "" {
		req.Header.Set("X-Api-Key", c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", &APIError{Status: resp.StatusCode, Method: "GET", Path: "/iptv/logos/" + logoPath, Body: string(raw)}
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

// SetChannelLogo uploads an image and sets it as the channel's logo, and nothing else.
//
// Still worth having now that PUT /api/channels merges instead of replacing: this takes the image
// bytes directly, so the caller never has to upload the artwork separately and then reference it.
func (c *Client) SetChannelLogo(channelID int, contentType string, image []byte) error {
	return c.do(
		http.MethodPut,
		fmt.Sprintf("/api/channels/%d/logo", channelID),
		contentType,
		bytes.NewReader(image),
		nil,
	)
}

// LogoContentType maps a file extension to the content types the server accepts.
func LogoContentType(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	default:
		return "", false
	}
}

// LogoHash is how the server names artwork: the uppercase MD5 of the content.
func LogoHash(image []byte) string {
	sum := md5.Sum(image)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// LogoExtension picks a file extension for a downloaded logo from its content type, so export writes
// logos/<channel>.png rather than an extensionless hash. It is the inverse of LogoContentType.
func LogoExtension(contentType string) string {
	switch {
	case strings.Contains(contentType, "png"):
		return ".png"
	case strings.Contains(contentType, "jpeg"), strings.Contains(contentType, "jpg"):
		return ".jpg"
	case strings.Contains(contentType, "gif"):
		return ".gif"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	default:
		return ".png"
	}
}

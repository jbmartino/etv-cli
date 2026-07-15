package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "schedules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schedules", "g4.seq.yaml"), []byte("content: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "g4.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "etv.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

// The three logo states are the whole contract: absent is unmanaged, empty means remove it, a path
// means set it. Collapsing absent and empty is what made deleting a logo line a silent no-op.
func TestLogoHasThreeStates(t *testing.T) {
	path, dir := write(t, `
channels:
  - { name: A, schedule: g4 }
  - { name: B, schedule: g4, logo: "" }
  - { name: C, schedule: g4, logo: g4.png }
`)

	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, managed := m.LogoFile(m.Channels[0]); managed {
		t.Error("an omitted logo should be unmanaged")
	}

	logo, managed := m.LogoFile(m.Channels[1])
	if !managed || logo != "" {
		t.Errorf(`an empty logo should be managed and empty, got managed=%v path=%q`, managed, logo)
	}

	logo, managed = m.LogoFile(m.Channels[2])
	if !managed || logo != filepath.Join(dir, "g4.png") {
		t.Errorf("a logo path should resolve relative to the manifest, got managed=%v path=%q", managed, logo)
	}
}

func TestRejectsDuplicateChannelNumbers(t *testing.T) {
	path, _ := write(t, `
channels:
  - { name: A, schedule: g4, number: "5" }
  - { name: B, schedule: g4, number: "5" }
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("want an error when two channels claim the same number")
	}
	if !strings.Contains(err.Error(), "number 5") {
		t.Errorf("error should name the number, got: %v", err)
	}
}

func TestRejectsDuplicateChannelNames(t *testing.T) {
	path, _ := write(t, `
channels:
  - { name: A, schedule: g4 }
  - { name: A, schedule: g4 }
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want an error when a channel is listed twice")
	}
}

func TestRejectsAMissingSchedule(t *testing.T) {
	path, _ := write(t, `
channels:
  - { name: A, schedule: nope }
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want an error when a channel references a schedule that is not on disk")
	}
}

func TestRejectsAMissingLogo(t *testing.T) {
	path, _ := write(t, `
channels:
  - { name: A, schedule: g4, logo: missing.png }
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want an error when a channel references a logo that is not on disk")
	}
}

// Unset optional settings must stay unset, because that is what apply reads as "leave it alone".
func TestOptionalChannelSettingsDefaultToUnset(t *testing.T) {
	path, _ := write(t, `
channels:
  - { name: A, schedule: g4 }
`)

	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	ch := m.Channels[0]
	if ch.Number != nil || ch.Group != nil || ch.FFmpegProfile != nil || ch.StreamingMode != nil ||
		ch.TranscodeMode != nil || ch.IdleBehavior != nil || ch.PreferredAudioLanguage != nil ||
		ch.Enabled != nil || ch.ShowInEpg != nil {
		t.Errorf("optional settings should be nil when omitted, got %+v", ch)
	}
}

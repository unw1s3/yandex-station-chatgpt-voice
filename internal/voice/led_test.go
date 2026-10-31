package voice

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func makeStationLEDFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, color := range []string{"red", "green", "blue"} {
		for zone := 1; zone <= 6; zone++ {
			name := filepath.Join(root, "mini2:"+color+":ring"+strconv.Itoa(zone))
			if err := os.MkdirAll(name, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(name, "brightness"), []byte("7\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"mini2:white:clockPLUS1", "mini2:white:clockPLUS2"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "brightness"), []byte("112\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readStationLEDValue(t *testing.T, root, color string, zone int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "mini2:"+color+":ring"+strconv.Itoa(zone), "brightness"))
	if err != nil {
		t.Fatal(err)
	}
	return stringTrimSpace(data)
}

func writeStationClockValue(t *testing.T, root, name, value string) {
	t.Helper()
	path := filepath.Join(root, name, "brightness")
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStationLEDColorsAndRestore(t *testing.T) {
	root := makeStationLEDFixture(t)
	led := newStationLED(root)
	if err := led.activate(); err != nil {
		t.Fatal(err)
	}
	const syncedBrightness = 168 // 112/170 of the clock range, scaled to 0..255.
	for zone := 1; zone <= 6; zone++ {
		if got := readStationLEDValue(t, root, "red", zone); got != strconv.Itoa(syncedBrightness) {
			t.Fatalf("connecting red zone %d = %s", zone, got)
		}
		if got := readStationLEDValue(t, root, "green", zone); got != strconv.Itoa(syncedBrightness) {
			t.Fatalf("connecting green zone %d = %s", zone, got)
		}
		if got := readStationLEDValue(t, root, "blue", zone); got != "0" {
			t.Fatalf("connecting blue zone %d = %s", zone, got)
		}
	}
	if err := led.set(stationLEDWaiting); err != nil {
		t.Fatal(err)
	}
	if got := readStationLEDValue(t, root, "blue", 3); got != strconv.Itoa(syncedBrightness) {
		t.Fatalf("waiting blue = %s", got)
	}
	if got := readStationLEDValue(t, root, "red", 3); got != "0" {
		t.Fatalf("waiting red = %s", got)
	}
	if err := led.set(stationLEDSpeaking); err != nil {
		t.Fatal(err)
	}
	if got := readStationLEDValue(t, root, "green", 2); got != strconv.Itoa(syncedBrightness) {
		t.Fatalf("speaking green = %s", got)
	}
	if got := readStationLEDValue(t, root, "blue", 2); got != "0" {
		t.Fatalf("speaking blue = %s", got)
	}
	if err := led.set(stationLEDError); err != nil {
		t.Fatal(err)
	}
	if got := readStationLEDValue(t, root, "red", 5); got != strconv.Itoa(syncedBrightness) {
		t.Fatalf("error red = %s", got)
	}
	if err := led.restore(); err != nil {
		t.Fatal(err)
	}
	for _, color := range []string{"red", "green", "blue"} {
		for zone := 1; zone <= 6; zone++ {
			if got := readStationLEDValue(t, root, color, zone); got != "7" {
				t.Fatalf("restored %s zone %d = %s, want 7", color, zone, got)
			}
		}
	}
}

func TestStationLEDFollowsClockBrightness(t *testing.T) {
	root := makeStationLEDFixture(t)
	led := newStationLED(root)
	if err := led.activate(); err != nil {
		t.Fatal(err)
	}

	writeStationClockValue(t, root, "mini2:white:clockPLUS1", "34\n")
	writeStationClockValue(t, root, "mini2:white:clockPLUS2", "34\n")
	if err := led.refreshClockBrightness(); err != nil {
		t.Fatal(err)
	}
	const expected = 51 // 34/170 of the clock range, scaled to 0..255.
	if got := readStationLEDValue(t, root, "red", 1); got != strconv.Itoa(expected) {
		t.Fatalf("updated connecting red = %s, want %d", got, expected)
	}
	if got := readStationLEDValue(t, root, "green", 1); got != strconv.Itoa(expected) {
		t.Fatalf("updated connecting green = %s, want %d", got, expected)
	}

	writeStationClockValue(t, root, "mini2:white:clockPLUS1", "0\n")
	writeStationClockValue(t, root, "mini2:white:clockPLUS2", "0\n")
	if err := led.refreshClockBrightness(); err != nil {
		t.Fatal(err)
	}
	if got := readStationLEDValue(t, root, "red", 1); got != "0" {
		t.Fatalf("zero-brightness red = %s, want 0", got)
	}
	if err := led.restore(); err != nil {
		t.Fatal(err)
	}
}

func TestStationLEDUnavailableDoesNotActivate(t *testing.T) {
	led := newStationLED(t.TempDir())
	if err := led.activate(); err == nil {
		t.Fatal("activation succeeded without clock or sysfs LEDs")
	}
	if err := led.set(stationLEDWaiting); err != nil {
		t.Fatalf("inactive LED update should be a no-op: %v", err)
	}
	if err := led.restore(); err != nil {
		t.Fatalf("inactive LED restore should be a no-op: %v", err)
	}
}

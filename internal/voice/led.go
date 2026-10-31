package voice

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const stationLEDBrightness = 48

type stationLEDColor uint8

const (
	stationLEDConnecting stationLEDColor = iota
	stationLEDWaiting
	stationLEDSpeaking
	stationLEDError
)

type stationLED struct {
	mu      sync.Mutex
	root    string
	active  bool
	state   stationLEDColor
	hasState bool
	original map[string]int
}

func newStationLED(root string) *stationLED {
	return &stationLED{root: root}
}

func (l *stationLED) activate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active {
		return nil
	}
	original := make(map[string]int, 18)
	for _, color := range []string{"red", "green", "blue"} {
		for zone := 1; zone <= 6; zone++ {
			name := fmt.Sprintf("mini2:%s:ring%d", color, zone)
			data, err := os.ReadFile(filepath.Join(l.root, name, "brightness"))
			if err != nil {
				return fmt.Errorf("read Station LED %s: %w", name, err)
			}
			value, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				return fmt.Errorf("parse Station LED %s brightness: %w", name, err)
			}
			original[name] = value
		}
	}
	l.original = original
	l.active = true
	if err := l.applyLocked(stationLEDConnecting); err != nil {
		_ = l.restoreLocked()
		return err
	}
	return nil
}

func (l *stationLED) set(color stationLEDColor) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active || (l.hasState && l.state == color) {
		return nil
	}
	return l.applyLocked(color)
}

func (l *stationLED) applyLocked(color stationLEDColor) error {
	var red, green, blue int
	switch color {
	case stationLEDConnecting:
		red, green = stationLEDBrightness, stationLEDBrightness
	case stationLEDWaiting:
		red, blue = 0, stationLEDBrightness
	case stationLEDSpeaking:
		green = stationLEDBrightness
	case stationLEDError:
		red = stationLEDBrightness
	default:
		return fmt.Errorf("unknown Station LED color %d", color)
	}
	for zone := 1; zone <= 6; zone++ {
		for _, channel := range []struct {
			name  string
			value int
		}{{"red", red}, {"green", green}, {"blue", blue}} {
			name := fmt.Sprintf("mini2:%s:ring%d", channel.name, zone)
			if err := writeStationBrightness(filepath.Join(l.root, name, "brightness"), channel.value); err != nil {
				return fmt.Errorf("write Station LED %s: %w", name, err)
			}
		}
	}
	l.state = color
	l.hasState = true
	return nil
}

func (l *stationLED) restore() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.restoreLocked()
}

func (l *stationLED) restoreLocked() error {
	if !l.active {
		return nil
	}
	var result error
	for name, value := range l.original {
		if err := writeStationBrightness(filepath.Join(l.root, name, "brightness"), value); err != nil {
			result = fmt.Errorf("restore Station LED %s: %w", name, err)
		}
	}
	l.active = false
	l.hasState = false
	l.original = nil
	return result
}

func writeStationBrightness(path string, value int) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := file.Write([]byte(strconv.Itoa(value)))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

package session

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// Computer controls drive the session's X display with xdotool, so they work
// the same for every browser. Arguments are passed as an argv array (never
// through a shell) and validated first.

const maxCoord = 20000

var (
	keySpec = regexp.MustCompile(`^[A-Za-z0-9_+\-]{1,64}$`)
	buttons = map[string]string{"": "1", "left": "1", "middle": "2", "right": "3"}
)

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func checkPoint(p Point) error {
	if p.X < 0 || p.Y < 0 || p.X > maxCoord || p.Y > maxCoord {
		return fmt.Errorf("coordinates out of range: (%d, %d)", p.X, p.Y)
	}
	return nil
}

func (m *Manager) xdotool(ctx context.Context, id string, args ...string) error {
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return err
	}
	_, se, code, err := m.exec(ctx, cid, "neko", append([]string{"xdotool"}, args...)...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("xdotool: %s", bytes.TrimSpace(se))
	}
	return nil
}

func a(n int) string { return strconv.Itoa(n) }

func (m *Manager) Move(ctx context.Context, id string, p Point) error {
	if err := checkPoint(p); err != nil {
		return err
	}
	return m.xdotool(ctx, id, "mousemove", a(p.X), a(p.Y))
}

func (m *Manager) Click(ctx context.Context, id string, p Point, button string, count int) error {
	if err := checkPoint(p); err != nil {
		return err
	}
	b, ok := buttons[button]
	if !ok {
		return fmt.Errorf("unknown button %q (left, middle, right)", button)
	}
	if count == 0 {
		count = 1
	}
	if count < 1 || count > 10 {
		return fmt.Errorf("num_clicks must be 1-10")
	}
	return m.xdotool(ctx, id, "mousemove", a(p.X), a(p.Y), "click", "--repeat", a(count), "--delay", "80", b)
}

func (m *Manager) TypeText(ctx context.Context, id, text string, delayMS int) error {
	if text == "" || len(text) > 10000 {
		return fmt.Errorf("text must be 1-10000 characters")
	}
	if delayMS == 0 {
		delayMS = 12
	}
	if delayMS < 0 || delayMS > 1000 {
		return fmt.Errorf("delay_ms must be 0-1000")
	}
	return m.xdotool(ctx, id, "type", "--delay", a(delayMS), "--", text)
}

// PressKeys sends key combinations in xdotool syntax, e.g. "Return", "ctrl+l".
func (m *Manager) PressKeys(ctx context.Context, id string, keys []string) error {
	if len(keys) == 0 || len(keys) > 20 {
		return fmt.Errorf("keys must have 1-20 entries")
	}
	for _, k := range keys {
		if !keySpec.MatchString(k) {
			return fmt.Errorf("invalid key %q", k)
		}
	}
	return m.xdotool(ctx, id, append([]string{"key", "--"}, keys...)...)
}

// Scroll turns wheel notches into xdotool buttons 4/5 (vertical) and 6/7
// (horizontal). Positive deltaY scrolls down, positive deltaX scrolls right.
func (m *Manager) Scroll(ctx context.Context, id string, p Point, deltaX, deltaY int) error {
	if err := checkPoint(p); err != nil {
		return err
	}
	if abs(deltaX) > 100 || abs(deltaY) > 100 {
		return fmt.Errorf("delta_x and delta_y are limited to 100 notches")
	}
	args := []string{"mousemove", a(p.X), a(p.Y)}
	notch := func(n int, neg, pos string) {
		if n == 0 {
			return
		}
		b := pos
		if n < 0 {
			b, n = neg, -n
		}
		args = append(args, "click", "--repeat", a(n), "--delay", "20", b)
	}
	notch(deltaY, "4", "5")
	notch(deltaX, "6", "7")
	return m.xdotool(ctx, id, args...)
}

// Drag presses the button at the first point, moves through the rest, and releases.
func (m *Manager) Drag(ctx context.Context, id string, path []Point, button string) error {
	if len(path) < 2 || len(path) > 200 {
		return fmt.Errorf("path needs 2-200 points")
	}
	b, ok := buttons[button]
	if !ok {
		return fmt.Errorf("unknown button %q (left, middle, right)", button)
	}
	for _, p := range path {
		if err := checkPoint(p); err != nil {
			return err
		}
	}
	args := []string{"mousemove", a(path[0].X), a(path[0].Y), "mousedown", b}
	for _, p := range path[1:] {
		args = append(args, "sleep", "0.03", "mousemove", a(p.X), a(p.Y))
	}
	args = append(args, "sleep", "0.03", "mouseup", b)
	return m.xdotool(ctx, id, args...)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

package webdriver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
)

// The W3C Actions API (https://www.w3.org/TR/webdriver2/#actions): key,
// pointer (mouse, pen, touch) and wheel input, dispatched tick by tick as
// real CDP input events. Input state (held keys and buttons, pointer
// positions) persists across Perform Actions calls until Release Actions.

// inputState is a session's input state table.
type inputState struct {
	sources   map[string]*inputSource
	order     []string // creation order, for release
	nextTouch int
}

type inputSource struct {
	id          string
	kind        string // none, key, pointer, wheel
	pointerType string // mouse, pen, touch
	touchID     int

	// Key sources: keys held, in press order, and their modifier bits.
	keys []heldKey
	mods int

	// Pointer sources.
	x, y    float64
	buttons []int // held, in press order
	props   pointerProps

	// Click counting, so down/up/down/up is a double click.
	clicks     int
	lastDown   time.Time
	lastButton int
	lastX      float64
	lastY      float64
}

type heldKey struct {
	value string
	def   keyDef
}

type pointerProps struct {
	width, height      float64
	pressure           float64
	tangentialPressure float64
	tiltX, tiltY       float64
	twist              int
}

func (st *inputState) get(id string) *inputSource {
	if st.sources == nil {
		return nil
	}
	return st.sources[id]
}

func (st *inputState) add(src *inputSource) {
	if st.sources == nil {
		st.sources = map[string]*inputSource{}
	}
	if src.pointerType == "touch" {
		st.nextTouch++
		src.touchID = st.nextTouch
	}
	st.sources[src.id] = src
	st.order = append(st.order, src.id)
}

// modifiers is the union of the modifier keys held by every key source.
func (st *inputState) modifiers() int {
	m := 0
	for _, s := range st.sources {
		m |= s.mods
	}
	return m
}

// touchPoints lists every touch contact currently down.
func (st *inputState) touchPoints() []cdp.TouchPoint {
	pts := []cdp.TouchPoint{}
	for _, id := range st.order {
		s := st.sources[id]
		if s.pointerType == "touch" && len(s.buttons) > 0 {
			pts = append(pts, cdp.TouchPoint{
				ID: s.touchID, X: s.x, Y: s.y,
				RadiusX: s.props.width / 2, RadiusY: s.props.height / 2,
				Force: s.props.pressure, TangentialPressure: s.props.tangentialPressure,
				TiltX: s.props.tiltX, TiltY: s.props.tiltY, Twist: s.props.twist,
			})
		}
	}
	return pts
}

// Wire format.

type rawSequence struct {
	Type       *string `json:"type"`
	ID         *string `json:"id"`
	Parameters *struct {
		PointerType *string `json:"pointerType"`
	} `json:"parameters"`
	Actions []rawAction `json:"actions"`
}

type rawAction struct {
	Type     string          `json:"type"`
	Duration *float64        `json:"duration"`
	Value    *string         `json:"value"`
	Button   *float64        `json:"button"`
	X        *float64        `json:"x"`
	Y        *float64        `json:"y"`
	DeltaX   *float64        `json:"deltaX"`
	DeltaY   *float64        `json:"deltaY"`
	Origin   json.RawMessage `json:"origin"`

	Width              *float64 `json:"width"`
	Height             *float64 `json:"height"`
	Pressure           *float64 `json:"pressure"`
	TangentialPressure *float64 `json:"tangentialPressure"`
	TiltX              *float64 `json:"tiltX"`
	TiltY              *float64 `json:"tiltY"`
	Twist              *float64 `json:"twist"`
}

// action is one validated action item.
type action struct {
	src      *inputSource
	typ      string
	duration time.Duration

	value string // keyDown, keyUp
	key   keyDef
	bit   int

	button int // pointerDown, pointerUp

	x, y, dx, dy float64 // pointerMove, scroll
	origin       string  // viewport, pointer, element
	element      string
	props        *pointerProps
}

var actionTypes = map[string]map[string]bool{
	"none":    {"pause": true},
	"key":     {"pause": true, "keyDown": true, "keyUp": true},
	"pointer": {"pause": true, "pointerDown": true, "pointerUp": true, "pointerMove": true, "pointerCancel": true},
	"wheel":   {"pause": true, "scroll": true},
}

// parseActions validates a Perform Actions body into per-source action
// lists. It creates input sources in st only once the whole body is valid.
func parseActions(st *inputState, body []byte) ([][]action, error) {
	var req struct {
		Actions *[]rawSequence `json:"actions"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	if req.Actions == nil {
		return nil, wdErr(ErrInvalidArgument, "actions must be an array")
	}
	var (
		out   [][]action
		fresh []*inputSource
		seen  = map[string]bool{}
	)
	for i, seq := range *req.Actions {
		if seq.Type == nil || actionTypes[*seq.Type] == nil {
			return nil, wdErr(ErrInvalidArgument, fmt.Sprintf("actions[%d]: type must be none, key, pointer or wheel", i))
		}
		if seq.ID == nil || *seq.ID == "" {
			return nil, wdErr(ErrInvalidArgument, fmt.Sprintf("actions[%d]: id is required", i))
		}
		kind, id := *seq.Type, *seq.ID
		if seen[id] {
			return nil, wdErr(ErrInvalidArgument, "input source "+id+" appears more than once")
		}
		seen[id] = true
		pointerType := ""
		if kind == "pointer" {
			pointerType = "mouse"
			if seq.Parameters != nil && seq.Parameters.PointerType != nil {
				pointerType = *seq.Parameters.PointerType
			}
			switch pointerType {
			case "mouse", "pen", "touch":
			default:
				return nil, wdErr(ErrInvalidArgument, "pointerType must be mouse, pen or touch")
			}
		}
		src := st.get(id)
		switch {
		case src == nil:
			src = &inputSource{id: id, kind: kind, pointerType: pointerType}
			fresh = append(fresh, src)
		case src.kind != kind:
			return nil, wdErr(ErrInvalidArgument, "input source "+id+" is a "+src.kind+" source, not "+kind)
		case src.pointerType != pointerType:
			return nil, wdErr(ErrInvalidArgument, "input source "+id+" has pointerType "+src.pointerType)
		}
		acts := make([]action, len(seq.Actions))
		for j, ra := range seq.Actions {
			a, err := parseAction(src, ra)
			if err != nil {
				return nil, wdErr(ErrInvalidArgument, fmt.Sprintf("actions[%d].actions[%d]: %s", i, j, toError(err).Message))
			}
			acts[j] = a
		}
		out = append(out, acts)
	}
	for _, src := range fresh {
		st.add(src)
	}
	return out, nil
}

func parseAction(src *inputSource, ra rawAction) (action, error) {
	a := action{src: src, typ: ra.Type}
	if !actionTypes[src.kind][ra.Type] {
		return a, wdErr(ErrInvalidArgument, "action type "+ra.Type+" is not valid for a "+src.kind+" source")
	}
	if ra.Duration != nil {
		if *ra.Duration < 0 || math.IsInf(*ra.Duration, 0) {
			return a, wdErr(ErrInvalidArgument, "duration must be a non-negative integer")
		}
		a.duration = time.Duration(*ra.Duration * float64(time.Millisecond))
	}
	switch ra.Type {
	case "keyDown", "keyUp":
		if ra.Value == nil || *ra.Value == "" {
			return a, wdErr(ErrInvalidArgument, "value must be a single key")
		}
		a.value = *ra.Value
		a.key, a.bit = lookupKey(a.value)
	case "pointerDown", "pointerUp":
		if ra.Button == nil || *ra.Button < 0 || *ra.Button != math.Trunc(*ra.Button) {
			return a, wdErr(ErrInvalidArgument, "button must be a non-negative integer")
		}
		a.button = int(*ra.Button)
		if ra.Type == "pointerDown" {
			p, err := parseProps(ra)
			if err != nil {
				return a, err
			}
			a.props = p
		}
	case "pointerMove", "scroll":
		a.x, a.y = deref(ra.X), deref(ra.Y)
		a.dx, a.dy = deref(ra.DeltaX), deref(ra.DeltaY)
		origin, element, err := parseOrigin(ra.Origin)
		if err != nil {
			return a, err
		}
		if origin == "pointer" && ra.Type == "scroll" {
			return a, wdErr(ErrInvalidArgument, `origin "pointer" is not valid for scroll`)
		}
		a.origin, a.element = origin, element
		if ra.Type == "pointerMove" {
			p, err := parseProps(ra)
			if err != nil {
				return a, err
			}
			a.props = p
		}
	}
	return a, nil
}

func parseOrigin(raw json.RawMessage) (string, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "viewport", "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "viewport" || s == "pointer" {
			return s, "", nil
		}
		return "", "", wdErr(ErrInvalidArgument, `origin must be "viewport", "pointer" or an element`)
	}
	var el map[string]any
	if json.Unmarshal(raw, &el) == nil {
		if ref, ok := el[ElementKey].(string); ok {
			return "element", ref, nil
		}
	}
	return "", "", wdErr(ErrInvalidArgument, `origin must be "viewport", "pointer" or an element`)
}

// parseProps reads the optional pointer properties; nil means none given.
func parseProps(ra rawAction) (*pointerProps, error) {
	if ra.Width == nil && ra.Height == nil && ra.Pressure == nil && ra.TangentialPressure == nil &&
		ra.TiltX == nil && ra.TiltY == nil && ra.Twist == nil {
		return nil, nil
	}
	p := &pointerProps{
		width: deref(ra.Width), height: deref(ra.Height),
		pressure: deref(ra.Pressure), tangentialPressure: deref(ra.TangentialPressure),
		tiltX: deref(ra.TiltX), tiltY: deref(ra.TiltY), twist: int(deref(ra.Twist)),
	}
	switch {
	case p.width < 0, p.height < 0:
		return nil, wdErr(ErrInvalidArgument, "width and height must be non-negative")
	case p.pressure < 0, p.pressure > 1:
		return nil, wdErr(ErrInvalidArgument, "pressure must be between 0 and 1")
	case p.tangentialPressure < -1, p.tangentialPressure > 1:
		return nil, wdErr(ErrInvalidArgument, "tangentialPressure must be between -1 and 1")
	case p.tiltX < -90, p.tiltX > 90, p.tiltY < -90, p.tiltY > 90:
		return nil, wdErr(ErrInvalidArgument, "tiltX and tiltY must be between -90 and 90")
	case p.twist < 0, p.twist > 359:
		return nil, wdErr(ErrInvalidArgument, "twist must be between 0 and 359")
	}
	return p, nil
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func (h *Handler) performActions(ctx context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	seqs, err := parseActions(&ws.input, body)
	if err != nil {
		return nil, err
	}
	p := &performer{ctx: ctx, tab: ws.s.Tab, st: &ws.input}
	ticks := 0
	for _, s := range seqs {
		ticks = max(ticks, len(s))
	}
	for i := 0; i < ticks; i++ {
		var tick []action
		for _, s := range seqs {
			if i < len(s) {
				tick = append(tick, s[i])
			}
		}
		if err := p.tick(tick); err != nil {
			return nil, err
		}
	}
	if p.pressed {
		settle(ctx, ws) // a click or Enter may have started a navigation
	}
	return nil, nil
}

func (h *Handler) releaseActions(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	p := &performer{ctx: ctx, tab: ws.s.Tab, st: &ws.input}
	err := p.releaseAll()
	ws.input = inputState{}
	if err != nil {
		return nil, err
	}
	if p.pressed {
		settle(ctx, ws)
	}
	return nil, nil
}

// performer dispatches actions for one command.
type performer struct {
	ctx context.Context
	tab cdp.Tab
	st  *inputState

	viewW, viewH float64
	haveViewport bool
	pressed      bool // a button or key was released, so a navigation may follow
}

// animation is a pointerMove or scroll spread over its duration.
type animation struct {
	a              action
	fromX, fromY   float64
	toX, toY       float64
	sentDX, sentDY float64
	done           bool
}

const frameInterval = 16 * time.Millisecond

func (p *performer) tick(acts []action) error {
	var dur time.Duration
	var anims []*animation
	for _, a := range acts {
		if a.typ == "pause" || a.typ == "pointerMove" || a.typ == "scroll" {
			dur = max(dur, a.duration)
		}
		switch a.typ {
		case "pause":
		case "keyDown":
			if err := p.keyDown(a); err != nil {
				return err
			}
		case "keyUp":
			if err := p.keyUp(a); err != nil {
				return err
			}
		case "pointerDown":
			if err := p.pointerDown(a); err != nil {
				return err
			}
		case "pointerUp":
			if err := p.pointerUp(a.src, a.button); err != nil {
				return err
			}
		case "pointerCancel":
			if err := p.pointerCancel(a.src); err != nil {
				return err
			}
		case "pointerMove", "scroll":
			x, y, err := p.target(a)
			if err != nil {
				return err
			}
			an := &animation{a: a, fromX: a.src.x, fromY: a.src.y, toX: x, toY: y}
			if a.typ == "pointerMove" && a.props != nil {
				a.src.props = *a.props
			}
			if a.duration == 0 {
				if err := p.step(an, 1); err != nil {
					return err
				}
				continue
			}
			anims = append(anims, an)
		}
	}
	if dur == 0 {
		return nil
	}
	start := time.Now()
	for {
		elapsed := time.Since(start)
		for _, an := range anims {
			if an.done {
				continue
			}
			frac := float64(elapsed) / float64(an.a.duration)
			if err := p.step(an, min(frac, 1)); err != nil {
				return err
			}
		}
		if elapsed >= dur {
			return nil
		}
		wait := min(frameInterval, dur-elapsed)
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-time.After(wait):
		}
	}
}

// step advances an animation to frac (0..1] of its way.
func (p *performer) step(an *animation, frac float64) error {
	final := frac >= 1
	an.done = final
	src := an.a.src
	if an.a.typ == "scroll" {
		dx, dy := an.a.dx*frac, an.a.dy*frac
		if !final {
			dx, dy = math.Trunc(dx), math.Trunc(dy)
		}
		ddx, ddy := dx-an.sentDX, dy-an.sentDY
		if ddx == 0 && ddy == 0 {
			return nil
		}
		an.sentDX, an.sentDY = dx, dy
		return p.tab.Mouse(p.ctx, cdp.MouseEvent{
			Type: "mouseWheel", X: an.toX, Y: an.toY, DeltaX: ddx, DeltaY: ddy,
			Modifiers: p.st.modifiers(), PointerType: "mouse",
		})
	}
	x, y := an.toX, an.toY
	if !final {
		x = math.Floor(an.fromX + (an.toX-an.fromX)*frac)
		y = math.Floor(an.fromY + (an.toY-an.fromY)*frac)
		if x == src.x && y == src.y {
			return nil
		}
	}
	src.x, src.y = x, y
	return p.move(src)
}

// target resolves a pointerMove or scroll destination in viewport
// coordinates and checks it is inside the viewport.
func (p *performer) target(a action) (float64, float64, error) {
	x, y := a.x, a.y
	switch a.origin {
	case "pointer":
		x, y = a.src.x+x, a.src.y+y
	case "element":
		cx, cy, err := p.elementCenter(a.element)
		if err != nil {
			return 0, 0, err
		}
		x, y = cx+x, cy+y
	}
	if err := p.viewport(); err != nil {
		return 0, 0, err
	}
	if x < 0 || y < 0 || x > p.viewW || y > p.viewH {
		return 0, 0, wdErr(ErrMoveTargetOutOfBounds,
			fmt.Sprintf("(%g, %g) is outside the %gx%g viewport", x, y, p.viewW, p.viewH))
	}
	return x, y, nil
}

func (p *performer) viewport() error {
	if p.haveViewport {
		return nil
	}
	raw, err := p.tab.Evaluate(p.ctx, "[window.innerWidth, window.innerHeight]", false)
	if err != nil {
		return err
	}
	var wh [2]float64
	if err := json.Unmarshal(raw, &wh); err != nil {
		return fmt.Errorf("reading viewport size: %w", err)
	}
	p.viewW, p.viewH, p.haveViewport = wh[0], wh[1], true
	return nil
}

// scriptInViewCenter returns the element's in-view centre point,
// scrolling it into view first if that point is off screen.
const scriptInViewCenter = `const e = arguments[0];
const center = () => {
  const r = e.getClientRects()[0];
  if (!r) return null;
  const left = Math.max(0, Math.min(r.left, r.right)), right = Math.min(innerWidth, Math.max(r.left, r.right));
  const top = Math.max(0, Math.min(r.top, r.bottom)), bottom = Math.min(innerHeight, Math.max(r.top, r.bottom));
  if (left > right || top > bottom) return null;
  return {x: Math.floor((left + right) / 2), y: Math.floor((top + bottom) / 2)};
};
let c = center();
if (!c && e.getClientRects().length) {
  e.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
  c = center();
}
return c;`

func (p *performer) elementCenter(ref string) (float64, float64, error) {
	v, err := runScript(p.ctx, p.tab, scriptInViewCenter, []json.RawMessage{elementArg(ref)}, false)
	if err != nil {
		return 0, 0, err
	}
	if v == nil {
		return 0, 0, wdErr(ErrMoveTargetOutOfBounds, "element is not rendered, so it has no point to move to")
	}
	var c struct{ X, Y float64 }
	if err := remarshal(v, &c); err != nil {
		return 0, 0, err
	}
	return c.X, c.Y, nil
}

// Keys.

// shiftedKeys are the US-layout characters Shift produces.
var shiftedKeys = map[string]string{
	"`": "~", "1": "!", "2": "@", "3": "#", "4": "$", "5": "%", "6": "^", "7": "&", "8": "*", "9": "(", "0": ")",
	"-": "_", "=": "+", "[": "{", "]": "}", "\\": "|", ";": ":", "'": "\"", ",": "<", ".": ">", "/": "?",
}

// withShift applies a held Shift to a printable key from the main block.
func withShift(d keyDef, mods int) keyDef {
	if mods&modShift == 0 || d.text == "" || len(d.key) != 1 ||
		(d.code != "" && !strings.HasPrefix(d.code, "Key") && !strings.HasPrefix(d.code, "Digit")) {
		return d
	}
	if c := d.key[0]; c >= 'a' && c <= 'z' {
		d.key = string(c - 'a' + 'A')
	} else if s, ok := shiftedKeys[d.key]; ok {
		d.key = s
	}
	d.text = d.key
	return d
}

func (p *performer) keyDown(a action) error {
	src := a.src
	src.keys = append(src.keys, heldKey{value: a.value, def: a.key})
	if a.bit != 0 {
		src.mods |= a.bit
		d := a.key
		return p.tab.Key(p.ctx, cdp.KeyEvent{Type: "rawKeyDown", Key: d.key, Code: d.code, KeyCode: d.vk, Modifiers: p.st.modifiers()})
	}
	mods := p.st.modifiers()
	return p.tab.Key(p.ctx, keyDownEvent(withShift(a.key, mods), mods))
}

func (p *performer) keyUp(a action) error {
	return p.releaseKey(a.src, a.value)
}

func (p *performer) releaseKey(src *inputSource, value string) error {
	i := -1
	for j, k := range src.keys {
		if k.value == value {
			i = j
		}
	}
	if i < 0 {
		return nil // not pressed: nothing to do
	}
	d := src.keys[i].def
	src.keys = append(src.keys[:i], src.keys[i+1:]...)
	if _, bit := lookupKey(value); bit != 0 {
		if !holds(src, bit) {
			src.mods &^= bit
		}
	} else {
		p.pressed = true
	}
	mods := p.st.modifiers()
	return p.tab.Key(p.ctx, keyUpEvent(withShift(d, mods), mods))
}

// holds reports whether src still holds another key for modifier bit.
func holds(src *inputSource, bit int) bool {
	for _, k := range src.keys {
		if _, b := lookupKey(k.value); b == bit {
			return true
		}
	}
	return false
}

// Pointers.

var buttonNames = []string{"left", "middle", "right", "back", "forward"}
var buttonBits = []int{1, 4, 2, 8, 16}

func buttonName(b int) string {
	if b >= 0 && b < len(buttonNames) {
		return buttonNames[b]
	}
	return "none"
}

func buttonMask(held []int) int {
	m := 0
	for _, b := range held {
		if b >= 0 && b < len(buttonBits) {
			m |= buttonBits[b]
		}
	}
	return m
}

func indexOf(held []int, b int) int {
	for i, h := range held {
		if h == b {
			return i
		}
	}
	return -1
}

// clickSlop and clickInterval bound what counts as a repeated click.
const (
	clickSlop     = 4
	clickInterval = 500 * time.Millisecond
)

func (p *performer) mouseEvent(src *inputSource, typ, button string, clicks int) cdp.MouseEvent {
	ev := cdp.MouseEvent{
		Type: typ, X: src.x, Y: src.y, Button: button, Buttons: buttonMask(src.buttons),
		ClickCount: clicks, Modifiers: p.st.modifiers(), PointerType: src.pointerType,
	}
	if src.pointerType == "pen" || typ != "mouseMoved" {
		ev.Force, ev.TangentialPressure = src.props.pressure, src.props.tangentialPressure
		ev.TiltX, ev.TiltY, ev.Twist = src.props.tiltX, src.props.tiltY, src.props.twist
	}
	return ev
}

func (p *performer) pointerDown(a action) error {
	src := a.src
	if indexOf(src.buttons, a.button) >= 0 {
		return nil // already pressed
	}
	if a.props != nil {
		src.props = *a.props
	}
	src.buttons = append(src.buttons, a.button)
	if src.pointerType == "touch" {
		return p.tab.Touch(p.ctx, cdp.TouchEvent{Type: "touchStart", TouchPoints: p.st.touchPoints(), Modifiers: p.st.modifiers()})
	}
	now := time.Now()
	if a.button == src.lastButton && now.Sub(src.lastDown) <= clickInterval &&
		math.Abs(src.x-src.lastX) <= clickSlop && math.Abs(src.y-src.lastY) <= clickSlop {
		src.clicks++
	} else {
		src.clicks = 1
	}
	src.lastDown, src.lastButton, src.lastX, src.lastY = now, a.button, src.x, src.y
	return p.tab.Mouse(p.ctx, p.mouseEvent(src, "mousePressed", buttonName(a.button), src.clicks))
}

func (p *performer) pointerUp(src *inputSource, button int) error {
	i := indexOf(src.buttons, button)
	if i < 0 {
		return nil // not pressed
	}
	src.buttons = append(src.buttons[:i], src.buttons[i+1:]...)
	p.pressed = true
	if src.pointerType == "touch" {
		return p.touchChanged(src)
	}
	clicks := src.clicks
	if clicks == 0 || button != src.lastButton {
		clicks = 1
	}
	return p.tab.Mouse(p.ctx, p.mouseEvent(src, "mouseReleased", buttonName(button), clicks))
}

// touchChanged reports a lifted finger: the remaining contacts, or the
// end of the touch sequence once none are left.
func (p *performer) touchChanged(src *inputSource) error {
	if len(src.buttons) > 0 {
		return nil // still touching
	}
	pts := p.st.touchPoints()
	if len(pts) == 0 {
		return p.tab.Touch(p.ctx, cdp.TouchEvent{Type: "touchEnd", Modifiers: p.st.modifiers()})
	}
	return p.tab.Touch(p.ctx, cdp.TouchEvent{Type: "touchMove", TouchPoints: pts, Modifiers: p.st.modifiers()})
}

// pointerCancel cancels an active touch. Mouse and pen buttons stay
// held, as with chromedriver.
func (p *performer) pointerCancel(src *inputSource) error {
	if src.pointerType != "touch" || len(src.buttons) == 0 {
		return nil
	}
	for _, s := range p.st.sources {
		if s.pointerType == "touch" {
			s.buttons = nil
		}
	}
	return p.tab.Touch(p.ctx, cdp.TouchEvent{Type: "touchCancel", Modifiers: p.st.modifiers()})
}

func (p *performer) move(src *inputSource) error {
	if src.pointerType == "touch" {
		if len(src.buttons) == 0 {
			return nil // a finger in the air has nothing to move
		}
		return p.tab.Touch(p.ctx, cdp.TouchEvent{Type: "touchMove", TouchPoints: p.st.touchPoints(), Modifiers: p.st.modifiers()})
	}
	button := "none"
	if len(src.buttons) > 0 {
		button = buttonName(src.buttons[0])
	}
	return p.tab.Mouse(p.ctx, p.mouseEvent(src, "mouseMoved", button, 0))
}

// releaseAll undoes every held key and button, newest source first.
func (p *performer) releaseAll() error {
	var first error
	keep := func(err error) {
		if first == nil && err != nil {
			first = err
		}
	}
	for i := len(p.st.order) - 1; i >= 0; i-- {
		src := p.st.sources[p.st.order[i]]
		for len(src.buttons) > 0 {
			keep(p.pointerUp(src, src.buttons[len(src.buttons)-1]))
		}
		for len(src.keys) > 0 {
			keep(p.releaseKey(src, src.keys[len(src.keys)-1].value))
		}
	}
	return first
}

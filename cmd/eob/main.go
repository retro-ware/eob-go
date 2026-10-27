// Command eob runs the Eye of the Beholder reimplementation. It requires the
// original game files (EOB.EXE, *.PAK, FONT*.FNT) from your own copy.
package main

import (
	"flag"
	"image"
	"image/color"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/audio"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/hajimehoshi/ebiten/v2/text" //nolint:staticcheck // v1 text API is enough here
	"github.com/hajimehoshi/ebiten/v2/vector"
	"golang.org/x/image/font/basicfont"

	"github.com/felix/goeob/internal/adlib"
	"github.com/felix/goeob/internal/eob"
	"github.com/felix/goeob/internal/pak"
	"github.com/felix/goeob/internal/pcspk"
)

type platform struct {
	mu     sync.Mutex
	frame  []byte
	events []eob.Event
	mx, my int
	start  time.Time
	done   bool
	help   bool
	paused time.Duration // game time frozen while help is open
	helpAt time.Time
	snd    soundDriver
	mus    *adlib.Player
	cd     *cdMusic // nil without the Sega CD soundtrack

	// the pointer the page shows (browser only): its picture in RGBA and hot spot;
	// virtual is set while the gamepad's stick, not the mouse, places the pointer
	cursor           *image.RGBA
	cursorX, cursorY int
	virtual          bool
}

// HardwareCursor: in the browser the page's cursor is the pointer, unless the gamepad
// moves it (the page's cursor follows only the mouse).
func (p *platform) HardwareCursor() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return hardwareCursor && !p.virtual
}

func (p *platform) ShowCursor(pix []byte, mask []bool, w, h, hotX, hotY int, pal []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pix == nil {
		p.cursor = nil
		return
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i, c := range pix {
		if mask[i] {
			r, g, b := pal[3*int(c)]&63, pal[3*int(c)+1]&63, pal[3*int(c)+2]&63
			copy(img.Pix[4*i:], []byte{r<<2 | r>>4, g<<2 | g>>4, b<<2 | b>>4, 255})
		}
	}
	p.cursor, p.cursorX, p.cursorY = img, hotX, hotY
}

func (p *platform) PlayLevelMusic(level int) {
	if p.cd != nil {
		p.cd.PlayLevelMusic(level)
	}
}

// soundDriver is the AdLib (adlib) or PC speaker (pcspk) effects player.
type soundDriver interface {
	Play(id int)
	PlayAt(id, dist int)
}

func (p *platform) PlayMusic(id int) {
	if p.mus != nil {
		p.mus.Play(id)
	}
}

func (p *platform) PlaySoundAt(id, dist int) {
	if p.snd != nil {
		p.snd.PlayAt(id, dist)
	}
}

func (p *platform) PlaySound(id int) {
	if p.snd != nil {
		p.snd.Play(id)
	}
}

func (p *platform) Present(pix, pal []byte) {
	p.mu.Lock()
	for i, c := range pix {
		r, g, b := pal[3*int(c)]&63, pal[3*int(c)+1]&63, pal[3*int(c)+2]&63
		p.frame[4*i], p.frame[4*i+1], p.frame[4*i+2], p.frame[4*i+3] = r<<2|r>>4, g<<2|g>>4, b<<2|b>>4, 255
	}
	p.mu.Unlock()
}

func (p *platform) PollEvent() (eob.Event, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.events) == 0 {
		return eob.Event{}, false
	}
	ev := p.events[0]
	p.events = p.events[1:]
	return ev, true
}

func (p *platform) Mouse() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mx, p.my
}

func (p *platform) Millis() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := time.Since(p.start) - p.paused
	if p.help {
		d -= time.Since(p.helpAt)
	}
	return uint32(d.Milliseconds())
}
func (p *platform) Sleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

var keyMap = map[ebiten.Key]int{
	ebiten.KeyArrowUp: eob.KeyKP8, ebiten.KeyArrowDown: eob.KeyKP2, ebiten.KeyArrowLeft: eob.KeyKP4, ebiten.KeyArrowRight: eob.KeyKP6,
	ebiten.KeyNumpad8: eob.KeyKP8, ebiten.KeyNumpad2: eob.KeyKP2, ebiten.KeyNumpad4: eob.KeyKP4, ebiten.KeyNumpad6: eob.KeyKP6,
	ebiten.KeyNumpad7: eob.KeyKP7, ebiten.KeyNumpad9: eob.KeyKP9, ebiten.KeyNumpad1: eob.KeyKP1, ebiten.KeyNumpad3: eob.KeyKP3,
	ebiten.KeyW: eob.KeyKP8, ebiten.KeyS: eob.KeyKP2, ebiten.KeyA: eob.KeyKP4, ebiten.KeyD: eob.KeyKP6, ebiten.KeyQ: eob.KeyKP7, ebiten.KeyE: eob.KeyKP9,
	ebiten.KeyEscape: eob.KeyEsc, ebiten.KeyEnter: eob.KeyEnter, ebiten.KeySpace: eob.KeySpace, ebiten.KeyBackspace: eob.KeyBack,
	ebiten.KeyMinus: eob.KeyMinus, ebiten.KeyEqual: eob.KeyEqual, ebiten.KeyNumpadSubtract: eob.KeyKPSub, ebiten.KeyNumpadAdd: eob.KeyKPAdd,
	ebiten.KeyF1: eob.KeyF1, ebiten.KeyF2: eob.KeyF2, ebiten.KeyF3: eob.KeyF3, ebiten.KeyF4: eob.KeyF4, ebiten.KeyF5: eob.KeyF5, ebiten.KeyF6: eob.KeyF6,
	// letter commands, as original key numbers (WASD/QE stay movement)
	ebiten.KeyC: 48, ebiten.KeyI: 24, ebiten.KeyP: 26, ebiten.KeyR: 52, ebiten.KeyG: eob.KeyThrow, ebiten.KeyT: eob.KeyPickup, ebiten.KeyM: eob.KeyMap,
	ebiten.KeyDigit1: 2, ebiten.KeyDigit2: 3, ebiten.KeyDigit3: 4, ebiten.KeyDigit4: 5, ebiten.KeyDigit5: 6,
	ebiten.KeyF: eob.KeyAttack, ebiten.KeyTab: eob.KeyTab, ebiten.KeyComma: 53, ebiten.KeyPeriod: 54,
}

// helpSections is the Ctrl+H overlay (extension; the game clock pauses while
// open): titled groups of key / action rows, laid out in two columns.
var helpSections = [2][]struct {
	title string
	rows  [][2]string
}{{
	{"MOVE", [][2]string{
		{"W S / Up Down", "forward, back"},
		{"A D / Left Right", "step left, right"},
		{"Q E", "turn left, right"},
		{"Space", "turn mode on / off"},
	}},
	{"PARTY", [][2]string{
		{"F1-F6", "character inventory"},
		{"I", "inventory"},
		{"P", "character info"},
		{"C", "camp"},
		{"F", "attack (front row)"},
		{"G", "throw / shoot ranged"},
		{"T", "pick up item"},
		{"1-5  , .", "spells"},
		{"R", "use the wall in front"},
	}},
}, {
	{"GAMEPAD", [][2]string{
		{"Stick", "cursor"},
		{"A / B", "left / right click"},
		{"D-pad", "move, turn"},
		{"LB / RB", "step left, right"},
		{"X", "attack"},
		{"LT / RT", "pick up / throw"},
		{"R3", "use front wall"},
		{"Y", "minimap"},
		{"L3", "turn mode"},
		{"Start", "camp"},
		{"Menus", "D-pad, A ok, B back"},
	}},
	{"WINDOW", [][2]string{
		{"Ctrl+F / F7-F10", "fullscreen"},
		{"Ctrl+H / Esc", "close this help"},
		{"Back (pad)", "help on / off"},
	}},
	{"MAP", [][2]string{
		{"Tab", "minimap on / off"},
		{"M / minimap click", "full map"},
		{"Left click cell", "walk there"},
		{"Right click cell", "note"},
	}},
}}

// extAscii holds the characters the original keyboard driver reports for
// editing keys (read by the line editor S082c_000b).
var extAscii = map[ebiten.Key]rune{ebiten.KeyArrowLeft: 0xb5, ebiten.KeyArrowRight: 0xb3}

type game struct {
	p              *platform
	img            *ebiten.Image
	vx, vy         float64 // cursor, driven by the mouse or the left stick
	lastMX, lastMY int
	outW, outH     int // the window's size in CSS pixels, for the page's cursor
}

// padKeys maps standard-layout gamepad buttons to game keys (extension).
var padKeys = map[ebiten.StandardGamepadButton]int{
	ebiten.StandardGamepadButtonFrontTopLeft: eob.KeyKP4, ebiten.StandardGamepadButtonFrontTopRight: eob.KeyKP6,
	ebiten.StandardGamepadButtonRightLeft: eob.KeyAttack, ebiten.StandardGamepadButtonLeftStick: eob.KeySpace, ebiten.StandardGamepadButtonRightTop: eob.KeyTab,
	ebiten.StandardGamepadButtonCenterRight:     48, // camp
	ebiten.StandardGamepadButtonRightStick:      52, // R: use the wall in front
	ebiten.StandardGamepadButtonFrontBottomLeft: eob.KeyPickup, ebiten.StandardGamepadButtonFrontBottomRight: eob.KeyThrow,
}

// padButtons are reported as pad events at the cursor; the engine makes them
// clicks / movement in the 3D view and confirm / cancel / navigation in menus.
var padButtons = map[ebiten.StandardGamepadButton]int{
	ebiten.StandardGamepadButtonRightBottom: eob.PadA, ebiten.StandardGamepadButtonRightRight: eob.PadB,
	ebiten.StandardGamepadButtonLeftTop: eob.PadUp, ebiten.StandardGamepadButtonLeftBottom: eob.PadDown,
	ebiten.StandardGamepadButtonLeftLeft: eob.PadLeft, ebiten.StandardGamepadButtonLeftRight: eob.PadRight,
}

func (g *game) Update() error {
	p := g.p
	mx, my := ebiten.CursorPosition()
	mx, my = mx/2, my/2 // the screen is laid out at 640x400 for the help text
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return ebiten.Termination
	}
	if mx != g.lastMX || my != g.lastMY { // the real mouse moved: it wins
		g.vx, g.vy, g.lastMX, g.lastMY = float64(mx), float64(my), mx, my
		p.virtual = false
	}
	var pads []ebiten.GamepadID
	pads = ebiten.AppendGamepadIDs(pads)
	padJust := func(b ebiten.StandardGamepadButton) bool {
		for _, id := range pads {
			if ebiten.IsStandardGamepadLayoutAvailable(id) && inpututil.IsStandardGamepadButtonJustPressed(id, b) {
				return true
			}
		}
		return false
	}
	for _, id := range pads {
		if !ebiten.IsStandardGamepadLayoutAvailable(id) {
			continue
		}
		ax := ebiten.StandardGamepadAxisValue(id, ebiten.StandardGamepadAxisLeftStickHorizontal)
		ay := ebiten.StandardGamepadAxisValue(id, ebiten.StandardGamepadAxisLeftStickVertical)
		if ax*ax+ay*ay > 0.04 { // dead zone
			g.vx = min(max(g.vx+ax*4, 0), 319)
			g.vy = min(max(g.vy+ay*4, 0), 199)
			p.virtual = true
		}
	}
	mx, my = int(g.vx), int(g.vy)
	p.mx, p.my = mx, my
	if ebiten.IsKeyPressed(ebiten.KeyControl) && inpututil.IsKeyJustPressed(ebiten.KeyH) || padJust(ebiten.StandardGamepadButtonCenterLeft) {
		if p.help = !p.help; p.help {
			p.helpAt = time.Now()
		} else {
			p.paused += time.Since(p.helpAt)
		}
		return nil
	}
	if p.help && inpututil.IsKeyJustPressed(ebiten.KeyEscape) {
		p.help = false
		p.paused += time.Since(p.helpAt)
		return nil
	}
	if p.help { // swallow input while help is shown
		ebiten.AppendInputChars(nil)
		return nil
	}
	ctrl := ebiten.IsKeyPressed(ebiten.KeyControl) || ebiten.IsKeyPressed(ebiten.KeyMeta)
	if ctrl && inpututil.IsKeyJustPressed(ebiten.KeyF) || inpututil.IsKeyJustPressed(ebiten.KeyF7) ||
		inpututil.IsKeyJustPressed(ebiten.KeyF8) || inpututil.IsKeyJustPressed(ebiten.KeyF9) || inpututil.IsKeyJustPressed(ebiten.KeyF10) {
		ebiten.SetFullscreen(!ebiten.IsFullscreen()) // F1-F6 are game keys
		return nil
	}
	for k, code := range keyMap {
		if inpututil.IsKeyJustPressed(k) && !(ctrl && k == ebiten.KeyF) {
			p.events = append(p.events, eob.Event{Kind: eob.EvKeyDown, Key: code, Ascii: extAscii[k]})
		}
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyDelete) {
		p.events = append(p.events, eob.Event{Kind: eob.EvKeyDown, Ascii: 0x7f})
	}
	for b, code := range padKeys {
		if padJust(b) {
			p.events = append(p.events, eob.Event{Kind: eob.EvKeyDown, Key: code})
		}
	}
	for b, n := range padButtons {
		for _, id := range pads {
			if !ebiten.IsStandardGamepadLayoutAvailable(id) {
				continue
			}
			if inpututil.IsStandardGamepadButtonJustPressed(id, b) {
				p.events = append(p.events, eob.Event{Kind: eob.EvPadDown, Key: n, X: mx, Y: my})
			}
			if inpututil.IsStandardGamepadButtonJustReleased(id, b) {
				p.events = append(p.events, eob.Event{Kind: eob.EvPadUp, Key: n, X: mx, Y: my})
			}
		}
	}
	for _, r := range ebiten.AppendInputChars(nil) {
		p.events = append(p.events, eob.Event{Kind: eob.EvKeyDown, Ascii: r})
	}
	for b, n := range map[ebiten.MouseButton]int{ebiten.MouseButtonLeft: 1, ebiten.MouseButtonRight: 2} {
		if inpututil.IsMouseButtonJustPressed(b) {
			p.events = append(p.events, eob.Event{Kind: eob.EvMouseDown, Button: n, X: mx, Y: my})
		}
		if inpututil.IsMouseButtonJustReleased(b) {
			p.events = append(p.events, eob.Event{Kind: eob.EvMouseUp, Button: n, X: mx, Y: my})
		}
	}
	return nil
}

func (g *game) Draw(screen *ebiten.Image) {
	g.p.mu.Lock()
	g.img.WritePixels(g.p.frame)
	help := g.p.help
	cur, hx, hy := g.p.cursor, g.p.cursorX, g.p.cursorY
	if !hardwareCursor || g.p.virtual {
		cur = nil
	}
	g.p.mu.Unlock()
	showCursor(cur, hx, hy, g.outW, g.outH)
	var op ebiten.DrawImageOptions
	op.GeoM.Scale(2, 2)
	screen.DrawImage(g.img, &op)
	if help {
		drawHelp(screen)
	}
	if cur != nil {
		drawCursor(screen)
	}
}

// drawHelp draws the help overlay at 640x400, so basicfont comes out half the
// size of the 320x200 game pixels.
func drawHelp(screen *ebiten.Image) {
	var (
		title = color.RGBA{255, 210, 90, 255}
		key   = color.RGBA{120, 200, 255, 255}
		desc  = color.RGBA{225, 225, 225, 255}
		rule  = color.RGBA{90, 90, 120, 255}
	)
	vector.DrawFilledRect(screen, 12, 12, 616, 376, color.RGBA{8, 8, 20, 235}, false)
	vector.StrokeRect(screen, 12, 12, 616, 376, 2, rule, false)
	face := basicfont.Face7x13
	text.Draw(screen, "EYE OF THE BEHOLDER - CONTROLS", face, 32, 38, title)
	vector.DrawFilledRect(screen, 32, 46, 576, 1, rule, false)
	for col, secs := range helpSections {
		x, y := 32+col*316, 70
		for _, sec := range secs {
			text.Draw(screen, sec.title, face, x, y, title)
			y += 16
			for _, r := range sec.rows {
				text.Draw(screen, r[0], face, x+8, y, key)
				text.Draw(screen, r[1], face, x+132, y, desc)
				y += 14
			}
			y += 12
		}
	}
}

func (g *game) Layout(w, h int) (int, int) {
	g.outW, g.outH = w, h
	return 640, 400
}

func main() {
	dir := flag.String("data", os.Getenv("EOB_DATA"), "path to your original Eye of the Beholder directory")
	save := flag.String("load", "", "original save file to load directly (default: main menu)")
	sound := flag.Int("sound", 1, "sound driver as EOB.EXE's second argument: 1 AdLib/Sound Blaster, 2 PC speaker, 3 Tandy (3-voice speaker), 4 none")
	imp := flag.String("import", "", "copy the game found under this directory into -data, then exit")
	impCD := flag.String("import-cd", "", "copy the Sega CD soundtrack found under this directory (.cue/.bin) into -data, then exit")
	flag.Parse()
	if *impCD != "" {
		if err := importCD(*impCD, *dir); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *imp != "" {
		if err := importGame(*imp, *dir); err != nil {
			log.Fatal(err)
		}
		return
	}
	if _, err := os.Stat("data"); *dir == "" && err == nil {
		*dir = "data" // goplay: started in games/eob with no arguments
	}
	if *dir == "" {
		log.Fatal("set -data or EOB_DATA to your original Eye of the Beholder directory")
	}
	p := &platform{frame: make([]byte, 64000*4), start: time.Now()}
	if fs, err := pak.Open(*dir); err == nil {
		ctx := audio.NewContext(44100)
		switch *sound {
		case 1:
			if pl := startAdlib(ctx, fs, *dir, "ADLIB.DAT", "EOB.EXE", "sfx"); pl != nil {
				p.snd = pl
			}
		case 2, 3:
			if pl := startSpeaker(ctx, fs, *dir, *sound); pl != nil {
				p.snd = pl
			}
		}
		p.mus = startAdlib(ctx, fs, *dir, "SOUND.DAT", "INTRO.EXE", "music")
		if hasCD(*dir) {
			p.cd = &cdMusic{ctx: ctx, dir: *dir}
		}
	}
	e, err := eob.New(*dir, p)
	if err != nil {
		log.Fatal(err)
	}
	e.NoSound = *sound == 4
	go func() {
		if err := e.Run(*save); err != nil {
			log.Println(err)
		}
		p.mu.Lock()
		p.done = true
		p.mu.Unlock()
	}()
	ebiten.SetWindowSize(960, 600)
	ebiten.SetWindowTitle("Eye of the Beholder")
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	if !hardwareCursor {
		ebiten.SetCursorMode(ebiten.CursorModeHidden) // the game draws its own cursor
	}
	ebiten.SetFullscreen(os.Getenv("GOPLAY_FULLSCREEN") == "1") // goplay starts games fullscreen
	if err := ebiten.RunGame(&game{p: p, img: ebiten.NewImage(320, 200)}); err != nil && err != ebiten.Termination {
		log.Fatal(err)
	}
}

// startSpeaker loads PCSOUND.DAT for the PC speaker driver (sound 2: one
// voice, 3: the Tandy option's three voices) and streams it.
func startSpeaker(ctx *audio.Context, fs *pak.FS, dir string, sound int) *pcspk.Player {
	d, err := fs.Read("PCSOUND.DAT")
	if err != nil {
		return nil
	}
	exe, err := os.ReadFile(filepath.Join(dir, "EOB.EXE"))
	if err != nil {
		return nil
	}
	voices := 1
	if sound == 3 {
		voices = 3
	}
	pl, err := pcspk.New(d, exe, 44100, voices)
	if err != nil {
		log.Printf("pcspk: %v; sound disabled", err)
		return nil
	}
	if ap, err := ctx.NewPlayer(faded{pl, "sfx"}); err == nil {
		ap.SetBufferSize(50 * time.Millisecond)
		ap.Play()
	}
	return pl
}

// startAdlib loads an ADLIB.DAT-format file and streams its player, using
// the driver tables of the executable that plays it, on the desk fader's
// kind (volume.go).
func startAdlib(ctx *audio.Context, fs *pak.FS, dir, name, exeName, kind string) *adlib.Player {
	d, err := fs.Read(name)
	if err != nil {
		return nil
	}
	pl, err := adlib.New(d, 44100)
	if err != nil {
		return nil
	}
	exe, err := os.ReadFile(filepath.Join(dir, exeName))
	if err != nil || !pl.LoadDriver(exe) {
		log.Printf("adlib: driver tables not found in %s; sound disabled", exeName)
		return nil
	}
	if ap, err := ctx.NewPlayer(faded{pl, kind}); err == nil {
		ap.SetBufferSize(50 * time.Millisecond)
		ap.Play()
	}
	return pl
}

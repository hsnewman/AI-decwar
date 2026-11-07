// DECWAR Telnet server -- a Go port of DECWAR 2.2 (TOPS-10 FORTRAN/MACRO).
//
// One process, one authoritative game state, one game goroutine.  Network
// goroutines only decode the Telnet byte stream and hand events to the game
// goroutine, so no locking of game state is needed.
//
//	go run decwar.go [-port 1701] [-dir .] [-computed-all] [-throttle] [-seed N]
//
// Connect with:  telnet host 1701   (or nc host 1701, or a MUD client)
//
// Spec tags used in comments:  (H) = from the help file, (D) = design choice.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	versionStr = "DECWAR Version 2.2, December 25, 1979"
	gridN      = 75
)

// ---------------------------------------------------------------------------
// Appendix A: tunable constants (D)
// ---------------------------------------------------------------------------

type Config struct {
	Listen          string
	Port            int
	MaxConn         int
	IdlePregame     time.Duration
	Stars           int
	Planets         int
	BasesPerTeam    int
	BasesMaxTotal   int
	BlackholeChance float64
	Blackholes      int
	RomulanMaxPlay  int
	RomulanRespawn  time.Duration
	RomulanSight    int
	ShipEnergy      float64
	ShieldEnergy    float64
	ShipDmgLimit    float64
	TorpsMax        int
	LifeReserve     int
	YellowEnergy    float64
	DeviceInop      float64
	WarpK           float64
	MaxWarp         float64
	MaxWarpDamaged  float64
	PhaserDefault   float64
	PhaserMin       float64
	PhaserMax       float64
	PhaserRange     int
	TorpRangeMin    int
	TorpRangeMax    int
	TorpBaseDmg     float64
	BaseRange       int
	PlanetRange     int
	BaseFire        time.Duration
	PlanetFire      time.Duration
	BaseCapacity    float64
	StardatePerSec  float64
	QuitLock        time.Duration
	CtrlCHold       time.Duration
	GhostHold       time.Duration
	EndWait         time.Duration
	ComputedForAll  bool
	SlowBPS         int
	Throttle        bool
	DataDir         string
	CritHits        bool
	MaxLine         int
	GripeMax        int
	MsgMax          int
	AutoRepairEvery time.Duration
	AutoRepairUnits float64
	RomulanEvery    time.Duration
}

var cfg = Config{
	Listen: "0.0.0.0", Port: 1701, MaxConn: 20, IdlePregame: 30 * time.Minute,
	Stars: 250, Planets: 60, BasesPerTeam: 5, BasesMaxTotal: 10,
	BlackholeChance: 0.25, Blackholes: 8,
	RomulanMaxPlay: 3, RomulanRespawn: 90 * time.Second, RomulanSight: 7,
	ShipEnergy: 5000, ShieldEnergy: 2500, ShipDmgLimit: 2500, TorpsMax: 10, LifeReserve: 5,
	YellowEnergy: 1000, DeviceInop: 300, WarpK: 10, MaxWarp: 6, MaxWarpDamaged: 3,
	PhaserDefault: 200, PhaserMin: 50, PhaserMax: 500, PhaserRange: 10,
	TorpRangeMin: 8, TorpRangeMax: 10, TorpBaseDmg: 300,
	BaseRange: 4, PlanetRange: 2, BaseFire: 3 * time.Second, PlanetFire: 4 * time.Second,
	BaseCapacity: 2500, StardatePerSec: 1.0 / 60.0,
	QuitLock: 120 * time.Second, CtrlCHold: 60 * time.Second, GhostHold: 30 * time.Second,
	EndWait: 60 * time.Second, SlowBPS: 1200, DataDir: ".", CritHits: true,
	MaxLine: 255, GripeMax: 4000, MsgMax: 120,
	AutoRepairEvery: 5 * time.Second, AutoRepairUnits: 30, RomulanEvery: 2 * time.Second,
}

// ---------------------------------------------------------------------------
// Basic types
// ---------------------------------------------------------------------------

const (
	NEU  = 0
	FED  = 1
	EMP  = 2
	ROMT = 3 // pseudo-team for the Romulan
)

type Kind uint8

const (
	KEmpty Kind = iota
	KShip
	KBase
	KPlanet
	KStar
	KHole
	KRom
)

type Cell struct {
	K Kind
	I int
}

// Devices (order matches the DAMAGES report)
const (
	DWarp = iota
	DImpulse
	DTorp
	DPhaser
	DShield
	DComputer
	DLife
	DRadio
	DTractor
	NDev
)

var devNames = [NDev]string{"Warp engines", "Impulse engines", "Photon tubes", "Phaser banks",
	"Deflector shields", "Computer", "Life support", "Sub-space radio", "Tractor beam"}
var devKeys = [NDev]string{"WARP", "IMPULSE", "TORPEDOES", "PHASERS", "SHIELDS", "COMPUTER", "LIFE", "RADIO", "TRACTOR"}

var shipNames = [10]string{"Lexington", "Nimitz", "Savannah", "Vulcan", "Yorktown",
	"Cobra", "Demon", "Hawk", "Jackal", "Wolf"}
var shipLetters = [10]byte{'L', 'N', 'S', 'V', 'Y', 'C', 'D', 'H', 'J', 'W'}

func teamOfShip(i int) int {
	if i < 5 {
		return FED
	}
	return EMP
}

type Score struct {
	DmgEnemy, DmgBase, Romulan float64
	Kills, Captured, Built     int
	Stars, Planets             int
	Commissioned               int
}

func (sc *Score) rows() [8]float64 {
	return [8]float64{sc.DmgEnemy / 10, float64(sc.Kills) * 500, sc.DmgBase / 10,
		float64(sc.Captured) * 100, float64(sc.Built) * 1000, sc.Romulan,
		float64(sc.Stars) * -50, float64(sc.Planets) * -100}
}
func (sc *Score) total() float64 {
	t := 0.0
	for _, v := range sc.rows() {
		t += v
	}
	return t
}

type Ship struct {
	idx       int
	team      int
	inGame    bool
	sess      *Session
	captain   string
	v, h      int
	energy    float64
	shieldE   float64
	shieldsUp bool
	damage    float64
	dev       [NDev]float64
	torps     int
	life      int
	red       bool
	docked    bool
	tractor   int // index of tractor peer or -1
	radioOn   bool
	gagged    [10]bool
	phReady   [2]time.Time
	torpReady time.Time
	score     Score
	commAt    time.Time
	ghostEnd  time.Time // non-zero: connection lost under red alert
	lastRep   time.Time
	turns     int
	uid       int
}

type Planet struct {
	v, h   int
	owner  int
	builds int
	alive  bool
	known  [3]bool
}

type Base struct {
	v, h     int
	team     int
	shieldE  float64
	damage   float64
	alive    bool
	known    [3]bool
	pristine bool // no distress call outstanding
}

type Romulan struct {
	alive     bool
	v, h      int
	hp        float64
	reveal    time.Time
	lastTgt   time.Time
	nextFire  time.Time
	phaserNx  bool
	nextSpawn time.Time
}

func (sh *Ship) cond() int { // 0 green 1 yellow 2 red
	if sh.red {
		return 2
	}
	if sh.energy <= cfg.YellowEnergy {
		return 1
	}
	return 0
}
func (sh *Ship) shieldPct() float64  { return sh.shieldE / cfg.ShieldEnergy * 100 }
func (sh *Ship) inop(d int) bool     { return sh.dev[d] >= cfg.DeviceInop }
func (sh *Ship) degraded(d int) bool { return sh.dev[d] > 0 && sh.dev[d] < cfg.DeviceInop }

// ---------------------------------------------------------------------------
// Small utilities
// ---------------------------------------------------------------------------

var rng = rand.New(rand.NewSource(time.Now().UnixNano()))

func ingal(v, h int) bool { return v >= 1 && v <= gridN && h >= 1 && h <= gridN }
func iabs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}
func cheb(v1, h1, v2, h2 int) int { return max(iabs(v1-v2), iabs(h1-h2)) }
func euclid(v1, h1, v2, h2 int) float64 {
	return math.Hypot(float64(v1-v2), float64(h1-h2))
}
func sgn(a int) int {
	if a < 0 {
		return -1
	}
	if a > 0 {
		return 1
	}
	return 0
}
func uni(lo, hi float64) float64 { return lo + (hi-lo)*rng.Float64() }
func clampf(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}
func fmtDur(d time.Duration) string { // H:MM:SS.s
	if d < 0 {
		d = 0
	}
	t := d.Seconds()
	h := int(t) / 3600
	m := (int(t) / 60) % 60
	s := t - float64(h*3600+m*60)
	return fmt.Sprintf("%d:%02d:%04.1f", h, m, s)
}

// ---------------------------------------------------------------------------
// Telnet layer (section 3)
// ---------------------------------------------------------------------------

const (
	tIAC  = 255
	tDONT = 254
	tDO   = 253
	tWONT = 252
	tWILL = 251
	tSB   = 250
	tSE   = 240
	tNOP  = 241
	tAO   = 245
	tIP   = 244

	oECHO  = 1
	oSGA   = 3
	oTTYPE = 24
	oNAWS  = 31
	oTSPD  = 32
)

// event kinds delivered to the game goroutine
const (
	evAccept = iota
	evData
	evSpeed
	evNAWS
	evTType
	evEOF
)

type event struct {
	kind int
	s    *Session
	data []byte
	a, b int
	str  string
	conn net.Conn
}

type telnetState struct {
	us, his map[byte]int // 0 off, 1 requested, 2 on, 3 refused
}

func (s *Session) rawWrite(b []byte) {
	s.wmu.Lock()
	s.conn.Write(b)
	s.wmu.Unlock()
}

func (s *Session) sendNeg(cmd, opt byte) { s.rawWrite([]byte{tIAC, cmd, opt}) }

// initial negotiation (3.2)
func (s *Session) initNeg() {
	s.tel.us = map[byte]int{}
	s.tel.his = map[byte]int{}
	s.tel.us[oECHO] = 1
	s.tel.us[oSGA] = 1
	s.tel.his[oSGA] = 1
	s.tel.his[oTSPD] = 1
	s.tel.his[oNAWS] = 1
	s.tel.his[oTTYPE] = 1
	s.rawWrite([]byte{tIAC, tWILL, oECHO, tIAC, tWILL, oSGA, tIAC, tDO, oSGA,
		tIAC, tDO, oTSPD, tIAC, tDO, oNAWS, tIAC, tDO, oTTYPE})
}

func (s *Session) handleOption(cmd, opt byte) {
	us, his := s.tel.us, s.tel.his
	switch cmd {
	case tDO:
		if opt == oECHO || opt == oSGA {
			switch us[opt] {
			case 1:
				us[opt] = 2
			case 0:
				us[opt] = 2
				s.sendNeg(tWILL, opt)
			}
			return
		}
		if us[opt] != 3 {
			us[opt] = 3
			s.sendNeg(tWONT, opt)
		}
	case tDONT:
		if opt == oECHO || opt == oSGA {
			if us[opt] == 2 {
				s.sendNeg(tWONT, opt)
			}
			us[opt] = 0
		}
	case tWILL:
		switch opt {
		case oSGA, oTSPD, oNAWS, oTTYPE:
			if his[opt] == 2 {
				return
			}
			if his[opt] == 0 {
				s.sendNeg(tDO, opt)
			}
			his[opt] = 2
			if opt == oTSPD {
				s.rawWrite([]byte{tIAC, tSB, oTSPD, 1, tIAC, tSE})
			} else if opt == oTTYPE {
				s.rawWrite([]byte{tIAC, tSB, oTTYPE, 1, tIAC, tSE})
			}
		default:
			if his[opt] != 3 {
				his[opt] = 3
				s.sendNeg(tDONT, opt)
			}
		}
	case tWONT:
		if his[opt] == 2 {
			s.sendNeg(tDONT, opt)
		}
		if his[opt] != 3 {
			his[opt] = 0
		}
	}
}

func (s *Session) handleSB(sb []byte) {
	if len(sb) < 2 {
		return
	}
	switch sb[0] {
	case oTSPD:
		if sb[1] == 0 {
			str := string(sb[2:])
			if i := strings.IndexByte(str, ','); i >= 0 {
				str = str[:i]
			}
			if n, err := strconv.Atoi(strings.TrimSpace(str)); err == nil && n > 0 {
				s.g.ev <- event{kind: evSpeed, s: s, a: n}
			}
		}
	case oNAWS:
		if len(sb) >= 5 {
			w := int(sb[1])<<8 | int(sb[2])
			h := int(sb[3])<<8 | int(sb[4])
			s.g.ev <- event{kind: evNAWS, s: s, a: w, b: h}
		}
	case oTTYPE:
		if sb[1] == 0 {
			s.g.ev <- event{kind: evTType, s: s, str: string(sb[2:])}
		}
	}
}

// readLoop decodes IAC sequences (3.3).  IP is delivered as ^C, AO as ^O.
func (s *Session) readLoop() {
	buf := make([]byte, 2048)
	state := 0
	var cmd byte
	var sb []byte
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			data := make([]byte, 0, n)
			for _, c := range buf[:n] {
				switch state {
				case 0:
					if c == tIAC {
						state = 1
					} else {
						data = append(data, c)
					}
				case 1:
					switch c {
					case tIAC:
						data = append(data, 0xFF)
						state = 0
					case tWILL, tWONT, tDO, tDONT:
						cmd = c
						state = 2
					case tSB:
						sb = sb[:0]
						state = 3
					case tIP:
						data = append(data, 0x03)
						state = 0
					case tAO:
						data = append(data, 0x0F)
						state = 0
					default:
						state = 0 // discard unknown commands
					}
				case 2:
					s.handleOption(cmd, c)
					state = 0
				case 3:
					if c == tIAC {
						state = 4
					} else if len(sb) < 256 {
						sb = append(sb, c)
					}
				case 4:
					if c == tSE {
						s.handleSB(sb)
						state = 0
					} else if c == tIAC {
						sb = append(sb, tIAC)
						state = 3
					} else {
						state = 0
					}
				}
			}
			if len(data) > 0 {
				s.g.ev <- event{kind: evData, s: s, data: data}
			}
		}
		if err != nil {
			s.g.ev <- event{kind: evEOF, s: s}
			return
		}
	}
}

// encode: every newline -> CR LF, literal 0xFF doubled (3.3)
func encodeOut(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/8+4)
	var prev byte
	for _, c := range b {
		switch c {
		case '\n':
			if prev != '\r' {
				out = append(out, '\r')
			}
			out = append(out, '\n')
		case 0xFF:
			out = append(out, 0xFF, 0xFF)
		default:
			out = append(out, c)
		}
		prev = c
	}
	return out
}

func (s *Session) writeLoop() {
	for chunk := range s.wq {
		if cfg.Throttle && s.speed > 0 {
			bps := float64(s.speed) / 10
			time.Sleep(time.Duration(float64(len(chunk)) / bps * float64(time.Second)))
		}
		s.wmu.Lock()
		_, err := s.conn.Write(chunk)
		s.wmu.Unlock()
		s.pending.Add(-int64(len(chunk)))
		if err != nil {
			s.conn.Close()
			for range s.wq {
			}
			break
		}
	}
	s.conn.Close()
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

type Mode int

const (
	mPregame Mode = iota
	mSetName
	mSetTeam
	mSetShip
	mGripe
	mPlay
)

type qcmd struct {
	text string
	line int
}

type Held struct {
	ship   Ship
	expiry time.Time
}

type Settings struct {
	output int // 0 short 1 medium 2 long
	scan   int // 0 short 1 long
	prompt int // 0 normal 1 informative
	ocdef  int // 0 abs 1 rel 2 both
	icdef  int // 0 abs 1 rel
	tty    string
}

type Session struct {
	g       *Game
	id      int
	connNo  int
	conn    net.Conn
	ip      string
	tel     telnetState
	wmu     sync.Mutex
	wq      chan []byte
	pending atomic.Int64
	out     []byte
	closed  bool
	created time.Time
	lastAct time.Time
	cpu     time.Duration
	cpuShip time.Duration

	line     []byte
	echo     bool
	crPend   bool
	mode     Mode
	promptSh bool
	cmdq     []qcmd
	lineSeq  int
	busyTil  time.Time
	lastCmd  string

	ship     *Ship
	held     *Held
	captain  string
	capKey   string
	uid      int
	setTeam  int
	gripe    []string
	gripeLen int
	helpProt bool
	set      Settings

	speed    int
	width    int
	height   int
	ttype    string
	pgPrompt string
}

func (s *Session) w(str string)          { s.out = append(s.out, str...) }
func (s *Session) pf(f string, a ...any) { s.out = append(s.out, fmt.Sprintf(f, a...)...) }
func (s *Session) echoStr(str string) {
	if s.echo {
		s.w(str)
	}
}

func (s *Session) flush() {
	if len(s.out) == 0 || s.closed {
		s.out = s.out[:0]
		return
	}
	enc := encodeOut(s.out)
	s.out = s.out[:0]
	s.pending.Add(int64(len(enc)))
	select {
	case s.wq <- enc:
	default:
		s.pending.Add(-int64(len(enc)))
		s.g.closeSession(s, "output overflow")
	}
}

func (s *Session) bell(n int) { s.w(strings.Repeat("\a", n)) }

func (s *Session) promptText() string {
	switch s.mode {
	case mPregame:
		return "PG> "
	case mPlay:
		if s.set.prompt == 0 || s.ship == nil {
			return "COMMAND:  "
		}
		sh := s.ship
		var b strings.Builder
		if sh.shieldPct() < 10 || !sh.shieldsUp {
			b.WriteString("S")
		}
		if sh.energy < cfg.YellowEnergy {
			b.WriteString("E")
		}
		if sh.damage > 2000 {
			b.WriteString("D")
		}
		if sh.inop(DLife) {
			b.WriteString(strconv.Itoa(sh.life) + "L")
		}
		return b.String() + "> "
	case mSetName:
		return "Enter your name: "
	case mSetTeam:
		return "Which side?  Federation or Empire: "
	case mSetShip:
		return "Ship? "
	}
	return ""
}

func (s *Session) showPrompt() {
	s.w(s.promptText())
	s.promptSh = true
}

// asynchronous output vs. a half-typed line (3.5)
func (s *Session) async(text string) {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if s.mode == mGripe {
		s.w(text)
		return
	}
	if s.promptSh {
		s.w("\n" + text + s.promptText())
		if s.echo {
			s.w(string(s.line))
		}
		return
	}
	if len(s.line) > 0 {
		s.w("\n" + text)
		if s.echo {
			s.w(string(s.line))
		}
		return
	}
	s.w(text)
}

func (s *Session) protected() bool {
	if s.ship == nil {
		return false
	}
	if s.ship.red {
		return false
	}
	if s.mode == mGripe {
		return true
	}
	return s.helpProt && s.pending.Load() > 0
}

// ---------------------------------------------------------------------------
// Line editor (3.4)
// ---------------------------------------------------------------------------

func (s *Session) input(c byte) {
	c &= 0x7F
	if s.crPend {
		s.crPend = false
		if c == 0 || c == 0x0A {
			return
		}
	}
	s.lastAct = time.Now()
	switch c {
	case 0x03:
		s.g.ctrlC(s)
	case 0x0F:
		s.ctrlO()
	case 0x0D:
		s.crPend = true
		s.endLine(c)
	case 0x0A, 0x0B, 0x0C, 0x1A, 0x1B:
		s.endLine(c)
	case 0x08, 0x7F:
		if len(s.line) > 0 {
			s.line = s.line[:len(s.line)-1]
			s.echoStr("\b \b")
		}
	case 0x15:
		s.line = s.line[:0]
		s.echo = true
		s.w("^U\n")
		if s.promptSh {
			s.w(s.promptText())
		}
	case 0x12:
		s.w("\n")
		if s.promptSh {
			s.w(s.promptText())
		}
		if s.echo {
			s.w(string(s.line))
		}
	case 0x07:
		s.echo = !s.echo
	default:
		if c >= 0x20 && c < 0x7F {
			if len(s.line) >= cfg.MaxLine {
				s.bell(1)
				return
			}
			s.line = append(s.line, c)
			s.echoStr(string(c))
		}
	}
}

// ^O discards pending (unsent) output (3.7)
func (s *Session) ctrlO() {
	s.out = s.out[:0]
	for {
		select {
		case c := <-s.wq:
			s.pending.Add(-int64(len(c)))
			continue
		default:
		}
		break
	}
	s.w("^O\n")
}

func (s *Session) endLine(term byte) {
	text := string(s.line)
	first := len(s.line) == 0
	s.line = s.line[:0]
	wasEcho := s.echo
	s.echo = true
	_ = wasEcho
	s.promptSh = false
	if term == 0x1B && first && (s.mode == mPlay || s.mode == mPregame) {
		s.g.repeatCmd(s)
		return
	}
	s.w("\n")
	s.g.lineReady(s, text, term)
}

// ---------------------------------------------------------------------------
// Game / World
// ---------------------------------------------------------------------------

type Game struct {
	ev       chan event
	sessions map[int]*Session
	nextSess int
	nextConn int

	ships   [10]Ship
	planets []Planet
	bases   []Base
	grid    [gridN + 1][gridN + 1]Cell
	rom     Romulan

	blackholes  bool
	start       time.Time
	gameStarted bool
	ending      bool
	endAt       time.Time

	teamScore [4]Score
	numShips  [4]int
	teamMem   map[string]int
	locks     map[string]time.Time
	uids      map[string]int
	nextUID   int

	nextBaseFire, nextPlanetFire, nextRegen, nextRom, nextLife time.Time

	help    *HelpDB
	ipLock  bool
	lastCPU time.Time
}

func newGame() *Game {
	g := &Game{ev: make(chan event, 4096), sessions: map[int]*Session{}, ipLock: true,
		teamMem: map[string]int{}, locks: map[string]time.Time{}, uids: map[string]int{}, nextUID: 1000}
	g.help = loadHelp()
	g.reset()
	return g
}

func (g *Game) setCell(v, h int, k Kind, i int) { g.grid[v][h] = Cell{k, i} }
func (g *Game) clearCell(v, h int)              { g.grid[v][h] = Cell{} }
func (g *Game) cell(v, h int) Cell {
	if !ingal(v, h) {
		return Cell{K: KHole + 100}
	}
	return g.grid[v][h]
}
func (g *Game) empty(v, h int) bool { return ingal(v, h) && g.grid[v][h].K == KEmpty }

func (g *Game) randEmpty(vlo, vhi, hlo, hhi int) (int, int) {
	for i := 0; i < 100000; i++ {
		v := vlo + rng.Intn(vhi-vlo+1)
		h := hlo + rng.Intn(hhi-hlo+1)
		if g.empty(v, h) {
			return v, h
		}
	}
	for v := 1; v <= gridN; v++ {
		for h := 1; h <= gridN; h++ {
			if g.empty(v, h) {
				return v, h
			}
		}
	}
	return 1, 1
}

func (g *Game) nearBase(v, h, r int) bool {
	for i := range g.bases {
		b := &g.bases[i]
		if b.alive && cheb(v, h, b.v, b.h) <= r {
			return true
		}
	}
	return false
}

// reset builds a brand-new galaxy (5.1-5.3)
func (g *Game) reset() {
	now := time.Now()
	for v := range g.grid {
		for h := range g.grid[v] {
			g.grid[v][h] = Cell{}
		}
	}
	for i := range g.ships {
		g.ships[i] = Ship{idx: i, team: teamOfShip(i), tractor: -1, radioOn: true}
	}
	g.planets = nil
	g.bases = nil
	g.rom = Romulan{nextSpawn: now}
	// starbases: Federation near low hpos, Empire near high hpos, >= 6 apart
	for team := FED; team <= EMP; team++ {
		for n := 0; n < cfg.BasesPerTeam; n++ {
			for try := 0; ; try++ {
				hlo, hhi := 2, 22
				if team == EMP {
					hlo, hhi = gridN-21, gridN-1
				}
				v, h := g.randEmpty(2, gridN-1, hlo, hhi)
				ok := true
				for i := range g.bases {
					if cheb(v, h, g.bases[i].v, g.bases[i].h) < 6 {
						ok = false
					}
				}
				if ok || try > 500 {
					g.bases = append(g.bases, Base{v: v, h: h, team: team, shieldE: cfg.ShieldEnergy, alive: true, pristine: true})
					g.setCell(v, h, KBase, len(g.bases)-1)
					break
				}
			}
		}
	}
	for n := 0; n < cfg.Planets; n++ {
		v, h := g.randEmpty(1, gridN, 1, gridN)
		g.planets = append(g.planets, Planet{v: v, h: h, alive: true})
		g.setCell(v, h, KPlanet, n)
	}
	placeFar := func(kind Kind, count int) {
		for n := 0; n < count; n++ {
			for try := 0; ; try++ {
				v, h := g.randEmpty(1, gridN, 1, gridN)
				if !g.nearBase(v, h, 1) || try > 1000 {
					g.setCell(v, h, kind, 0)
					break
				}
			}
		}
	}
	placeFar(KStar, cfg.Stars)
	g.blackholes = rng.Float64() < cfg.BlackholeChance
	if g.blackholes {
		placeFar(KHole, cfg.Blackholes)
	}
	g.start = now
	g.gameStarted = false
	g.ending = false
	g.teamScore = [4]Score{}
	g.numShips = [4]int{}
	g.teamMem = map[string]int{}
	g.locks = map[string]time.Time{}
	g.nextBaseFire = now.Add(cfg.BaseFire)
	g.nextPlanetFire = now.Add(cfg.PlanetFire)
	g.nextRegen = now.Add(cfg.AutoRepairEvery)
	g.nextRom = now.Add(cfg.RomulanEvery)
	g.nextLife = now.Add(60 * time.Second)
}

func (g *Game) stardate(now time.Time) float64 {
	return now.Sub(g.start).Seconds() * cfg.StardatePerSec
}

func (g *Game) numPlayers() int {
	n := 0
	for i := range g.ships {
		if g.ships[i].inGame {
			n++
		}
	}
	return n
}
func (g *Game) teamShips(team int) int {
	n := 0
	for i := range g.ships {
		if g.ships[i].inGame && g.ships[i].team == team {
			n++
		}
	}
	return n
}
func (g *Game) baseCount(team int) int {
	n := 0
	for i := range g.bases {
		if g.bases[i].alive && g.bases[i].team == team {
			n++
		}
	}
	return n
}
func (g *Game) capturedCount(team int) int {
	n := 0
	for i := range g.planets {
		if g.planets[i].alive && g.planets[i].owner == team {
			n++
		}
	}
	return n
}
func (g *Game) totalBases() int { return g.baseCount(FED) + g.baseCount(EMP) }

// slow_factor over all in-game sessions (9.2)
func (g *Game) slowFactor() float64 {
	minSpd := 9600
	for i := range g.ships {
		sh := &g.ships[i]
		if sh.inGame && sh.sess != nil && sh.sess.speed > 0 && sh.sess.speed < minSpd {
			minSpd = sh.sess.speed
		}
	}
	return clampf(float64(9600-minSpd)/float64(9600-300), 0, 1)
}
func (g *Game) slowPause(lo, hi float64) time.Duration {
	f := g.slowFactor()
	return time.Duration((lo + (hi-lo)*f) * float64(time.Second))
}

// ---------------------------------------------------------------------------
// Actors and hit messages (section 10.1)
// ---------------------------------------------------------------------------

type Actor struct {
	k    Kind
	i    int
	team int
}

type HitKind int

const (
	HPhaser HitKind = iota
	HTorp
	HNova
)

type Hit struct {
	kind       HitKind
	src, dst   Actor
	sv, sh     int
	dv, dh     int // target position (after displacement if displaced)
	srcPct     float64
	dstPct     float64
	srcBuilds  int
	dstBuilds  int
	amount     float64
	deflected  bool
	displaced  bool
	killed     bool
	holeKill   bool
	critDev    int
	critAmt    float64
	showSrcPct bool
	showDstPct bool
	applied    float64
}

func teamAbbr(t int) string {
	switch t {
	case FED:
		return "Fed"
	case EMP:
		return "Emp"
	}
	return "Neu"
}

func descr(a Actor, builds int, style int) string {
	switch a.k {
	case KShip:
		if style == 2 {
			return shipNames[a.i]
		}
		return string(shipLetters[a.i])
	case KRom:
		if style == 2 {
			return "Romulan"
		}
		return "R"
	case KStar:
		if style == 2 {
			return "Star"
		}
		return "*"
	case KBase:
		if style == 2 {
			return teamAbbr(a.team) + " base"
		}
		if a.team == FED {
			return "<>"
		}
		return ")("
	case KPlanet:
		if style == 2 {
			n := teamAbbr(a.team) + " planet"
			if a.team == NEU {
				n = "Neutral planet"
			}
			if builds > 0 {
				n += fmt.Sprintf("(%d)", builds)
			}
			return n
		}
		sym := "@"
		if a.team == FED {
			sym = "+@"
		} else if a.team == EMP {
			sym = "-@"
		}
		if builds > 0 {
			sym += strconv.Itoa(builds)
		}
		return sym
	}
	return "?"
}

// locFmt implements PRLOC: absolute v-h, relative dv,dh, or both.
func locFmt(v, h, sv, sh, mode int, at bool) string {
	var b strings.Builder
	if mode != 1 {
		if at {
			b.WriteString("@")
		}
		fmt.Fprintf(&b, "%d-%d", v, h)
	}
	if !(v == sv && h == sh) && mode != 0 {
		if mode == 2 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%+d,%+d", v-sv, h-sh)
	}
	return b.String()
}

func (s *Session) loc(v, h int, at bool) string {
	if s.ship == nil || !s.ship.inGame {
		return locFmt(v, h, 0, 0, 0, at)
	}
	return locFmt(v, h, s.ship.v, s.ship.h, s.set.ocdef, at)
}
func (s *Session) locAbs(v, h int) string { return fmt.Sprintf("%d-%d", v, h) }

func pctStr(p float64, style int) string {
	if style == 0 {
		return fmt.Sprintf("%+d", int(p))
	}
	return fmt.Sprintf("%+.1f%%", p)
}

func fmtHit(h *Hit, s *Session) string {
	st := s.set.output
	at := st != 0
	var b strings.Builder
	b.WriteString(descr(h.src, h.srcBuilds, st))
	b.WriteString(" ")
	b.WriteString(s.loc(h.sv, h.sh, at))
	if h.showSrcPct {
		if st != 0 {
			b.WriteString(",")
		}
		b.WriteString(" " + pctStr(h.srcPct, st))
	}
	if st == 2 && !h.showSrcPct {
		b.WriteString(" ")
	} else {
		b.WriteString("  ")
	}
	letter := "P"
	word := "phaser hit on "
	switch h.kind {
	case HTorp:
		letter, word = "T", "torpedo hit on "
	case HNova:
		letter, word = "N", "hit on "
	}
	switch st {
	case 2:
		fmt.Fprintf(&b, "makes %.1f unit %s", h.amount, word)
	case 1:
		fmt.Fprintf(&b, "%.1f unit %s  ", h.amount, letter)
	default:
		fmt.Fprintf(&b, "%d%s  ", int(h.amount), letter)
	}
	b.WriteString(descr(h.dst, h.dstBuilds, st))
	b.WriteString(" ")
	if h.displaced {
		switch st {
		case 2:
			b.WriteString("displaced to ")
		case 1:
			b.WriteString("-->")
		default:
			b.WriteString(">")
		}
		b.WriteString(s.loc(h.dv, h.dh, false))
	} else {
		b.WriteString(s.loc(h.dv, h.dh, at))
	}
	if h.showDstPct && !h.killed {
		if st != 0 {
			b.WriteString(",")
		}
		b.WriteString(" " + pctStr(h.dstPct, st))
	}
	if h.deflected {
		switch st {
		case 2:
			b.WriteString(" (deflected by shields)")
		case 1:
			b.WriteString(" DEFL")
		default:
			b.WriteString(" D")
		}
	}
	if h.critDev >= 0 && h.dst.k == KShip && s.ship != nil && s.ship.idx == h.dst.i {
		if st == 2 {
			fmt.Fprintf(&b, "; %s damaged %.1f units", devNames[h.critDev], h.critAmt)
		} else {
			fmt.Fprintf(&b, "; %s %d", devKeys[h.critDev][:2], int(h.critAmt))
		}
	}
	if h.killed {
		if h.holeKill {
			b.WriteString("\n" + descr(h.dst, 0, st) + " displaced into a black hole!")
		}
		if h.dst.k == KShip || h.dst.k == KRom || h.dst.k == KBase {
			if st == 2 {
				b.WriteString("\n" + descr(h.dst, 0, st) + " destroyed")
			} else {
				b.WriteString("\n" + descr(h.dst, 0, st) + " D")
			}
		}
	}
	return b.String()
}

// deliverHit sends the hit message to every ship within 10 sectors of the
// source or the target (plus the participants).
func (g *Game) deliverHit(h *Hit) {
	for i := range g.ships {
		sh := &g.ships[i]
		if !sh.inGame || sh.sess == nil {
			continue
		}
		near := cheb(sh.v, sh.h, h.sv, h.sh) <= 10 || cheb(sh.v, sh.h, h.dv, h.dh) <= 10
		part := (h.src.k == KShip && h.src.i == i) || (h.dst.k == KShip && h.dst.i == i)
		if near || part {
			sh.sess.async(fmtHit(h, sh.sess))
		}
	}
}

func (g *Game) sendShip(sh *Ship, text string) {
	if sh != nil && sh.inGame && sh.sess != nil {
		sh.sess.async(text)
	}
}

// radio broadcast to a team's operational radios (base calls etc.)
func (g *Game) teamRadio(team int, f func(s *Session) string) {
	for i := range g.ships {
		sh := &g.ships[i]
		if !sh.inGame || sh.sess == nil || sh.team != team || !sh.radioOn || sh.inop(DRadio) {
			continue
		}
		sh.sess.async(f(sh.sess))
	}
}

func (g *Game) baseCall(b *Base, destroyed bool) {
	g.teamRadio(b.team, func(s *Session) string {
		st := s.set.output
		if st == 2 {
			if destroyed {
				return fmt.Sprintf("Base at %s destroyed", s.locAbs(b.v, b.h))
			}
			return fmt.Sprintf("Base at %s under attack", s.locAbs(b.v, b.h))
		}
		c := "A"
		if destroyed {
			c = "D"
		}
		return fmt.Sprintf("%s %s %s", descr(Actor{k: KBase, team: b.team}, 0, st), s.locAbs(b.v, b.h), c)
	})
}

// ---------------------------------------------------------------------------
// Scoring helpers
// ---------------------------------------------------------------------------

func (g *Game) scoreFor(att Actor) []*Score {
	switch att.k {
	case KShip:
		return []*Score{&g.ships[att.i].score, &g.teamScore[g.ships[att.i].team]}
	case KBase, KPlanet:
		if att.team == FED || att.team == EMP {
			return []*Score{&g.teamScore[att.team]}
		}
	case KRom:
		return []*Score{&g.teamScore[ROMT]}
	}
	return nil
}

func (g *Game) creditKill(att Actor) {
	for _, sc := range g.scoreFor(att) {
		sc.Kills++
	}
}

// ---------------------------------------------------------------------------
// Removing ships from play
// ---------------------------------------------------------------------------

func (g *Game) breakTractor(sh *Ship) {
	if sh.tractor < 0 {
		return
	}
	p := &g.ships[sh.tractor]
	sh.tractor = -1
	if p.tractor == sh.idx {
		p.tractor = -1
		g.sendShip(p, "Tractor beam broken.")
	}
	g.sendShip(sh, "Tractor beam broken.")
}

func (g *Game) takeOffGrid(sh *Ship) {
	g.breakTractor(sh)
	if sh.inGame && g.grid[sh.v][sh.h].K == KShip && g.grid[sh.v][sh.h].I == sh.idx {
		g.clearCell(sh.v, sh.h)
	}
	sh.inGame = false
}

func (g *Game) toPregame(s *Session) {
	s.mode = mPregame
	s.ship = nil
	s.cmdq = nil
	s.busyTil = time.Time{}
	s.promptSh = false
	s.helpProt = false
	s.line = s.line[:0]
}

// destroyShip: ship killed by att (att.k==0 means environment)
func (g *Game) destroyShip(sh *Ship, victimMsg string, att Actor) {
	if !sh.inGame {
		return
	}
	if att.k != 0 && att.team != sh.team && att.team != NEU {
		g.creditKill(att)
	}
	s := sh.sess
	g.takeOffGrid(sh)
	name := shipNames[sh.idx]
	g.teamRadio(sh.team, func(rs *Session) string {
		if rs == s {
			return ""
		}
		return name + " destroyed"
	})
	if s != nil {
		g.teamMem[s.capKey] = sh.team
		s.async("\n" + victimMsg)
		s.w("\n")
		g.printPoints(s, true, false, false, false, true)
		s.w("\nYour ship has been destroyed.  You may ACTIVATE again.\n")
		g.toPregame(s)
	}
	sh.sess = nil
	g.checkEnd()
}

// leaveShip: QUIT / ^C / disconnect.
func (g *Game) leaveShip(sh *Ship, lock bool) {
	if !sh.inGame {
		return
	}
	s := sh.sess
	g.takeOffGrid(sh)
	if s != nil {
		g.teamMem[s.capKey] = sh.team
		if lock {
			exp := time.Now().Add(cfg.QuitLock)
			g.locks[s.capKey] = exp
			if g.ipLock {
				g.locks["ip:"+s.ip] = exp
			}
		}
	}
	sh.sess = nil
	g.checkEnd()
}

// ---------------------------------------------------------------------------
// End of game (5.4)
// ---------------------------------------------------------------------------

func (g *Game) checkEnd() {
	if g.ending || !g.gameStarted {
		return
	}
	lost := [3]bool{}
	for t := FED; t <= EMP; t++ {
		lost[t] = g.baseCount(t) == 0 && g.capturedCount(t) == 0 && g.teamShips(t) == 0
	}
	if !lost[FED] && !lost[EMP] {
		return
	}
	g.ending = true
	g.endAt = time.Now().Add(cfg.EndWait)
	var msg string
	switch {
	case lost[FED] && lost[EMP]:
		msg = "Total destruction!  Both fleets have been annihilated.  Nobody wins."
	case lost[EMP]:
		msg = "The Empire has been destroyed.  The Federation is victorious!"
	default:
		msg = "The Federation has been destroyed.  The Empire is victorious!"
	}
	for _, s := range g.sessions {
		s.async("\n*** " + msg + " ***")
		if s.mode == mPlay && s.ship != nil {
			g.printPoints(s, true, true, true, g.rom.alive || g.teamScore[ROMT].Kills > 0 || true, false)
		}
		s.async(fmt.Sprintf("A new game will begin in %d seconds.", int(cfg.EndWait.Seconds())))
	}
}

func (g *Game) finishEnd() {
	for _, s := range g.sessions {
		if s.ship != nil && s.ship.inGame {
			s.ship.sess = nil
		}
		if s.mode != mPregame {
			s.async("The galaxy is being reinitialized.")
		}
		g.toPregame(s)
		s.async("\nA new game is ready.  Type ACTIVATE to join.")
	}
	g.reset()
}

// ---------------------------------------------------------------------------
// Combat (sections 7.4, 9.3, 9.4)
// ---------------------------------------------------------------------------

func signedPct(sh *Ship) float64 {
	p := sh.shieldPct()
	if !sh.shieldsUp {
		return -p
	}
	return p
}

func basePct(b *Base) float64 { return b.shieldE / cfg.ShieldEnergy * 100 }

// fillSrc fills the source side of a hit message.
func (g *Game) fillSrc(h *Hit, att Actor, srcShip *Ship) {
	switch att.k {
	case KShip:
		if srcShip != nil {
			h.srcPct = signedPct(srcShip)
			h.showSrcPct = true
		}
	case KBase:
		h.srcPct = basePct(&g.bases[att.i])
		h.showSrcPct = true
	case KPlanet:
		h.srcBuilds = g.planets[att.i].builds
	}
}

func (g *Game) newHit(kind HitKind, att Actor, srcShip *Ship, sv, sh int) *Hit {
	h := &Hit{kind: kind, src: att, sv: sv, sh: sh, critDev: -1}
	g.fillSrc(h, att, srcShip)
	return h
}

// displace moves a hit ship 0..2 sectors away from the blast (9.3 point 6).
func (g *Game) displace(t *Ship, fromV, fromH int) (moved, hole bool) {
	n := rng.Intn(3)
	dv, dh := sgn(t.v-fromV), sgn(t.h-fromH)
	if dv == 0 && dh == 0 {
		dv = 1
	}
	for i := 0; i < n; i++ {
		nv, nh := t.v+dv, t.h+dh
		if !ingal(nv, nh) {
			break
		}
		c := g.grid[nv][nh]
		if c.K == KHole {
			g.clearCell(t.v, t.h)
			t.v, t.h = nv, nh
			t.docked = false
			return true, true
		}
		if c.K != KEmpty {
			break
		}
		g.clearCell(t.v, t.h)
		g.setCell(nv, nh, KShip, t.idx)
		t.v, t.h = nv, nh
		moved = true
	}
	if moved {
		t.red = true
		t.docked = false
	}
	return
}

// hitShip applies a hit of size X to ship t.  Returns nil if the target is
// protected (HELP/GRIPE and not red alert).
func (g *Game) hitShip(att Actor, srcShip *Ship, sv, sh int, t *Ship, X float64, kind HitKind) *Hit {
	if t.sess != nil && t.sess.protected() {
		return nil
	}
	h := g.newHit(kind, att, srcShip, sv, sh)
	h.dst = Actor{KShip, t.idx, t.team}
	h.dv, h.dh = t.v, t.h
	h.showDstPct = true
	h.amount = X
	t.red = true
	eff := 0.0
	if t.shieldsUp {
		eff = t.shieldPct() * clampf(1-t.dev[DShield]/cfg.DeviceInop, 0, 1)
	}
	if kind == HTorp && t.shieldsUp && eff >= 85 && rng.Float64() < (eff-70)/30 {
		h.deflected = true
		h.amount = 0
		t.shieldE = math.Max(0, t.shieldE-uni(0, 50))
		if t.shieldE <= 0 {
			t.shieldsUp = false
		}
		g.breakTractor(t)
		h.dstPct = signedPct(t)
		return h
	}
	absorbed := 0.0
	if t.shieldsUp {
		absorbed = math.Min(X*eff/100, t.shieldE)
		t.shieldE -= absorbed
	}
	rem := X - absorbed
	h.applied = rem
	t.damage += rem
	t.energy -= rem
	if t.shieldE <= 0 {
		t.shieldE = 0
		t.shieldsUp = false
	}
	if cfg.CritHits && rem*(rng.Float64()+0.1) >= 170 {
		d := rng.Intn(NDev)
		amt := rem / 2
		t.dev[d] += amt
		if d == DShield && t.inop(DShield) {
			t.shieldsUp = false
		}
		h.critDev, h.critAmt = d, amt
	}
	if kind != HPhaser {
		g.breakTractor(t)
	}
	if t.damage >= cfg.ShipDmgLimit || t.energy <= 0 {
		h.killed = true
	} else if kind != HPhaser {
		if moved, hole := g.displace(t, sv, sh); moved {
			h.displaced = true
			h.dv, h.dh = t.v, t.h
			if hole {
				h.killed, h.holeKill = true, true
			}
		}
	}
	h.dstPct = signedPct(t)
	return h
}

func (g *Game) afterShipHit(att Actor, victim *Ship, h *Hit) {
	if h == nil {
		return
	}
	g.creditHit(att, h.dst, h.applied)
	g.deliverHit(h)
	if h.killed {
		g.destroyShip(victim, "Your ship has been destroyed!", att)
	}
}

func (g *Game) creditHit(att, tgt Actor, applied float64) {
	if att.team == tgt.team || applied <= 0 {
		return
	}
	for _, sc := range g.scoreFor(att) {
		switch tgt.k {
		case KShip:
			sc.DmgEnemy += applied
		case KBase:
			sc.DmgBase += applied
		case KRom:
			sc.Romulan += applied / 10
		}
	}
}

// hitBase: starbase shields absorb, the remainder accumulates as damage.
func (g *Game) hitBase(att Actor, srcShip *Ship, sv, sh int, bi int, X float64, kind HitKind) {
	b := &g.bases[bi]
	if !b.alive {
		return
	}
	h := g.newHit(kind, att, srcShip, sv, sh)
	h.dst = Actor{KBase, bi, b.team}
	h.dv, h.dh = b.v, b.h
	h.showDstPct = true
	h.amount = X
	p := basePct(b)
	if kind == HTorp && p >= 85 && rng.Float64() < (p-70)/30 {
		h.deflected, h.amount = true, 0
		b.shieldE = math.Max(0, b.shieldE-uni(0, 50))
		h.dstPct = basePct(b)
		g.deliverHit(h)
		return
	}
	first := b.pristine
	absorbed := math.Min(X*p/100, b.shieldE)
	b.shieldE -= absorbed
	rem := X - absorbed
	b.damage += rem
	h.applied = rem
	b.pristine = false
	if first && b.damage < cfg.BaseCapacity {
		g.baseCall(b, false)
	}
	h.dstPct = basePct(b)
	if b.damage >= cfg.BaseCapacity {
		h.killed = true
	}
	g.creditHit(att, h.dst, h.applied)
	g.deliverHit(h)
	if h.killed {
		g.destroyBase(bi, att)
	}
}

func (g *Game) destroyBase(bi int, att Actor) {
	b := &g.bases[bi]
	if !b.alive {
		return
	}
	b.alive = false
	if g.grid[b.v][b.h].K == KBase && g.grid[b.v][b.h].I == bi {
		g.clearCell(b.v, b.h)
	}
	if att.k != 0 && att.team != b.team && att.team != NEU {
		g.creditKill(att)
	}
	g.baseCall(b, true)
	g.checkDocked(b.team)
	g.checkEnd()
}

// checkDocked: ships docked at a port that no longer exists are undocked (BASKIL)
func (g *Game) checkDocked(team int) {
	for i := range g.ships {
		sh := &g.ships[i]
		if !sh.inGame || !sh.docked || sh.team != team {
			continue
		}
		if g.adjacentPorts(sh) == 0 {
			sh.docked = false
			sh.red = true
		}
	}
}

// adjacentPorts: 2 per adjacent friendly base, 1 per adjacent friendly planet
func (g *Game) adjacentPorts(sh *Ship) int {
	n := 0
	for i := range g.bases {
		b := &g.bases[i]
		if b.alive && b.team == sh.team && cheb(sh.v, sh.h, b.v, b.h) <= 1 {
			n += 2
		}
	}
	for i := range g.planets {
		p := &g.planets[i]
		if p.alive && p.owner == sh.team && cheb(sh.v, sh.h, p.v, p.h) <= 1 {
			n++
		}
	}
	return n
}

func (g *Game) destroyPlanet(pi int, att Actor, attShip *Ship) {
	p := &g.planets[pi]
	if !p.alive {
		return
	}
	p.alive = false
	if g.grid[p.v][p.h].K == KPlanet && g.grid[p.v][p.h].I == pi {
		g.clearCell(p.v, p.h)
	}
	for _, sc := range g.scoreFor(att) {
		sc.Planets++
	}
	if p.owner != NEU {
		g.checkDocked(p.owner)
	}
	g.checkEnd()
}

// planetHit: torpedoes reduce builds; a planet with no builds is destroyed by
// a further hit (9.3).  Phasers reduce builds but never destroy (8.13).
func (g *Game) planetHit(att Actor, srcShip *Ship, sv, sh int, pi int, X float64, kind HitKind, chance float64) {
	p := &g.planets[pi]
	if !p.alive {
		return
	}
	h := g.newHit(kind, att, srcShip, sv, sh)
	h.dst = Actor{KPlanet, pi, p.owner}
	h.dv, h.dh = p.v, p.h
	h.amount = X
	destroyed := false
	if rng.Float64() < chance {
		if p.builds > 0 {
			p.builds--
		} else if kind != HPhaser {
			destroyed = true
		}
	}
	h.dstBuilds = p.builds
	if destroyed {
		h.dstBuilds = 0
	}
	g.deliverHit(h)
	if destroyed {
		if att.k == KShip && srcShip != nil {
			g.sendShip(srcShip, "Planet destroyed!  (-100 points)")
		}
		g.destroyPlanet(pi, att, srcShip)
	}
}

func (g *Game) hitRom(att Actor, srcShip *Ship, sv, sh int, X float64, kind HitKind) {
	r := &g.rom
	if !r.alive {
		return
	}
	h := g.newHit(kind, att, srcShip, sv, sh)
	h.dst = Actor{KRom, 0, ROMT}
	h.dv, h.dh = r.v, r.h
	h.amount = X
	r.hp -= X
	r.reveal = time.Now().Add(5 * time.Second)
	h.applied = X
	g.creditHit(att, h.dst, X)
	if r.hp <= 0 {
		h.killed = true
		r.alive = false
		if g.grid[r.v][r.h].K == KRom {
			g.clearCell(r.v, r.h)
		}
		r.nextSpawn = time.Now().Add(cfg.RomulanRespawn)
		for _, sc := range g.scoreFor(att) {
			sc.Romulan += 1000
		}
	}
	g.deliverHit(h)
}

// phaserHitCell resolves one phaser blast of size X from att at (sv,sh) onto
// whatever is in cell (tv,th).  Used by players, bases, planets, Romulan.
func (g *Game) phaserHitCell(att Actor, srcShip *Ship, sv, sh, tv, th int, X float64) {
	c := g.cell(tv, th)
	switch c.K {
	case KShip:
		t := &g.ships[c.I]
		g.afterShipHit(att, t, g.hitShip(att, srcShip, sv, sh, t, X, HPhaser))
	case KBase:
		g.hitBase(att, srcShip, sv, sh, c.I, X, HPhaser)
	case KPlanet:
		d := euclid(sv, sh, tv, th)
		if d < 1 {
			d = 1
		}
		chance := 0.0
		if float64(1+rng.Intn(100))*X/(25*d) > 150 {
			chance = 1
		}
		g.planetHit(att, srcShip, sv, sh, c.I, X, HPhaser, chance)
	case KRom:
		g.hitRom(att, srcShip, sv, sh, X, HPhaser)
	}
}

// ---------------------------------------------------------------------------
// Torpedoes
// ---------------------------------------------------------------------------

func (g *Game) torpNote(sh *Ship, long, short string) {
	if sh == nil || sh.sess == nil {
		return
	}
	if sh.sess.set.output == 2 {
		sh.sess.async(long)
	} else {
		sh.sess.async(short)
	}
}

// flyTorpedo launches one torpedo.  att/srcShip identify the shooter.
func (g *Game) flyTorpedo(att Actor, srcShip *Ship, ov, oh, tv, th int, sigmaDeg float64, num int) {
	dv, dh := float64(tv-ov), float64(th-oh)
	ang := math.Atan2(dh, dv) + rng.NormFloat64()*sigmaDeg*math.Pi/180
	cv, ch := math.Cos(ang), math.Sin(ang)
	R := cfg.TorpRangeMin + rng.Intn(cfg.TorpRangeMax-cfg.TorpRangeMin+1)
	lv, lh := ov, oh
	for step := 1; step <= R*2+2; step++ {
		t := float64(step) * 0.5
		v := ov + int(math.Round(cv*t))
		h := oh + int(math.Round(ch*t))
		if v == lv && h == lh {
			continue
		}
		if cheb(ov, oh, v, h) > R || !ingal(v, h) {
			break
		}
		lv, lh = v, h
		c := g.grid[v][h]
		if c.K == KEmpty {
			continue
		}
		g.torpResolve(att, srcShip, ov, oh, v, h, c, num)
		return
	}
	g.torpNote(srcShip, fmt.Sprintf("Torpedo %d missed; self-destructed at %s", num, locFmt(lv, lh, ov, oh, 0, false)),
		fmt.Sprintf("T%d miss %d-%d", num, lv, lh))
}

func (g *Game) torpResolve(att Actor, srcShip *Ship, ov, oh, v, h int, c Cell, num int) {
	X := uni(cfg.TorpBaseDmg*0.9, cfg.TorpBaseDmg*1.1)
	neutral := func(what string) {
		g.torpNote(srcShip, fmt.Sprintf("Torpedo %d neutralized by friendly %s at %d-%d", num, what, v, h),
			fmt.Sprintf("T%d neut %d-%d", num, v, h))
	}
	switch c.K {
	case KStar:
		g.nova(v, h, att, srcShip)
	case KHole:
		g.torpNote(srcShip, fmt.Sprintf("Torpedo %d absorbed by a black hole at %d-%d", num, v, h),
			fmt.Sprintf("T%d hole %d-%d", num, v, h))
	case KShip:
		t := &g.ships[c.I]
		if att.team == t.team {
			neutral(shipNames[t.idx])
			return
		}
		g.afterShipHit(att, t, g.hitShip(att, srcShip, ov, oh, t, X, HTorp))
	case KBase:
		b := &g.bases[c.I]
		if att.team == b.team {
			neutral("starbase")
			return
		}
		g.hitBase(att, srcShip, ov, oh, c.I, X, HTorp)
	case KPlanet:
		p := &g.planets[c.I]
		if att.team == p.owner && p.owner != NEU {
			neutral("planet")
			return
		}
		g.planetHit(att, srcShip, ov, oh, c.I, X, HTorp, 0.25)
	case KRom:
		if att.k != KRom {
			g.hitRom(att, srcShip, ov, oh, X, HTorp)
		}
	}
}

// nova: a torpedoed star explodes, hitting everything within 3 sectors.
func (g *Game) nova(v, h int, att Actor, attShip *Ship) {
	g.clearCell(v, h)
	for _, sc := range g.scoreFor(att) {
		sc.Stars++
	}
	for i := range g.ships {
		sh := &g.ships[i]
		if sh.inGame && sh.sess != nil && cheb(sh.v, sh.h, v, h) <= 10 {
			sh.sess.async(fmt.Sprintf("Star at %s goes nova!", sh.sess.loc(v, h, false)))
		}
	}
	type victim struct {
		k Kind
		i int
	}
	var vs []victim
	for dv := -3; dv <= 3; dv++ {
		for dh := -3; dh <= 3; dh++ {
			c := g.cell(v+dv, h+dh)
			switch c.K {
			case KShip, KBase, KPlanet, KRom:
				vs = append(vs, victim{c.K, c.I})
			}
		}
	}
	star := Actor{k: KStar, team: NEU}
	for _, x := range vs {
		X := uni(cfg.TorpBaseDmg*0.9, cfg.TorpBaseDmg*1.1)
		switch x.k {
		case KShip:
			t := &g.ships[x.i]
			if !t.inGame {
				continue
			}
			hit := g.hitShip(star, nil, v, h, t, X, HNova)
			if hit != nil {
				g.creditHit(att, hit.dst, hit.applied)
				g.deliverHit(hit)
				if hit.killed {
					g.destroyShip(t, "Your ship has been destroyed by a nova!", att)
				}
			}
		case KBase:
			if g.bases[x.i].alive {
				g.hitBaseNova(star, att, v, h, x.i, X)
			}
		case KPlanet:
			p := &g.planets[x.i]
			if !p.alive {
				continue
			}
			hit := g.newHit(HNova, star, nil, v, h)
			hit.dst = Actor{KPlanet, x.i, p.owner}
			hit.dv, hit.dh = p.v, p.h
			hit.amount = X
			p.builds -= 3
			dead := p.builds < 0
			if p.builds < 0 {
				p.builds = 0
			}
			hit.dstBuilds = p.builds
			g.deliverHit(hit)
			if dead {
				g.destroyPlanet(x.i, att, attShip)
			}
		case KRom:
			if g.rom.alive {
				g.hitRom(att, attShip, v, h, X, HNova)
			}
		}
	}
}

// base hit by a nova: credit goes to whoever fired the torpedo
func (g *Game) hitBaseNova(star, att Actor, v, h, bi int, X float64) {
	g.hitBase(star, nil, v, h, bi, X, HNova)
	// hitBase credits nothing for a neutral star source; the kill credit
	// below handles the firer.
	if !g.bases[bi].alive {
		return
	}
	_ = att
}

// ---------------------------------------------------------------------------
// Automata and scheduler (2.1, 9.4)
// ---------------------------------------------------------------------------

func (g *Game) repairDevices(sh *Ship, units float64) {
	lifeWas := sh.inop(DLife)
	for i := range sh.dev {
		sh.dev[i] = math.Max(0, sh.dev[i]-units)
	}
	if lifeWas && !sh.inop(DLife) {
		sh.life = cfg.LifeReserve
	}
}

func (g *Game) timedTurn(sh *Ship) {
	sh.turns++
	g.repairDevices(sh, cfg.AutoRepairUnits)
}

func (g *Game) tick(now time.Time) {
	for i := range g.ships {
		sh := &g.ships[i]
		if sh.inGame && sh.sess == nil && !sh.ghostEnd.IsZero() && now.After(sh.ghostEnd) {
			sh.ghostEnd = time.Time{}
			g.leaveShip(sh, false)
		}
	}
	for _, s := range g.sessions {
		if s.held != nil && now.After(s.held.expiry) {
			s.held = nil
		}
		if s.mode != mPlay && now.Sub(s.lastAct) > cfg.IdlePregame {
			s.w("\nIdle timeout.  Goodbye.\n")
			g.closeSession(s, "idle")
		}
	}
	if g.ending && now.After(g.endAt) {
		g.finishEnd()
		return
	}
	if g.numPlayers() == 0 {
		g.nextBaseFire = now.Add(cfg.BaseFire)
		g.nextPlanetFire = now.Add(cfg.PlanetFire)
		g.nextRegen = now.Add(cfg.AutoRepairEvery)
		g.nextRom = now.Add(cfg.RomulanEvery)
		g.nextLife = now.Add(60 * time.Second)
		return
	}
	if now.After(g.nextBaseFire) {
		g.nextBaseFire = now.Add(cfg.BaseFire)
		g.basesFire()
	}
	if now.After(g.nextPlanetFire) {
		g.nextPlanetFire = now.Add(cfg.PlanetFire)
		g.planetsFire()
	}
	if now.After(g.nextRegen) {
		g.nextRegen = now.Add(cfg.AutoRepairEvery)
		g.regen()
	}
	if now.After(g.nextRom) {
		g.nextRom = now.Add(cfg.RomulanEvery)
		g.romTick(now)
	}
	if now.After(g.nextLife) {
		g.nextLife = now.Add(60 * time.Second)
		g.lifeTick()
	}
}

func (g *Game) regen() {
	for i := range g.bases {
		b := &g.bases[i]
		if !b.alive {
			continue
		}
		b.shieldE = math.Min(cfg.ShieldEnergy, b.shieldE+cfg.ShieldEnergy*0.02)
		b.damage = math.Max(0, b.damage-20)
		if b.shieldE >= cfg.ShieldEnergy && b.damage == 0 {
			b.pristine = true
		}
	}
	for i := range g.ships {
		sh := &g.ships[i]
		if sh.inGame {
			g.repairDevices(sh, cfg.AutoRepairUnits)
		}
	}
}

func (g *Game) lifeTick() {
	for i := range g.ships {
		sh := &g.ships[i]
		if !sh.inGame || !sh.inop(DLife) || sh.docked {
			continue
		}
		sh.life--
		if sh.life < 0 {
			g.destroyShip(sh, "Your life support reserves are exhausted.  The crew has died.", Actor{})
			continue
		}
		g.sendShip(sh, fmt.Sprintf("Life support critical!  %d stardates of reserve remain.", sh.life))
	}
}

func (g *Game) basesFire() {
	for bi := range g.bases {
		b := &g.bases[bi]
		if !b.alive {
			continue
		}
		att := Actor{KBase, bi, b.team}
		for i := range g.ships {
			t := &g.ships[i]
			if !t.inGame || t.team == b.team || cheb(t.v, t.h, b.v, b.h) > cfg.BaseRange {
				continue
			}
			X := 400 / math.Max(1, euclid(t.v, t.h, b.v, b.h))
			g.afterShipHit(att, t, g.hitShip(att, nil, b.v, b.h, t, X, HPhaser))
			if !b.alive {
				break
			}
		}
		if g.rom.alive && cheb(g.rom.v, g.rom.h, b.v, b.h) <= cfg.BaseRange {
			g.hitRom(att, nil, b.v, b.h, 400/math.Max(1, euclid(g.rom.v, g.rom.h, b.v, b.h)), HPhaser)
		}
	}
}

func (g *Game) planetsFire() {
	for pi := range g.planets {
		p := &g.planets[pi]
		if !p.alive {
			continue
		}
		att := Actor{KPlanet, pi, p.owner}
		for i := range g.ships {
			t := &g.ships[i]
			if !t.inGame || (p.owner != NEU && t.team == p.owner) || cheb(t.v, t.h, p.v, p.h) > cfg.PlanetRange {
				continue
			}
			X := float64(100+60*p.builds) / math.Max(1, euclid(t.v, t.h, p.v, p.h))
			g.afterShipHit(att, t, g.hitShip(att, nil, p.v, p.h, t, X, HPhaser))
		}
		if g.rom.alive && cheb(g.rom.v, g.rom.h, p.v, p.h) <= cfg.PlanetRange {
			g.hitRom(att, nil, p.v, p.h, float64(100+60*p.builds), HPhaser)
		}
	}
}

// ---- Romulan ----

func (g *Game) romVisible(vw *Ship, now time.Time) bool {
	r := &g.rom
	return r.alive && (now.Before(r.reveal) || (vw != nil && cheb(vw.v, vw.h, r.v, r.h) <= 1))
}

func (g *Game) romStep(dv, dh, n int) {
	r := &g.rom
	for i := 0; i < n; i++ {
		nv, nh := r.v+dv, r.h+dh
		if !g.empty(nv, nh) {
			return
		}
		g.clearCell(r.v, r.h)
		r.v, r.h = nv, nh
		g.setCell(nv, nh, KRom, 0)
	}
}

func (g *Game) romTick(now time.Time) {
	r := &g.rom
	if !r.alive {
		n := g.numPlayers()
		if n >= 1 && n <= cfg.RomulanMaxPlay && now.After(r.nextSpawn) {
			v, h := g.randEmpty(1, gridN, 1, gridN)
			r.alive, r.v, r.h, r.hp = true, v, h, uni(300, 600)
			r.lastTgt = now
			r.nextFire = now.Add(3 * time.Second)
			g.setCell(v, h, KRom, 0)
			for i := range g.ships {
				sh := &g.ships[i]
				if sh.inGame && sh.sess != nil && cheb(sh.v, sh.h, v, h) <= 10 {
					sh.sess.async(fmt.Sprintf("Romulan detected at %s", sh.sess.loc(v, h, false)))
				}
			}
		}
		return
	}
	// nearest suitable target (ship or base)
	bd, bv, bh := 1000, 0, 0
	for i := range g.ships {
		t := &g.ships[i]
		if t.inGame {
			if d := cheb(t.v, t.h, r.v, r.h); d < bd {
				bd, bv, bh = d, t.v, t.h
			}
		}
	}
	for i := range g.bases {
		b := &g.bases[i]
		if b.alive {
			if d := cheb(b.v, b.h, r.v, r.h); d < bd {
				bd, bv, bh = d, b.v, b.h
			}
		}
	}
	if bd > cfg.RomulanSight {
		dv, dh := rng.Intn(3)-1, rng.Intn(3)-1
		if dv != 0 || dh != 0 {
			g.romStep(dv, dh, 1+rng.Intn(3))
		}
		return
	}
	r.lastTgt = now
	if bd > 1 && rng.Intn(2) == 0 {
		g.romStep(sgn(bv-r.v), sgn(bh-r.h), 2)
		bd = cheb(bv, bh, r.v, r.h)
	}
	if now.After(r.nextFire) && bd <= cfg.PhaserRange {
		r.reveal = now.Add(5 * time.Second)
		r.nextFire = now.Add(3 * time.Second)
		att := Actor{KRom, 0, ROMT}
		if r.phaserNx {
			g.phaserHitCell(att, nil, r.v, r.h, bv, bh, 300/math.Max(1, euclid(bv, bh, r.v, r.h)))
		} else {
			for k := 1; k <= 3 && r.alive; k++ {
				g.flyTorpedo(att, nil, r.v, r.h, bv, bh, 2.0, k)
			}
		}
		r.phaserNx = !r.phaserNx
	}
}

// ---------------------------------------------------------------------------
// Sessions: accept, events, close
// ---------------------------------------------------------------------------

func (g *Game) handle(e event) {
	s := e.s
	switch e.kind {
	case evAccept:
		g.accept(e)
		return
	}
	if s == nil || s.closed {
		return
	}
	switch e.kind {
	case evData:
		t0 := time.Now()
		for _, b := range e.data {
			if s.closed {
				break
			}
			s.input(b)
		}
		s.cpu += time.Since(t0)
	case evSpeed:
		if e.a > 0 {
			s.speed = e.a
		}
	case evNAWS:
		s.width, s.height = e.a, e.b
	case evTType:
		s.ttype = e.str
	case evEOF:
		g.closeSession(s, "eof")
	}
}

func (g *Game) optionsLine() string {
	r, b := "yes", "no"
	if g.blackholes {
		b = "yes"
	}
	return fmt.Sprintf("Options in effect: Romulans %s, black holes %s.", r, b)
}

func (g *Game) accept(e event) {
	s := e.s
	if len(g.sessions) >= cfg.MaxConn {
		s.conn.Write([]byte("Sorry, the server is full.\r\n"))
		s.conn.Close()
		return
	}
	g.nextSess++
	s.id = g.nextSess
	g.nextConn++
	s.connNo = g.nextConn
	g.sessions[s.id] = s
	s.created, s.lastAct = time.Now(), time.Now()
	s.w("\n" + versionStr + "\n" + g.optionsLine() + "\n")
	if g.ending {
		s.w("A game has just ended; a new one begins shortly.\n")
	}
	s.w("Type ACTIVATE to join the game, HELP for help.\n\n")
	go s.readLoop()
}

func (g *Game) closeSession(s *Session, why string) {
	if s.closed {
		return
	}
	if s.ship != nil && s.ship.inGame {
		sh := s.ship
		s.ship = nil
		if sh.red {
			sh.sess = nil
			sh.ghostEnd = time.Now().Add(cfg.GhostHold)
			g.teamMem[s.capKey] = sh.team
		} else {
			g.leaveShip(sh, false)
		}
	}
	s.flush()
	s.closed = true
	delete(g.sessions, s.id)
	close(s.wq)
}

func (g *Game) ctrlC(s *Session) {
	now := time.Now()
	switch s.mode {
	case mPregame:
		s.w("^C\n")
		s.promptSh = false
	case mSetName, mSetTeam, mSetShip:
		s.w("^C\n")
		g.toPregame(s)
	case mGripe:
		s.w("^C\nGripe aborted.\n")
		s.gripe = nil
		s.helpProt = false
		if s.ship != nil {
			s.mode = mPlay
		} else {
			s.mode = mPregame
		}
		s.promptSh = false
	case mPlay:
		s.line = s.line[:0]
		if len(s.cmdq) > 0 || now.Before(s.busyTil) {
			s.cmdq = nil
			s.w("^C\n")
			s.bell(3)
			s.promptSh = false
			return
		}
		sh := s.ship
		if sh == nil {
			return
		}
		if sh.red {
			s.w("^C\nSorry, Captain, you cannot leave under red alert.  Use QUIT.\n")
			s.bell(2)
			s.promptSh = false
			return
		}
		s.held = &Held{ship: *sh, expiry: now.Add(cfg.CtrlCHold)}
		g.leaveShip(sh, false)
		g.toPregame(s)
		s.w(fmt.Sprintf("^C\nGame aborted.  Your ship was released.  ACTIVATE within %d seconds to resume.\n",
			int(cfg.CtrlCHold.Seconds())))
	}
}

func (g *Game) repeatCmd(s *Session) {
	if s.mode != mPlay || s.lastCmd == "" || s.ship == nil {
		s.bell(1)
		s.promptSh = false
		return
	}
	s.w(s.lastCmd + "\n")
	s.lineSeq++
	s.cmdq = append(s.cmdq, qcmd{s.lastCmd, s.lineSeq})
}

func (g *Game) lineReady(s *Session, text string, term byte) {
	switch s.mode {
	case mPregame:
		g.pregameLine(s, text)
	case mSetName, mSetTeam, mSetShip:
		g.setupLine(s, text)
	case mGripe:
		g.gripeLine(s, text, term)
	case mPlay:
		s.lineSeq++
		for _, c := range splitCommands(text) {
			s.cmdq = append(s.cmdq, qcmd{c, s.lineSeq})
		}
	}
}

func (s *Session) gateOK() bool {
	if s.speed >= 9600 && !cfg.Throttle {
		return true
	}
	return s.pending.Load() == 0
}

func (g *Game) pumpAll(now time.Time) {
	for _, s := range g.sessions {
		g.pump(s, now)
		s.flush()
	}
}

func (g *Game) pump(s *Session, now time.Time) {
	if s.closed {
		return
	}
	if s.mode == mPlay {
		for len(s.cmdq) > 0 && s.mode == mPlay && !now.Before(s.busyTil) && s.gateOK() {
			c := s.cmdq[0]
			s.cmdq = s.cmdq[1:]
			g.execute(s, c, now)
		}
		if s.mode == mPlay && len(s.cmdq) == 0 && !now.Before(s.busyTil) && !s.promptSh {
			s.showPrompt()
		}
	} else if s.mode != mGripe && !s.promptSh {
		s.showPrompt()
	}
}

func (g *Game) run() {
	tk := time.NewTicker(100 * time.Millisecond)
	for {
		select {
		case e := <-g.ev:
			g.handle(e)
			g.pumpAll(time.Now())
		case <-tk.C:
			now := time.Now()
			g.tick(now)
			g.pumpAll(now)
		}
	}
}

// ---------------------------------------------------------------------------
// Tokenizing and command resolution (3.4, 3.6, 8.0)
// ---------------------------------------------------------------------------

type Tok struct {
	orig  string
	up    string // upper-case, first 5 characters only (H)
	isNum bool
	n     int
}

func tokenize(line string) []Tok {
	var toks []Tok
	add := func(w string) {
		t := Tok{orig: w}
		u := strings.ToUpper(w)
		if len(u) > 5 {
			u = u[:5]
		}
		t.up = u
		body := w
		if len(body) > 0 && (body[0] == '+' || body[0] == '-') {
			body = body[1:]
		}
		if body != "" && strings.Trim(body, "0123456789") == "" {
			if n, err := strconv.Atoi(w); err == nil && n >= -10000 && n <= 10000 {
				t.isNum, t.n = true, n
			}
		}
		toks = append(toks, t)
	}
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			add(cur.String())
			cur.Reset()
		}
	}
	for _, c := range line {
		switch c {
		case ' ', '\t', ',', ';':
			flush()
		case '&':
			flush()
			add("&")
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return toks
}

// resolve: exact match, else unique prefix.  status 0 ok, 1 none, 2 ambiguous.
func resolve(word string, names []string) (int, int) {
	w := strings.ToUpper(word)
	if len(w) > 5 {
		w = w[:5]
	}
	if w == "" {
		return -1, 1
	}
	found := -1
	count := 0
	for i, n := range names {
		nu := strings.ToUpper(n)
		nt := nu
		if len(nt) > 5 {
			nt = nt[:5]
		}
		if nt == w || nu == w {
			return i, 0
		}
		if strings.HasPrefix(nu, w) {
			found = i
			count++
		}
	}
	switch count {
	case 0:
		return -1, 1
	case 1:
		return found, 0
	}
	return -1, 2
}

func kwIdx(t Tok, set ...string) int {
	i, st := resolve(t.orig, set)
	if st != 0 {
		return -1
	}
	return i
}

// splitCommands: '/' separates commands, ';' starts a comment, TELL swallows
// the remainder of the line (3.6).
func splitCommands(line string) []string {
	var cmds []string
	rest := line
	for len(rest) > 0 {
		trim := strings.TrimLeft(rest, " \t,")
		w := trim
		if i := strings.IndexAny(w, " \t,;/"); i >= 0 {
			w = w[:i]
		}
		if w != "" {
			if i, st := resolve(w, cmdNames()); st == 0 && cmdNames()[i] == "TELL" {
				cmds = append(cmds, strings.TrimSpace(trim))
				break
			}
		}
		k := strings.IndexAny(rest, "/;")
		if k < 0 {
			cmds = append(cmds, rest)
			break
		}
		cmds = append(cmds, rest[:k])
		if rest[k] == ';' {
			break
		}
		rest = rest[k+1:]
	}
	var out []string
	for _, c := range cmds {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

type cmdResult struct {
	pause time.Duration
	timed bool
	usage string
	retry time.Time
}

type Cmd struct {
	name string
	fn   func(g *Game, s *Session, t []Tok, raw string) cmdResult
	use  string
}

var gameCmds []Cmd

func cmdNames() []string {
	n := make([]string, len(gameCmds))
	for i, c := range gameCmds {
		n[i] = c.name
	}
	return n
}

func (g *Game) execute(s *Session, c qcmd, now time.Time) {
	t0 := time.Now()
	defer func() { s.cpu += time.Since(t0) }()
	toks := tokenize(c.text)
	if len(toks) == 0 {
		return
	}
	idx, st := resolve(toks[0].orig, cmdNames())
	if st != 0 {
		if st == 2 {
			s.w("Ambiguous command")
		} else {
			s.w("Invalid command")
		}
		if s.set.output != 0 {
			s.w(".  Type HELP for help.")
		}
		s.w("\n")
		return
	}
	cmd := gameCmds[idx]
	if cmd.name != "TELL" {
		s.lastCmd = c.text
	}
	s.helpProt = false
	r := cmd.fn(g, s, toks, c.text)
	if s.mode != mPlay || s.ship == nil || !s.ship.inGame {
		return
	}
	if r.usage != "" {
		s.w("Syntax error.  Usage: " + r.usage + "\n")
		keep := s.cmdq[:0]
		for _, q := range s.cmdq {
			if q.line != c.line {
				keep = append(keep, q)
			}
		}
		s.cmdq = keep
		return
	}
	if !r.retry.IsZero() {
		s.cmdq = append([]qcmd{c}, s.cmdq...)
		s.busyTil = r.retry
		return
	}
	if r.timed {
		g.timedTurn(s.ship)
	}
	if r.pause > 0 {
		s.busyTil = now.Add(r.pause)
	}
}

// ---------------------------------------------------------------------------
// Commands: helpers (8.0)
// ---------------------------------------------------------------------------

func usage(u string) cmdResult { return cmdResult{usage: u} }

// shipByName: 0..9 ship, -1 Romulan, -2 unknown, -3 ambiguous
func shipByName(word string) int {
	if i, st := resolve(word, shipNames[:]); st == 0 {
		return i
	} else if st == 2 {
		return -3
	}
	w := strings.ToUpper(word)
	if len(w) > 5 {
		w = w[:5]
	}
	if w != "" && strings.HasPrefix("ROMULAN", w) {
		return -1
	}
	return -2
}

// parseMode strips an optional ABSOLUTE/RELATIVE/COMPUTED keyword.
func parseMode(s *Session, t []Tok) (byte, []Tok) {
	mode := byte('A')
	if s.set.icdef == 1 {
		mode = 'R'
	}
	if len(t) > 0 && !t[0].isNum {
		if k := kwIdx(t[0], "ABSOLUTE", "RELATIVE", "COMPUTED"); k >= 0 {
			return "ARC"[k], t[1:]
		}
	}
	return mode, t
}

// coordsFrom: status 0 ok, 1 syntax error, 2 error already reported.
func (g *Game) coordsFrom(s *Session, mode byte, t []Tok, n int) ([][2]int, int) {
	sh := s.ship
	var pts [][2]int
	if mode == 'C' {
		if len(t) != n {
			return nil, 1
		}
		if sh.inop(DComputer) {
			s.w("Computer inoperative.  COMPUTED coordinates are not available.\n")
			return nil, 2
		}
		if !(s.speed < cfg.SlowBPS || cfg.ComputedForAll) {
			s.w("COMPUTED coordinates are available only on slow terminals.\n")
			return nil, 2
		}
		for _, tk := range t {
			if tk.isNum {
				return nil, 1
			}
			i := shipByName(tk.orig)
			switch {
			case i == -3:
				s.w("Ambiguous ship name " + tk.orig + "\n")
				return nil, 2
			case i == -2:
				s.w("No such ship: " + tk.orig + "\n")
				return nil, 2
			case i == -1:
				if !g.rom.alive {
					s.w("Romulan is not in the game.\n")
					return nil, 2
				}
				pts = append(pts, [2]int{g.rom.v, g.rom.h})
			default:
				x := &g.ships[i]
				if !x.inGame {
					s.w(shipNames[i] + " is not in the game.\n")
					return nil, 2
				}
				pts = append(pts, [2]int{x.v, x.h})
			}
		}
		return pts, 0
	}
	if len(t) != 2*n {
		return nil, 1
	}
	for _, tk := range t {
		if !tk.isNum {
			return nil, 1
		}
	}
	bv, bh := 0, 0
	if mode == 'R' {
		bv, bh = sh.v, sh.h
	}
	for i := 0; i < n; i++ {
		v, h := t[2*i].n+bv, t[2*i+1].n+bh
		if !ingal(v, h) {
			s.w("Invalid coordinates.\n")
			return nil, 2
		}
		pts = append(pts, [2]int{v, h})
	}
	return pts, 0
}

func (g *Game) coords(s *Session, t []Tok, n int) ([][2]int, int) {
	mode, rest := parseMode(s, t)
	return g.coordsFrom(s, mode, rest, n)
}

func (g *Game) objName(v, h int) string {
	c := g.cell(v, h)
	switch c.K {
	case KShip:
		return shipNames[c.I]
	case KBase:
		return "starbase"
	case KPlanet:
		return "planet"
	case KStar:
		return "star"
	case KHole:
		return "black hole"
	case KRom:
		return "Romulan"
	}
	return "object"
}

// ---------------------------------------------------------------------------
// MOVE / IMPULSE (8.9, 8.10)
// ---------------------------------------------------------------------------

func cmdMove(g *Game, s *Session, t []Tok, raw string) cmdResult {
	if s.ship.inop(DWarp) {
		s.w("Warp engines inoperative.  Use IMPULSE.\n")
		return cmdResult{}
	}
	pts, st := g.coords(s, t[1:], 1)
	if st == 1 {
		return usage("MOVE [A|R|C] <vpos> <hpos>")
	}
	if st == 2 {
		return cmdResult{}
	}
	return g.doMove(s, pts[0][0], pts[0][1], false)
}

func cmdImpulse(g *Game, s *Session, t []Tok, raw string) cmdResult {
	if s.ship.inop(DImpulse) {
		s.w("Impulse engines inoperative.\n")
		return cmdResult{}
	}
	pts, st := g.coords(s, t[1:], 1)
	if st == 1 {
		return usage("IMPULSE [A|R|C] <vpos> <hpos>")
	}
	if st == 2 {
		return cmdResult{}
	}
	return g.doMove(s, pts[0][0], pts[0][1], true)
}

func (g *Game) doMove(s *Session, tv, th int, impulse bool) cmdResult {
	sh := s.ship
	dv, dh := tv-sh.v, th-sh.h
	if dv == 0 && dh == 0 {
		s.w("You are already there.\n")
		return cmdResult{}
	}
	w := math.Hypot(float64(dv), float64(dh))
	if impulse {
		if cheb(sh.v, sh.h, tv, th) != 1 {
			s.w("Impulse engines move exactly one sector.\n")
			return cmdResult{}
		}
		if !g.empty(tv, th) {
			s.w("That sector is not empty.\n")
			return cmdResult{}
		}
		w = 1
	} else {
		maxw := cfg.MaxWarp
		if sh.degraded(DWarp) {
			maxw = cfg.MaxWarpDamaged
		}
		if w > maxw+1e-9 {
			s.pf("Maximum warp factor is %.0f.\n", maxw)
			return cmdResult{}
		}
	}
	cost := cfg.WarpK * w * w
	if sh.shieldsUp {
		cost *= 2
	}
	if sh.tractor >= 0 {
		cost *= 3
	}
	if cost >= sh.energy {
		s.w("Insufficient energy for that maneuver.\n")
		return cmdResult{}
	}
	sh.energy -= cost
	if !impulse {
		p := 0.0
		if w >= 6 {
			p = 0.15
		} else if w >= 5 {
			p = 0.05
		}
		if s.set.output == 2 && w >= 5 {
			s.w("Warning: engines are being overstressed.\n")
		}
		if rng.Float64() < p {
			d := uni(20, 120)
			sh.dev[DWarp] += d
			s.pf("Warp engines damaged by overheating: %.1f units.\n", d)
		}
	}
	sh.red = false
	sh.docked = false
	// travel along the line, sector by sector (Bresenham-like)
	n := max(iabs(dv), iabs(dh))
	path := [][2]int{{sh.v, sh.h}}
	var blocked Cell
	hole := false
	for i := 1; i <= n; i++ {
		v := sh.v + int(math.Round(float64(dv*i)/float64(n)))
		h := sh.h + int(math.Round(float64(dh*i)/float64(n)))
		c := g.grid[v][h]
		if c.K == KHole {
			path = append(path, [2]int{v, h})
			hole = true
			break
		}
		if c.K != KEmpty {
			blocked = c
			break
		}
		path = append(path, [2]int{v, h})
	}
	oldV, oldH := sh.v, sh.h
	fin := path[len(path)-1]
	// inexact navigation with a damaged computer (7.2)
	if !hole && blocked.K == KEmpty && !impulse {
		e := 0
		if sh.inop(DComputer) {
			e = 2
		} else if sh.degraded(DComputer) && rng.Float64() < sh.dev[DComputer]/cfg.DeviceInop {
			e = 1
		}
		if e > 0 {
			for try := 0; try < 8; try++ {
				v, h := fin[0]+rng.Intn(2*e+1)-e, fin[1]+rng.Intn(2*e+1)-e
				if (v != oldV || h != oldH) && g.empty(v, h) {
					fin = [2]int{v, h}
					s.w("Navigation is inexact; the computer is damaged.\n")
					break
				}
			}
		}
	}
	if fin[0] != oldV || fin[1] != oldH {
		g.clearCell(oldV, oldH)
		sh.v, sh.h = fin[0], fin[1]
		if hole {
			g.destroyShip(sh, "Your ship has been pulled into a black hole!", Actor{})
			return cmdResult{}
		}
		g.setCell(sh.v, sh.h, KShip, sh.idx)
		if sh.tractor >= 0 {
			p := &g.ships[sh.tractor]
			trail := [2]int{oldV, oldH}
			if len(path) >= 2 && fin == path[len(path)-1] {
				trail = path[len(path)-2]
			}
			if trail == [2]int{p.v, p.h} {
			} else if g.empty(trail[0], trail[1]) {
				g.clearCell(p.v, p.h)
				p.v, p.h = trail[0], trail[1]
				g.setCell(p.v, p.h, KShip, p.idx)
				p.docked = false
				g.sendShip(p, fmt.Sprintf("You are towed to %d-%d.", p.v, p.h))
			} else {
				g.breakTractor(sh)
			}
		}
	}
	if blocked.K != KEmpty {
		bv, bh := path[len(path)-1][0], path[len(path)-1][1]
		_ = bv
		_ = bh
		s.pf("Your path was blocked by a %s; you stop at %s.\n", g.objNameCell(blocked), s.loc(sh.v, sh.h, false))
	}
	if sh.energy <= cfg.YellowEnergy {
		s.bell(1)
	}
	return cmdResult{pause: g.slowPause(2, 4), timed: true}
}

func (g *Game) objNameCell(c Cell) string {
	switch c.K {
	case KShip:
		return shipNames[c.I]
	case KBase:
		return "starbase"
	case KPlanet:
		return "planet"
	case KStar:
		return "star"
	case KRom:
		return "Romulan"
	}
	return "object"
}

// ---------------------------------------------------------------------------
// SHIELDS (8.20)
// ---------------------------------------------------------------------------

func cmdShields(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	u := "SHIELDS Up | Down | Transfer <energy>"
	if len(t) < 2 {
		return usage(u)
	}
	switch kwIdx(t[1], "UP", "DOWN", "TRANSFER") {
	case 0:
		if sh.inop(DShield) {
			s.w("Deflector shields inoperative.\n")
			return cmdResult{}
		}
		if sh.shieldsUp {
			s.w("Shields are already up.\n")
			return cmdResult{}
		}
		if sh.energy <= 100 {
			s.w("Insufficient energy to raise shields.\n")
			return cmdResult{}
		}
		sh.energy -= 100
		sh.shieldsUp = true
		g.breakTractor(sh)
		s.pf("Shields raised.  Shield strength %s\n", pctStr(sh.shieldPct(), 2))
	case 1:
		sh.shieldsUp = false
		s.w("Shields lowered.\n")
	case 2:
		if len(t) != 3 || !t[2].isNum {
			return usage(u)
		}
		n := float64(t[2].n)
		if n >= 0 {
			amt := math.Min(n, math.Min(sh.energy-1, cfg.ShieldEnergy-sh.shieldE))
			amt = math.Max(amt, 0)
			sh.energy -= amt
			sh.shieldE += amt
		} else {
			amt := math.Min(-n, math.Min(sh.shieldE, cfg.ShipEnergy-sh.energy))
			amt = math.Max(amt, 0)
			sh.energy += amt
			sh.shieldE -= amt
		}
		if sh.shieldE <= 0 {
			sh.shieldsUp = false
		}
		s.pf("Shield energy %.1f units (%s), ship energy %.1f\n", sh.shieldE, pctStr(signedPct(sh), 2), sh.energy)
	default:
		return usage(u)
	}
	return cmdResult{}
}

// ---------------------------------------------------------------------------
// STATUS / DAMAGES (8.21, 8.4)
// ---------------------------------------------------------------------------

var statKeys = []string{"CONDITION", "LOCATION", "TORPEDOES", "ENERGY", "DAMAGE", "SHIELDS", "RADIO"}

func (g *Game) statusReport(s *Session, t []Tok) bool {
	sh := s.ship
	var want []int
	if len(t) == 0 {
		want = []int{0, 1, 2, 3, 4, 5, 6}
	} else {
		for _, tk := range t {
			k := kwIdx(tk, statKeys...)
			if k < 0 {
				return false
			}
			want = append(want, k)
		}
	}
	st := s.set.output
	sd := g.stardate(time.Now())
	condStr := []string{"GREEN", "YELLOW", "RED"}[sh.cond()]
	if sh.docked {
		condStr += " (docked)"
	}
	radio := "On"
	if sh.inop(DRadio) {
		radio = "Inoperative"
	} else if !sh.radioOn {
		radio = "Off"
	}
	if st == 0 {
		var parts []string
		parts = append(parts, fmt.Sprintf("SD%.1f", sd))
		for _, k := range want {
			switch k {
			case 0:
				parts = append(parts, "C"+condStr[:1])
			case 1:
				parts = append(parts, fmt.Sprintf("L%d-%d", sh.v, sh.h))
			case 2:
				parts = append(parts, fmt.Sprintf("T%d", sh.torps))
			case 3:
				parts = append(parts, fmt.Sprintf("E%d", int(sh.energy)))
			case 4:
				parts = append(parts, fmt.Sprintf("D%d", int(sh.damage)))
			case 5:
				parts = append(parts, fmt.Sprintf("SH%s", pctStr(signedPct(sh), 0)))
			case 6:
				parts = append(parts, "R"+radio[:2])
			}
		}
		s.w(strings.Join(parts, " ") + "\n")
		return true
	}
	if st == 2 {
		s.pf("Stardate      %.1f\n", sd)
	} else {
		s.pf("Stardate  %.1f\n", sd)
	}
	for _, k := range want {
		switch k {
		case 0:
			s.pf("Condition     %s\n", condStr)
		case 1:
			s.pf("Location      %s\n", locFmt(sh.v, sh.h, sh.v, sh.h, 0, false))
		case 2:
			s.pf("Torpedoes     %d\n", sh.torps)
		case 3:
			s.pf("Energy        %.1f\n", sh.energy)
		case 4:
			s.pf("Damage        %.1f\n", sh.damage)
		case 5:
			ud := "down"
			if sh.shieldsUp {
				ud = "up"
			}
			s.pf("Shields       %s  %.1f units (%s)\n", pctStr(signedPct(sh), 2), sh.shieldE, ud)
		case 6:
			s.pf("Radio         %s\n", radio)
		}
	}
	return true
}

func cmdStatus(g *Game, s *Session, t []Tok, raw string) cmdResult {
	if !g.statusReport(s, t[1:]) {
		return usage("STATUS [Condition|Location|Torpedoes|Energy|Damage|Shields|Radio]")
	}
	return cmdResult{}
}

func (g *Game) damageReport(s *Session, t []Tok) bool {
	sh := s.ship
	var which []int
	if len(t) > 0 {
		for _, tk := range t {
			k := kwIdx(tk, devKeys[:]...)
			if k < 0 {
				return false
			}
			which = append(which, k)
		}
	} else {
		for i := 0; i < NDev; i++ {
			if sh.dev[i] > 0 {
				which = append(which, i)
			}
		}
		if len(which) == 0 {
			s.w("All devices are operational.\n")
			return true
		}
		if s.set.output == 2 {
			s.pf("Damage report for %s:\n", shipNames[sh.idx])
		}
	}
	for _, i := range which {
		note := ""
		if s.set.output == 2 {
			if sh.inop(i) {
				note = "  (inoperative)"
			} else if sh.dev[i] > 0 {
				note = "  (degraded)"
			}
		}
		s.pf("  %-18s %7.1f units%s\n", devNames[i], sh.dev[i], note)
	}
	return true
}

func cmdDamages(g *Game, s *Session, t []Tok, raw string) cmdResult {
	if !g.damageReport(s, t[1:]) {
		return usage("DAMAGES [device names]")
	}
	return cmdResult{}
}

// ---------------------------------------------------------------------------
// DOCK (8.5)
// ---------------------------------------------------------------------------

func cmdDock(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	f := float64(g.adjacentPorts(sh))
	if f == 0 {
		s.w("Not adjacent to a friendly base or planet.\n")
		return cmdResult{}
	}
	dmgStep := 50 * f
	if sh.docked {
		dmgStep = 100 * f
	}
	sh.energy = math.Min(cfg.ShipEnergy, sh.energy+500*f)
	sh.shieldE = math.Min(cfg.ShieldEnergy, sh.shieldE+250*f)
	sh.torps = cfg.TorpsMax
	if int(5*f)+0 < 0 {
		sh.torps = 0
	}
	sh.life = cfg.LifeReserve
	sh.damage = math.Max(0, sh.damage-dmgStep)
	sh.docked = true
	sh.red = false
	s.w("Docking complete.\n")
	if len(t) > 1 {
		if kwIdx(t[1], "STATUS") == 0 {
			g.statusReport(s, t[2:])
		}
	}
	return cmdResult{pause: g.slowPause(2, 4), timed: true}
}

// ---------------------------------------------------------------------------
// ENERGY (8.6)
// ---------------------------------------------------------------------------

func cmdEnergy(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	u := "ENERGY <ship> <units>"
	if len(t) != 3 || t[1].isNum || !t[2].isNum {
		return usage(u)
	}
	i := shipByName(t[1].orig)
	if i < 0 {
		s.w("No such ship.\n")
		return cmdResult{}
	}
	if i == sh.idx {
		s.w("You cannot transfer energy to yourself.\n")
		return cmdResult{}
	}
	r := &g.ships[i]
	if !r.inGame {
		s.w(shipNames[i] + " is not in the game.\n")
		return cmdResult{}
	}
	if r.team != sh.team {
		s.w("You cannot transfer energy to an enemy ship.\n")
		return cmdResult{}
	}
	if cheb(sh.v, sh.h, r.v, r.h) > 1 {
		s.w(shipNames[i] + " is not adjacent.\n")
		return cmdResult{}
	}
	units := float64(t[2].n)
	if units <= 0 {
		s.w("Energy transfer must be positive.\n")
		return cmdResult{}
	}
	if units >= sh.energy {
		s.w("Insufficient energy for that transfer.\n")
		return cmdResult{}
	}
	gain := units * 0.9
	if r.energy+gain > cfg.ShipEnergy {
		gain = cfg.ShipEnergy - r.energy
		units = gain / 0.9
	}
	if gain <= 0 {
		s.w(shipNames[i] + " is already at full energy.\n")
		return cmdResult{}
	}
	sh.energy -= units
	r.energy += gain
	s.pf("Transferred %.1f units to %s (%.1f received).\n", units, shipNames[i], gain)
	g.sendShip(r, fmt.Sprintf("%s transfers %.1f units of energy to you.", shipNames[sh.idx], gain))
	return cmdResult{}
}

// ---------------------------------------------------------------------------
// REPAIR (8.17)
// ---------------------------------------------------------------------------

func cmdRepair(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	units := 50.0
	if sh.docked {
		units = 100
	}
	rest := t[1:]
	if len(rest) > 0 && rest[0].isNum {
		if rest[0].n <= 0 {
			return usage("REPAIR [units | ALL] [DAMAGES]")
		}
		units = float64(rest[0].n)
		rest = rest[1:]
	} else if len(rest) > 0 && kwIdx(rest[0], "ALL") == 0 {
		mx := 0.0
		for _, d := range sh.dev {
			mx = math.Max(mx, d)
		}
		units = mx
		rest = rest[1:]
	}
	showDmg := false
	if len(rest) > 0 {
		if kwIdx(rest[0], "DAMAGES") != 0 {
			return usage("REPAIR [units | ALL] [DAMAGES]")
		}
		showDmg = true
	}
	g.repairDevices(sh, units)
	pause := time.Duration(0.08 * units * float64(time.Second))
	if sh.docked {
		pause /= 2
	}
	s.pf("Repairs of %.0f units applied to each damaged device.\n", units)
	if showDmg {
		g.damageReport(s, nil)
	}
	return cmdResult{pause: pause, timed: true}
}

// ---------------------------------------------------------------------------
// BUILD / CAPTURE (8.2, 8.3)
// ---------------------------------------------------------------------------

func (g *Game) adjacentPlanet(s *Session, t []Tok, u string) (int, cmdResult, bool) {
	sh := s.ship
	pts, st := g.coords(s, t, 1)
	if st == 1 {
		return 0, usage(u), false
	}
	if st == 2 {
		return 0, cmdResult{}, false
	}
	v, h := pts[0][0], pts[0][1]
	if cheb(sh.v, sh.h, v, h) > 1 || (v == sh.v && h == sh.h) {
		s.w("Target is not adjacent to your ship.\n")
		return 0, cmdResult{}, false
	}
	c := g.grid[v][h]
	if c.K != KPlanet {
		s.w("There is no planet at " + locFmt(v, h, sh.v, sh.h, 0, false) + ".\n")
		return 0, cmdResult{}, false
	}
	return c.I, cmdResult{}, true
}

func cmdBuild(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	pi, r, ok := g.adjacentPlanet(s, t[1:], "BUILD [A|R] <vpos> <hpos>")
	if !ok {
		return r
	}
	p := &g.planets[pi]
	if p.owner != sh.team {
		s.w("That planet has not been captured by your team.\n")
		return cmdResult{}
	}
	if p.builds >= 4 {
		if g.totalBases() >= cfg.BasesMaxTotal {
			s.w("Starbase limit reached.\n")
			return cmdResult{}
		}
		slot := -1
		for i := range g.bases {
			if !g.bases[i].alive {
				slot = i
				break
			}
		}
		nb := Base{v: p.v, h: p.h, team: sh.team, shieldE: cfg.ShieldEnergy, alive: true, pristine: true}
		nb.known[sh.team] = true
		if slot < 0 {
			g.bases = append(g.bases, nb)
			slot = len(g.bases) - 1
		} else {
			g.bases[slot] = nb
		}
		p.alive = false
		g.setCell(p.v, p.h, KBase, slot)
		sh.score.Built++
		g.teamScore[sh.team].Built++
		s.pf("A new starbase has been completed at %s!  (+1000 points)\n", locFmt(p.v, p.h, sh.v, sh.h, 0, false))
		g.teamRadio(sh.team, func(rs *Session) string {
			return fmt.Sprintf("New starbase at %s", rs.locAbs(p.v, p.h))
		})
	} else {
		p.builds++
		s.pf("Planet now has %d build", p.builds)
		if p.builds != 1 {
			s.w("s")
		}
		s.w(".\n")
	}
	return cmdResult{pause: time.Duration(uni(5, 7) * float64(time.Second)), timed: true}
}

func cmdCapture(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	pi, r, ok := g.adjacentPlanet(s, t[1:], "CAPTURE [A|R] <vpos> <hpos>")
	if !ok {
		return r
	}
	p := &g.planets[pi]
	if p.owner == sh.team {
		s.w("That planet is already yours.\n")
		return cmdResult{}
	}
	cost := 50 * float64(p.builds)
	if cost >= sh.energy {
		s.w("Insufficient energy to capture that planet.\n")
		return cmdResult{}
	}
	sh.energy -= cost
	// the planet defends itself with a last phaser blast
	att := Actor{KPlanet, pi, p.owner}
	X := float64(100+60*p.builds) / math.Max(1, euclid(sh.v, sh.h, p.v, p.h))
	old := p.owner
	hit := g.hitShip(att, nil, p.v, p.h, sh, X, HPhaser)
	if hit != nil {
		hit.srcBuilds = p.builds
		g.creditHit(att, hit.dst, hit.applied)
		g.deliverHit(hit)
		if hit.killed {
			g.destroyShip(sh, "Your ship was destroyed by the planet's defenses!", att)
			return cmdResult{}
		}
	}
	pause := 5*time.Second + time.Duration(p.builds)*time.Second
	p.owner = sh.team
	p.builds = 0
	p.known[sh.team] = true
	sh.score.Captured++
	g.teamScore[sh.team].Captured++
	if old != NEU {
		g.checkDocked(old)
	}
	s.pf("Planet at %s captured for the %s!  (+100 points)\n", locFmt(p.v, p.h, sh.v, sh.h, 0, false),
		map[int]string{FED: "Federation", EMP: "Empire"}[sh.team])
	g.checkEnd()
	return cmdResult{pause: pause, timed: true}
}

// ---------------------------------------------------------------------------
// TRACTOR (8.25)
// ---------------------------------------------------------------------------

func cmdTractor(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	if len(t) == 1 || (len(t) == 2 && kwIdx(t[1], "OFF") == 0) {
		if sh.tractor < 0 {
			s.w("Tractor beam is not active.\n")
			return cmdResult{}
		}
		g.breakTractor(sh)
		return cmdResult{}
	}
	if len(t) != 2 || t[1].isNum {
		return usage("TRACTOR <ship> | OFF")
	}
	if sh.inop(DTractor) {
		s.w("Tractor beam inoperative.\n")
		return cmdResult{}
	}
	i := shipByName(t[1].orig)
	if i < 0 {
		s.w("No such ship.\n")
		return cmdResult{}
	}
	r := &g.ships[i]
	switch {
	case sh.tractor >= 0:
		s.w("Tractor beam is already active.\n")
	case i == sh.idx:
		s.w("You cannot tractor your own ship.\n")
	case !r.inGame:
		s.w(shipNames[i] + " is not in the game.\n")
	case r.team != sh.team:
		s.w("You cannot tractor an enemy ship.\n")
	case cheb(sh.v, sh.h, r.v, r.h) > 1:
		s.w(shipNames[i] + " is not adjacent.\n")
	case r.tractor >= 0:
		s.w(shipNames[i] + " already has an active tractor beam.\n")
	case sh.shieldsUp:
		s.w("Your shields must be down to use the tractor beam.\n")
	case r.shieldsUp:
		s.w(shipNames[i] + " must lower its shields first.\n")
	case r.inop(DTractor):
		s.w(shipNames[i] + "'s tractor beam is inoperative.\n")
	default:
		sh.tractor, r.tractor = i, sh.idx
		s.pf("Tractor beam locked on %s.\n", shipNames[i])
		g.sendShip(r, fmt.Sprintf("%s has locked a tractor beam on your ship.", shipNames[sh.idx]))
	}
	return cmdResult{}
}

// ---------------------------------------------------------------------------
// PHASERS (8.13)
// ---------------------------------------------------------------------------

func cmdPhasers(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	u := "PHASERS [A|R|C] [energy] <vpos> <hpos>"
	if sh.inop(DPhaser) {
		s.w("Phaser banks inoperative.\n")
		return cmdResult{}
	}
	mode, rest := parseMode(s, t[1:])
	E := cfg.PhaserDefault
	explicit := false
	if mode == 'C' {
		if len(rest) == 2 && rest[0].isNum {
			E, explicit, rest = float64(rest[0].n), true, rest[1:]
		}
	} else if len(rest) == 3 && rest[0].isNum {
		E, explicit, rest = float64(rest[0].n), true, rest[1:]
	}
	pts, st := g.coordsFrom(s, mode, rest, 1)
	if st == 1 {
		return usage(u)
	}
	if st == 2 {
		return cmdResult{}
	}
	tv, th := pts[0][0], pts[0][1]
	if explicit && (E < cfg.PhaserMin || E > cfg.PhaserMax) {
		s.pf("Improper size for phaser blast (%.0f to %.0f).\n", cfg.PhaserMin, cfg.PhaserMax)
		return cmdResult{}
	}
	c := g.grid[tv][th]
	if c.K != KShip && c.K != KBase && c.K != KPlanet && c.K != KRom {
		s.w("There is nothing to fire at there.\n")
		return cmdResult{}
	}
	if tv == sh.v && th == sh.h {
		s.w("You cannot fire at your own location.\n")
		return cmdResult{}
	}
	friendly := false
	switch c.K {
	case KShip:
		friendly = g.ships[c.I].team == sh.team
	case KBase:
		friendly = g.bases[c.I].team == sh.team
	case KPlanet:
		friendly = g.planets[c.I].owner == sh.team
	}
	if friendly {
		s.pf("Weapons officer cancels phaser blast at friendly %s.\n", g.objName(tv, th))
		return cmdResult{}
	}
	if cheb(sh.v, sh.h, tv, th) > cfg.PhaserRange {
		s.w("Target out of range.\n")
		return cmdResult{}
	}
	cost := E
	if sh.shieldsUp {
		cost += 200
	}
	if cost >= sh.energy {
		s.w("Insufficient energy for that phaser blast.\n")
		return cmdResult{}
	}
	now := time.Now()
	bank := -1
	for b := 0; b < 2; b++ {
		if !now.Before(sh.phReady[b]) && (bank < 0 || sh.phReady[b].Before(sh.phReady[bank])) {
			bank = b
		}
	}
	if bank < 0 {
		s.w("Phasers still cooling.\n")
		return cmdResult{}
	}
	sh.energy -= cost
	sh.red = true
	// overheating risk
	p := 0.05
	if E >= 200 {
		p = 0.05 + 0.60*((E-200)/300)
	}
	if rng.Float64() < p {
		d := uni(0.2, 1.0) * E / 2
		sh.dev[DPhaser] += d
		s.pf("Phasers overheated!  Phaser banks damaged %.1f units.\n", d)
		if rng.Float64() < 0.10 {
			dv := rng.Intn(NDev)
			sh.dev[dv] += d / 2
			s.pf("%s also damaged.\n", devNames[dv])
		}
	}
	d := math.Max(1, euclid(sh.v, sh.h, tv, th))
	X := E * 0.9 / d * clampf(1-sh.dev[DPhaser]/cfg.DeviceInop, 0, 1) * clampf(1-sh.dev[DComputer]/(2*cfg.DeviceInop), 0, 1)
	sh.phReady[bank] = now.Add(g.slowPause(3, 6) + time.Duration(sh.dev[DPhaser]/100*float64(time.Second)))
	att := Actor{KShip, sh.idx, sh.team}
	if c.K == KPlanet {
		// planets: chance of losing a build scales with blast energy
		r := float64(1 + rng.Intn(100))
		chance := 0.0
		if r*E/(25*d) > 150 {
			chance = 1
		}
		g.planetHit(att, sh, sh.v, sh.h, c.I, X, HPhaser, chance)
	} else {
		g.phaserHitCell(att, sh, sh.v, sh.h, tv, th, X)
	}
	return cmdResult{timed: true}
}

// ---------------------------------------------------------------------------
// TORPEDOES (8.24)
// ---------------------------------------------------------------------------

func cmdTorp(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	u := "TORPEDOES [A|R|C] <n> <vpos> <hpos> [<vpos> <hpos> [<vpos> <hpos>]]"
	if sh.inop(DTorp) {
		s.w("Photon tubes inoperative.\n")
		return cmdResult{}
	}
	mode, rest := parseMode(s, t[1:])
	if len(rest) < 2 || !rest[0].isNum {
		return usage(u)
	}
	n := rest[0].n
	rest = rest[1:]
	var k int
	if mode == 'C' {
		k = len(rest)
	} else {
		if len(rest)%2 != 0 {
			return usage(u)
		}
		k = len(rest) / 2
	}
	if k < 1 || k > 3 || n < 1 || n > 3 || k > n {
		return usage(u)
	}
	pts, st := g.coordsFrom(s, mode, rest, k)
	if st == 1 {
		return usage(u)
	}
	if st == 2 {
		return cmdResult{}
	}
	if sh.torps <= 0 {
		s.w("No photon torpedoes remaining.\n")
		return cmdResult{}
	}
	if n > sh.torps {
		s.pf("Only %d torpedoes remaining.\n", sh.torps)
		return cmdResult{}
	}
	for _, p := range pts {
		if p[0] == sh.v && p[1] == sh.h {
			s.w("You cannot fire at your own location.\n")
			return cmdResult{}
		}
		if cheb(sh.v, sh.h, p[0], p[1]) > cfg.PhaserRange {
			s.w("Target out of range.\n")
			return cmdResult{}
		}
	}
	now := time.Now()
	if now.Before(sh.torpReady) {
		return cmdResult{retry: sh.torpReady}
	}
	counts := make([]int, k)
	for i := range counts {
		counts[i] = 1
	}
	counts[k-1] += n - k
	sh.red = true
	att := Actor{KShip, sh.idx, sh.team}
	num := 0
	misfire := false
	for ti, p := range pts {
		for c := 0; c < counts[ti] && !misfire && sh.inGame; c++ {
			num++
			if rng.Float64() < 0.02+sh.dev[DTorp]/1000 {
				s.pf("Torpedo %d misfired!  The rest of the burst is aborted.\n", num)
				if rng.Float64() < 0.2 {
					sh.dev[DTorp] += uni(50, 300)
					s.w("The photon tubes are damaged.\n")
				}
				if !sh.docked {
					sh.torps--
				}
				misfire = true
				break
			}
			if !sh.docked {
				sh.torps--
			}
			sigma := 1.0
			if mode == 'C' {
				sigma = 0.5
			}
			if sh.shieldsUp {
				sigma += 0.03 * sh.shieldPct()
			}
			sigma += 0.05*sh.dev[DComputer] + 0.05*sh.dev[DTorp]
			g.flyTorpedo(att, sh, sh.v, sh.h, p[0], p[1], sigma, num)
		}
	}
	if sh.inGame {
		sh.torpReady = now.Add(time.Duration(num) * (g.slowPause(2, 4) + time.Duration(sh.dev[DTorp]/100*float64(time.Second))))
	}
	return cmdResult{timed: true}
}

// ---------------------------------------------------------------------------
// RADIO / TELL (8.16, 8.22)
// ---------------------------------------------------------------------------

func cmdRadio(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	u := "RADIO ON | OFf | Gag <ship> | Ungag <ship>"
	if len(t) < 2 {
		return usage(u)
	}
	switch kwIdx(t[1], "ON", "OFF", "GAG", "UNGAG") {
	case 0:
		sh.radioOn = true
		s.w("Radio is on.\n")
	case 1:
		sh.radioOn = false
		s.w("Radio is off.\n")
	case 2, 3:
		gag := kwIdx(t[1], "ON", "OFF", "GAG", "UNGAG") == 2
		if len(t) != 3 || t[2].isNum {
			return usage(u)
		}
		i := shipByName(t[2].orig)
		if i < 0 {
			s.w("No such ship.\n")
			return cmdResult{}
		}
		if i == sh.idx {
			return cmdResult{}
		}
		sh.gagged[i] = gag
		if gag {
			s.pf("Messages from %s are now gagged.\n", shipNames[i])
		} else {
			s.pf("Messages from %s are no longer gagged.\n", shipNames[i])
		}
	default:
		return usage(u)
	}
	return cmdResult{}
}

func cmdTell(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	u := "TELL <recipients> ; <message>"
	semi := strings.IndexByte(raw, ';')
	if semi < 0 {
		return usage(u)
	}
	msg := strings.TrimSpace(raw[semi+1:])
	if len(msg) > cfg.MsgMax {
		msg = msg[:cfg.MsgMax]
	}
	head := raw[:semi]
	toks := tokenize(head)
	if len(toks) < 2 || msg == "" {
		return usage(u)
	}
	if sh.inop(DRadio) {
		s.w("Sub-space radio inoperative.\n")
		return cmdResult{}
	}
	if !sh.radioOn {
		s.w("Your radio is off.\n")
		return cmdResult{}
	}
	var dest [10]bool
	groups := []string{"ALL", "FEDERATION", "HUMAN", "EMPIRE", "KLINGON", "ENEMY", "FRIENDLY"}
	for _, tk := range toks[1:] {
		if i := shipByName(tk.orig); i >= 0 {
			dest[i] = true
			continue
		} else if i == -1 {
			s.w("The Romulan cannot be reached by radio.\n")
			continue
		}
		gi, gs := resolve(tk.orig, groups)
		if gs == 2 {
			s.w("Ambiguous recipient " + tk.orig + "\n")
			return cmdResult{}
		}
		if gs == 1 {
			s.w("Unrecognized recipient " + tk.orig + "\n")
			return cmdResult{}
		}
		for j := 0; j < 10; j++ {
			tm := teamOfShip(j)
			ok := false
			switch groups[gi] {
			case "ALL":
				ok = true
			case "FEDERATION", "HUMAN":
				ok = tm == FED
			case "EMPIRE", "KLINGON":
				ok = tm == EMP
			case "ENEMY":
				ok = tm != sh.team
			case "FRIENDLY":
				ok = tm == sh.team
			}
			if ok {
				dest[j] = true
			}
		}
	}
	got := 0
	for j := 0; j < 10; j++ {
		if !dest[j] || j == sh.idx {
			continue
		}
		r := &g.ships[j]
		if !r.inGame || r.sess == nil || !r.radioOn || r.inop(DRadio) || r.gagged[sh.idx] {
			continue
		}
		r.sess.async(fmt.Sprintf("From %s: %s", shipNames[sh.idx], msg))
		got++
	}
	if got == 0 {
		s.w("No one received your message.\n")
	}
	return cmdResult{}
}

// ---------------------------------------------------------------------------
// SET / TYPE (8.19, 8.26)
// ---------------------------------------------------------------------------

var ttyTypes = []string{"CRT", "ADM-3A", "ADM-2", "SOROC", "BEEHIVE", "ACT-IV", "ACT-V", "DATAPOINT"}

func cmdSet(g *Game, s *Session, t []Tok, raw string) cmdResult {
	u := "SET NAme|Output|SCan|Prompt|OCdef|Icdef|Ttytype <value>"
	if len(t) < 3 {
		return usage(u)
	}
	val := t[2]
	switch kwIdx(t[1], "NAME", "OUTPUT", "SCAN", "SCANS", "PROMPT", "OCDEF", "ICDEF", "TTYTYPE") {
	case 0:
		// everything after the keyword, case preserved
		f := strings.Fields(raw)
		name := strings.Join(f[2:], " ")
		if len(name) > 12 {
			name = name[:12]
		}
		s.captain = name
		if s.ship != nil {
			s.ship.captain = name
		}
		s.pf("Captain name set to %s.\n", name)
	case 1:
		k := kwIdx(val, "SHORT", "MEDIUM", "LONG")
		if k < 0 {
			return usage("SET OUTPUT Short|Medium|Long")
		}
		s.set.output = k
	case 2, 3:
		k := kwIdx(val, "SHORT", "LONG")
		if k < 0 {
			return usage("SET SCAN Short|Long")
		}
		s.set.scan = k
	case 4:
		k := kwIdx(val, "NORMAL", "INFORMATIVE")
		if k < 0 {
			return usage("SET PROMPT Normal|Informative")
		}
		s.set.prompt = k
	case 5:
		k := kwIdx(val, "ABSOLUTE", "RELATIVE", "BOTH")
		if k < 0 {
			return usage("SET OCDEF Absolute|Relative|Both")
		}
		s.set.ocdef = k
	case 6:
		k := kwIdx(val, "ABSOLUTE", "RELATIVE")
		if k < 0 {
			return usage("SET ICDEF Absolute|Relative")
		}
		s.set.icdef = k
	case 7:
		k := kwIdx(val, ttyTypes...)
		if k < 0 {
			s.w("Terminal types: " + strings.Join(ttyTypes, ", ") + "\n")
			return cmdResult{}
		}
		s.set.tty = ttyTypes[k] // accepted, does nothing (H)
	default:
		return usage(u)
	}
	return cmdResult{}
}

func cmdType(g *Game, s *Session, t []Tok, raw string) cmdResult {
	if len(t) < 2 {
		return usage("TYPE OPtion | OUtput")
	}
	if t[1].up == "O" {
		s.w("Ambiguous switch.\n")
		return cmdResult{}
	}
	switch kwIdx(t[1], "OPTION", "OUTPUT") {
	case 0:
		g.typeOption(s)
	case 1:
		o := []string{"SHORT", "MEDIUM", "LONG"}[s.set.output]
		s.pf("%s output messages\n", o)
		s.pf("%s command prompt\n", []string{"NORMAL", "INFORMATIVE"}[s.set.prompt])
		s.pf("%s scans\n", []string{"SHORT", "LONG"}[s.set.scan])
		s.pf("%s input coordinate default\n", []string{"ABSOLUTE", "RELATIVE"}[s.set.icdef])
		s.pf("%s output coordinates\n", []string{"ABSOLUTE", "RELATIVE", "BOTH"}[s.set.ocdef])
		s.pf("Terminal type is %s\n", s.set.tty)
	default:
		return usage("TYPE OPtion | OUtput")
	}
	return cmdResult{}
}

func (g *Game) typeOption(s *Session) {
	s.w(versionStr + "\n")
	s.w("Romulans are included in this game.\n")
	if g.blackholes {
		s.w("Black holes are included in this game.\n")
	} else {
		s.w("Black holes are not included in this game.\n")
	}
}

// ---------------------------------------------------------------------------
// TIME / USERS / QUIT (8.23, 8.27, 8.15)
// ---------------------------------------------------------------------------

func cmdTime(g *Game, s *Session, t []Tok, raw string) cmdResult {
	now := time.Now()
	s.pf("Elapsed time of game:      %s\n", fmtDur(now.Sub(g.start)))
	if s.ship != nil {
		s.pf("Elapsed time of ship:      %s\n", fmtDur(now.Sub(s.ship.commAt)))
		s.pf("Run time of ship:          %s\n", fmtDur(s.cpu-s.cpuShip))
	}
	s.pf("Total run time:            %s\n", fmtDur(s.cpu))
	s.pf("Present time:              %s\n", now.Format("15:04:05.0"))
	return cmdResult{}
}

func cmdUsers(g *Game, s *Session, t []Tok, raw string) cmdResult {
	st := s.set.output
	switch st {
	case 2:
		s.w("Ship        Captain       Speed  PPN       TTY  Job\n")
	case 1:
		s.w("Ship  Captain       Speed  PPN\n")
	}
	any := false
	for i := range g.ships {
		x := &g.ships[i]
		if i == 5 && any {
			s.w("\n")
		}
		if !x.inGame {
			continue
		}
		any = true
		spd, tty, job := 0, 0, 0
		if x.sess != nil {
			spd, tty, job = x.sess.speed, x.sess.connNo, x.sess.id
		}
		switch st {
		case 2:
			s.pf("%-11s %-12s %6d  %-8d %4d %4d\n", shipNames[i], x.captain, spd, x.uid, tty, job)
		case 1:
			s.pf("%-5s %-12s %6d  %-8d\n", string(shipLetters[i]), x.captain, spd, x.uid)
		default:
			s.pf("%s %s\n", string(shipLetters[i]), x.captain)
		}
	}
	if !any {
		s.w("No ships are in the game.\n")
	}
	return cmdResult{}
}

func cmdQuit(g *Game, s *Session, t []Tok, raw string) cmdResult {
	sh := s.ship
	g.printPoints(s, true, false, false, false, true)
	g.leaveShip(sh, true)
	g.toPregame(s)
	s.pf("\nYou have left the game.  You may not rejoin for %d seconds.\n", int(cfg.QuitLock.Seconds()))
	return cmdResult{}
}

// ---------------------------------------------------------------------------
// POINTS (8.14)
// ---------------------------------------------------------------------------

var pointRows = []string{"Damage to enemies", "Enemies destroyed", "Damage to bases", "Planets captured",
	"Bases built", "Romulans damaged/destroyed", "Stars destroyed", "Planets destroyed"}

func (g *Game) printPoints(s *Session, me, fed, emp, rom bool, final bool) {
	type col struct {
		name string
		sc   *Score
		team int
		ship *Ship
	}
	var cols []col
	if me && s.ship != nil {
		cols = append(cols, col{shipNames[s.ship.idx], &s.ship.score, 0, s.ship})
	}
	if fed {
		cols = append(cols, col{"Federation", &g.teamScore[FED], FED, nil})
	}
	if emp {
		cols = append(cols, col{"Empire", &g.teamScore[EMP], EMP, nil})
	}
	if rom {
		cols = append(cols, col{"Romulan", &g.teamScore[ROMT], ROMT, nil})
	}
	if len(cols) == 0 {
		return
	}
	s.w("\n" + strings.Repeat(" ", 28))
	for _, c := range cols {
		s.pf("%12s", c.name)
	}
	s.w("\n")
	for r := 0; r < 8; r++ {
		nz := false
		for _, c := range cols {
			if c.sc.rows()[r] != 0 {
				nz = true
			}
		}
		if !nz {
			continue
		}
		s.pf("%-28s", pointRows[r])
		for _, c := range cols {
			s.pf("%12.1f", c.sc.rows()[r])
		}
		s.w("\n")
	}
	s.pf("%-28s", "Total points")
	for _, c := range cols {
		s.pf("%12.1f", c.sc.total())
	}
	s.w("\n")
	s.pf("%-28s", "Ships commissioned")
	for _, c := range cols {
		switch {
		case c.ship != nil:
			s.pf("%12s", "")
		case c.team == ROMT:
			s.pf("%12d", 1)
		default:
			s.pf("%12d", g.numShips[c.team])
		}
	}
	s.w("\n")
	s.pf("%-28s", "Total / number of players")
	for _, c := range cols {
		n := 1
		if c.ship == nil && c.team != ROMT && g.numShips[c.team] > 0 {
			n = g.numShips[c.team]
		}
		if c.ship != nil {
			s.pf("%12s", "")
		} else {
			s.pf("%12.1f", c.sc.total()/float64(n))
		}
	}
	s.w("\n")
	s.pf("%-28s", "Total / stardates")
	now := time.Now()
	for _, c := range cols {
		el := g.stardate(now)
		if c.ship != nil {
			el = now.Sub(c.ship.commAt).Seconds() * cfg.StardatePerSec
		}
		s.pf("%12.1f", c.sc.total()/math.Max(el, 0.1))
	}
	s.w("\n")
}

func cmdPoints(g *Game, s *Session, t []Tok, raw string) cmdResult {
	me, fed, emp, rom := false, false, false, false
	if len(t) == 1 {
		if s.ship != nil {
			me = true
		} else {
			fed, emp, rom = true, true, true
		}
	}
	for _, tk := range t[1:] {
		switch kwIdx(tk, "ME", "I", "FEDERATION", "HUMAN", "EMPIRE", "KLINGON", "ROMULAN", "ALL") {
		case 0, 1:
			me = s.ship != nil
		case 2, 3:
			fed = true
		case 4, 5:
			emp = true
		case 6:
			rom = true
		case 7:
			me, fed, emp, rom = s.ship != nil, true, true, true
		default:
			return usage("POINTS [Me|I|Federation|Human|Empire|Klingon|Romulan|All]")
		}
	}
	g.printPoints(s, me, fed, emp, rom, false)
	return cmdResult{}
}

// ---------------------------------------------------------------------------
// SCAN / SRSCAN (8.18)
// ---------------------------------------------------------------------------

func cmdScan(g *Game, s *Session, t []Tok, raw string) cmdResult {
	return g.scan(s, t, 10, "SCAN [Up|Down|Right|Left|Corner] [range | vr hr] [Warning]")
}
func cmdSrscan(g *Game, s *Session, t []Tok, raw string) cmdResult {
	return g.scan(s, t, 7, "SRSCAN [Up|Down|Right|Left|Corner] [range | vr hr] [Warning]")
}

func (g *Game) scan(s *Session, t []Tok, def int, u string) cmdResult {
	sh := s.ship
	rest := t[1:]
	warn := false
	if len(rest) > 0 && !rest[len(rest)-1].isNum && kwIdx(rest[len(rest)-1], "WARNING") == 0 {
		warn = true
		rest = rest[:len(rest)-1]
	}
	// dist: up, down, right, left
	dist := [4]int{def, def, def, def}
	mod := -1
	if len(rest) > 0 && !rest[0].isNum {
		mod = kwIdx(rest[0], "DOWN", "UP", "LEFT", "RIGHT", "CORNER")
		if mod < 0 {
			return usage(u)
		}
		rest = rest[1:]
	}
	nn := 0
	switch len(rest) {
	case 0:
	case 1:
		if !rest[0].isNum {
			return usage(u)
		}
		d := rest[0].n
		dist = [4]int{d, d, d, d}
		nn = 1
	case 2:
		if !rest[0].isNum || !rest[1].isNum {
			return usage(u)
		}
		dist = [4]int{rest[0].n, rest[0].n, rest[1].n, rest[1].n}
		nn = 2
	default:
		return usage(u)
	}
	switch mod {
	case 0: // DOWN: no up
		dist[0] = 0
	case 1: // UP: no down
		dist[1] = 0
	case 2: // LEFT: no right
		dist[2] = 0
	case 3: // RIGHT: no left
		dist[3] = 0
	case 4:
		if nn != 2 {
			return usage(u)
		}
		if dist[0] > 0 {
			dist[1] = 0
		} else if dist[0] < 0 {
			dist[1] = -dist[0]
		}
		if dist[2] > 0 {
			dist[3] = 0
		} else if dist[2] < 0 {
			dist[3] = -dist[2]
		}
	}
	for i := range dist {
		dist[i] = max(0, min(dist[i], 10))
	}
	vmax, vmin := min(sh.v+dist[0], gridN), max(sh.v-dist[1], 1)
	hmax, hmin := min(sh.h+dist[2], gridN), max(sh.h-dist[3], 1)
	now := time.Now()
	long := s.set.scan == 1
	cw := 3
	if long {
		cw = 4
	}
	sym := func(v, h int) string {
		c := g.grid[v][h]
		var r string
		switch c.K {
		case KEmpty:
			r = " ."
			if warn {
				for i := range g.bases {
					b := &g.bases[i]
					if b.alive && b.team != sh.team && cheb(v, h, b.v, b.h) <= 4 {
						r = " !"
					}
				}
				for i := range g.planets {
					p := &g.planets[i]
					if p.alive && p.owner != sh.team && cheb(v, h, p.v, p.h) <= 2 {
						r = " !"
					}
				}
			}
		case KShip:
			r = " " + string(shipLetters[c.I])
		case KBase:
			if g.bases[c.I].team == FED {
				r = "<>"
			} else {
				r = ")("
			}
			if cheb(v, h, sh.v, sh.h) <= 10 {
				g.bases[c.I].known[sh.team] = true
			}
		case KPlanet:
			p := &g.planets[c.I]
			r = [3]string{" @", "@F", "@E"}[p.owner]
			p.known[sh.team] = true
		case KStar:
			r = " *"
		case KHole:
			r = "  "
		case KRom:
			if g.romVisible(sh, now) {
				r = "??"
			} else {
				r = " ."
			}
		}
		if !long {
			r = r[1:]
		}
		return r
	}
	var hdr strings.Builder
	hdr.WriteString(strings.Repeat(" ", 6))
	for h := hmin; h <= hmax; h++ {
		hdr.WriteString(fmt.Sprintf("%*d", cw, h))
	}
	hdr.WriteString("\n")
	s.w(hdr.String())
	for v := vmax; v >= vmin; v-- {
		s.pf("%4d  ", v)
		for h := hmin; h <= hmax; h++ {
			cell := sym(v, h)
			if long {
				s.pf("  %s", cell)
			} else {
				s.pf("%*s", cw, cell)
			}
		}
		s.pf("  %d\n", v)
	}
	s.w(hdr.String())
	return cmdResult{}
}

// ---------------------------------------------------------------------------
// LIST / SUMMARY / BASES / PLANETS / TARGETS engine (8.1)
// ---------------------------------------------------------------------------

const (
	obShip = 1
	obBase = 2
	obPlan = 4
	sdNeu  = 1
	sdFed  = 2
	sdEmp  = 4
	sdRom  = 8
	infR   = 1 << 30
)

func sideBit(t int) int {
	switch t {
	case NEU:
		return sdNeu
	case FED:
		return sdFed
	case EMP:
		return sdEmp
	}
	return sdRom
}

type lsel struct {
	objs, sides int
	rng         int
	rngSet      bool
	sidesSet    bool
	objsSet     bool
	closest     bool
	coordSet    bool
	cv, ch      int
	names       []int
	list, sum   bool
	captured    bool
}

type litem struct {
	k    Kind
	i    int
	v, h int
	side int
}

var listKW = []string{"SHIPS", "BASES", "PLANETS", "PORTS", "FRIENDLY", "ENEMY", "TARGETS", "FEDERATION", "HUMAN",
	"EMPIRE", "KLINGON", "NEUTRAL", "CAPTURED", "ALL", "CLOSEST", "LIST", "SUMMARY"}

func (g *Game) listCmd(s *Session, t []Tok, which string) cmdResult {
	T := 0
	if s.ship != nil {
		T = s.ship.team
	}
	enemy := 3 - T
	// split into groups at AND / &
	var groups [][]Tok
	cur := []Tok{}
	for _, tk := range t[1:] {
		if tk.up == "&" || tk.up == "AND" {
			groups = append(groups, cur)
			cur = []Tok{}
			continue
		}
		cur = append(cur, tk)
	}
	groups = append(groups, cur)
	printed := map[[2]int]bool{}
	for gi, grp := range groups {
		if len(grp) == 0 && gi > 0 {
			s.w("Null group illegal.\n")
			return cmdResult{}
		}
		sel := lsel{objs: 7, sides: 15, rng: infR}
		switch which {
		case "LIST":
			sel.list = true
		case "SUMMARY":
			sel.sum = true
		case "BASES":
			sel.objs, sel.list = obBase, true
			if T != 0 {
				sel.sides = sideBit(T)
			} else {
				sel.sides = sdFed | sdEmp
			}
		case "PLANETS":
			sel.objs, sel.sides, sel.rng, sel.list = obPlan, sdNeu|sdFed|sdEmp, 10, true
		case "TARGETS":
			sel.sides, sel.rng, sel.list = sideBit(enemy)|sdRom, 10, true
			if T == 0 {
				sel.sides = 15
			}
		}
		if !g.parseGroup(s, &sel, grp, which, T) {
			return cmdResult{}
		}
		items := g.evalGroup(s, &sel, which, T)
		if sel.list {
			for _, it := range items {
				key := [2]int{int(it.k), it.i}
				if printed[key] {
					continue
				}
				printed[key] = true
				g.printItem(s, it, which, T)
			}
		}
		if sel.sum {
			g.printSummary(s, items, &sel, T)
		}
		if len(items) == 0 && !sel.coordSet && len(sel.names) == 0 {
			g.noneMsg(s, &sel, which)
		}
	}
	return cmdResult{}
}

func (g *Game) parseGroup(s *Session, sel *lsel, grp []Tok, which string, T int) bool {
	bad := func(tk Tok) bool {
		s.w("Illegal keyword " + tk.orig + "\n")
		return false
	}
	for i := 0; i < len(grp); i++ {
		tk := grp[i]
		if tk.isNum {
			if i+1 < len(grp) && grp[i+1].isNum {
				if which == "SUMMARY" {
					s.w("Syntax error near keyword " + tk.orig + "\n")
					return false
				}
				v, h := tk.n, grp[i+1].n
				if !ingal(v, h) {
					s.pf("Illegal coordinate %d-%d\n", v, h)
					return false
				}
				sel.coordSet, sel.cv, sel.ch = true, v, h
				i++
				continue
			}
			if tk.n < 1 {
				return bad(tk)
			}
			sel.rng, sel.rngSet = tk.n, true
			continue
		}
		if which == "LIST" || which == "TARGETS" {
			if n := shipByName(tk.orig); n >= -1 {
				// ship names take precedence over keywords
				if n >= 0 || (n == -1 && len(tk.orig) >= 1) {
					sel.names = append(sel.names, n)
					sel.objs, sel.objsSet = obShip, true
					continue
				}
			}
		}
		k := kwIdx(tk, listKW...)
		if k < 0 {
			return bad(tk)
		}
		own := sideBit(T)
		switch listKW[k] {
		case "SHIPS":
			sel.objs, sel.objsSet = obShip, true
		case "BASES":
			sel.objs, sel.objsSet = obBase, true
		case "PLANETS":
			sel.objs, sel.objsSet = obPlan, true
		case "PORTS":
			sel.objs, sel.objsSet = obBase|obPlan, true
			if !sel.sidesSet {
				if T != 0 {
					sel.sides = own
				} else {
					sel.sides = sdFed | sdEmp
				}
			}
		case "FRIENDLY":
			if T == 0 {
				return bad(tk)
			}
			sel.sides, sel.sidesSet = own, true
		case "ENEMY", "TARGETS":
			if T == 0 {
				return bad(tk)
			}
			sel.sides, sel.sidesSet = sideBit(3-T)|sdRom, true
		case "FEDERATION", "HUMAN":
			sel.sides, sel.sidesSet = sdFed|(sel.sides&sdRom), true
		case "EMPIRE", "KLINGON":
			sel.sides, sel.sidesSet = sdEmp|(sel.sides&sdRom), true
		case "NEUTRAL":
			sel.sides, sel.sidesSet = sdNeu, true
			sel.objs, sel.objsSet = obPlan, true
		case "CAPTURED":
			sel.sides, sel.sidesSet = sdFed|sdEmp, true
			sel.objs, sel.objsSet = obPlan, true
			sel.captured = true
		case "ALL":
			if !sel.sidesSet && which != "TARGETS" {
				sel.sides = 15
			}
			if !sel.rngSet {
				sel.rng = infR
			}
		case "CLOSEST":
			if T == 0 || which == "SUMMARY" {
				return bad(tk)
			}
			sel.closest, sel.list = true, true
			if !sel.rngSet {
				sel.rng = infR
			}
		case "LIST":
			sel.list, sel.sum = true, false
		case "SUMMARY":
			sel.sum, sel.list = true, false
			if !sel.rngSet {
				sel.rng = infR
			}
		}
	}
	return true
}

func (g *Game) evalGroup(s *Session, sel *lsel, which string, T int) []litem {
	god := s.ship == nil
	vv, vh := 0, 0
	if !god {
		vv, vh = s.ship.v, s.ship.h
	}
	now := time.Now()
	dist := func(v, h int) int {
		if god {
			return 0
		}
		return cheb(vv, vh, v, h)
	}
	var out []litem
	itemAt := func(c Cell, v, h int) (litem, bool) {
		switch c.K {
		case KShip:
			return litem{KShip, c.I, v, h, g.ships[c.I].team}, true
		case KBase:
			return litem{KBase, c.I, v, h, g.bases[c.I].team}, true
		case KPlanet:
			return litem{KPlanet, c.I, v, h, g.planets[c.I].owner}, true
		case KRom:
			return litem{KRom, 0, v, h, ROMT}, true
		}
		return litem{}, false
	}
	known := func(it litem) bool {
		if god || it.side == T {
			return true
		}
		d := dist(it.v, it.h)
		switch it.k {
		case KBase:
			b := &g.bases[it.i]
			if d <= 10 {
				b.known[T] = true
			}
			return b.known[T]
		case KPlanet:
			p := &g.planets[it.i]
			if d <= 10 {
				p.known[T] = true
			}
			return p.known[T] || it.side == T
		}
		return true
	}
	if sel.coordSet {
		c := g.cell(sel.cv, sel.ch)
		it, ok := itemAt(c, sel.cv, sel.ch)
		want := map[Kind]int{KShip: obShip, KRom: obShip, KBase: obBase, KPlanet: obPlan}[c.K]
		if !ok || sel.objs&want == 0 {
			what := "object"
			switch which {
			case "BASES":
				what = "base"
			case "PLANETS":
				what = "planet"
			case "TARGETS":
				what = "target"
			}
			s.pf("No %s at %s\n", what, locFmt(sel.cv, sel.ch, vv, vh, 0, false))
			return nil
		}
		if it.side != T && dist(sel.cv, sel.ch) > 10 && !known(it) {
			s.pf("Sir, our sensors can't scan as far as %s\n", locFmt(sel.cv, sel.ch, vv, vh, 0, false))
			return nil
		}
		return []litem{it}
	}
	if len(sel.names) > 0 {
		for _, n := range sel.names {
			if n == -1 {
				if !g.rom.alive {
					s.w("The Romulan is dead.\n")
					continue
				}
				out = append(out, litem{KRom, 0, g.rom.v, g.rom.h, ROMT})
				continue
			}
			x := &g.ships[n]
			if !x.inGame {
				s.pf("%s is not in the game.\n", shipNames[n])
				continue
			}
			out = append(out, litem{KShip, n, x.v, x.h, x.team})
		}
		return out
	}
	if sel.objs&obShip != 0 {
		if sel.sides&sdRom != 0 && g.rom.alive {
			if sel.rng == infR || (g.romVisible(s.ship, now) && dist(g.rom.v, g.rom.h) <= sel.rng) {
				out = append(out, litem{KRom, 0, g.rom.v, g.rom.h, ROMT})
			}
		}
		for i := range g.ships {
			x := &g.ships[i]
			if !x.inGame || sel.sides&sideBit(x.team) == 0 || dist(x.v, x.h) > sel.rng {
				continue
			}
			if sel.closest && s.ship != nil && x.idx == s.ship.idx {
				continue
			}
			out = append(out, litem{KShip, i, x.v, x.h, x.team})
		}
	}
	if sel.objs&obBase != 0 {
		for i := range g.bases {
			b := &g.bases[i]
			it := litem{KBase, i, b.v, b.h, b.team}
			if !b.alive || sel.sides&sideBit(b.team) == 0 || dist(b.v, b.h) > sel.rng || !known(it) {
				continue
			}
			out = append(out, it)
		}
	}
	if sel.objs&obPlan != 0 {
		for i := range g.planets {
			p := &g.planets[i]
			it := litem{KPlanet, i, p.v, p.h, p.owner}
			if !p.alive || sel.sides&sideBit(p.owner) == 0 || dist(p.v, p.h) > sel.rng || !known(it) {
				continue
			}
			if sel.captured && p.owner == NEU {
				continue
			}
			out = append(out, it)
		}
	}
	if sel.closest && len(out) > 0 {
		best := out[0]
		for _, it := range out[1:] {
			d1, d2 := dist(it.v, it.h), dist(best.v, best.h)
			if d1 < d2 || (d1 == d2 && (it.v < best.v || (it.v == best.v && it.h < best.h))) {
				best = it
			}
		}
		out = []litem{best}
	}
	return out
}

func (g *Game) printItem(s *Session, it litem, which string, T int) {
	st := s.set.output
	flag := " "
	if it.side != T && it.side != NEU && T != 0 && which != "TARGETS" {
		flag = "*"
	}
	var a Actor
	builds := 0
	switch it.k {
	case KShip:
		a = Actor{KShip, it.i, it.side}
	case KRom:
		a = Actor{KRom, 0, ROMT}
	case KBase:
		a = Actor{KBase, it.i, it.side}
	case KPlanet:
		builds = 0
		a = Actor{KPlanet, it.i, it.side}
	}
	name := descr(a, 0, st)
	w := 5
	if st == 2 {
		w = 15
	}
	line := fmt.Sprintf("%s%-*s", flag, w-1, name)
	dist := 0
	if s.ship != nil {
		dist = cheb(s.ship.v, s.ship.h, it.v, it.h)
	}
	near := s.ship == nil || dist <= 10
	pct := func(p float64) string {
		if st == 0 {
			return fmt.Sprintf("%+d", int(p))
		}
		return fmt.Sprintf("%+.1f%%", p)
	}
	switch it.k {
	case KShip:
		x := &g.ships[it.i]
		if it.side == T || near {
			line += fmt.Sprintf("%-14s %s", s.loc(it.v, it.h, false), pct(signedPct(x)))
		}
	case KRom:
		if g.romVisible(s.ship, time.Now()) {
			line += s.loc(it.v, it.h, false)
		}
	case KBase:
		b := &g.bases[it.i]
		line += fmt.Sprintf("%-14s", s.loc(it.v, it.h, false))
		if it.side == T || near {
			line += " " + pct(basePct(b))
		}
	case KPlanet:
		p := &g.planets[it.i]
		builds = p.builds
		line += s.loc(it.v, it.h, false)
		if builds > 0 {
			switch st {
			case 2:
				if builds == 1 {
					line += "  1 build"
				} else {
					line += fmt.Sprintf("  %d builds", builds)
				}
			case 1:
				line += fmt.Sprintf("  %d b", builds)
			default:
				line += fmt.Sprintf(" %d", builds)
			}
		}
	}
	s.w(strings.TrimRight(line, " ") + "\n")
}

func (g *Game) printSummary(s *Session, items []litem, sel *lsel, T int) {
	var cnt [3][4]int // [objtype][side 0..3]
	for _, it := range items {
		switch it.k {
		case KShip:
			cnt[0][it.side]++
		case KRom:
			cnt[0][3]++
		case KBase:
			cnt[1][it.side]++
		case KPlanet:
			cnt[2][it.side]++
		}
	}
	suffix := " in game"
	if sel.rng != infR {
		suffix = " in range"
		if sel.rngSet {
			suffix = " in specified range"
		}
	}
	if s.set.output == 0 {
		suffix = ""
	}
	line := func(n int, what string) {
		if n == 0 {
			return
		}
		pl := ""
		if n != 1 {
			pl = "s"
		}
		s.pf("%3d %s%s%s\n", n, what, pl, suffix)
	}
	line(cnt[0][3], "Romulan")
	line(cnt[0][FED], "Federation ship")
	line(cnt[0][EMP], "Empire ship")
	line(cnt[1][FED], "Federation base")
	line(cnt[1][EMP], "Empire base")
	line(cnt[2][NEU], "neutral planet")
	line(cnt[2][FED], "Federation planet")
	line(cnt[2][EMP], "Empire planet")
	tot := 0
	for i := range cnt {
		for j := range cnt[i] {
			tot += cnt[i][j]
		}
	}
	if tot == 0 {
		g.noneMsg(s, sel, "SUMMARY")
	}
}

func (g *Game) noneMsg(s *Session, sel *lsel, which string) {
	var b strings.Builder
	if s.set.output == 2 {
		b.WriteString("Sir, there are no")
	} else {
		b.WriteString("No")
	}
	switch {
	case sel.captured:
		b.WriteString(" captured")
	case sel.sides == sdNeu:
		b.WriteString(" neutral")
	case sel.sides == sdFed:
		b.WriteString(" Federation")
	case sel.sides == sdEmp:
		b.WriteString(" Empire")
	}
	switch sel.objs {
	case obPlan:
		b.WriteString(" planets")
	case obBase:
		b.WriteString(" bases")
	case obShip:
		b.WriteString(" ships")
	case obBase | obPlan:
		b.WriteString(" ports")
	default:
		b.WriteString(" forces")
	}
	if s.set.output != 0 {
		if sel.rng == infR {
			b.WriteString(" in game")
		} else {
			b.WriteString(" in range")
		}
	}
	s.w(b.String() + "\n")
}

func cmdBases(g *Game, s *Session, t []Tok, r string) cmdResult   { return g.listCmd(s, t, "BASES") }
func cmdPlanets(g *Game, s *Session, t []Tok, r string) cmdResult { return g.listCmd(s, t, "PLANETS") }
func cmdTargets(g *Game, s *Session, t []Tok, r string) cmdResult { return g.listCmd(s, t, "TARGETS") }
func cmdList(g *Game, s *Session, t []Tok, r string) cmdResult    { return g.listCmd(s, t, "LIST") }
func cmdSummary(g *Game, s *Session, t []Tok, r string) cmdResult { return g.listCmd(s, t, "SUMMARY") }

// ---------------------------------------------------------------------------
// Command table
// ---------------------------------------------------------------------------

func init() {
	gameCmds = []Cmd{
		{"BASES", cmdBases, ""}, {"BUILD", cmdBuild, ""}, {"CAPTURE", cmdCapture, ""},
		{"DAMAGES", cmdDamages, ""}, {"DOCK", cmdDock, ""}, {"ENERGY", cmdEnergy, ""},
		{"GRIPE", cmdGripe, ""}, {"HELP", cmdHelp, ""}, {"IMPULSE", cmdImpulse, ""},
		{"LIST", cmdList, ""}, {"MOVE", cmdMove, ""}, {"NEWS", cmdNews, ""},
		{"PHASERS", cmdPhasers, ""}, {"PLANETS", cmdPlanets, ""}, {"POINTS", cmdPoints, ""},
		{"QUIT", cmdQuit, ""}, {"RADIO", cmdRadio, ""}, {"REPAIR", cmdRepair, ""},
		{"SCAN", cmdScan, ""}, {"SET", cmdSet, ""}, {"SHIELDS", cmdShields, ""},
		{"SRSCAN", cmdSrscan, ""}, {"STATUS", cmdStatus, ""}, {"SUMMARY", cmdSummary, ""},
		{"TARGETS", cmdTargets, ""}, {"TELL", cmdTell, ""}, {"TIME", cmdTime, ""},
		{"TORPEDOES", cmdTorp, ""}, {"TRACTOR", cmdTractor, ""}, {"TYPE", cmdType, ""},
		{"USERS", cmdUsers, ""},
	}
}

// ---------------------------------------------------------------------------
// GRIPE, NEWS, HELP
// ---------------------------------------------------------------------------

func cmdGripe(g *Game, s *Session, t []Tok, raw string) cmdResult {
	s.mode = mGripe
	s.gripe = nil
	s.gripeLen = 0
	s.helpProt = true
	s.w("Enter your gripe.  End with ^Z to send, ^C to abort.\n")
	return cmdResult{}
}

func (g *Game) gripeLine(s *Session, text string, term byte) {
	if text != "" || term != 0x1A {
		if s.gripeLen+len(text) <= cfg.GripeMax {
			s.gripe = append(s.gripe, text)
			s.gripeLen += len(text) + 1
		} else {
			s.w("(gripe is full)\n")
		}
	}
	if term != 0x1A {
		return
	}
	now := time.Now()
	ship := "(none)"
	if s.ship != nil {
		ship = shipNames[s.ship.idx]
	}
	hdr := fmt.Sprintf("%s  %s %s\nShip: %s  Captain: %s  TTY speed: %d  PPN: %d  TTY: %d  Job: %d\nRomulans: yes  Black holes: %v\n",
		versionStr, now.Format("2006-01-02"), now.Format("15:04:05"), ship, s.captain, s.speed, s.uid, s.connNo, s.id, g.blackholes)
	entry := hdr + strings.Join(s.gripe, "\n") + "\n" + strings.Repeat("-", 60) + "\n"
	path := filepath.Join(cfg.DataDir, "DECWAR.GRP")
	old, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append([]byte(entry), old...), 0644); err != nil {
		s.w("Sorry, the gripe could not be filed.\n")
	} else {
		s.w("Gripe filed.  Thank you, Captain.\n")
	}
	s.gripe = nil
	s.helpProt = false
	if s.ship != nil {
		s.mode = mPlay
	} else {
		s.mode = mPregame
	}
	s.promptSh = false
}

func cmdNews(g *Game, s *Session, t []Tok, raw string) cmdResult {
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "DECWAR.NWS"))
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		s.w("No news.\n")
		return cmdResult{}
	}
	s.w(strings.ReplaceAll(string(b), "\f", "") + "\n")
	return cmdResult{}
}

type HelpTopic struct {
	name, title, body string
}
type HelpDB struct{ topics []HelpTopic }

func loadHelp() *HelpDB {
	db := &HelpDB{}
	if f, err := os.Open(filepath.Join(cfg.DataDir, "DECWAR.HLP")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var cur *HelpTopic
		state := 0
		for sc.Scan() {
			ln := strings.ReplaceAll(strings.TrimRight(sc.Text(), "\r"), "\f", "")
			switch {
			case cur == nil:
				if len(ln) > 1 && ln[0] == '.' {
					f := strings.Fields(ln[1:])
					if len(f) > 0 {
						cur = &HelpTopic{name: strings.ToUpper(f[0])}
						state = 1
					}
				}
			case state == 1:
				cur.title = ln
				state = 2
			case ln == ".":
				db.topics = append(db.topics, *cur)
				cur = nil
			default:
				cur.body += ln + "\n"
			}
		}
		if cur != nil {
			db.topics = append(db.topics, *cur)
		}
	}
	if len(db.topics) == 0 {
		db.topics = builtinHelp()
	}
	have := map[string]bool{}
	for _, t := range db.topics {
		have[t.name] = true
	}
	if !have["ESCAPE"] {
		db.topics = append(db.topics, HelpTopic{"ESCAPE", "Repeating commands",
			"Typing ESCAPE (altmode) as the first character of a line repeats the previous command.\nIt cannot be used to repeat a TELL command.\n"})
	}
	return db
}

func (db *HelpDB) names() []string {
	n := make([]string, len(db.topics))
	for i, t := range db.topics {
		n[i] = t.name
	}
	return n
}

func cmdHelp(g *Game, s *Session, t []Tok, raw string) cmdResult {
	s.helpProt = true
	g.helpFor(s, t[1:])
	return cmdResult{}
}

func (g *Game) helpFor(s *Session, t []Tok) {
	db := g.help
	if len(t) == 0 {
		t = []Tok{{orig: "INTRODUCTION"}}
	}
	for _, tk := range t {
		if tk.orig == "*" {
			s.w("Help is available on:\n")
			var cols []string
			for _, c := range gameCmds {
				cols = append(cols, c.name)
			}
			for i := 0; i < len(cols); i += 5 {
				e := min(i+5, len(cols))
				s.w("  " + strings.Join(padAll(cols[i:e], 12), "") + "\n")
			}
			s.w("Other topics: INTRODUCTION INPUT OUTPUT PREGAME HINTS PAUSES CTL-C ESCAPE\n")
			continue
		}
		i, st := resolve(tk.orig, db.names())
		if st != 0 {
			s.w("No help available on " + tk.orig + ".  Type HELP * for a list of topics.\n")
			continue
		}
		tp := db.topics[i]
		s.w("\n" + tp.name + " -- " + tp.title + "\n" + tp.body)
	}
}

func padAll(a []string, w int) []string {
	o := make([]string, len(a))
	for i, x := range a {
		o[i] = fmt.Sprintf("%-*s", w, x)
	}
	return o
}

func builtinHelp() []HelpTopic {
	T := func(n, t, b string) HelpTopic { return HelpTopic{n, t, b} }
	return []HelpTopic{
		T("INTRODUCTION", "DECWAR", "DECWAR is a real-time multiplayer space war game for up to ten players.\nThe Federation (L N S V Y) fights the Empire (C D H J W) over a 75x75 galaxy of\nstars, planets and starbases.  Capture planets, build starbases, destroy the enemy.\nA team wins when the other has no starbases, no captured planets and no ships.\nCommands (type HELP <command>): BAses BUild CApture DAmages DOck Energy Gripe Help\nImpulse LIst Move NEws PHasers PLanets POints Quit RAdio REpair SCan SEt SHields\nSRscan STatus SUmmary TArgets TEll TIme TOrpedoes TRactor TYpe Users.\n"),
		T("INPUT", "Command input", "Commands may be abbreviated to the shortest unambiguous prefix; only the first five\ncharacters of a word count.  Separate words with spaces, tabs or commas.  Several\ncommands may be typed on a line separated by '/'.  ';' begins a comment, except in\nTELL where the message follows the first ';'.  Coordinates are vpos hpos; prefix them\nwith ABSOLUTE, RELATIVE or COMPUTED.  ^H/DEL delete, ^U kills the line, ^R retypes,\n^G toggles echo, ^O discards output, ^C aborts stacked commands.\n"),
		T("OUTPUT", "Output formats", "SET OUTPUT Long|Medium|Short selects message length; SET SCAN, SET PROMPT, SET OCDEF\nand SET ICDEF select scan symbols, prompt, and coordinate styles.  TYPE OUTPUT shows them.\n"),
		T("PREGAME", "Pre-game", "Before joining you may use ACTIVATE, GRIPE, HELP, NEWS, POINTS, QUIT, SUMMARY, TIME, USERS.\nACTIVATE asks for your name, side and ship.\n"),
		T("HINTS", "Hints", "Keep your shields up in battle, dock to repair, and watch your energy.  Phasers hit\nharder at close range; torpedoes need no energy but can be deflected by strong shields.\n"),
		T("PAUSES", "Pauses", "Time-consuming commands (MOVE, IMPULSE, DOCK, BUILD, CAPTURE, REPAIR) hold your queue\nfor a few seconds; pauses depend on the slowest terminal in the game.\n"),
		T("CTL-C", "Control-C", "^C aborts stacked commands.  At the command prompt it releases your ship; you may\nACTIVATE within 60 seconds to resume.  Under red alert only QUIT works.\n"),
		T("BASES", "BASES", "BASES [keywords]   List friendly starbases (whole galaxy).\n"),
		T("BUILD", "BUILD", "BUILD [A|R] <v> <h>   Fortify an adjacent captured planet; the fifth build makes a starbase.\n"),
		T("CAPTURE", "CAPTURE", "CAPTURE [A|R] <v> <h>   Capture an adjacent neutral or enemy planet.\n"),
		T("DAMAGES", "DAMAGES", "DAMAGES [devices]   Report damaged devices.\n"),
		T("DOCK", "DOCK", "DOCK [STATUS]   Resupply at an adjacent friendly starbase or captured planet.\n"),
		T("ENERGY", "ENERGY", "ENERGY <ship> <units>   Transfer energy to an adjacent friendly ship (10% lost).\n"),
		T("GRIPE", "GRIPE", "GRIPE   Enter a complaint; end with ^Z, abort with ^C.\n"),
		T("HELP", "HELP", "HELP [* | topics]   Print help.\n"),
		T("IMPULSE", "IMPULSE", "IMPULSE [A|R] <v> <h>   Move exactly one sector (warp factor 1).\n"),
		T("LIST", "LIST", "LIST [ships|bases|planets|ports|sides|range|ship names|v h|CLOSEST|SUMMARY] [& ...]\n"),
		T("MOVE", "MOVE", "MOVE [A|R|C] <v> <h>   Warp to a sector; warp factor is the distance (max 6, 3 if damaged).\n"),
		T("NEWS", "NEWS", "NEWS   Print the news file.\n"),
		T("PHASERS", "PHASERS", "PHASERS [A|R|C] [50-500] <v> <h>   Fire phasers at one target within 10 sectors.\n"),
		T("PLANETS", "PLANETS", "PLANETS [keywords]   List planets within 10 sectors.\n"),
		T("POINTS", "POINTS", "POINTS [Me|I|Federation|Human|Empire|Klingon|Romulan|All]\n"),
		T("QUIT", "QUIT", "QUIT   Leave the game (two-minute lockout).\n"),
		T("RADIO", "RADIO", "RADIO ON | OFF | GAG <ship> | UNGAG <ship>\n"),
		T("REPAIR", "REPAIR", "REPAIR [units|ALL]   Repair devices (50 units, or 100 docked).\n"),
		T("SCAN", "SCAN", "SCAN [Up|Down|Right|Left|Corner] [range | vr hr] [Warning]   Default range 10.\n"),
		T("SET", "SET", "SET NAme|Output|SCan|Prompt|OCdef|Icdef|Ttytype <value>\n"),
		T("SHIELDS", "SHIELDS", "SHIELDS Up | Down | Transfer <energy>   Raising shields costs 100 energy.\n"),
		T("SRSCAN", "SRSCAN", "SRSCAN [same as SCAN]   Short-range scan, default range 7.\n"),
		T("STATUS", "STATUS", "STATUS [Condition|Location|Torpedoes|Energy|Damage|Shields|Radio]\n"),
		T("SUMMARY", "SUMMARY", "SUMMARY [keywords]   Counts of ships, bases and planets.\n"),
		T("TARGETS", "TARGETS", "TARGETS [keywords]   List enemy objects within 10 sectors.\n"),
		T("TELL", "TELL", "TELL <recipients> ; <message>   Recipients: All, FEderation, EMpire, ENemy, FRiendly, ship names.\n"),
		T("TIME", "TIME", "TIME   Print elapsed and run times.\n"),
		T("TORPEDOES", "TORPEDOES", "TORPEDOES [A|R|C] <n> <v1> <h1> [<v2> <h2> [<v3> <h3>]]   Fire 1 to 3 photon torpedoes.\n"),
		T("TRACTOR", "TRACTOR", "TRACTOR <ship> | OFF   Tow an adjacent friendly ship (shields down).\n"),
		T("TYPE", "TYPE", "TYPE OPtion | OUtput\n"),
		T("USERS", "USERS", "USERS   List ships in the game and their captains.\n"),
	}
}

// ---------------------------------------------------------------------------
// Pre-game and SETUP (4.2, 4.3)
// ---------------------------------------------------------------------------

var pregameCmds = []string{"ACTIVATE", "GRIPE", "HELP", "NEWS", "POINTS", "QUIT", "SUMMARY", "TIME", "USERS"}

func (g *Game) pregameLine(s *Session, text string) {
	for _, c := range splitCommands(text) {
		toks := tokenize(c)
		if len(toks) == 0 {
			continue
		}
		i, st := resolve(toks[0].orig, pregameCmds)
		if st != 0 {
			if _, st2 := resolve(toks[0].orig, cmdNames()); st2 == 0 {
				s.w("Command not legal in pre-game.\n")
			} else if st == 2 {
				s.w("Ambiguous command.\n")
			} else {
				s.w("Invalid command.  Type HELP for help.\n")
			}
			continue
		}
		switch pregameCmds[i] {
		case "ACTIVATE":
			g.activate(s)
			return
		case "QUIT":
			s.w("Goodbye.\n")
			g.closeSession(s, "quit")
			return
		case "GRIPE":
			cmdGripe(g, s, toks, c)
			return
		case "HELP":
			g.helpFor(s, toks[1:])
		case "NEWS":
			cmdNews(g, s, toks, c)
		case "POINTS":
			cmdPoints(g, s, toks, c)
		case "SUMMARY":
			g.listCmd(s, toks, "SUMMARY")
		case "TIME":
			cmdTime(g, s, toks, c)
		case "USERS":
			cmdUsers(g, s, toks, c)
		}
	}
}

func (g *Game) uidFor(key string) int {
	if u, ok := g.uids[key]; ok {
		return u
	}
	g.nextUID++
	g.uids[key] = g.nextUID
	return g.nextUID
}

func (g *Game) activate(s *Session) {
	if g.ending {
		s.pf("The game is over; a new game begins in %d seconds.\n", int(time.Until(g.endAt).Seconds())+1)
		return
	}
	if g.numPlayers() >= 10 {
		s.w("Sorry, Captain, all ships are in use.\n")
		return
	}
	if h := s.held; h != nil && time.Now().Before(h.expiry) {
		slot := &g.ships[h.ship.idx]
		if !slot.inGame && g.empty(h.ship.v, h.ship.h) {
			*slot = h.ship
			slot.sess, slot.inGame, slot.tractor = s, true, -1
			g.setCell(slot.v, slot.h, KShip, slot.idx)
			s.held = nil
			s.ship = slot
			s.mode = mPlay
			s.promptSh = false
			s.pf("Resuming command of the %s at %s.\n", shipNames[slot.idx], s.loc(slot.v, slot.h, false))
			return
		}
		s.w("Sorry, Captain, your ship or its sector has been taken.\n")
		s.held = nil
	}
	s.mode = mSetName
	s.promptSh = false
}

func capKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func (g *Game) freeShips(team int) []int {
	var r []int
	for i := range g.ships {
		if g.ships[i].team == team && !g.ships[i].inGame {
			r = append(r, i)
		}
	}
	return r
}

func (g *Game) setupLine(s *Session, text string) {
	text = strings.TrimSpace(text)
	switch s.mode {
	case mSetName:
		name := text
		if name == "" {
			name = s.captain
		}
		if name == "" {
			name = "Captain"
		}
		if len(name) > 12 {
			name = name[:12]
		}
		key := capKey(name)
		now := time.Now()
		if exp, ok := g.locks[key]; ok && now.Before(exp) {
			s.pf("You may not rejoin for another %d seconds.\n", int(time.Until(exp).Seconds())+1)
			g.toPregame(s)
			return
		}
		if exp, ok := g.locks["ip:"+s.ip]; ok && g.ipLock && now.Before(exp) {
			s.pf("This address may not rejoin for another %d seconds.\n", int(time.Until(exp).Seconds())+1)
			g.toPregame(s)
			return
		}
		s.captain, s.capKey, s.uid = name, key, g.uidFor(key)
		fed, emp := g.teamShips(FED), g.teamShips(EMP)
		if t, ok := g.teamMem[key]; ok && len(g.freeShips(t)) > 0 {
			s.pf("Welcome back, Captain %s.  You rejoin the %s.\n", name, teamName(t))
			g.chooseShip(s, t)
			return
		}
		if iabs(fed-emp) >= 2 {
			t := FED
			if fed > emp {
				t = EMP
			}
			s.pf("The fleets are unbalanced; you are assigned to the %s.\n", teamName(t))
			g.chooseShip(s, t)
			return
		}
		s.mode = mSetTeam
		s.promptSh = false
	case mSetTeam:
		t := FED
		if g.teamShips(EMP) < g.teamShips(FED) {
			t = EMP
		}
		if text != "" {
			switch kwIdx(Tok{orig: text}, "FEDERATION", "HUMAN", "EMPIRE", "KLINGON") {
			case 0, 1:
				t = FED
			case 2, 3:
				t = EMP
			default:
				s.w("Please answer Federation or Empire.\n")
				s.promptSh = false
				return
			}
		}
		if len(g.freeShips(t)) == 0 {
			s.w("Sorry, Captain, all ships of the " + teamName(t) + " are in use.\n")
			s.promptSh = false
			return
		}
		g.chooseShip(s, t)
	case mSetShip:
		free := g.freeShips(s.setTeam)
		if len(free) == 0 {
			s.w("Sorry, all ships of that team are now in use.\n")
			g.toPregame(s)
			return
		}
		if text == "" {
			g.chooseShip(s, s.setTeam)
			return
		}
		var names []string
		for _, i := range free {
			names = append(names, shipNames[i])
		}
		k, st := resolve(text, names)
		if st != 0 {
			s.w("No such available ship.\n")
			g.chooseShip(s, s.setTeam)
			return
		}
		g.commission(s, free[k])
	}
}

func teamName(t int) string {
	if t == FED {
		return "Federation"
	}
	return "Empire"
}

func (g *Game) chooseShip(s *Session, team int) {
	s.setTeam = team
	free := g.freeShips(team)
	if len(free) == 0 {
		s.w("Sorry, Captain, all ships of the " + teamName(team) + " are in use.\n")
		g.toPregame(s)
		return
	}
	s.w("Available ships of the " + teamName(team) + ":\n")
	for _, i := range free {
		s.pf("  %s\n", shipNames[i])
	}
	s.mode = mSetShip
	s.promptSh = false
}

func (g *Game) startPos(team int) (int, int) {
	enemyPort := func(v, h int) bool {
		for i := range g.bases {
			b := &g.bases[i]
			if b.alive && b.team != team && cheb(v, h, b.v, b.h) <= 2 {
				return true
			}
		}
		for i := range g.planets {
			p := &g.planets[i]
			if p.alive && p.owner != team && p.owner != NEU && cheb(v, h, p.v, p.h) <= 2 {
				return true
			}
		}
		return false
	}
	var own []*Base
	for i := range g.bases {
		if g.bases[i].alive && g.bases[i].team == team {
			own = append(own, &g.bases[i])
		}
	}
	for try := 0; try < 400; try++ {
		var v, h int
		if len(own) > 0 {
			b := own[rng.Intn(len(own))]
			v, h = b.v+rng.Intn(17)-8, b.h+rng.Intn(17)-8
		} else {
			v, h = 1+rng.Intn(gridN), 1+rng.Intn(gridN)
		}
		if g.empty(v, h) && !enemyPort(v, h) && !g.nearBase(v, h, 1) {
			return v, h
		}
	}
	return g.randEmpty(1, gridN, 1, gridN)
}

func (g *Game) commission(s *Session, idx int) {
	sh := &g.ships[idx]
	now := time.Now()
	v, h := g.startPos(sh.team)
	*sh = Ship{idx: idx, team: sh.team, inGame: true, sess: s, captain: s.captain, v: v, h: h,
		energy: cfg.ShipEnergy, shieldE: cfg.ShieldEnergy, shieldsUp: true, torps: cfg.TorpsMax,
		life: cfg.LifeReserve, tractor: -1, radioOn: true, commAt: now, uid: s.uid}
	sh.score.Commissioned = 1
	g.setCell(v, h, KShip, idx)
	g.numShips[sh.team]++
	g.teamScore[sh.team].Commissioned++
	g.gameStarted = true
	g.teamMem[s.capKey] = sh.team
	s.ship = sh
	s.mode = mPlay
	s.cmdq = nil
	s.busyTil = time.Time{}
	s.cpuShip = s.cpu
	s.promptSh = false
	s.pf("\nYou are now captain of the %s %s, at %s.\nAll systems are operational.  Good luck, Captain.\n",
		teamName(sh.team), shipNames[idx], s.loc(v, h, false))
	// replay the captain's profile (stand-in for DECWAR.INI)
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "profiles", sanitize(s.captain)+".ini")); err == nil {
		mark := len(s.out)
		for _, ln := range strings.Split(string(b), "\n") {
			ln = strings.TrimSpace(ln)
			if strings.HasPrefix(strings.ToUpper(ln), "SET") {
				toks := tokenize(ln)
				cmdSet(g, s, toks, ln)
			}
		}
		s.out = s.out[:mark]
	}
	name := shipNames[idx]
	cap := s.captain
	g.teamRadio(FED, func(rs *Session) string {
		if rs == s {
			return ""
		}
		return fmt.Sprintf("%s (%s) has entered the game.", name, cap)
	})
	g.teamRadio(EMP, func(rs *Session) string {
		if rs == s {
			return ""
		}
		return fmt.Sprintf("%s (%s) has entered the game.", name, cap)
	})
}

func sanitize(n string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(n) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "captain"
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	ipl := true
	seed := int64(0)
	flag.IntVar(&cfg.Port, "port", cfg.Port, "TCP port")
	flag.StringVar(&cfg.Listen, "listen", cfg.Listen, "listen address")
	flag.StringVar(&cfg.DataDir, "dir", cfg.DataDir, "directory for DECWAR.HLP/.NWS/.GRP and profiles/")
	flag.BoolVar(&cfg.ComputedForAll, "computed-all", false, "allow COMPUTED coordinates on all terminals")
	flag.BoolVar(&cfg.Throttle, "throttle", false, "meter output at terminal speed")
	flag.BoolVar(&ipl, "iplock", true, "enforce the QUIT rejoin lock by source IP as well as captain name")
	flag.Int64Var(&seed, "seed", 0, "random seed (0 = time based)")
	flag.Parse()
	if seed != 0 {
		rng = rand.New(rand.NewSource(seed))
	}
	g := newGame()
	g.ipLock = ipl
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Listen, strconv.Itoa(cfg.Port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "%s listening on %s\n", versionStr, ln.Addr())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			ip := c.RemoteAddr().String()
			if h, _, e := net.SplitHostPort(ip); e == nil {
				ip = h
			}
			s := &Session{g: g, conn: c, ip: ip, echo: true, speed: 9600, width: 80, height: 24,
				wq: make(chan []byte, 256), set: Settings{output: 1, scan: 1, tty: "CRT"}}
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			s.initNeg()
			go s.writeLoop()
			g.ev <- event{kind: evAccept, s: s, conn: c}
		}
	}()
	g.run()
}

var _ = sort.Ints

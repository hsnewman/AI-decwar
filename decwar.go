// DECWAR Telnet Server - single-file Go implementation of the
// "DECWAR Telnet Server: Programming Specification" (DECWAR 2.2, 1979 clone).
//
// Build:  go build -o decwar decwar.go
// Run:    ./decwar [-addr :1701] [-help DECWAR.HLP] [-selftest]
// Play:   telnet localhost 1701   (or nc / any MUD client)
//
// Everything in the game state is mutated on ONE goroutine (gameLoop);
// per-connection goroutines only parse the Telnet byte stream and flush output.
package main

import (
	"container/heap"
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

const version = "DECWAR Version 2.2, December 25, 1979"

// ---------------------------------------------------------------- config (Appendix A)

type Config struct {
	Addr                                   string
	MaxConn                                int
	IdlePregame                            time.Duration
	Stars, Planets, BasesPerTeam, BasesMax int
	BlackholeChance                        float64
	Blackholes                             int
	RomulanEnabled                         bool
	RomulanMaxPlayers                      int
	RomulanRespawn                         time.Duration
	RomulanSight                           int
	QuitLock, CtrlCHold, DropHold          time.Duration
	ComputedForAll                         bool
	SlowBps                                int
	Throttle                               bool
	HelpFile, NewsFile, GripeFile          string
	FixedFile, ProfileDir                  string
	Seed                                   int64
	StardatePerSec                         float64
}

var cfg = Config{
	Addr: ":1701", MaxConn: 20, IdlePregame: 30 * time.Minute,
	Stars: 250, Planets: 60, BasesPerTeam: 5, BasesMax: 10,
	BlackholeChance: 0.25, Blackholes: 8,
	RomulanEnabled: true, RomulanMaxPlayers: 3, RomulanRespawn: 90 * time.Second, RomulanSight: 7,
	QuitLock: 120 * time.Second, CtrlCHold: 60 * time.Second, DropHold: 30 * time.Second,
	SlowBps: 1200, StardatePerSec: 1.0 / 60,
	HelpFile: "DECWAR.HLP", NewsFile: "DECWAR.NWS", GripeFile: "DECWAR.GRP",
	FixedFile: "DECWAR.FXD", ProfileDir: "profiles",
}

const (
	gridN       = 75
	shipEnergyM = 5000.0
	shieldM     = 2500.0
	dmgLimit    = 2500.0
	torpsMax    = 10
	lifeMax     = 5
	yellowE     = 1000.0
	devInop     = 300.0
	warpK       = 10.0
	phaserDef   = 200
	phaserMin   = 50
	phaserMax   = 500
	phaserRange = 10
	torpBase    = 300.0
	baseRange   = 4
	planetRange = 2
	baseFire    = 3 * time.Second
	planetFire  = 4 * time.Second
	scanRange   = 10
)

const (
	FED  = 0
	EMP  = 1
	NEUT = 2
)

const (
	KEmpty = iota
	KStar
	KHole
	KShip
	KBase
	KPlanet
	KRom
)

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
)

var devNames = []string{"Warp engines", "Impulse engines", "Photon tubes", "Phaser banks",
	"Deflector shields", "Computer", "Life support", "Sub-space radio", "Tractor beam"}
	var devKeys = []string{"WARP", "IMPULSE", "TORPEDOES", "PHASERS", "SHIELDS", "COMPUTER", "LIFE", "RADIO", "TRACTOR"}

	var shipNames = [10]string{"Lexington", "Nimitz", "Savannah", "Vulcan", "Yorktown",
		"Cobra", "Demon", "Hawk", "Jackal", "Wolf"}

		var teamNames = [2]string{"Federation", "Empire"}

		// ---------------------------------------------------------------- data structures

		type Score struct {
			dmgEnemy, dmgBase                                 float64
			kills, planets, built, romKills, stars, plDestroy int
			commissioned                                      int
		}

		func (sc *Score) total() int {
			return int(sc.dmgEnemy/10) + 500*sc.kills + int(sc.dmgBase/10) + 100*sc.planets +
			1000*sc.built + 1000*sc.romKills - 50*sc.stars - 100*sc.plDestroy
		}

		type Rec struct { // per-captain memory (rejoin, score, lock)
			key       string
			team      int
			sc        Score
			cas       int
			lockUntil time.Time
		}

		type Ship struct {
			name        string
			team, idx   int
			active      bool
			sess        *Session
			rec         *Rec
			v, h        int
			energy      float64
			shield      float64
			damage      float64
			shieldsUp   bool
			dev         [9]float64
			torps       int
			life        int
			lifeAcc     float64
			red, docked bool
			tractor     *Ship
			radioOn     bool
			gagged      map[*Ship]bool
			phReady     [2]time.Time
			torpReady   time.Time
			nextAuto    time.Time
			orphanUntil time.Time
			moveTok     int
			lastAtk     *Ship
			lastAtkRom  bool
			enteredAt   time.Time
		}

		type Planet struct {
			v, h      int
			owner     int
			builds    int
			integrity float64
			known     [2]bool
			nextFire  time.Time
		}

		type Base struct {
			v, h      int
			team      int
			shield    float64
			damage    float64
			known     [2]bool
			alive     bool
			distress  bool
			nextFire  time.Time
			nextRegen time.Time
		}

		type Romulan struct {
			v, h        int
			revealUntil time.Time
			damage      float64
			nextAct     time.Time
			nextMove    time.Time
			volley      int
		}

		type Cell struct {
			k    int
			ship *Ship
			base *Base
			pl   *Planet
		}

		type World struct {
			grid       [gridN + 1][gridN + 1]Cell
			planets    []*Planet
			bases      []*Base
			ships      [10]*Ship
			rom        *Romulan
			romSpawnAt time.Time
			romScore   Score
			start      time.Time
			sd0        float64
			blackholes bool
			romulan    bool
			over       bool
			winner     int
			resetAt    time.Time
			recs       map[string]*Rec
			ipLock     map[string]time.Time
		}

		var w *World

		// ---------------------------------------------------------------- scheduler

		type evt struct {
			at  time.Time
			seq int
			fn  func()
		}
		type evHeap []*evt

		func (h evHeap) Len() int { return len(h) }
		func (h evHeap) Less(i, j int) bool {
			if h[i].at.Equal(h[j].at) {
				return h[i].seq < h[j].seq
			}
			return h[i].at.Before(h[j].at)
		}
		func (h evHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
		func (h *evHeap) Push(x interface{}) { *h = append(*h, x.(*evt)) }
		func (h *evHeap) Pop() interface{} {
			o := *h
			n := len(o)
			x := o[n-1]
			*h = o[:n-1]
			return x
		}

		var sched evHeap
		var schedSeq int

		func after(d time.Duration, fn func()) {
			schedSeq++
			heap.Push(&sched, &evt{time.Now().Add(d), schedSeq, fn})
		}
		func runSched(now time.Time) {
			for len(sched) > 0 && !sched[0].at.After(now) {
				e := heap.Pop(&sched).(*evt)
				e.fn()
			}
		}

		// ---------------------------------------------------------------- small helpers

		func abs(x int) int {
			if x < 0 {
				return -x
			}
			return x
		}
		func sgn(x int) int {
			if x < 0 {
				return -1
			}
			if x > 0 {
				return 1
			}
			return 0
		}
		func cheb(a, b, c, d int) int { return max(abs(a-c), abs(b-d)) }
		func dist(a, b, c, d int) float64 {
			return math.Max(1, math.Hypot(float64(a-c), float64(b-d)))
		}
		func inGrid(v, h int) bool        { return v >= 1 && v <= gridN && h >= 1 && h <= gridN }
		func rf(a, b float64) float64     { return a + rand.Float64()*(b-a) }
		func up(s string) string          { return strings.ToUpper(s) }
		func sec(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
		func trunc5(s string) string {
			if len(s) > 5 {
				return s[:5]
			}
			return s
		}

		// resolve a word against a keyword list: exact (5 significant chars) first, then unique prefix.
		// returns match and status 0=ok 1=ambiguous 2=none
		func resolve(word string, list []string) (string, int) {
			wd := trunc5(up(word))
			if wd == "" {
				return "", 2
			}
			for _, c := range list {
				if c == wd || trunc5(c) == wd {
					return c, 0
				}
			}
			var m []string
			for _, c := range list {
				if strings.HasPrefix(c, wd) {
					m = append(m, c)
				}
			}
			switch len(m) {
				case 1:
					return m[0], 0
				case 0:
					return "", 2
			}
			return "", 1
		}

		func kw(tok string, set ...string) string {
			m, st := resolve(tok, set)
			if st != 0 {
				return ""
			}
			return m
		}

		func num(s string) (int, bool) {
			n, err := strconv.Atoi(s)
			if err != nil || n > 10000 || n < -10000 {
				return 0, false
			}
			return n, true
		}

		func fmtDur(d time.Duration) string {
			t := d.Seconds()
			hh := int(t) / 3600
			mm := (int(t) % 3600) / 60
			ss := t - float64(hh*3600+mm*60)
			return fmt.Sprintf("%d:%02d:%04.1f", hh, mm, ss)
		}

		func sanitizeKey(n string) string {
			var b []byte
			for _, c := range []byte(strings.ToLower(n)) {
				if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
					b = append(b, c)
				}
			}
			if len(b) == 0 {
				return "anon"
			}
			return string(b)
		}

		// ---------------------------------------------------------------- world construction

		func (g *World) randEmpty(hlo, hhi int) (int, int) {
			for i := 0; i < 20000; i++ {
				v := 1 + rand.Intn(gridN)
				h := hlo + rand.Intn(hhi-hlo+1)
				if g.grid[v][h].k == KEmpty {
					return v, h
				}
			}
			return 1, 1
		}

		func (g *World) nearBase(v, h, r int) bool {
			for _, b := range g.bases {
				if b.alive && cheb(v, h, b.v, b.h) <= r {
					return true
				}
			}
			return false
		}

		func newWorld() *World {
			g := &World{recs: map[string]*Rec{}, ipLock: map[string]time.Time{}}
			g.start = time.Now()
			g.sd0 = 1000.0
			g.romulan = cfg.RomulanEnabled
			g.blackholes = rand.Float64() < cfg.BlackholeChance
			for i := 0; i < 10; i++ {
				g.ships[i] = &Ship{name: shipNames[i], team: i / 5, idx: i, gagged: map[*Ship]bool{}}
			}
			for team := 0; team < 2; team++ {
				for n := 0; n < cfg.BasesPerTeam && len(g.bases) < cfg.BasesMax; n++ {
					var v, h int
					for try := 0; try < 2000; try++ {
						lo, hi := 3, 18
						if team == EMP {
							lo, hi = 58, 73
						}
						v, h = g.randEmpty(lo, hi)
						if !g.nearBase(v, h, 5) {
							break
						}
					}
					b := &Base{v: v, h: h, team: team, shield: shieldM, alive: true}
					g.bases = append(g.bases, b)
					g.grid[v][h] = Cell{k: KBase, base: b}
				}
			}
			for n := 0; n < cfg.Stars; n++ {
				for try := 0; try < 200; try++ {
					v, h := g.randEmpty(1, gridN)
					if g.nearBase(v, h, 1) {
						continue
					}
					g.grid[v][h] = Cell{k: KStar}
					break
				}
			}
			for n := 0; n < cfg.Planets; n++ {
				v, h := g.randEmpty(1, gridN)
				p := &Planet{v: v, h: h, owner: NEUT, integrity: 300}
				g.planets = append(g.planets, p)
				g.grid[v][h] = Cell{k: KPlanet, pl: p}
			}
			if g.blackholes {
				for n := 0; n < cfg.Blackholes; n++ {
					v, h := g.randEmpty(1, gridN)
					g.grid[v][h] = Cell{k: KHole}
				}
			}
			g.romSpawnAt = time.Now()
			return g
		}

		func (g *World) stardate() float64 {
			return g.sd0 + time.Since(g.start).Seconds()*cfg.StardatePerSec
		}

		func nPlayers() int {
			n := 0
			for _, s := range w.ships {
				if s.active {
					n++
				}
			}
			return n
		}
		func teamCount(t int) int {
			n := 0
			for _, s := range w.ships {
				if s.active && s.team == t {
					n++
				}
			}
			return n
		}

		func (sh *Ship) pct() float64 { return sh.shield / shieldM * 100 }
		func (sh *Ship) effPct() float64 {
			if !sh.shieldsUp {
				return 0
			}
			p := sh.pct()
			if d := sh.dev[DShield]; d > 0 {
				p *= math.Max(0, 1-d/devInop)
			}
			return p
		}
		func (sh *Ship) protected() bool {
			return sh.sess != nil && (sh.sess.inGripe || sh.sess.helpActive) && !sh.red
		}

		func moveShipTo(sh *Ship, v, h int) {
			if inGrid(sh.v, sh.h) && w.grid[sh.v][sh.h].ship == sh {
				w.grid[sh.v][sh.h] = Cell{}
			}
			sh.v, sh.h = v, h
			w.grid[v][h] = Cell{k: KShip, ship: sh}
		}
		func removeShip(sh *Ship) {
			if inGrid(sh.v, sh.h) && w.grid[sh.v][sh.h].ship == sh {
				w.grid[sh.v][sh.h] = Cell{}
			}
		}

		// slowFactor: 1.0 at 300 baud, 0 at >=9600, based on the slowest in-game terminal.
		func slowFactor() float64 {
			min := 9600
			for _, s := range sessions {
				if s.state == stPlay && s.speed < min {
					min = s.speed
				}
			}
			return math.Min(1, math.Max(0, float64(9600-min)/float64(9600-300)))
		}
		func pauseFor(lo, hi float64) float64 { return lo + (hi-lo)*slowFactor() }

		var _ = sort.Ints
		var _ = filepath.Join
		var _ = os.Exit
		var _ = sync.Mutex{}
		var _ = atomic.AddInt32
		var _ = net.Listen
		var _ = flag.Parse

		// ---------------------------------------------------------------- sessions & telnet layer

		const (
			stPre = iota
			stSetup
			stPlay
		)
		const (
			subName = iota
			subTeam
			subShip
		)
		const (
			OutLong = iota
			OutMed
			OutShort
		)
		const (
			OcAbs = iota
			OcRel
			OcBoth
		)

		const (
			tIAC, tDONT, tDO, tWONT, tWILL, tSB, tSE = 255, 254, 253, 252, 251, 250, 240
			tIP, tAO                                 = 244, 245
			oECHO, oSGA, oTTYPE, oNAWS, oTSPEED      = 1, 3, 24, 31, 32
		)

		type Settings struct {
			output, scan, prompt, ocdef, icdef int // scan 0 long 1 short; prompt 0 normal 1 informative; icdef 0 abs 1 rel
			ttype                              string
		}

		type qcmd struct {
			text string
			seq  int
		}

		type Hold struct {
			ship      *Ship
			v, h      int
			until     time.Time
			enteredAt time.Time
		}

		const (
			evOpen = iota
			evClose
			evByte
			evSpeed
			evNaws
			evTtype
		)

		type Event struct {
			s    *Session
			kind int
			b    byte
			a, c int
			str  string
		}

		type Session struct {
			conn     net.Conn
			id       int
			ip       string
			omu      sync.Mutex
			obuf     []byte
			inflight int
			kick     chan struct{}
			dead     bool
			// telnet parser (reader goroutine only)
			tst     int
			tcmd    byte
			sb      []byte
			hisOn   map[byte]bool
			refused map[byte]bool
			// game side
			state, sub  int
			line        []byte
			echo        bool
			lastCR      bool
			queue       []qcmd
			seq         int
			busyUntil   time.Time
			promptShown bool
			lastCmd     string
			lastTell    bool
			curRaw      string
			ship        *Ship
			captain     string
			key         string
			set         Settings
			speed       int
			nawsW       int
			nawsH       int
			ttype       string
			connectAt   time.Time
			lastIn      time.Time
			cpu         time.Duration
			hold        *Hold
			gripe       []string
			gripeLen    int
			inGripe     bool
			helpActive  bool
			setTeam     int
			setAuto     bool
			executing   bool
			closing     bool
		}

		var sessions []*Session
		var evCh = make(chan Event, 4096)
		var connCount int32
		var sessCounter int

		func acceptLoop(ln net.Listener) {
			for {
				c, err := ln.Accept()
				if err != nil {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				if int(atomic.LoadInt32(&connCount)) >= cfg.MaxConn {
					c.Write([]byte("Server full. Try again later.\r\n"))
					c.Close()
					continue
				}
				atomic.AddInt32(&connCount, 1)
				host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
				s := &Session{conn: c, ip: host, kick: make(chan struct{}, 1), echo: true, speed: 9600,
				hisOn: map[byte]bool{}, refused: map[byte]bool{}, connectAt: time.Now(), lastIn: time.Now()}
				// initial negotiation (RFC 854/857/858/1073/1079/1091)
				c.Write([]byte{tIAC, tWILL, oECHO, tIAC, tWILL, oSGA, tIAC, tDO, oSGA,
					tIAC, tDO, oTSPEED, tIAC, tDO, oNAWS, tIAC, tDO, oTTYPE})
				go s.writer()
				go s.reader()
				evCh <- Event{s: s, kind: evOpen}
			}
		}

		func (s *Session) reader() {
			buf := make([]byte, 2048)
			for {
				n, err := s.conn.Read(buf)
				for i := 0; i < n; i++ {
					s.telnetByte(buf[i])
				}
				if err != nil {
					evCh <- Event{s: s, kind: evClose}
					return
				}
			}
		}

		func (s *Session) telnetByte(b byte) {
			switch s.tst {
				case 0:
					if b == tIAC {
						s.tst = 1
					} else {
						evCh <- Event{s: s, kind: evByte, b: b}
					}
				case 1:
					s.tst = 0
					switch b {
						case tIAC:
							evCh <- Event{s: s, kind: evByte, b: 0xFF}
						case tWILL, tWONT, tDO, tDONT:
							s.tcmd = b
							s.tst = 2
						case tSB:
							s.sb = s.sb[:0]
							s.tst = 4
						case tIP:
							evCh <- Event{s: s, kind: evByte, b: 0x03}
						case tAO:
							evCh <- Event{s: s, kind: evByte, b: 0x0F}
					}
						case 2:
							s.tst = 0
							s.handleOpt(s.tcmd, b)
						case 4:
							if b == tIAC {
								s.tst = 5
							} else if len(s.sb) < 256 {
								s.sb = append(s.sb, b)
							}
						case 5:
							switch b {
								case tSE:
									s.handleSB(s.sb)
									s.tst = 0
								case tIAC:
									s.sb = append(s.sb, tIAC)
									s.tst = 4
								default:
									s.tst = 0
							}
			}
		}

		func (s *Session) handleOpt(cmd, opt byte) {
			reply := func(v byte) { s.conn.Write([]byte{tIAC, v, opt}) }
			switch cmd {
				case tDO:
					if opt == oECHO || opt == oSGA {
						return
					}
					if !s.refused[opt] {
						s.refused[opt] = true
						reply(tWONT)
					}
				case tWILL:
					switch opt {
						case oSGA, oNAWS, oTTYPE, oTSPEED:
							if !s.hisOn[opt] {
								s.hisOn[opt] = true
								if opt == oTTYPE || opt == oTSPEED {
									s.conn.Write([]byte{tIAC, tSB, opt, 1, tIAC, tSE})
								}
							}
						default:
							if !s.refused[opt] {
								s.refused[opt] = true
								reply(tDONT)
							}
					}
			}
		}

		func (s *Session) handleSB(sb []byte) {
			if len(sb) < 2 {
				return
			}
			switch sb[0] {
				case oTSPEED:
					if sb[1] == 0 {
						p := strings.SplitN(string(sb[2:]), ",", 2)
						if n, err := strconv.Atoi(strings.TrimSpace(p[0])); err == nil && n > 0 {
							evCh <- Event{s: s, kind: evSpeed, a: n}
						}
					}
				case oNAWS:
					if len(sb) >= 5 {
						evCh <- Event{s: s, kind: evNaws, a: int(sb[1])<<8 | int(sb[2]), c: int(sb[3])<<8 | int(sb[4])}
					}
				case oTTYPE:
					if sb[1] == 0 {
						evCh <- Event{s: s, kind: evTtype, str: string(sb[2:])}
					}
			}
		}

		func (s *Session) writer() {
			for range s.kick {
				for {
					s.omu.Lock()
					if len(s.obuf) == 0 {
						s.omu.Unlock()
						break
					}
					n := len(s.obuf)
					if cfg.Throttle && n > 64 {
						n = 64
					}
					chunk := append([]byte(nil), s.obuf[:n]...)
					s.obuf = s.obuf[n:]
					s.inflight = n
					s.omu.Unlock()
					_, err := s.conn.Write(chunk)
					s.omu.Lock()
					s.inflight = 0
					s.omu.Unlock()
					if err != nil {
						s.omu.Lock()
						s.obuf = nil
						s.omu.Unlock()
						break
					}
					if cfg.Throttle && s.speed > 0 {
						time.Sleep(time.Duration(float64(n) * 10 / float64(s.speed) * float64(time.Second)))
					}
				}
			}
		}

		// out queues text; \n becomes CR LF and 0xFF is doubled.
		func (s *Session) out(str string) {
			if s.dead {
				return
			}
			b := make([]byte, 0, len(str)+8)
			for i := 0; i < len(str); i++ {
				switch c := str[i]; c {
					case '\n':
						b = append(b, '\r', '\n')
					case '\r':
					case 0xFF:
						b = append(b, 0xFF, 0xFF)
					default:
						b = append(b, c)
				}
			}
			s.omu.Lock()
			s.obuf = append(s.obuf, b...)
			s.omu.Unlock()
			select {
					case s.kick <- struct{}{}:
					default:
			}
		}
		func (s *Session) printf(f string, a ...interface{}) { s.out(fmt.Sprintf(f, a...)) }
		func (s *Session) pending() bool {
			s.omu.Lock()
			defer s.omu.Unlock()
			return len(s.obuf)+s.inflight > 0
		}
		func (s *Session) flushOut() {
			s.omu.Lock()
			s.obuf = nil
			s.omu.Unlock()
		}
		func (s *Session) shutdown() {
			go func() {
				for i := 0; i < 30 && s.pending(); i++ {
					time.Sleep(100 * time.Millisecond)
				}
				s.conn.Close()
			}()
		}

		// async output (hits, TELLs, distress): never corrupts a half-typed line (3.5)
		func (s *Session) async(t string) {
			if s.dead {
				return
			}
			partial := ""
			if s.echo {
				partial = string(s.line)
			}
			switch {
				case s.promptShown:
					s.out("\n" + t + s.promptText() + partial)
				case len(s.line) > 0:
					s.out("\n" + t + partial)
				default:
					s.out(t)
			}
		}

		func (s *Session) team() int {
			if s.ship != nil {
				return s.ship.team
			}
			return -1
		}

		func (s *Session) promptText() string {
			switch s.state {
				case stPre:
					return "PG> "
				case stSetup:
					return [...]string{"Enter your name: ", "Federation or Empire? ", "Ship: "}[s.sub]
			}
			if s.inGripe {
				return ""
			}
			if s.set.prompt == 1 && s.ship != nil {
				sh := s.ship
				f := ""
				if !sh.shieldsUp || sh.pct() < 10 {
					f += "S"
				}
				if sh.energy < yellowE {
					f += "E"
				}
				if sh.damage > 2000 {
					f += "D"
				}
				if sh.dev[DLife] >= devInop {
					f += fmt.Sprintf("%dL", sh.life)
				}
				return f + "> "
			}
			return "COMMAND:  "
		}

		// ---------------------------------------------------------------- line editor & dispatcher

		func (s *Session) inputByte(b byte) {
			b &= 0x7f
			s.lastIn = time.Now()
			if s.lastCR {
				s.lastCR = false
				if b == 0x0A || b == 0 {
					return
				}
			}
			switch b {
				case 0x0D:
					s.lastCR = true
					s.terminate(b)
				case 0x0A, 0x0B, 0x0C, 0x1B, 0x1A:
					s.terminate(b)
				case 0x08, 0x7F:
					if len(s.line) > 0 {
						s.line = s.line[:len(s.line)-1]
						if s.echo {
							s.out("\b \b")
						}
					}
				case 0x15:
					s.line = s.line[:0]
					s.echo = true
					s.out("^U\n")
					s.promptShown = false
				case 0x12:
					s.out("\n" + s.promptText() + string(s.line))
					s.promptShown = true
				case 0x07:
					s.echo = !s.echo
				case 0x0F:
					s.flushOut()
					s.out("^O\n")
				case 0x03:
					s.ctrlC()
				case 0x00:
				default:
					if b >= 0x20 || b == 9 {
						if len(s.line) >= 255 {
							s.out("\a")
						} else {
							s.line = append(s.line, b)
							if s.echo {
								s.out(string(rune(b)))
							}
						}
					}
			}
		}

		func (s *Session) terminate(b byte) {
			if b == 0x1B && len(s.line) == 0 && s.state != stSetup && !s.inGripe {
				if s.lastCmd == "" || s.lastTell {
					s.out("\a")
					return
				}
				s.out(s.lastCmd + "\n")
				s.seq++
				s.queue = append(s.queue, qcmd{s.lastCmd, s.seq})
				s.promptShown = false
				return
			}
			line := string(s.line)
			s.line = s.line[:0]
			s.echo = true
			s.out("\n")
			s.promptShown = false
			switch {
				case s.state == stSetup:
					s.setupLine(line)
				case s.inGripe:
					s.gripeLine(line, b)
				default:
					s.seq++
					for _, c := range splitLine(line) {
						if strings.TrimSpace(c) != "" {
							s.queue = append(s.queue, qcmd{c, s.seq})
						}
					}
			}
		}

		func isSep(r rune) bool { return r == ' ' || r == '\t' || r == ',' }

		func firstWord(seg string) string {
			if i := strings.IndexAny(seg, ";/"); i >= 0 {
				seg = seg[:i]
			}
			f := strings.FieldsFunc(seg, isSep)
			if len(f) == 0 {
				return ""
			}
			return f[0]
		}

		// splitLine implements the 3.6 grammar: '/' separates commands, ';' starts a comment,
		// except TELL which swallows the remainder of the line.
		func splitLine(line string) []string {
			var res []string
			rest := line
			for {
				rest = strings.TrimLeft(rest, " \t")
				if rest == "" {
					break
				}
				if fw := firstWord(rest); fw != "" {
					if m, st := resolve(fw, gameCmdList); st == 0 && m == "TELL" {
						res = append(res, rest)
						break
					}
				}
				i := strings.IndexByte(rest, '/')
				seg := rest
				if i >= 0 {
					seg = rest[:i]
				}
				if j := strings.IndexByte(seg, ';'); j >= 0 {
					res = append(res, strings.TrimSpace(seg[:j]))
					break
				}
				res = append(res, seg)
				if i < 0 {
					break
				}
				rest = rest[i+1:]
			}
			return res
		}

		func (s *Session) discard(seq int) {
			q := s.queue[:0]
			for _, c := range s.queue {
				if c.seq != seq {
					q = append(q, c)
				}
			}
			s.queue = q
		}

		func (s *Session) ctrlC() {
			busy := len(s.queue) > 0 || time.Now().Before(s.busyUntil)
			s.line = s.line[:0]
			s.echo = true
			s.out("^C\n")
			s.promptShown = false
			switch {
				case s.state == stSetup:
					s.state = stPre
				case s.inGripe:
					s.inGripe = false
					s.gripe = nil
					s.out("Gripe aborted.\n")
				case busy:
					s.queue = nil
					s.busyUntil = time.Time{}
					s.out("\a\a\a")
				case s.state == stPlay && s.ship != nil:
					if s.ship.red {
						s.out("\aRed alert: ^C ignored. Use QUIT to leave.\n")
					} else {
						s.abortGame()
					}
			}
		}

		func (s *Session) abortGame() {
			sh := s.ship
			s.hold = &Hold{ship: sh, v: sh.v, h: sh.h, until: time.Now().Add(cfg.CtrlCHold), enteredAt: sh.enteredAt}
			radioTeam(sh.team, fmt.Sprintf("%s has left the game.\n", sh.name), sh)
			releaseShip(sh)
			s.ship = nil
			s.state = stPre
			s.out("Game aborted; ship released. ACTIVATE within 60 seconds to resume.\n")
		}

		func (s *Session) pump() {
			if s.dead || s.closing {
				return
			}
			now := time.Now()
			for s.state != stSetup && !s.inGripe && len(s.queue) > 0 && !now.Before(s.busyUntil) && !s.pending() {
				c := s.queue[0]
				s.queue = s.queue[1:]
				s.promptShown = false
				s.exec(c)
				if s.dead {
					return
				}
				now = time.Now()
			}
			if s.helpActive && !s.pending() {
				s.helpActive = false
			}
			if !s.promptShown && !s.inGripe && len(s.queue) == 0 && !now.Before(s.busyUntil) && !s.pending() {
				s.promptShown = true
				s.out(s.promptText() + string(s.line))
			}
		}

		type cmdErr struct {
			msg   string
			fatal bool
		}

		func (e cmdErr) Error() string { return e.msg }
		func bad(m string) error       { return cmdErr{m, false} }
		func syn(cmd string) error     { return cmdErr{"Usage: " + usage[cmd], true} }

		var gameCmdList = []string{"BASES", "BUILD", "CAPTURE", "DAMAGES", "DOCK", "ENERGY", "GRIPE", "HELP",
			"IMPULSE", "LIST", "MOVE", "NEWS", "PHASERS", "PLANETS", "POINTS", "QUIT", "RADIO", "REPAIR",
			"SCAN", "SET", "SHIELDS", "SRSCAN", "STATUS", "SUMMARY", "TARGETS", "TELL", "TIME", "TORPEDOES",
			"TRACTOR", "TYPE", "USERS"}
			var preCmdList = []string{"ACTIVATE", "GRIPE", "HELP", "NEWS", "POINTS", "QUIT", "SUMMARY", "TIME", "USERS"}
			var infoCmds = map[string]bool{"BASES": true, "DAMAGES": true, "GRIPE": true, "HELP": true, "LIST": true,
				"NEWS": true, "PLANETS": true, "POINTS": true, "QUIT": true, "RADIO": true, "SCAN": true, "SET": true,
				"SRSCAN": true, "STATUS": true, "SUMMARY": true, "TARGETS": true, "TELL": true, "TIME": true,
				"TYPE": true, "USERS": true}

				// capName renders a command with its minimum prefix capitalised, computed from the table.
				func capName(name string, list []string) string {
					for n := 1; n <= len(name); n++ {
						if m, st := resolve(name[:n], list); st == 0 && m == name {
							return name[:n] + strings.ToLower(name[n:])
						}
					}
					return name
				}

				type handler func(s *Session, a []string) error

				var handlers map[string]handler

				func (s *Session) exec(c qcmd) {
					words := strings.FieldsFunc(c.text, isSep)
					if len(words) == 0 {
						return
					}
					s.curRaw = c.text
					list := gameCmdList
					if s.state == stPre {
						list = preCmdList
					}
					name, st := resolve(words[0], list)
					if st != 0 {
						if s.state == stPre {
							if _, st2 := resolve(words[0], gameCmdList); st2 == 0 {
								s.out("Command not legal in pre-game.\n")
								s.discard(c.seq)
								return
							}
						}
						if st == 1 {
							s.out("Ambiguous command\n")
						} else {
							s.out("Invalid command\n")
						}
						s.discard(c.seq)
						return
					}
					if s.state == stPlay && (s.ship == nil || !s.ship.active) {
						s.state = stPre
						return
					}
					if w.over && s.state == stPlay && !infoCmds[name] {
						s.out("The game is over.\n")
						return
					}
					if name == "TELL" {
						s.lastTell = true
					} else {
						s.lastCmd, s.lastTell = c.text, false
					}
					t0 := time.Now()
					err := handlers[name](s, words[1:])
					s.cpu += time.Since(t0)
					if err != nil {
						if ce, ok := err.(cmdErr); ok {
							s.out(ce.msg + "\n")
							if ce.fatal {
								s.discard(c.seq)
							}
						} else {
							s.out(err.Error() + "\n")
							s.discard(c.seq)
						}
					}
				}

				// timed: block the command queue and run one auto-repair "turn".
				func (s *Session) timed(secs float64) {
					s.busyUntil = time.Now().Add(sec(secs))
					if s.ship != nil {
						autoRepair(s.ship)
					}
				}

				// ---------------------------------------------------------------- session events

				func handleEvent(e Event) {
					s := e.s
					switch e.kind {
						case evOpen:
							sessCounter++
							s.id = sessCounter
							sessions = append(sessions, s)
							s.out(version + "\n")
							s.out(optionNote() + "\n")
							s.out("Type HELP for help, ACTIVATE to enter the game.\n")
							s.pump()
						case evClose:
							s.disconnected()
						case evByte:
							if !s.dead {
								s.inputByte(e.b)
								s.pump()
							}
						case evSpeed:
							s.speed = e.a
						case evNaws:
							s.nawsW, s.nawsH = e.a, e.c
						case evTtype:
							s.ttype = e.str
					}
				}

				func optionNote() string {
					yn := func(b bool) string {
						if b {
							return "included"
						}
						return "not included"
					}
					return fmt.Sprintf("Romulan: %s. Black holes: %s.", yn(w.romulan), yn(w.blackholes))
				}

				func (s *Session) disconnected() {
					if s.dead {
						return
					}
					if s.ship != nil && s.ship.active {
						sh := s.ship
						if sh.red {
							sh.sess = nil
							sh.orphanUntil = time.Now().Add(cfg.DropHold)
						} else {
							radioTeam(sh.team, fmt.Sprintf("%s has left the game.\n", sh.name), sh)
							releaseShip(sh)
						}
					}
					s.ship = nil
					s.dead = true
					for i, x := range sessions {
						if x == s {
							sessions = append(sessions[:i], sessions[i+1:]...)
							break
						}
					}
					close(s.kick)
					s.conn.Close()
					atomic.AddInt32(&connCount, -1)
				}

				func releaseShip(sh *Ship) {
					breakTractor(sh)
					removeShip(sh)
					sh.active = false
					sh.sess = nil
					sh.orphanUntil = time.Time{}
					sh.moveTok++
					checkWin()
				}

				// ---------------------------------------------------------------- pre-game / setup flow

				func (s *Session) startSetup() {
					if h := s.hold; h != nil {
						s.hold = nil
						sh := h.ship
						if !sh.active && time.Now().Before(h.until) && sh.enteredAt.Equal(h.enteredAt) &&
							inGrid(h.v, h.h) && w.grid[h.v][h.h].k == KEmpty && !w.over {
								sh.active = true
								sh.sess = s
								sh.v, sh.h = 0, 0
								moveShipTo(sh, h.v, h.h)
								s.ship = sh
								s.state = stPlay
								s.printf("Resuming command of the %s.\n", sh.name)
								return
							}
							s.out("Your ship is no longer available.\n")
					}
					s.state = stSetup
					s.sub = subName
					s.setAuto = false
				}

				func (s *Session) availShips(team int) []*Ship {
					var r []*Ship
					for _, sh := range w.ships {
						if sh.team == team && !sh.active {
							r = append(r, sh)
						}
					}
					return r
				}

				func (s *Session) enterShipPrompt() {
					av := s.availShips(s.setTeam)
					if len(av) == 0 {
						s.printf("All %s ships are occupied.\n", teamNames[s.setTeam])
						if s.setAuto {
							s.state = stPre
						} else {
							s.sub = subTeam
						}
						return
					}
					var n []string
					for _, sh := range av {
						n = append(n, sh.name)
					}
					s.printf("Available ships: %s\n", strings.Join(n, ", "))
					s.sub = subShip
				}

				func (s *Session) setupLine(line string) {
					line = strings.TrimSpace(line)
					switch s.sub {
						case subName:
							if line == "" {
								return
							}
							if len(line) > 12 {
								line = line[:12]
							}
							key := sanitizeKey(line)
							now := time.Now()
							for _, sh := range w.ships {
								if sh.active && sh.rec != nil && sh.rec.key == key {
									s.out("That captain is already in the game.\n")
									s.state = stPre
									return
								}
							}
							rec := w.recs[key]
							if rec != nil && now.Before(rec.lockUntil) {
								s.printf("You must wait %d seconds before rejoining.\n", int(rec.lockUntil.Sub(now).Seconds())+1)
								s.state = stPre
								return
							}
							if t, ok := w.ipLock[s.ip]; ok && now.Before(t) {
								s.printf("You must wait %d seconds before rejoining.\n", int(t.Sub(now).Seconds())+1)
								s.state = stPre
								return
							}
							s.captain, s.key = line, key
							fed, emp := teamCount(FED), teamCount(EMP)
							switch {
								case rec != nil:
									s.setTeam, s.setAuto = rec.team, true
									s.printf("Welcome back. You rejoin the %s.\n", teamNames[rec.team])
									s.enterShipPrompt()
								case abs(fed-emp) >= 2:
									t := FED
									if emp < fed {
										t = EMP
									}
									s.setTeam, s.setAuto = t, true
									s.printf("Teams are unbalanced. You are assigned to the %s.\n", teamNames[t])
									s.enterShipPrompt()
								default:
									s.sub = subTeam
							}
								case subTeam:
									switch kw(line, "FEDERATION", "HUMAN", "EMPIRE", "KLINGON") {
										case "FEDERATION", "HUMAN":
											s.setTeam = FED
										case "EMPIRE", "KLINGON":
											s.setTeam = EMP
										default:
											s.out("Please answer Federation (Human) or Empire (Klingon).\n")
											return
									}
									s.enterShipPrompt()
										case subShip:
											if line == "" {
												return
											}
											var names []string
											for _, sh := range s.availShips(s.setTeam) {
												names = append(names, up(sh.name))
											}
											m, st := resolve(line, names)
											if st != 0 {
												s.out("Invalid ship. Choose one of the available ships.\n")
												return
											}
											for _, sh := range w.ships {
												if up(sh.name) == m {
													s.commission(sh)
													return
												}
											}
					}
				}

				func (s *Session) startPos(team int) (int, int) {
					var mine []*Base
					for _, b := range w.bases {
						if b.alive && b.team == team {
							mine = append(mine, b)
						}
					}
					safe := func(v, h int) bool {
						if !inGrid(v, h) || w.grid[v][h].k != KEmpty {
							return false
						}
						for _, b := range w.bases {
							if b.alive && b.team != team && cheb(v, h, b.v, b.h) <= 2 {
								return false
							}
						}
						for _, p := range w.planets {
							if p.owner != team && cheb(v, h, p.v, p.h) <= planetRange {
								return false
							}
						}
						return true
					}
					for try := 0; try < 5000; try++ {
						var v, h int
						if len(mine) > 0 {
							b := mine[rand.Intn(len(mine))]
							v, h = b.v+rand.Intn(17)-8, b.h+rand.Intn(17)-8
						} else {
							v, h = 1+rand.Intn(gridN), 1+rand.Intn(gridN)
						}
						if safe(v, h) {
							return v, h
						}
					}
					return w.randEmpty(1, gridN)
				}

				func (s *Session) commission(sh *Ship) {
					rec := w.recs[s.key]
					if rec == nil {
						rec = &Rec{key: s.key}
						w.recs[s.key] = rec
					}
					rec.team = sh.team
					rec.sc.commissioned++
					now := time.Now()
					sh.rec, sh.sess, sh.active = rec, s, true
					sh.energy, sh.shield, sh.damage = shipEnergyM, shieldM, 0
					sh.dev = [9]float64{}
					sh.torps, sh.life, sh.lifeAcc = torpsMax, lifeMax, 0
					sh.red, sh.docked, sh.shieldsUp, sh.tractor = false, false, false, nil
					sh.radioOn = true
					sh.gagged = map[*Ship]bool{}
					sh.phReady, sh.torpReady = [2]time.Time{}, time.Time{}
					sh.nextAuto = now.Add(5 * time.Second)
					sh.enteredAt = now
					sh.lastAtk, sh.lastAtkRom = nil, false
					sh.moveTok++
					sh.v, sh.h = 0, 0
					v, h := s.startPos(sh.team)
					moveShipTo(sh, v, h)
					s.ship = sh
					s.state = stPlay
					s.queue = nil
					s.applyProfile()
					s.printf("You are captain of the %s (%s), %s.\n", sh.name, s.captain, teamNames[sh.team])
					radioAll(fmt.Sprintf("%s (%s) has entered the game.\n", sh.name, s.captain), sh)
				}

				func (s *Session) profilePath() string { return filepath.Join(cfg.ProfileDir, s.key+".ini") }

				func (s *Session) applyProfile() {
					data, err := os.ReadFile(s.profilePath())
					if err != nil {
						return
					}
					for _, ln := range strings.Split(string(data), "\n") {
						f := strings.FieldsFunc(ln, isSep)
						if len(f) > 1 && up(f[0]) == "SET" {
							cmdSet(s, f[1:])
						}
					}
				}

				func (s *Session) saveProfile() {
					if s.key == "" {
						return
					}
					os.MkdirAll(cfg.ProfileDir, 0o755)
					st := s.set
					oc := [...]string{"ABSOLUTE", "RELATIVE", "BOTH"}[st.ocdef]
					ic := [...]string{"ABSOLUTE", "RELATIVE"}[st.icdef]
					out := [...]string{"LONG", "MEDIUM", "SHORT"}[st.output]
					sc := [...]string{"LONG", "SHORT"}[st.scan]
					pr := [...]string{"NORMAL", "INFORMATIVE"}[st.prompt]
					txt := fmt.Sprintf("SET NAME %s\nSET OUTPUT %s\nSET SCAN %s\nSET PROMPT %s\nSET OCDEF %s\nSET ICDEF %s\n",
							   s.captain, out, sc, pr, oc, ic)
					os.WriteFile(s.profilePath(), []byte(txt), 0o644)
				}

				// ---------------------------------------------------------------- radios, "who" descriptors, hit formatting

				func radioTeam(team int, text string, except *Ship) {
					for _, sh := range w.ships {
						if sh.active && sh.sess != nil && sh != except && (team < 0 || sh.team == team) &&
							sh.radioOn && sh.dev[DRadio] < devInop {
								sh.sess.async(text)
							}
					}
				}
				func radioAll(text string, except *Ship) { radioTeam(-1, text, except) }

				type Who struct {
					long, abbr string
					v, h       int
					pct        float64
					hasPct, up bool
					rom        bool
				}

				func whoShip(sh *Ship) Who {
					return Who{long: sh.name, abbr: sh.name[:1], v: sh.v, h: sh.h, pct: sh.pct(), hasPct: true, up: sh.shieldsUp}
				}
				func whoBase(b *Base) Who {
					n, a := "Fed base", "<>"
					if b.team == EMP {
						n, a = "Emp base", ")("
					}
					return Who{long: n, abbr: a, v: b.v, h: b.h, pct: b.shield / shieldM * 100, hasPct: true, up: true}
				}
				func whoPlanet(p *Planet) Who {
					n, a := "Neutral planet", "@"
					switch p.owner {
						case FED:
							n, a = "Fed planet", "+@"
						case EMP:
							n, a = "Emp planet", "-@"
					}
					return Who{long: fmt.Sprintf("%s(%d)", n, p.builds), abbr: fmt.Sprintf("%s%d", a, p.builds), v: p.v, h: p.h}
				}
				func whoStar(v, h int) Who { return Who{long: "Star", abbr: "*", v: v, h: h} }
				func whoRom() Who {
					x := Who{long: "Romulan", abbr: "?", rom: true}
					if w.rom != nil {
						x.v, x.h = w.rom.v, w.rom.h
					}
					return x
				}

				// loc renders a coordinate per the viewer's OCDEF.
				func (s *Session) loc(v, h int) string {
					if s.ship == nil || s.set.ocdef == OcAbs {
						return fmt.Sprintf("%d-%d", v, h)
					}
					rel := fmt.Sprintf("%+d,%+d", v-s.ship.v, h-s.ship.h)
					if s.set.ocdef == OcRel {
						return rel
					}
					return fmt.Sprintf("%d-%d %s", v, h, rel)
				}

				func pctStr(x Who, short bool) string {
					sg := '+'
					if !x.up {
						sg = '-'
					}
					if short {
						return fmt.Sprintf("%c%d", sg, int(x.pct))
					}
					return fmt.Sprintf("%c%.1f%%", sg, x.pct)
				}

				func fmtHit(r *Session, src, tgt Who, amt float64, typ byte, disp bool) string {
					switch r.set.output {
						case OutMed:
							b := src.abbr + " @" + r.loc(src.v, src.h)
							if src.hasPct {
								b += ", " + pctStr(src, false)
							}
							b += fmt.Sprintf("  %.1f unit %c  %s ", amt, typ, tgt.abbr)
							if disp {
								b += "-->" + r.loc(tgt.v, tgt.h)
							} else {
								b += "@" + r.loc(tgt.v, tgt.h)
							}
							if tgt.hasPct {
								b += ", " + pctStr(tgt, false)
							}
							return b + "\n"
						case OutShort:
							b := src.abbr + " " + r.loc(src.v, src.h)
							if src.hasPct {
								b += " " + pctStr(src, true)
							}
							b += fmt.Sprintf("  %d%c  %s ", int(amt), typ, tgt.abbr)
							if disp {
								b += ">"
							}
							b += r.loc(tgt.v, tgt.h)
							if tgt.hasPct {
								b += " " + pctStr(tgt, true)
							}
							return b + "\n"
					}
					kind := ""
					switch typ {
						case 'T':
							kind = "torpedo "
						case 'P':
							kind = "phaser "
					}
					b := src.long + " @" + r.loc(src.v, src.h)
					if src.hasPct {
						b += ", " + pctStr(src, false) + "  "
					} else {
						b += " "
					}
					b += fmt.Sprintf("makes %.1f unit %shit on %s", amt, kind, tgt.long)
					if disp {
						b += " displaced to " + r.loc(tgt.v, tgt.h)
					} else {
						b += " @" + r.loc(tgt.v, tgt.h)
					}
					if tgt.hasPct {
						b += ", " + pctStr(tgt, false)
					}
					return b + "\n"
				}

				func tellHit(sh *Ship, src, tgt Who, amt float64, typ byte, disp bool) {
					if sh != nil && sh.sess != nil {
						sh.sess.async(fmtHit(sh.sess, src, tgt, amt, typ, disp))
					}
				}

				func note(sh *Ship, msg string) {
					if sh != nil && sh.sess != nil {
						sh.sess.async(msg)
					}
				}

				// ---------------------------------------------------------------- damage application

				func devDamage(sh *Ship, dev int, amt float64) {
					was := sh.dev[dev]
					sh.dev[dev] += amt
					if dev == DShield && sh.dev[dev] >= devInop {
						sh.shieldsUp = false
					}
					if was < devInop && sh.dev[dev] >= devInop {
						note(sh, fmt.Sprintf("%s inoperative.\n", devNames[dev]))
					} else {
						note(sh, fmt.Sprintf("%s damaged.\n", devNames[dev]))
					}
				}

				func breakTractor(sh *Ship) {
					if sh.tractor == nil {
						return
					}
					p := sh.tractor
					sh.tractor, p.tractor = nil, nil
					note(sh, "Tractor beam broken.\n")
					note(p, "Tractor beam broken.\n")
				}

				func displace(t *Ship, sv, sh int) bool {
					n := rand.Intn(3)
					vd, hd := t.v-sv, t.h-sh
					m := max(abs(vd), abs(hd))
					var dv, dh int
					if m == 0 {
						for dv == 0 && dh == 0 {
							dv, dh = rand.Intn(3)-1, rand.Intn(3)-1
						}
					} else {
						dv = int(math.Round(float64(vd) / float64(m)))
						dh = int(math.Round(float64(hd) / float64(m)))
					}
					for i := 0; i < n; i++ {
						nv, nh := t.v+dv, t.h+dh
						if !inGrid(nv, nh) {
							break
						}
						switch w.grid[nv][nh].k {
							case KHole:
								return true
							case KEmpty:
								moveShipTo(t, nv, nh)
							default:
								return false
						}
					}
					return false
				}

				func hitShip(t, atk *Ship, src Who, amt float64, typ byte) {
					if !t.active {
						return
					}
					if t.protected() {
						note(atk, fmt.Sprintf("%s is protected.\n", t.name))
						return
					}
					p := t.effPct()
					deflected := typ == 'T' && p >= 85 && rand.Float64() < (p-70)/30
					absorbed := 0.0
					if t.shieldsUp {
						absorbed = math.Min(amt*p/100, t.shield)
					}
					rem := amt - absorbed
					if deflected {
						rem = 0
					}
					t.shield -= absorbed
					t.damage += rem
					t.energy -= rem
					t.red = true
					t.lastAtk, t.lastAtkRom = atk, src.rom
					if typ == 'T' || typ == 'N' {
						breakTractor(t)
					}
					switch {
						case atk != nil && atk.team != t.team:
							atk.rec.sc.dmgEnemy += amt
						case src.rom:
							w.romScore.dmgEnemy += amt
					}
					died, disp := false, false
					if (typ == 'T' || typ == 'N') && !deflected && t.damage < dmgLimit && t.energy > 0 {
						ov, oh := t.v, t.h
						died = displace(t, src.v, src.h)
						disp = t.v != ov || t.h != oh
					}
					tellHit(t, src, whoShip(t), amt, typ, disp)
					if atk != nil && atk != t {
						tellHit(atk, src, whoShip(t), amt, typ, disp)
					}
					if deflected {
						note(t, "Torpedo deflected by shields.\n")
					}
					if died {
						note(t, "Displaced into a black hole!\n")
					}
					if died || t.damage >= dmgLimit || t.energy <= 0 {
						destroyShip(t, atk, src.rom)
					}
				}

				func destroyShip(t, killer *Ship, krom bool) {
					if !t.active {
						return
					}
					s := t.sess
					breakTractor(t)
					removeShip(t)
					t.active, t.sess = false, nil
					t.moveTok++
					if t.rec != nil {
						t.rec.cas++
					}
					text := fmt.Sprintf("*** %s destroyed ***\n", t.name)
					if killer != nil && killer.team != t.team && killer.rec != nil {
						killer.rec.sc.kills++
						text = fmt.Sprintf("*** %s destroyed by %s ***\n", t.name, killer.name)
						note(killer, text)
					} else if krom {
						w.romScore.kills++
						text = fmt.Sprintf("*** %s destroyed by the Romulan ***\n", t.name)
					}
					radioTeam(t.team, text, nil)
					if s != nil {
						s.ship = nil
						s.state = stPre
						s.queue = nil
						s.busyUntil = time.Time{}
						s.async("*** Your ship has been destroyed. Returning to pre-game. ***\n")
					}
					checkWin()
				}

				func baseHit(b *Base, atk *Ship, src Who, amt float64, typ byte) {
					if !b.alive {
						return
					}
					pc := b.shield / shieldM
					a := amt * pc
					b.shield -= a
					b.damage += amt - a
					enemy := atk != nil && atk.team != b.team
					if enemy {
						atk.rec.sc.dmgBase += amt
					}
					tellHit(atk, src, whoBase(b), amt, typ, false)
					if !b.distress {
						b.distress = true
						radioTeam(b.team, fmt.Sprintf("Starbase at %d-%d under attack!\n", b.v, b.h), nil)
					}
					if b.damage >= dmgLimit {
						b.alive = false
						w.grid[b.v][b.h] = Cell{}
						if enemy {
							atk.rec.sc.kills++
							note(atk, fmt.Sprintf("Enemy starbase at %d-%d destroyed!\n", b.v, b.h))
						}
						radioTeam(b.team, fmt.Sprintf("Starbase at %d-%d destroyed!\n", b.v, b.h), nil)
						checkWin()
					}
				}

				func removePlanet(p *Planet) {
					w.grid[p.v][p.h] = Cell{}
					for i, x := range w.planets {
						if x == p {
							w.planets = append(w.planets[:i], w.planets[i+1:]...)
							break
						}
					}
				}

				// planet hit by phaser/nova: installations only, never destroyed
				func planetSoftHit(p *Planet, atk *Ship, src Who, amt float64, typ byte) {
					if p.builds <= 0 {
						note(atk, "That planet has no installations left to damage.\n")
						return
					}
					p.integrity -= amt
					if p.integrity <= 0 {
						p.builds--
						p.integrity = 300
					}
					if atk != nil && atk.team != p.owner {
						atk.rec.sc.dmgBase += amt
					}
					tellHit(atk, src, whoPlanet(p), amt, typ, false)
				}

				func planetTorpHit(p *Planet, atk *Ship, src Who, amt float64) {
					tellHit(atk, src, whoPlanet(p), amt, 'T', false)
					if p.builds > 0 {
						p.builds--
						p.integrity = 300
						return
					}
					removePlanet(p)
					if atk != nil {
						atk.rec.sc.plDestroy++
						note(atk, fmt.Sprintf("Planet at %d-%d destroyed. (-100 points)\n", p.v, p.h))
					}
					checkWin()
				}

				func romHit(atk *Ship, src Who, amt float64, typ byte) {
					rm := w.rom
					if rm == nil {
						return
					}
					rm.damage += amt
					rm.revealUntil = time.Now().Add(5 * time.Second)
					if atk != nil {
						atk.rec.sc.dmgEnemy += amt
						tellHit(atk, src, whoRom(), amt, typ, false)
					}
					if rm.damage >= 3000 {
						w.grid[rm.v][rm.h] = Cell{}
						w.rom = nil
						w.romSpawnAt = time.Now().Add(cfg.RomulanRespawn)
						by := "unknown forces"
						if atk != nil {
							atk.rec.sc.romKills++
							by = atk.name
						}
						radioAll(fmt.Sprintf("*** The Romulan has been destroyed by %s ***\n", by), nil)
					}
				}

				func nova(fs *Ship, v, h int) {
					w.grid[v][h] = Cell{}
					src := whoStar(v, h)
					if fs != nil {
						fs.rec.sc.stars++
						note(fs, fmt.Sprintf("Star at %d-%d goes nova! (-50 points)\n", v, h))
					}
					amt := math.Round((torpBase+rf(-30, 30))*10) / 10
					var ships []*Ship
					var bases []*Base
					var pls []*Planet
					rom := false
					for dv := -3; dv <= 3; dv++ {
						for dh := -3; dh <= 3; dh++ {
							nv, nh := v+dv, h+dh
							if !inGrid(nv, nh) {
								continue
							}
							c := w.grid[nv][nh]
							switch c.k {
								case KShip:
									ships = append(ships, c.ship)
								case KBase:
									bases = append(bases, c.base)
								case KPlanet:
									pls = append(pls, c.pl)
								case KRom:
									rom = true
							}
						}
					}
					for _, t := range ships {
						hitShip(t, fs, src, amt, 'N')
					}
					for _, b := range bases {
						baseHit(b, fs, src, amt, 'N')
					}
					for _, p := range pls {
						planetSoftHit(p, fs, src, amt, 'N')
					}
					if rom {
						romHit(fs, src, amt, 'N')
					}
				}

				// ---------------------------------------------------------------- torpedoes

				func launchTorpedo(fs *Ship, fv, fh, tv, th int, comp bool) bool {
					if fs != nil {
						if rand.Float64() < 0.02+fs.dev[DTorp]/1000 {
							note(fs, "Torpedo misfire! Burst aborted.\n")
							devDamage(fs, DTorp, rf(10, 60))
							return false
						}
					}
					sigma := 1.0
					if comp {
						sigma = 0.5
					}
					if fs != nil {
						sigma += 0.03*fs.effPct() + 0.05*fs.dev[DComputer] + 0.05*fs.dev[DTorp]
					}
					ang := math.Atan2(float64(th-fh), float64(tv-fv)) + rand.NormFloat64()*sigma*math.Pi/180
					uv, uh := math.Cos(ang), math.Sin(ang)
					rng := 8 + rand.Intn(3)
					lv, lh := fv, fh
					for i := 1; i <= rng; i++ {
						v := fv + int(math.Round(float64(i)*uv))
						h := fh + int(math.Round(float64(i)*uh))
						if v == lv && h == lh {
							continue
						}
						lv, lh = v, h
						if !inGrid(v, h) {
							break
						}
						if torpImpact(fs, v, h) {
							break
						}
					}
					return true
				}

				func torpImpact(fs *Ship, v, h int) bool {
					c := w.grid[v][h]
					src := whoRom()
					if fs != nil {
						src = whoShip(fs)
					}
					amt := math.Round((torpBase+rf(-30, 30))*10) / 10
					switch c.k {
						case KStar:
							nova(fs, v, h)
							return true
						case KHole:
							note(fs, "Torpedo swallowed by a black hole.\n")
							return true
						case KShip:
							if fs != nil && c.ship.team == fs.team {
								note(fs, fmt.Sprintf("Torpedo accidentally hit friendly %s: neutralized.\n", c.ship.name))
								return true
							}
							hitShip(c.ship, fs, src, amt, 'T')
							return true
						case KRom:
							if fs == nil {
								return false
							}
							romHit(fs, src, amt, 'T')
							return true
						case KBase:
							if fs != nil && c.base.team == fs.team {
								note(fs, "Torpedo accidentally hit a friendly starbase: neutralized.\n")
								return true
							}
							baseHit(c.base, fs, src, amt, 'T')
							return true
						case KPlanet:
							if fs != nil && c.pl.owner == fs.team {
								note(fs, "Torpedo accidentally hit a friendly planet: neutralized.\n")
								return true
							}
							planetTorpHit(c.pl, fs, src, amt)
							return true
					}
					return false
				}

				// ---------------------------------------------------------------- repair, win, reset

				func autoRepair(sh *Ship) {
					wasLife := sh.dev[DLife] >= devInop
					for i := range sh.dev {
						if sh.dev[i] > 0 {
							sh.dev[i] = math.Max(0, sh.dev[i]-30)
						}
					}
					if wasLife && sh.dev[DLife] < devInop {
						sh.life = lifeMax
					}
					sh.nextAuto = time.Now().Add(5 * time.Second)
				}

				func hasAssets(team int) bool {
					for _, b := range w.bases {
						if b.alive && b.team == team {
							return true
						}
					}
					for _, p := range w.planets {
						if p.owner == team {
							return true
						}
					}
					for _, s := range w.ships {
						if s.active && s.team == team {
							return true
						}
					}
					return false
				}

				func checkWin() {
					if w.over {
						return
					}
					for t := 0; t < 2; t++ {
						if hasAssets(t) && !hasAssets(1-t) {
							w.over, w.winner = true, t
							w.resetAt = time.Now().Add(60 * time.Second)
							msg := fmt.Sprintf("*** The %s has won the game! New game in 60 seconds. ***\n", teamNames[t])
							for _, s := range sessions {
								s.async(msg)
								if s.state == stPlay {
									s.out(pointsText(s, []string{"ALL"}))
								}
							}
							return
						}
					}
				}

				func resetGame() {
					for _, sh := range w.ships {
						sh.active, sh.sess = false, nil
						sh.moveTok++
					}
					w = newWorld()
					for _, s := range sessions {
						s.ship, s.hold, s.queue, s.inGripe = nil, nil, nil, false
						s.busyUntil = time.Time{}
						s.state = stPre
						s.async("A new game has begun. Returning to pre-game; ACTIVATE to play.\n")
					}
				}

				// ---------------------------------------------------------------- automata

				var lastTick = time.Now()
				var nextObserve time.Time

				func autoTick(now time.Time) {
					dt := now.Sub(lastTick).Seconds()
					lastTick = now
					if w.over {
						if now.After(w.resetAt) {
							resetGame()
						}
						return
					}
					// starbases
					for _, b := range w.bases {
						if !b.alive {
							continue
						}
						if now.After(b.nextFire) {
							b.nextFire = now.Add(baseFire)
							if t := nearestEnemyShip(b.v, b.h, baseRange, b.team); t != nil {
								hitShip(t, nil, whoBase(b), 400/dist(b.v, b.h, t.v, t.h), 'P')
							}
						}
						if now.After(b.nextRegen) {
							b.nextRegen = now.Add(5 * time.Second)
							b.shield = math.Min(shieldM, b.shield+50)
							b.damage = math.Max(0, b.damage-20)
							if b.damage == 0 {
								b.distress = false
							}
						}
					}
					// planets
					for _, p := range append([]*Planet(nil), w.planets...) {
						if now.After(p.nextFire) {
							p.nextFire = now.Add(planetFire)
							if t := nearestEnemyShip(p.v, p.h, planetRange, p.owner); t != nil {
								hitShip(t, nil, whoPlanet(p), float64(100+60*p.builds)/dist(p.v, p.h, t.v, t.h), 'P')
							}
						}
					}
					romTick(now)
					// ships
					observe := now.After(nextObserve)
					if observe {
						nextObserve = now.Add(time.Second)
					}
					for _, sh := range w.ships {
						if !sh.active {
							continue
						}
						if now.After(sh.nextAuto) {
							autoRepair(sh)
						}
						if sh.dev[DLife] >= devInop {
							sh.lifeAcc += dt * cfg.StardatePerSec
							if sh.lifeAcc >= 1 {
								sh.lifeAcc--
								sh.life--
								if sh.life <= 0 {
									note(sh, "Life support failure: the crew is dead.\n")
									destroyShip(sh, nil, false)
									continue
								}
								note(sh, fmt.Sprintf("Life support critical: %d stardates of reserve.\n", sh.life))
							}
						}
						if !sh.orphanUntil.IsZero() && now.After(sh.orphanUntil) {
							releaseShip(sh)
							continue
						}
						if sh.tractor != nil && cheb(sh.v, sh.h, sh.tractor.v, sh.tractor.h) > 1 {
							breakTractor(sh)
						}
						if observe {
							markKnown(sh.team, sh.v, sh.h, scanRange)
						}
					}
				}

				func markKnown(team, v, h, r int) {
					for _, b := range w.bases {
						if b.alive && cheb(v, h, b.v, b.h) <= r {
							b.known[team] = true
						}
					}
					for _, p := range w.planets {
						if cheb(v, h, p.v, p.h) <= r {
							p.known[team] = true
						}
					}
				}

				func nearestEnemyShip(v, h, r, owner int) *Ship {
					var best *Ship
					bd := 1e9
					for _, t := range w.ships {
						if !t.active || t.team == owner || cheb(v, h, t.v, t.h) > r {
							continue
						}
						if d := dist(v, h, t.v, t.h); d < bd {
							best, bd = t, d
						}
					}
					return best
				}

				func romTick(now time.Time) {
					if w.rom == nil {
						if w.romulan && nPlayers() <= cfg.RomulanMaxPlayers && !now.Before(w.romSpawnAt) {
							v, h := w.randEmpty(1, gridN)
							w.rom = &Romulan{v: v, h: h, nextMove: now.Add(3 * time.Second), nextAct: now.Add(5 * time.Second)}
							w.grid[v][h] = Cell{k: KRom}
						}
						return
					}
					rm := w.rom
					if now.After(rm.nextMove) {
						rm.nextMove = now.Add(3 * time.Second)
						dv, dh := rand.Intn(3)-1, rand.Intn(3)-1
						for i := 0; i < 1+rand.Intn(3) && (dv != 0 || dh != 0); i++ {
							nv, nh := rm.v+dv, rm.h+dh
							if !inGrid(nv, nh) || w.grid[nv][nh].k != KEmpty {
								break
							}
							w.grid[rm.v][rm.h] = Cell{}
							rm.v, rm.h = nv, nh
							w.grid[nv][nh] = Cell{k: KRom}
						}
					}
					if !now.After(rm.nextAct) {
						return
					}
					var ts *Ship
					var tb *Base
					bd := 1e9
					for _, t := range w.ships {
						if t.active && cheb(rm.v, rm.h, t.v, t.h) <= cfg.RomulanSight {
							if d := dist(rm.v, rm.h, t.v, t.h); d < bd {
								ts, tb, bd = t, nil, d
							}
						}
					}
					for _, b := range w.bases {
						if b.alive && cheb(rm.v, rm.h, b.v, b.h) <= cfg.RomulanSight {
							if d := dist(rm.v, rm.h, b.v, b.h); d < bd {
								ts, tb, bd = nil, b, d
							}
						}
					}
					if ts == nil && tb == nil {
						rm.nextAct = now.Add(time.Second)
						return
					}
					rm.nextAct = now.Add(4 * time.Second)
					rm.revealUntil = now.Add(5 * time.Second)
					rm.volley++
					tv, th := 0, 0
					if ts != nil {
						tv, th = ts.v, ts.h
					} else {
						tv, th = tb.v, tb.h
					}
					if rm.volley%2 == 0 {
						launchTorpedo(nil, rm.v, rm.h, tv, th, false)
					} else if ts != nil {
						hitShip(ts, nil, whoRom(), 300/bd, 'P')
					} else {
						baseHit(tb, nil, whoRom(), 300/bd, 'P')
					}
				}

				// ---------------------------------------------------------------- command usage & table

				var usage = map[string]string{
					"ACTIVATE": "ACTIVATE", "BASES": "BAses [keywords]", "BUILD": "BUild [A|R] vpos hpos",
					"CAPTURE": "CApture [A|R] vpos hpos", "DAMAGES": "DAmages [device ...]",
					"DOCK": "DOck [STatus [attributes]]", "ENERGY": "Energy ship units", "GRIPE": "Gripe",
					"HELP": "Help [* | topic ...]", "IMPULSE": "Impulse [A|R] vpos hpos",
					"LIST": "LIst [keywords]", "MOVE": "Move [A|R|C] vpos hpos | C ship", "NEWS": "NEws",
					"PHASERS": "PHasers [A|R|C] [energy 50-500] vpos hpos | C [energy] ship",
					"PLANETS": "PLanets [keywords]", "POINTS": "POints [Me|Federation|Empire|Romulan|All]",
					"QUIT": "Quit", "RADIO": "RAdio ON | OFf | Gag ship | Ungag ship", "REPAIR": "REpair [units]",
					"SCAN":    "SCan [Up|Down|Right|Left|Corner] [range | vr hr] [W]",
					"SRSCAN":  "SRscan [Up|Down|Right|Left|Corner] [range | vr hr] [W]",
					"SET":     "SEt NAme|Output|Scan|Prompt|OCdef|Icdef|Ttytype value",
					"SHIELDS": "SHields Up | Down | Transfer energy", "STATUS": "STatus [attributes]",
					"SUMMARY": "SUmmary [keywords]", "TARGETS": "TArgets [keywords]",
					"TELL": "TEll recipients ; message", "TIME": "TIme",
					"TORPEDOES": "TOrpedoes [A|R|C] n vpos hpos [vpos hpos [vpos hpos]] | C n ship",
					"TRACTOR":   "TRactor ship | Off", "TYPE": "TYpe OPtion | OUtput", "USERS": "Users",
				}

				func init() {
					handlers = map[string]handler{
						"ACTIVATE": func(s *Session, a []string) error { s.startSetup(); return nil },
						"BASES":    func(s *Session, a []string) error { return listEngine(s, "BASES", a) },
						"BUILD":    cmdBuild, "CAPTURE": cmdCapture, "DAMAGES": cmdDamages, "DOCK": cmdDock,
						"ENERGY": cmdEnergy, "GRIPE": cmdGripe, "HELP": cmdHelp, "IMPULSE": cmdImpulse,
						"LIST":    func(s *Session, a []string) error { return listEngine(s, "LIST", a) },
						"MOVE":    cmdMove,
						"NEWS":    cmdNews,
						"PHASERS": cmdPhasers,
						"PLANETS": func(s *Session, a []string) error { return listEngine(s, "PLANETS", a) },
						"POINTS":  cmdPoints, "QUIT": cmdQuit, "RADIO": cmdRadio, "REPAIR": cmdRepair,
						"SCAN":   func(s *Session, a []string) error { return cmdScan(s, a, false) },
						"SRSCAN": func(s *Session, a []string) error { return cmdScan(s, a, true) },
						"SET":    cmdSet, "SHIELDS": cmdShields, "STATUS": cmdStatus,
						"SUMMARY": func(s *Session, a []string) error { return listEngine(s, "SUMMARY", a) },
						"TARGETS": func(s *Session, a []string) error { return listEngine(s, "TARGETS", a) },
						"TELL":    cmdTell, "TIME": cmdTime, "TORPEDOES": cmdTorpedoes, "TRACTOR": cmdTractor,
						"TYPE": cmdType, "USERS": cmdUsers,
					}
				}

				// ---------------------------------------------------------------- ship / coordinate argument helpers

				var shipKeyList = func() []string {
					var r []string
					for _, n := range shipNames {
						r = append(r, up(n))
					}
					return append(r, "ROMULAN")
				}()

				// shipRef resolves a ship name; returns (ship, isRomulan, status)
				func shipRef(tok string) (*Ship, bool, int) {
					m, st := resolve(tok, shipKeyList)
					if st != 0 {
						return nil, false, st
					}
					if m == "ROMULAN" {
						return nil, true, 0
					}
					for _, sh := range w.ships {
						if up(sh.name) == m {
							return sh, false, 0
						}
					}
					return nil, false, 2
				}

				func lookupShip(tok string) (*Ship, error) {
					sh, rom, st := shipRef(tok)
					if st != 0 || rom {
						return nil, bad("No such ship")
					}
					if !sh.active {
						return nil, bad(sh.name + " is not in the game")
					}
					return sh, nil
				}

				func romVisible(v *Ship) bool {
					r := w.rom
					if r == nil {
						return false
					}
					return time.Now().Before(r.revealUntil) || (v != nil && cheb(v.v, v.h, r.v, r.h) <= 1)
				}

				func (s *Session) coordMode(a []string) (int, []string) {
					if len(a) > 0 {
						switch kw(a[0], "ABSOLUTE", "RELATIVE", "COMPUTED") {
							case "ABSOLUTE":
								return 0, a[1:]
							case "RELATIVE":
								return 1, a[1:]
							case "COMPUTED":
								return 2, a[1:]
						}
					}
					return s.set.icdef, a
				}

				func (s *Session) computedOK() error {
					if s.ship.dev[DComputer] >= devInop {
						return bad("Computer inoperative.")
					}
					if !cfg.ComputedForAll && s.speed >= cfg.SlowBps {
						return bad("Computed coordinates are available only on slow terminals.")
					}
					return nil
				}

				// coords parses exactly n coordinates of the given mode.
				func (s *Session) coords(cmd string, mode int, a []string, n int) ([][2]int, error) {
					sh := s.ship
					var out [][2]int
					if mode == 2 {
						if err := s.computedOK(); err != nil {
							return nil, err
						}
						if len(a) != n {
							return nil, syn(cmd)
						}
						for _, t := range a {
							x, rom, st := shipRef(t)
							switch {
								case st != 0:
									return nil, bad("No such ship")
								case rom:
									if !romVisible(sh) {
										return nil, bad("Romulan cannot be located.")
									}
									out = append(out, [2]int{w.rom.v, w.rom.h})
								case !x.active:
									return nil, bad(x.name + " is not in the game")
								default:
									out = append(out, [2]int{x.v, x.h})
							}
						}
						return out, nil
					}
					if len(a) != 2*n {
						return nil, syn(cmd)
					}
					for i := 0; i < n; i++ {
						dv, ok1 := num(a[2*i])
						dh, ok2 := num(a[2*i+1])
						if !ok1 || !ok2 {
							return nil, syn(cmd)
						}
						if mode == 1 {
							dv, dh = sh.v+dv, sh.h+dh
						}
						if !inGrid(dv, dh) {
							return nil, bad("Invalid coordinates")
						}
						out = append(out, [2]int{dv, dh})
					}
					return out, nil
				}

				func round1(x float64) float64 { return math.Round(x*10) / 10 }

				// ---------------------------------------------------------------- information family

				type iobj struct {
					kind int
					ship *Ship
					base *Base
					pl   *Planet
					v, h int
					side int // FED EMP NEUT or 3 = Romulan
					d    int
				}

				type grp struct {
					sides                                       [4]bool
					kinds                                       [3]bool // ships bases planets
					rng                                         int
					closest, summary, captured                  bool
					sideGiven, rngGiven, kindGiven, portsNoSide bool
					relCoord                                    bool
					coords                                      [][2]int
					ships                                       []string
				}

				var listKeys = []string{"CLOSEST", "SHIPS", "BASES", "PLANETS", "PORTS", "FEDERATION", "HUMAN", "EMPIRE",
					"KLINGON", "FRIENDLY", "ENEMY", "TARGETS", "NEUTRAL", "CAPTURED", "ALL", "LIST", "SUMMARY", "AND",
					"ABSOLUTE", "RELATIVE"}

					func (s *Session) friendSide() int { return s.team() }

					func (s *Session) defGrp(cmd string) *grp {
						vt := s.team()
						g := &grp{rng: 1 << 30, kinds: [3]bool{true, true, true}, sides: [4]bool{true, true, true, true}}
						g.relCoord = s.set.icdef == 1
						switch cmd {
							case "SUMMARY":
								g.summary = true
							case "BASES":
								g.kinds = [3]bool{false, true, false}
								g.sides = [4]bool{}
								if vt >= 0 {
									g.sides[vt] = true
								} else {
									g.sides[FED], g.sides[EMP] = true, true
								}
							case "PLANETS":
								g.kinds = [3]bool{false, false, true}
								g.rng = 10
							case "TARGETS":
								g.rng = 10
								g.sides = [4]bool{}
								if vt >= 0 {
									g.sides[1-vt], g.sides[3] = true, true
								}
						}
						return g
					}

					func (s *Session) setSide(g *grp, idx ...int) {
						if !g.sideGiven {
							g.sides = [4]bool{}
							g.sideGiven = true
						}
						for _, i := range idx {
							if i >= 0 && i < 4 {
								g.sides[i] = true
							}
						}
					}

					func (s *Session) parseGroups(cmd string, a []string) ([]*grp, error) {
						var parts [][]string
						cur := []string{}
						for _, t := range a {
							if t == "&" || (len(t) >= 2 && kw(t, "AND") == "AND") {
								parts = append(parts, cur)
								cur = nil
								continue
							}
							cur = append(cur, t)
						}
						parts = append(parts, cur)
						vt := s.team()
						var gs []*grp
						for _, toks := range parts {
							g := s.defGrp(cmd)
							for i := 0; i < len(toks); i++ {
								t := toks[i]
								if n1, ok := num(t); ok {
									if i+1 < len(toks) {
										if n2, ok2 := num(toks[i+1]); ok2 {
											v, h := n1, n2
											if g.relCoord && s.ship != nil {
												v, h = s.ship.v+n1, s.ship.h+n2
											}
											if !inGrid(v, h) {
												return nil, bad("Invalid coordinates")
											}
											g.coords = append(g.coords, [2]int{v, h})
											i++
											continue
										}
									}
									if n1 < 0 {
										return nil, bad("Invalid range")
									}
									g.rng, g.rngGiven = n1, true
									continue
								}
								key := ""
								if len(t) >= 2 {
									key = kw(t, listKeys...)
								}
								if key == "" {
									if _, rom, st := shipRef(t); st == 0 {
										_ = rom
										g.ships = append(g.ships, t)
										continue
									}
									if key = kw(t, listKeys...); key == "" {
										return nil, bad("Unknown keyword: " + t)
									}
								}
								switch key {
									case "CLOSEST":
										g.closest = true
									case "SHIPS", "BASES", "PLANETS", "PORTS":
										if !g.kindGiven {
											g.kinds = [3]bool{}
											g.kindGiven = true
										}
										switch key {
											case "SHIPS":
												g.kinds[0] = true
											case "BASES":
												g.kinds[1] = true
											case "PLANETS":
												g.kinds[2] = true
											case "PORTS":
												g.kinds[1], g.kinds[2] = true, true
												g.portsNoSide = true
										}
											case "FEDERATION", "HUMAN":
												s.setSide(g, FED)
											case "EMPIRE", "KLINGON":
												s.setSide(g, EMP)
											case "FRIENDLY":
												s.setSide(g, vt)
											case "ENEMY", "TARGETS":
												if vt >= 0 {
													s.setSide(g, 1-vt, 3)
												} else {
													s.setSide(g)
												}
											case "NEUTRAL":
												s.setSide(g, NEUT)
											case "CAPTURED":
												s.setSide(g, FED, EMP)
												g.captured = true
												if !g.kindGiven {
													g.kinds, g.kindGiven = [3]bool{false, false, true}, true
												}
											case "ALL":
												if !g.sideGiven {
													g.sides = [4]bool{true, true, true, true}
												}
												if !g.rngGiven {
													g.rng = 1 << 30
												}
											case "LIST":
												g.summary = false
											case "SUMMARY":
												g.summary = true
											case "ABSOLUTE":
												g.relCoord = false
											case "RELATIVE":
												g.relCoord = true
								}
							}
							if g.portsNoSide && !g.sideGiven {
								g.sides = [4]bool{}
								if vt >= 0 {
									g.sides[vt] = true
								} else {
									g.sides[FED], g.sides[EMP] = true, true
								}
							}
							gs = append(gs, g)
						}
						return gs, nil
					}

					func (s *Session) allObjs() []iobj {
						var r []iobj
						v0, h0 := 0, 0
						if s.ship != nil {
							v0, h0 = s.ship.v, s.ship.h
						}
						for _, sh := range w.ships {
							if sh.active && sh != s.ship {
								r = append(r, iobj{kind: KShip, ship: sh, v: sh.v, h: sh.h, side: sh.team, d: cheb(v0, h0, sh.v, sh.h)})
							}
						}
						if w.rom != nil {
							r = append(r, iobj{kind: KRom, v: w.rom.v, h: w.rom.h, side: 3, d: cheb(v0, h0, w.rom.v, w.rom.h)})
						}
						for _, b := range w.bases {
							if b.alive {
								r = append(r, iobj{kind: KBase, base: b, v: b.v, h: b.h, side: b.team, d: cheb(v0, h0, b.v, b.h)})
							}
						}
						for _, p := range w.planets {
							r = append(r, iobj{kind: KPlanet, pl: p, v: p.v, h: p.h, side: p.owner, d: cheb(v0, h0, p.v, p.h)})
						}
						return r
					}

					func (s *Session) knownTo(o iobj) bool {
						vt := s.team()
						if vt < 0 || o.side == vt {
							return true
						}
						if o.d <= scanRange {
							return true
						}
						switch o.kind {
							case KBase:
								return o.base.known[vt]
							case KPlanet:
								return o.pl.known[vt]
						}
						return true
					}

					func (s *Session) evalGroup(g *grp, msgs *[]string) []iobj {
						all := s.allObjs()
						vt := s.team()
						var res []iobj
						if len(g.coords) > 0 || len(g.ships) > 0 {
							for _, c := range g.coords {
								found := false
								for _, o := range all {
									if o.v == c[0] && o.h == c[1] {
										res = append(res, o)
										found = true
										if vt >= 0 {
											switch o.kind {
												case KBase:
													o.base.known[vt] = true
												case KPlanet:
													o.pl.known[vt] = true
											}
										}
									}
								}
								if !found {
									*msgs = append(*msgs, fmt.Sprintf("Nothing at %d-%d.", c[0], c[1]))
								}
							}
							for _, t := range g.ships {
								sh, rom, _ := shipRef(t)
								found := false
								for _, o := range all {
									if (rom && o.kind == KRom) || (!rom && o.ship == sh && sh != nil) {
										res = append(res, o)
										found = true
									}
								}
								if !found {
									if rom {
										*msgs = append(*msgs, "Romulan is not in the game.")
									} else if sh == s.ship {
										*msgs = append(*msgs, "That is your own ship.")
									} else {
										*msgs = append(*msgs, sh.name+" is not in the game.")
									}
								}
							}
							return res
						}
						for _, o := range all {
							k := 0
							switch o.kind {
								case KBase:
									k = 1
								case KPlanet:
									k = 2
							}
							if !g.kinds[k] || !g.sides[o.side] || o.d > g.rng {
								continue
							}
							if g.captured && o.kind == KPlanet && o.side == NEUT {
								continue
							}
							if !s.knownTo(o) {
								continue
							}
							res = append(res, o)
						}
						sortObjs(res)
						if g.closest && len(res) > 0 {
							best := res[0]
							for _, o := range res[1:] {
								if o.d < best.d || (o.d == best.d && (o.v < best.v || (o.v == best.v && o.h < best.h))) {
									best = o
								}
							}
							res = []iobj{best}
						}
						return res
					}

					func sortObjs(r []iobj) {
						sort.SliceStable(r, func(i, j int) bool {
							a, b := r[i], r[j]
							if a.kind != b.kind {
								return a.kind < b.kind
							}
							if a.d != b.d {
								return a.d < b.d
							}
							if a.v != b.v {
								return a.v < b.v
							}
							return a.h < b.h
						})
					}

					func pct0(x Who) string {
						sg := '+'
						if !x.up {
							sg = '-'
						}
						return fmt.Sprintf("%c%d%%", sg, int(x.pct))
					}

					func (s *Session) objLine(o iobj, noflag bool) string {
						vt := s.team()
						enemy := vt >= 0 && o.side != vt && o.side != NEUT
						flag := ' '
						if enemy && !noflag {
							flag = '*'
						}
						switch o.kind {
							case KShip:
								sh := o.ship
								if enemy && o.d > scanRange {
									return fmt.Sprintf("%c %-10s", flag, sh.name)
								}
								return fmt.Sprintf("%c %-10s %s  %s", flag, sh.name, s.loc(o.v, o.h), pct0(whoShip(sh)))
							case KRom:
								if !romVisible(s.ship) {
									return fmt.Sprintf("%c %-10s", flag, "Romulan")
								}
								return fmt.Sprintf("%c %-10s %s", flag, "Romulan", s.loc(o.v, o.h))
							case KBase:
								sym := "<>"
								if o.base.team == EMP {
									sym = ")("
								}
								l := fmt.Sprintf("%c %s %s", flag, sym, s.loc(o.v, o.h))
								if !enemy || o.d <= scanRange {
									l += "  " + fmt.Sprintf("%3.0f%%", o.base.shield/shieldM*100)
								}
								return l
						}
						sym := " @"
						switch o.pl.owner {
							case FED:
								sym = "@F"
							case EMP:
								sym = "@E"
						}
						return fmt.Sprintf("%c %s %s  builds %d", flag, sym, s.loc(o.v, o.h), o.pl.builds)
					}

					func (s *Session) catOf(o iobj) int {
						vt := s.team()
						switch {
							case o.side == 3:
								return 3
							case o.side == NEUT:
								return 2
							case vt < 0:
								return o.side
							case o.side == vt:
								return 0
						}
						return 1
					}

					func (s *Session) catName(c int) string {
						if s.team() < 0 && c < 2 {
							return teamNames[c]
						}
						return [...]string{"friendly", "enemy", "neutral", "Romulan"}[c]
					}

					func (s *Session) summarize(g *grp, objs []iobj) {
						var cnt [3][4]int
						for _, o := range objs {
							k := 0
							switch o.kind {
								case KBase:
									k = 1
								case KPlanet:
									k = 2
							}
							cnt[k][s.catOf(o)]++
						}
						vt := s.team()
						for k, label := range [...]string{"Ships", "Bases", "Planets"} {
							if !g.kinds[k] {
								continue
							}
							var parts []string
							for c := 0; c < 4; c++ {
								if (k != 0 && c == 3) || (k == 2 && false) {
									continue
								}
								side := c
								switch c {
									case 0:
										side = vt
										if vt < 0 {
											side = FED
										}
									case 1:
										side = 1 - vt
										if vt < 0 {
											side = EMP
										}
								}
								if !g.sides[side] {
									continue
								}
								if c == 2 && k != 2 {
									continue
								}
								parts = append(parts, fmt.Sprintf("%s %d", s.catName(c), cnt[k][c]))
							}
							if len(parts) > 0 {
								s.printf("%-8s %s\n", label+":", strings.Join(parts, ", "))
							}
						}
					}

					func listEngine(s *Session, cmd string, a []string) error {
						gs, err := s.parseGroups(cmd, a)
						if err != nil {
							return err
						}
						seen := map[string]bool{}
						for _, g := range gs {
							var msgs []string
							objs := s.evalGroup(g, &msgs)
							for _, m := range msgs {
								s.out(m + "\n")
							}
							if g.summary {
								s.summarize(g, objs)
								continue
							}
							if cmd == "BASES" {
								s.out("Friendly bases:\n")
							}
							n := 0
							for _, o := range objs {
								key := fmt.Sprintf("%d/%d/%d", o.kind, o.v, o.h)
								if seen[key] {
									continue
								}
								seen[key] = true
								s.out(s.objLine(o, cmd == "TARGETS") + "\n")
								n++
							}
							if n == 0 && len(msgs) == 0 {
								s.out("None found.\n")
							}
						}
						return nil
					}

					// ---------------------------------------------------------------- points

					type pent struct {
						name string
						sc   Score
						n    int
					}

					func pointsText(s *Session, a []string) string {
						var ents []pent
						teamEnt := func(t int) pent {
							e := pent{name: teamNames[t]}
							for _, r := range w.recs {
								if r.team == t {
									e.sc.dmgEnemy += r.sc.dmgEnemy
									e.sc.dmgBase += r.sc.dmgBase
									e.sc.kills += r.sc.kills
									e.sc.planets += r.sc.planets
									e.sc.built += r.sc.built
									e.sc.romKills += r.sc.romKills
									e.sc.stars += r.sc.stars
									e.sc.plDestroy += r.sc.plDestroy
									e.sc.commissioned += r.sc.commissioned
									e.n++
								}
							}
							return e
						}
						romEnt := pent{name: "Romulan", sc: w.romScore, n: 1}
						want := map[string]bool{}
						for _, t := range a {
							if k := kw(t, "ME", "I", "FEDERATION", "HUMAN", "EMPIRE", "KLINGON", "ROMULAN", "ALL"); k != "" {
								want[k] = true
							}
						}
						if len(a) == 0 {
							want["ME"] = true
						}
						if want["ME"] || want["I"] {
							e := pent{name: "Me", n: 1}
							if s.key != "" {
								if r := w.recs[s.key]; r != nil {
									e.sc = r.sc
								}
							}
							ents = append(ents, e)
						}
						if want["ALL"] || want["FEDERATION"] || want["HUMAN"] {
							ents = append(ents, teamEnt(FED))
						}
						if want["ALL"] || want["EMPIRE"] || want["KLINGON"] {
							ents = append(ents, teamEnt(EMP))
						}
						if (want["ALL"] || want["ROMULAN"]) && (w.rom != nil || w.romScore.total() != 0) {
							ents = append(ents, romEnt)
						}
						if len(ents) == 0 {
							return "No such score table.\n"
						}
						var b strings.Builder
						b.WriteString(fmt.Sprintf("%-28s", ""))
						for _, e := range ents {
							b.WriteString(fmt.Sprintf("%12s", e.name))
						}
						b.WriteString("\n")
						row := func(label string, f func(e pent) string) {
							b.WriteString(fmt.Sprintf("%-28s", label))
							for _, e := range ents {
								b.WriteString(fmt.Sprintf("%12s", f(e)))
							}
							b.WriteString("\n")
						}
						row("Damage to enemies", func(e pent) string { return strconv.Itoa(int(e.sc.dmgEnemy / 10)) })
						row("Enemies destroyed", func(e pent) string { return strconv.Itoa(500 * e.sc.kills) })
						row("Damage to bases", func(e pent) string { return strconv.Itoa(int(e.sc.dmgBase / 10)) })
						row("Planets captured", func(e pent) string { return strconv.Itoa(100 * e.sc.planets) })
						row("Bases built", func(e pent) string { return strconv.Itoa(1000 * e.sc.built) })
						row("Romulans destroyed", func(e pent) string { return strconv.Itoa(1000 * e.sc.romKills) })
						row("Stars destroyed", func(e pent) string { return strconv.Itoa(-50 * e.sc.stars) })
						row("Planets destroyed", func(e pent) string { return strconv.Itoa(-100 * e.sc.plDestroy) })
						row("Total points", func(e pent) string { return strconv.Itoa(e.sc.total()) })
						row("Ships commissioned", func(e pent) string { return strconv.Itoa(e.sc.commissioned) })
						row("Total / number of players", func(e pent) string {
							return fmt.Sprintf("%.1f", float64(e.sc.total())/math.Max(1, float64(e.n)))
						})
						elapsed := math.Max(0.1, w.stardate()-w.sd0)
						row("Total / stardates", func(e pent) string { return fmt.Sprintf("%.1f", float64(e.sc.total())/elapsed) })
						return b.String()
					}

					func cmdPoints(s *Session, a []string) error {
						s.out(pointsText(s, a))
						return nil
					}

					// ---------------------------------------------------------------- planets: BUILD / CAPTURE

					func (s *Session) planetArg(cmd string, a []string) (*Planet, error) {
						mode, rest := s.coordMode(a)
						cs, err := s.coords(cmd, mode, rest, 1)
						if err != nil {
							return nil, err
						}
						sh := s.ship
						c := w.grid[cs[0][0]][cs[0][1]]
						if c.k != KPlanet {
							return nil, bad("Not a planet.")
						}
						if cheb(sh.v, sh.h, c.pl.v, c.pl.h) > 1 {
							return nil, bad("Planet is not adjacent.")
						}
						return c.pl, nil
					}

					func aliveBases() int {
						n := 0
						for _, b := range w.bases {
							if b.alive {
								n++
							}
						}
						return n
					}

					func cmdBuild(s *Session, a []string) error {
						sh := s.ship
						p, err := s.planetArg("BUILD", a)
						if err != nil {
							return err
						}
						if p.owner != sh.team {
							return bad("That planet is not yours.")
						}
						if p.builds >= 4 && aliveBases() >= cfg.BasesMax {
							return bad("Starbase limit reached.")
						}
						pause := pauseFor(5, 7)
						if slowFactor() == 0 {
							pause = 5 + rand.Float64()*2
						}
						s.timed(pause)
						s.out("Construction under way...\n")
						tok := sh.moveTok
						after(sec(pause), func() {
							if !sh.active || sh.moveTok != tok || w.grid[p.v][p.h].pl != p || p.owner != sh.team {
								return
							}
							if p.builds < 4 {
								p.builds++
								p.integrity = 300
								note(sh, fmt.Sprintf("Planet at %d-%d now has %d builds.\n", p.v, p.h, p.builds))
								return
							}
							if aliveBases() >= cfg.BasesMax {
								note(sh, "Starbase limit reached.\n")
								return
							}
							removePlanet(p)
							b := &Base{v: p.v, h: p.h, team: sh.team, shield: shieldM, alive: true}
							b.known[sh.team] = true
							w.bases = append(w.bases, b)
							w.grid[p.v][p.h] = Cell{k: KBase, base: b}
							sh.rec.sc.built++
							note(sh, fmt.Sprintf("Starbase completed at %d-%d! (+1000 points)\n", p.v, p.h))
							radioTeam(sh.team, fmt.Sprintf("New starbase at %d-%d.\n", p.v, p.h), sh)
						})
						return nil
					}

					func cmdCapture(s *Session, a []string) error {
						sh := s.ship
						p, err := s.planetArg("CAPTURE", a)
						if err != nil {
							return err
						}
						if p.owner == sh.team {
							return bad("You already own that planet.")
						}
						cost, pause := 0.0, 5.0
						if p.owner != NEUT {
							cost, pause = float64(50*p.builds), 5+float64(p.builds)
						}
						if sh.energy-cost < 1 {
							return bad("Insufficient energy to capture.")
						}
						sh.energy -= cost
						s.timed(pause)
						s.out("Capture under way...\n")
						tok := sh.moveTok
						after(sec(pause), func() {
							if !sh.active || sh.moveTok != tok || w.grid[p.v][p.h].pl != p || p.owner == sh.team {
								return
							}
							p.owner, p.builds, p.integrity = sh.team, 0, 300
							p.known[sh.team] = true
							sh.rec.sc.planets++
							note(sh, fmt.Sprintf("Planet at %d-%d captured! (+100 points)\n", p.v, p.h))
						})
						return nil
					}

					// ---------------------------------------------------------------- DAMAGES / STATUS

					func devIndex(tok string) int {
						if up(tok) == "T" {
							return DTorp
						}
						m, st := resolve(tok, devKeys)
						if st != 0 {
							return -1
						}
						for i, k := range devKeys {
							if k == m {
								return i
							}
						}
						return -1
					}

					func cmdDamages(s *Session, a []string) error {
						sh := s.ship
						var idx []int
						if len(a) == 0 {
							for i, d := range sh.dev {
								if d > 0 {
									idx = append(idx, i)
								}
							}
						}
						for _, t := range a {
							i := devIndex(t)
							if i < 0 {
								return syn("DAMAGES")
							}
							idx = append(idx, i)
						}
						if len(idx) == 0 {
							s.out("No device damage.\n")
						}
						for _, i := range idx {
							x := ""
							if sh.dev[i] >= devInop {
								x = "  (inoperative)"
							}
							s.printf("%-18s %7.1f%s\n", devNames[i], sh.dev[i], x)
						}
						return nil
					}

					func cmdStatus(s *Session, a []string) error {
						sh := s.ship
						want := map[string]bool{}
						for _, t := range a {
							k := kw(t, "CONDITION", "LOCATION", "TORPEDOES", "ENERGY", "DAMAGE", "SHIELDS", "RADIO")
							if k == "" {
								return syn("STATUS")
							}
							want[k] = true
						}
						all := len(a) == 0
						s.printf("Stardate   %.1f\n", w.stardate())
						if all || want["CONDITION"] {
							c := "GREEN"
							if sh.red {
								c = "RED"
							} else if sh.energy <= yellowE {
								c = "YELLOW"
							}
							if sh.docked {
								c += " (docked)"
							}
							s.printf("Condition  %s\n", c)
						}
						if all || want["LOCATION"] {
							s.printf("Location   %d-%d\n", sh.v, sh.h)
						}
						if all || want["TORPEDOES"] {
							s.printf("Torpedoes  %d\n", sh.torps)
						}
						if all || want["ENERGY"] {
							s.printf("Energy     %.1f\n", sh.energy)
						}
						if all || want["DAMAGE"] {
							s.printf("Damage     %.1f\n", sh.damage)
						}
						if all || want["SHIELDS"] {
							ud := "down"
							if sh.shieldsUp {
								ud = "up"
							}
							s.printf("Shields    %s %s (energy %.1f)\n", ud, pctStr(whoShip(sh), false), sh.shield)
						}
						if all || want["RADIO"] {
							o := "off"
							if sh.radioOn {
								o = "on"
							}
							s.printf("Radio      %s\n", o)
						}
						return nil
					}

					// ---------------------------------------------------------------- DOCK / ENERGY / SHIELDS / REPAIR

					func cmdDock(s *Session, a []string) error {
						sh := s.ship
						var base *Base
						var pl *Planet
						for dv := -1; dv <= 1; dv++ {
							for dh := -1; dh <= 1; dh++ {
								v, h := sh.v+dv, sh.h+dh
								if (dv == 0 && dh == 0) || !inGrid(v, h) {
									continue
								}
								c := w.grid[v][h]
								if c.k == KBase && c.base.team == sh.team && c.base.alive {
									base = c.base
								}
								if c.k == KPlanet && c.pl.owner == sh.team {
									pl = c.pl
								}
							}
						}
						if base == nil && pl == nil {
							return bad("Not adjacent to a friendly base or planet.")
						}
						f := 1.0
						if base == nil {
							f = 0.5
						}
						if sh.energy >= shipEnergyM && sh.shield >= shieldM && sh.torps >= torpsMax && sh.life >= lifeMax && sh.damage <= 0 {
							return bad("Nothing needs restoring; dock has no effect.")
						}
						red := 100 * f
						if sh.docked {
							red = 200 * f
						}
						sh.energy = math.Min(shipEnergyM, sh.energy+1000*f)
						sh.shield = math.Min(shieldM, sh.shield+500*f)
						sh.torps = torpsMax
						sh.life = min(lifeMax, sh.life+5)
						sh.damage = math.Max(0, sh.damage-red)
						sh.red, sh.docked = false, true
						breakTractor(sh)
						s.out("Docked. Supplies replenished.\n")
						s.timed(pauseFor(2, 4))
						if len(a) > 0 && kw(a[0], "STATUS") == "STATUS" {
							return cmdStatus(s, a[1:])
						}
						return nil
					}

					func cmdEnergy(s *Session, a []string) error {
						sh := s.ship
						if len(a) != 2 {
							return syn("ENERGY")
						}
						o, err := lookupShip(a[0])
						if err != nil {
							return err
						}
						units, ok := num(a[1])
						if !ok || units <= 0 {
							return syn("ENERGY")
						}
						if o == sh || o.team != sh.team {
							return bad("Energy can only be sent to another friendly ship.")
						}
						if cheb(sh.v, sh.h, o.v, o.h) > 1 {
							return bad(o.name + " is not adjacent.")
						}
						if sh.energy-float64(units) < 1 {
							return bad("Insufficient energy.")
						}
						gain := 0.9 * float64(units)
						if o.energy+gain > shipEnergyM {
							gain = shipEnergyM - o.energy
						}
						if gain <= 0 {
							return bad(o.name + " is already fully charged.")
						}
						sent := gain / 0.9
						sh.energy -= sent
						o.energy += gain
						s.printf("Transferred %.1f units to %s (%.1f received).\n", sent, o.name, gain)
						note(o, fmt.Sprintf("%s transfers %.1f units of energy to you.\n", sh.name, gain))
						return nil
					}

					func cmdShields(s *Session, a []string) error {
						sh := s.ship
						if len(a) == 0 {
							return syn("SHIELDS")
						}
						switch kw(a[0], "UP", "DOWN", "TRANSFER") {
							case "UP":
								switch {
									case sh.dev[DShield] >= devInop:
										return bad("Deflector shields inoperative.")
									case sh.shieldsUp:
										return bad("Shields are already up.")
									case sh.energy <= 100:
										return bad("Insufficient energy to raise shields.")
								}
								sh.energy -= 100
								sh.shieldsUp = true
								breakTractor(sh)
									case "DOWN":
										sh.shieldsUp = false
									case "TRANSFER":
										if len(a) != 2 {
											return syn("SHIELDS")
										}
										n, ok := num(a[1])
										if !ok {
											return syn("SHIELDS")
										}
										amt := float64(n)
										if amt >= 0 {
											amt = math.Min(amt, math.Min(sh.energy-1, shieldM-sh.shield))
											amt = math.Max(0, amt)
											sh.energy -= amt
											sh.shield += amt
										} else {
											amt = math.Min(-amt, math.Min(sh.shield, shipEnergyM-sh.energy))
											amt = math.Max(0, amt)
											sh.energy += amt
											sh.shield -= amt
										}
									default:
										return syn("SHIELDS")
						}
						ud := "down"
						if sh.shieldsUp {
							ud = "up"
						}
						s.printf("Shields %s %s, shield energy %.1f, ship energy %.1f.\n", ud, pctStr(whoShip(sh), false), sh.shield, sh.energy)
						return nil
					}

					func cmdRepair(s *Session, a []string) error {
						sh := s.ship
						units := 50
						if sh.docked {
							units = 100
						}
						if len(a) > 1 {
							return syn("REPAIR")
						}
						if len(a) == 1 {
							n, ok := num(a[0])
							if !ok || n <= 0 || n > 1000 {
								return syn("REPAIR")
							}
							units = n
						}
						wasLife := sh.dev[DLife] >= devInop
						for i := range sh.dev {
							sh.dev[i] = math.Max(0, sh.dev[i]-float64(units))
						}
						if wasLife && sh.dev[DLife] < devInop {
							sh.life = lifeMax
						}
						pause := 0.08 * float64(units)
						if sh.docked {
							pause *= 0.5
						}
						s.printf("Repairing %d units per device...\n", units)
						s.timed(pause)
						return nil
					}

					// ---------------------------------------------------------------- movement

					func linePath(v0, h0, v1, h1 int) [][2]int {
						var p [][2]int
						dv, dh := abs(v1-v0), abs(h1-h0)
						sv, sh := sgn(v1-v0), sgn(h1-h0)
						v, h := v0, h0
						if dv >= dh {
							e := dv / 2
							for v != v1 {
								v += sv
								e -= dh
								if e < 0 {
									h += sh
									e += dv
								}
								p = append(p, [2]int{v, h})
							}
						} else {
							e := dh / 2
							for h != h1 {
								h += sh
								e -= dv
								if e < 0 {
									v += sv
									e += dh
								}
								p = append(p, [2]int{v, h})
							}
						}
						return p
					}

					func finishMove(sh *Ship, tv, th int) {
						path := linePath(sh.v, sh.h, tv, th)
						var steps [][2]int
						for _, c := range path {
							cell := w.grid[c[0]][c[1]]
							if cell.k == KHole {
								note(sh, "Your ship is pulled into a black hole!\n")
								destroyShip(sh, nil, false)
								return
							}
							if cell.k != KEmpty {
								break
							}
							steps = append(steps, c)
						}
						if len(steps) == 0 {
							note(sh, "Path blocked; ship holds position.\n")
							return
						}
						final := steps[len(steps)-1]
						errMax := 0
						if sh.dev[DComputer] >= devInop {
							errMax = 2
						} else if sh.dev[DComputer] >= 100 {
							errMax = 1
						}
						if errMax > 0 {
							for try := 0; try < 20; try++ {
								cv, ch := final[0]+rand.Intn(2*errMax+1)-errMax, final[1]+rand.Intn(2*errMax+1)-errMax
								if inGrid(cv, ch) && w.grid[cv][ch].k == KEmpty {
									final = [2]int{cv, ch}
									break
								}
							}
						}
						prev := [2]int{sh.v, sh.h}
						if len(steps) >= 2 {
							prev = steps[len(steps)-2]
						}
						moveShipTo(sh, final[0], final[1])
						if p := sh.tractor; p != nil {
							if w.grid[prev[0]][prev[1]].k == KEmpty {
								moveShipTo(p, prev[0], prev[1])
							} else {
								breakTractor(sh)
							}
						}
						sh.red, sh.docked = false, false
						note(sh, fmt.Sprintf("Arrived at %d-%d.\n", sh.v, sh.h))
						if sh.energy <= yellowE && sh.sess != nil {
							sh.sess.out("\a")
						}
					}

					func moveCost(sh *Ship, wf float64) float64 {
						c := warpK * wf * wf
						if sh.shieldsUp {
							c *= 2
						}
						if sh.tractor != nil {
							c *= 3
						}
						return c
					}

					func cmdMove(s *Session, a []string) error {
						sh := s.ship
						if sh.dev[DWarp] >= devInop {
							return bad("Warp engines inoperative. Use IMPULSE.")
						}
						mode, rest := s.coordMode(a)
						cs, err := s.coords("MOVE", mode, rest, 1)
						if err != nil {
							return err
						}
						tv, th := cs[0][0], cs[0][1]
						wf := math.Hypot(float64(tv-sh.v), float64(th-sh.h))
						if wf == 0 {
							return bad("You are already there.")
						}
						maxw := 6.0
						if sh.dev[DWarp] > 0 {
							maxw = 3
						}
						if wf > maxw+1e-9 {
							return bad(fmt.Sprintf("Warp factor %.1f exceeds maximum warp %.0f.", wf, maxw))
						}
						cost := moveCost(sh, wf)
						if sh.energy-cost < 1 {
							return bad("Insufficient energy.")
						}
						sh.energy -= cost
						if wf >= 6 && rand.Float64() < 0.15 || wf >= 5 && wf < 6 && rand.Float64() < 0.05 {
							devDamage(sh, DWarp, rf(20, 120))
						}
						pause := pauseFor(2, 4)
						s.timed(pause)
						s.printf("Engaging warp %.1f...\n", wf)
						tok := sh.moveTok
						after(sec(pause), func() {
							if sh.active && sh.moveTok == tok {
								finishMove(sh, tv, th)
							}
						})
						return nil
					}

					func cmdImpulse(s *Session, a []string) error {
						sh := s.ship
						if sh.dev[DImpulse] >= devInop {
							return bad("Impulse engines inoperative.")
						}
						mode, rest := s.coordMode(a)
						cs, err := s.coords("IMPULSE", mode, rest, 1)
						if err != nil {
							return err
						}
						tv, th := cs[0][0], cs[0][1]
						if cheb(sh.v, sh.h, tv, th) != 1 {
							return bad("Impulse moves exactly one sector.")
						}
						if w.grid[tv][th].k != KEmpty {
							return bad("That sector is occupied.")
						}
						cost := moveCost(sh, 1)
						if sh.energy-cost < 1 {
							return bad("Insufficient energy.")
						}
						sh.energy -= cost
						pause := pauseFor(2, 4)
						s.timed(pause)
						tok := sh.moveTok
						after(sec(pause), func() {
							if sh.active && sh.moveTok == tok {
								finishMove(sh, tv, th)
							}
						})
						return nil
					}

					// ---------------------------------------------------------------- weapons

					func cmdPhasers(s *Session, a []string) error {
						sh := s.ship
						mode, rest := s.coordMode(a)
						n := 2
						if mode == 2 {
							n = 1
						}
						E := phaserDef
						if len(rest) == n+1 {
							e, ok := num(rest[0])
							if !ok || e < phaserMin || e > phaserMax {
								return bad("Phaser energy must be between 50 and 500.")
							}
							E, rest = e, rest[1:]
						}
						cs, err := s.coords("PHASERS", mode, rest, 1)
						if err != nil {
							return err
						}
						if sh.dev[DPhaser] >= devInop {
							return bad("Phaser banks inoperative.")
						}
						tv, th := cs[0][0], cs[0][1]
						d := cheb(sh.v, sh.h, tv, th)
						if d == 0 {
							return bad("Invalid target.")
						}
						if d > phaserRange {
							return bad("Target out of range.")
						}
						now := time.Now()
						bank := -1
						for i := 0; i < 2; i++ {
							if !now.Before(sh.phReady[i]) {
								bank = i
								break
							}
						}
						if bank < 0 {
							return bad("Phasers still cooling.")
						}
						c := w.grid[tv][th]
						switch c.k {
							case KShip:
								if c.ship.team == sh.team {
									s.printf("Weapons officer cancels phaser blast at friendly %s.\n", c.ship.name)
									return nil
								}
							case KBase:
								if c.base.team == sh.team {
									s.out("Weapons officer cancels phaser blast at friendly starbase.\n")
									return nil
								}
							case KPlanet:
								if c.pl.owner == sh.team {
									s.out("Weapons officer cancels phaser blast at friendly planet.\n")
									return nil
								}
							case KRom:
							default:
								return bad("Nothing to fire at there.")
						}
						cost := float64(E)
						if sh.shieldsUp {
							cost += 200
						}
						if sh.energy-cost < 1 {
							return bad("Insufficient energy.")
						}
						sh.energy -= cost
						sh.red = true
						sh.phReady[bank] = now.Add(sec(pauseFor(3, 6) + sh.dev[DPhaser]/100))
						hit := float64(E) * 0.9 / dist(sh.v, sh.h, tv, th)
						hit *= (1 - sh.dev[DPhaser]/devInop) * (1 - sh.dev[DComputer]/600)
						hit = round1(math.Max(0, hit))
						p := 0.05
						if E > 200 {
							p = 0.05 + 0.60*float64(E-200)/300
						}
						if rand.Float64() < p {
							devDamage(sh, DPhaser, round1(rf(0.2, 1.0)*float64(E)/2))
							if rand.Float64() < 0.10 {
								if dv := rand.Intn(9); dv != DPhaser {
									devDamage(sh, dv, round1(rf(10, 60)))
								}
							}
						}
						src := whoShip(sh)
						switch c.k {
							case KShip:
								hitShip(c.ship, sh, src, hit, 'P')
							case KRom:
								w.rom.revealUntil = now.Add(5 * time.Second)
								romHit(sh, src, hit, 'P')
							case KBase:
								baseHit(c.base, sh, src, hit, 'P')
							case KPlanet:
								planetSoftHit(c.pl, sh, src, hit, 'P')
						}
						return nil
					}

					func torpCounts(n, k int) []int {
						c := make([]int, k)
						for i := range c {
							c[i] = 1
						}
						c[k-1] += n - k
						return c
					}

					func cmdTorpedoes(s *Session, a []string) error {
						sh := s.ship
						mode, rest := s.coordMode(a)
						if len(rest) < 2 {
							return syn("TORPEDOES")
						}
						n, ok := num(rest[0])
						if !ok || n < 1 || n > 3 {
							return syn("TORPEDOES")
						}
						rest = rest[1:]
						k := len(rest)
						if mode != 2 {
							k = len(rest) / 2
						}
						if k < 1 || k > n {
							return syn("TORPEDOES")
						}
						cs, err := s.coords("TORPEDOES", mode, rest, k)
						if err != nil {
							return err
						}
						if sh.dev[DTorp] >= devInop {
							return bad("Photon tubes inoperative.")
						}
						if sh.torps < n {
							return bad("Insufficient torpedoes.")
						}
						now := time.Now()
						if now.Before(sh.torpReady) {
							return bad("Torpedo tubes still reloading.")
						}
						for _, c := range cs {
							d := cheb(sh.v, sh.h, c[0], c[1])
							if d == 0 {
								return bad("Invalid target.")
							}
							if d > 10 {
								return bad("Target out of range.")
							}
						}
						counts := torpCounts(n, k)
						sh.red = true
						fired := 0
						burst:
						for j, c := range cs {
							for t := 0; t < counts[j]; t++ {
								sh.torps--
								fired++
								if !launchTorpedo(sh, sh.v, sh.h, c[0], c[1], mode == 2) || !sh.active {
									break burst
								}
							}
						}
						if sh.active {
							sh.torpReady = now.Add(sec(float64(fired) * (pauseFor(2, 4) + sh.dev[DTorp]/100)))
						}
						return nil
					}

					func cmdTractor(s *Session, a []string) error {
						sh := s.ship
						if len(a) != 1 {
							return syn("TRACTOR")
						}
						if kw(a[0], "OFF") == "OFF" {
							if sh.tractor == nil {
								s.out("No tractor beam is engaged.\n")
							}
							breakTractor(sh)
							return nil
						}
						o, err := lookupShip(a[0])
						if err != nil {
							return err
						}
						switch {
							case o == sh || o.team != sh.team:
								return bad("Tractor can only link to another friendly ship.")
							case cheb(sh.v, sh.h, o.v, o.h) > 1:
								return bad(o.name + " is not adjacent.")
							case sh.shieldsUp || o.shieldsUp:
								return bad("Both ships must have shields down.")
							case sh.dev[DTractor] >= devInop || o.dev[DTractor] >= devInop:
								return bad("Tractor beam inoperative.")
						}
						breakTractor(sh)
						breakTractor(o)
						sh.tractor, o.tractor = o, sh
						s.printf("Tractor beam engaged with %s.\n", o.name)
						note(o, fmt.Sprintf("%s has engaged a tractor beam on you.\n", sh.name))
						return nil
					}

					// ---------------------------------------------------------------- radio / tell

					func cmdRadio(s *Session, a []string) error {
						sh := s.ship
						if len(a) == 0 {
							return syn("RADIO")
						}
						switch kw(a[0], "ON", "OFF", "GAG", "UNGAG") {
							case "ON":
								sh.radioOn = true
								s.out("Radio on.\n")
							case "OFF":
								sh.radioOn = false
								s.out("Radio off.\n")
							case "GAG", "UNGAG":
								if len(a) != 2 {
									return syn("RADIO")
								}
								o, err := lookupShip(a[1])
								if err != nil {
									return err
								}
								if kw(a[0], "GAG", "UNGAG") == "GAG" {
									sh.gagged[o] = true
									s.printf("%s gagged.\n", o.name)
								} else {
									delete(sh.gagged, o)
									s.printf("%s ungagged.\n", o.name)
								}
							default:
								return syn("RADIO")
						}
						return nil
					}

					func garble(msg string, dmg float64) string {
						if dmg <= 0 {
							return msg
						}
						b := []byte(msg)
						for i := range b {
							if b[i] != ' ' && rand.Float64() < dmg/600 {
								b[i] = '#'
							}
						}
						return string(b)
					}

					func cmdTell(s *Session, a []string) error {
						sh := s.ship
						raw := s.curRaw
						i := strings.IndexByte(raw, ';')
						if i < 0 {
							return syn("TELL")
						}
						toks := strings.FieldsFunc(raw[:i], isSep)
						if len(toks) < 2 {
							return syn("TELL")
						}
						msg := strings.TrimSpace(raw[i+1:])
						if msg == "" {
							return syn("TELL")
						}
						if len(msg) > 120 {
							msg = msg[:120]
						}
						if sh.dev[DRadio] >= devInop {
							return bad("Sub-space radio inoperative.")
						}
						if !sh.radioOn {
							return bad("Your radio is off.")
						}
						targets := map[*Ship]bool{}
						for _, t := range toks[1:] {
							key := ""
							if len(t) >= 2 {
								key = kw(t, "ALL", "FEDERATION", "HUMAN", "EMPIRE", "KLINGON", "ENEMY", "FRIENDLY")
							}
							for _, o := range w.ships {
								if !o.active || o == sh {
									continue
								}
								switch key {
									case "ALL":
										targets[o] = true
									case "FEDERATION", "HUMAN":
										targets[o] = targets[o] || o.team == FED
									case "EMPIRE", "KLINGON":
										targets[o] = targets[o] || o.team == EMP
									case "ENEMY":
										targets[o] = targets[o] || o.team != sh.team
									case "FRIENDLY":
										targets[o] = targets[o] || o.team == sh.team
								}
							}
							if key == "" {
								x, rom, st := shipRef(t)
								if st != 0 || rom {
									return bad("Unknown recipient: " + t)
								}
								if x.active && x != sh {
									targets[x] = true
								}
							}
						}
						n := 0
						for o, ok := range targets {
							if !ok || o.sess == nil || !o.radioOn || o.dev[DRadio] >= devInop || o.gagged[sh] {
								continue
							}
							n++
							dm := math.Max(sh.dev[DRadio], o.dev[DRadio])
							text := fmt.Sprintf("From %s: %s\n", sh.name, garble(msg, dm))
							rcv := o
							if dm > 0 {
								after(sec(dm/100), func() { note(rcv, text) })
							} else {
								note(rcv, text)
							}
						}
						if n == 0 {
							s.out("No one received your message.\n")
						}
						return nil
					}

					// ---------------------------------------------------------------- SCAN

					func clampR(x int) int {
						if x > scanRange {
							return scanRange
						}
						if x < -scanRange {
							return -scanRange
						}
						return x
					}

					func scanSym(s *Session, v, h int, warn, short bool) string {
						if !inGrid(v, h) {
							return ""
						}
						sh := s.ship
						c := w.grid[v][h]
						pick := func(l, sh string) string {
							if short {
								return sh
							}
							return l
						}
						switch c.k {
							case KShip:
								return pick(c.ship.name[:1]+" ", c.ship.name[:1])
							case KRom:
								if romVisible(sh) {
									return pick("??", "?")
								}
							case KBase:
								c.base.known[sh.team] = true
								if c.base.team == FED {
									return pick("<>", "<")
								}
								return pick(")(", "(")
							case KPlanet:
								c.pl.known[sh.team] = true
								switch c.pl.owner {
									case FED:
										return pick("@F", "F")
									case EMP:
										return pick("@E", "E")
								}
								return pick(" @", "@")
									case KStar:
										return pick(" *", "*")
									case KHole:
										return pick("  ", " ")
						}
						if warn {
							for _, b := range w.bases {
								if b.alive && b.team != sh.team && cheb(v, h, b.v, b.h) <= baseRange {
									return pick(" !", "!")
								}
							}
							for _, p := range w.planets {
								if p.owner != sh.team && cheb(v, h, p.v, p.h) <= planetRange {
									return pick(" !", "!")
								}
							}
						}
						return pick(" .", ".")
					}

					func cmdScan(s *Session, a []string, sr bool) error {
						sh := s.ship
						cmd := "SCAN"
						def := scanRange
						if sr {
							cmd, def = "SRSCAN", 7
						}
						dir, warn := "", false
						var nums []int
						for _, t := range a {
							if n, ok := num(t); ok {
								nums = append(nums, n)
								continue
							}
							switch k := kw(t, "UP", "DOWN", "RIGHT", "LEFT", "CORNER", "WARNING"); k {
								case "":
									return syn(cmd)
								case "WARNING":
									warn = true
								default:
									dir = k
							}
						}
						vr, hr := def, def
						switch len(nums) {
							case 0:
							case 1:
								vr, hr = nums[0], nums[0]
							case 2:
								vr, hr = nums[0], nums[1]
							default:
								return syn(cmd)
						}
						vr, hr = clampR(vr), clampR(hr)
						var vlo, vhi, hlo, hhi int
						if dir == "CORNER" {
							vlo, vhi = min(sh.v, sh.v+vr), max(sh.v, sh.v+vr)
							hlo, hhi = min(sh.h, sh.h+hr), max(sh.h, sh.h+hr)
						} else {
							vr, hr = abs(vr), abs(hr)
							vlo, vhi, hlo, hhi = sh.v-vr, sh.v+vr, sh.h-hr, sh.h+hr
							switch dir {
								case "UP":
									vlo = sh.v
								case "DOWN":
									vhi = sh.v
								case "RIGHT":
									hlo = sh.h
								case "LEFT":
									hhi = sh.h
							}
						}
						short := s.set.scan == 1
						if s.nawsW > 0 && 8+4*(hhi-hlo+1) > s.nawsW {
							s.out("Warning: this scan is wider than your terminal.\n")
						}
						hdr := func() {
							var b strings.Builder
							b.WriteString("    ")
							for h := hlo; h <= hhi; h++ {
								if inGrid(sh.v, h) {
									b.WriteString(fmt.Sprintf("%4d", h))
								} else {
									b.WriteString("    ")
								}
							}
							s.out(b.String() + "\n")
						}
						hdr()
						for v := vhi; v >= vlo; v-- {
							var b strings.Builder
							lbl := "   "
							if inGrid(v, 1) {
								lbl = fmt.Sprintf("%3d", v)
							}
							b.WriteString(lbl + " ")
							for h := hlo; h <= hhi; h++ {
								sym := scanSym(s, v, h, warn, short)
								if short {
									b.WriteString(fmt.Sprintf("   %-1s", sym))
								} else {
									b.WriteString(fmt.Sprintf("  %-2s", sym))
								}
							}
							b.WriteString(" " + strings.TrimSpace(lbl))
							s.out(b.String() + "\n")
						}
						hdr()
						return nil
					}

					// ---------------------------------------------------------------- SET / TYPE / USERS / TIME

					func cmdSet(s *Session, a []string) error {
						if len(a) < 1 {
							return syn("SET")
						}
						k := kw(a[0], "NAME", "OUTPUT", "SCAN", "PROMPT", "OCDEF", "ICDEF", "TTYTYPE")
						val := func(opts ...string) (string, error) {
							if len(a) != 2 {
								return "", syn("SET")
							}
							v := kw(a[1], opts...)
							if v == "" {
								return "", syn("SET")
							}
							return v, nil
						}
						switch k {
							case "NAME":
								n := strings.Join(a[1:], " ")
								if len(n) > 12 {
									n = n[:12]
								}
								if n == "" {
									return syn("SET")
								}
								s.captain = n
							case "OUTPUT":
								v, err := val("LONG", "MEDIUM", "SHORT")
								if err != nil {
									return err
								}
								s.set.output = map[string]int{"LONG": OutLong, "MEDIUM": OutMed, "SHORT": OutShort}[v]
							case "SCAN":
								v, err := val("LONG", "SHORT")
								if err != nil {
									return err
								}
								s.set.scan = map[string]int{"LONG": 0, "SHORT": 1}[v]
							case "PROMPT":
								v, err := val("NORMAL", "INFORMATIVE")
								if err != nil {
									return err
								}
								s.set.prompt = map[string]int{"NORMAL": 0, "INFORMATIVE": 1}[v]
							case "OCDEF":
								v, err := val("ABSOLUTE", "RELATIVE", "BOTH")
								if err != nil {
									return err
								}
								s.set.ocdef = map[string]int{"ABSOLUTE": OcAbs, "RELATIVE": OcRel, "BOTH": OcBoth}[v]
							case "ICDEF":
								v, err := val("ABSOLUTE", "RELATIVE")
								if err != nil {
									return err
								}
								s.set.icdef = map[string]int{"ABSOLUTE": 0, "RELATIVE": 1}[v]
							case "TTYTYPE":
								v, err := val("CRT", "ADM-3", "ADM-2", "SOROC", "BEEHI", "ACT-I", "ACT-V")
								if err != nil {
									return err
								}
								s.set.ttype = v // accepted, no effect
							default:
								return syn("SET")
						}
						if s.state == stPlay {
							s.saveProfile()
						}
						return nil
					}

					func cmdType(s *Session, a []string) error {
						if len(a) != 1 {
							return syn("TYPE")
						}
						yn := func(b bool) string {
							if b {
								return "included"
							}
							return "not included"
						}
						switch kw(a[0], "OPTION", "OUTPUT") {
							case "OPTION":
								s.out(version + "\n")
								s.printf("Romulan %s; black holes %s.\n", yn(w.romulan), yn(w.blackholes))
							case "OUTPUT":
								st := s.set
								tty := st.ttype
								if tty == "" {
									tty = "CRT"
								}
								s.printf("%s output\n", [...]string{"LONG", "MEDIUM", "SHORT"}[st.output])
								s.printf("%s prompt\n", [...]string{"NORMAL", "INFORMATIVE"}[st.prompt])
								s.printf("%s scans\n", [...]string{"LONG", "SHORT"}[st.scan])
								s.printf("%s input default\n", [...]string{"ABSOLUTE", "RELATIVE"}[st.icdef])
								s.printf("%s output coordinates\n", [...]string{"ABSOLUTE", "RELATIVE", "BOTH"}[st.ocdef])
								s.printf("TTYTYPE %s\n", tty)
							default:
								return syn("TYPE")
						}
						return nil
					}

					func cmdUsers(s *Session, a []string) error {
						level := s.set.output
						switch level {
							case OutLong:
								s.out("Ship       Captain      Speed   PPN   TTY  Job  Program\n")
							case OutMed:
								s.out("Ship       Captain      Speed   PPN\n")
							default:
								s.out("Ship       Captain\n")
						}
						for _, sh := range w.ships {
							if !sh.active {
								continue
							}
							cap, speed, id, prog := "(dropped)", 0, 0, "DECWAR"
							ppn := 0
							if sh.rec != nil {
								for _, c := range sh.rec.key {
									ppn += int(c)
								}
								ppn = 100 + ppn%900
							}
							if x := sh.sess; x != nil {
								cap, speed, id = x.captain, x.speed, x.id
								if len(x.queue) == 0 && !time.Now().Before(x.busyUntil) {
									prog = "DECWTI"
								}
							}
							switch level {
								case OutLong:
									s.printf("%-10s %-12s %5d %5d %5d %4d  %s\n", sh.name, cap, speed, ppn, id, 1000+id, prog)
								case OutMed:
									s.printf("%-10s %-12s %5d %5d\n", sh.name, cap, speed, ppn)
								default:
									s.printf("%-10s %s\n", sh.name, cap)
							}
						}
						return nil
					}

					func cmdTime(s *Session, a []string) error {
						now := time.Now()
						s.printf("Time since game started:     %s\n", fmtDur(now.Sub(w.start)))
						if s.ship != nil {
							s.printf("Time since ship entered:     %s\n", fmtDur(now.Sub(s.ship.enteredAt)))
						} else {
							s.printf("Time since ship entered:     %s\n", fmtDur(0))
						}
						s.printf("Run time this game:          %s\n", fmtDur(s.cpu))
						s.printf("Total run time since login:  %s\n", fmtDur(s.cpu))
						s.printf("Current time:                %s\n", now.Format("15:04:05"))
						return nil
					}

					// ---------------------------------------------------------------- NEWS / QUIT / GRIPE / HELP

					func cmdNews(s *Session, a []string) error {
						d, err := os.ReadFile(cfg.NewsFile)
						if err != nil {
							s.out("No news.\n")
							return nil
						}
						s.out(strings.ReplaceAll(string(d), "\f", "") + "\n")
						return nil
					}

					func cmdQuit(s *Session, a []string) error {
						now := time.Now()
						if s.state == stPlay && s.ship != nil {
							sh := s.ship
							if sh.rec != nil {
								sh.rec.cas++
								sh.rec.lockUntil = now.Add(cfg.QuitLock)
							}
							w.ipLock[s.ip] = now.Add(cfg.QuitLock)
							radioTeam(sh.team, fmt.Sprintf("%s has left the game.\n", sh.name), sh)
							releaseShip(sh)
							s.ship = nil
						}
						s.state = stPre
						s.queue = nil
						s.out("Goodbye.\n")
						s.closing = true
						s.shutdown()
						return nil
					}

					func cmdGripe(s *Session, a []string) error {
						s.inGripe = true
						s.gripe, s.gripeLen = nil, 0
						s.out("Enter your gripe. ^Z sends it, ^C aborts.\n")
						return nil
					}

					func (s *Session) gripeLine(line string, term byte) {
						add := func(l string) {
							if s.gripeLen+len(l) <= 4000 {
								s.gripe = append(s.gripe, l)
								s.gripeLen += len(l) + 1
							}
						}
						if term != 0x1A {
							add(line)
							return
						}
						if line != "" {
							add(line)
						}
						s.inGripe = false
						now := time.Now()
						ship := "(none)"
						if s.ship != nil {
							ship = s.ship.name
						}
						yn := func(b bool) string {
							if b {
								return "yes"
							}
							return "no"
						}
						hdr := fmt.Sprintf("%s\nDate %s  Time %s\nShip %s  User %s\nTTY speed %d  PPN %d  TTY %d  Job %d\nRomulans %s  Black holes %s\n",
								   version, now.Format("2006-01-02"), now.Format("15:04:05"), ship, s.captain, s.speed,
								   100+s.id, s.id, 1000+s.id, yn(w.romulan), yn(w.blackholes))
						entry := hdr + strings.Join(s.gripe, "\n") + "\n" + strings.Repeat("-", 40) + "\n"
						old, _ := os.ReadFile(cfg.GripeFile)
						if err := os.WriteFile(cfg.GripeFile, append([]byte(entry), old...), 0o644); err != nil {
							s.out("Could not file gripe.\n")
						} else {
							s.out("Gripe filed. Thank you.\n")
						}
						s.gripe = nil
					}

					type Topic struct {
						name, title string
						body        []string
					}

					var helpTopics []*Topic

					func loadHelp() {
						if data, err := os.ReadFile(cfg.HelpFile); err == nil {
							lines := strings.Split(strings.ReplaceAll(string(data), "\f", ""), "\n")
							for i := 0; i < len(lines); i++ {
								ln := strings.TrimRight(lines[i], "\r")
								f := strings.Fields(ln)
								if strings.HasPrefix(ln, ".") && len(f) > 0 && len(f[0]) > 1 {
									t := &Topic{name: up(f[0][1:])}
									if len(f) > 1 && false {
										t.title = ""
									}
									i++
									if i < len(lines) {
										t.title = strings.TrimRight(lines[i], "\r")
									}
									for i++; i < len(lines); i++ {
										l := strings.TrimRight(lines[i], "\r")
										if strings.TrimSpace(l) == "." {
											break
										}
										t.body = append(t.body, l)
									}
									helpTopics = append(helpTopics, t)
								}
							}
						}
						has := func(n string) bool {
							for _, t := range helpTopics {
								if t.name == n {
									return true
								}
							}
							return false
						}
						if !has("INTRODUCTION") {
							t := &Topic{name: "INTRODUCTION", title: "Introduction to DECWAR"}
							t.body = []string{version, "", "DECWAR is a real-time multiplayer space war game: Federation vs Empire.",
								"Type HELP * for the list of commands and HELP <command> for details.", ""}
								for i, c := range gameCmdList {
									t.body = append(t.body, fmt.Sprintf("%3d. %s", i+1, capName(c, gameCmdList)))
								}
								helpTopics = append(helpTopics, t)
						}
						if !has("ESCAPE") {
							helpTopics = append(helpTopics, &Topic{name: "ESCAPE", title: "The ESC key",
								body: []string{"Pressing ESC as the first character of a line repeats the previous command.",
									"TELL commands cannot be repeated."}})
						}
						for _, c := range gameCmdList {
							if !has(c) {
								helpTopics = append(helpTopics, &Topic{name: c, title: capName(c, gameCmdList),
									body: []string{"Syntax: " + usage[c]}})
							}
						}
					}

					func findTopic(word string) *Topic {
						wd := trunc5(up(word))
						for _, t := range helpTopics {
							if trunc5(t.name) == wd {
								return t
							}
						}
						for _, t := range helpTopics {
							if strings.HasPrefix(t.name, wd) {
								return t
							}
						}
						return nil
					}

					func cmdHelp(s *Session, a []string) error {
						s.helpActive = true
						if len(a) == 0 {
							a = []string{"INTRODUCTION"}
						}
						for _, k := range a {
							if k == "*" {
								s.out("Commands:\n")
								printed := false
								if t := findTopic("INTRODUCTION"); t != nil {
									for _, l := range t.body {
										f := strings.Fields(l)
										if len(f) > 1 && strings.HasSuffix(f[0], ".") {
											if _, err := strconv.Atoi(strings.TrimSuffix(f[0], ".")); err == nil {
												s.out(l + "\n")
												printed = true
											}
										}
									}
								}
								if !printed {
									for i, c := range gameCmdList {
										s.printf("%3d. %s\n", i+1, capName(c, gameCmdList))
									}
								}
								continue
							}
							t := findTopic(k)
							if t == nil {
								s.printf("No help available for %s.\n", k)
								continue
							}
							s.printf("%s\n", t.title)
							for _, l := range t.body {
								s.out(l + "\n")
							}
						}
						return nil
					}

					// ---------------------------------------------------------------- main loop

					func tick(now time.Time) {
						runSched(now)
						autoTick(now)
						for _, s := range append([]*Session(nil), sessions...) {
							if s.state != stPlay && now.Sub(s.lastIn) > cfg.IdlePregame {
								s.out("Idle timeout. Goodbye.\n")
								s.lastIn = now
								s.shutdown()
							}
							s.pump()
						}
					}

					func gameLoop() {
						tk := time.NewTicker(100 * time.Millisecond)
						for {
							select {
								case e := <-evCh:
									handleEvent(e)
								case now := <-tk.C:
									tick(now)
							}
						}
					}

					func main() {
						flag.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
						flag.StringVar(&cfg.HelpFile, "help", cfg.HelpFile, "DECWAR.HLP path")
						flag.StringVar(&cfg.NewsFile, "news", cfg.NewsFile, "DECWAR.NWS path")
						flag.StringVar(&cfg.GripeFile, "gripes", cfg.GripeFile, "DECWAR.GRP path")
						flag.StringVar(&cfg.ProfileDir, "profiles", cfg.ProfileDir, "profile directory")
						flag.IntVar(&cfg.MaxConn, "maxconn", cfg.MaxConn, "max connections")
						flag.Int64Var(&cfg.Seed, "seed", 0, "random seed (0 = random)")
						flag.BoolVar(&cfg.ComputedForAll, "computed-all", false, "allow COMPUTED coordinates on fast terminals")
						flag.BoolVar(&cfg.Throttle, "throttle", false, "pace output at terminal baud rate")
						noRom := flag.Bool("no-romulan", false, "disable the Romulan")
						selftest := flag.Bool("selftest", false, "run built-in tests and exit")
						flag.Parse()
						if *noRom {
							cfg.RomulanEnabled = false
						}
						if cfg.Seed != 0 {
							rand.Seed(cfg.Seed)
						}
						if *selftest {
							os.Exit(runSelfTest())
						}
						w = newWorld()
						loadHelp()
						ln, err := net.Listen("tcp", cfg.Addr)
						if err != nil {
							fmt.Fprintln(os.Stderr, "listen:", err)
							os.Exit(1)
						}
						fmt.Fprintf(os.Stderr, "%s listening on %s\n", version, cfg.Addr)
						go acceptLoop(ln)
						gameLoop()
					}

					// ---------------------------------------------------------------- self test

					func runSelfTest() int {
						fails := 0
						check := func(name string, got, want interface{}) {
							if fmt.Sprint(got) != fmt.Sprint(want) {
								fails++
								fmt.Printf("FAIL %s\n  got : %v\n  want: %v\n", name, got, want)
							} else {
								fmt.Printf("ok   %s\n", name)
							}
						}
						mk := func(o int, oc int) *Session {
							s := &Session{}
							s.set.output, s.set.ocdef = o, oc
							return s
						}
						tp := Who{long: "Goblin", abbr: "G", v: 22, h: 31, pct: 83.6, hasPct: true, up: true}
						tt := Who{long: "Vulcan", abbr: "V", v: 20, h: 31, pct: 72.1, hasPct: true, up: true}
						check("hit long", fmtHit(mk(OutLong, OcAbs), tp, tt, 285.3, 'T', true),
						      "Goblin @22-31, +83.6%  makes 285.3 unit torpedo hit on Vulcan displaced to 20-31, +72.1%\n")
						check("hit medium", fmtHit(mk(OutMed, OcAbs), tp, tt, 285.3, 'T', true),
						      "G @22-31, +83.6%  285.3 unit T  V -->20-31, +72.1%\n")
						check("hit short", fmtHit(mk(OutShort, OcAbs), tp, tt, 285.3, 'T', true), "G 22-31 +83  285T  V >20-31 +72\n")
						pl := Who{long: "Emp planet(3)", abbr: "-@3", v: 15, h: 16}
						bz := Who{long: "Buzzard", abbr: "B", v: 15, h: 17, pct: 66.8, hasPct: true, up: true}
						check("hit short planet", fmtHit(mk(OutShort, OcAbs), pl, bz, 155.5, 'P', false), "-@3 15-16  155P  B 15-17 +66\n")
						st := Who{long: "Star", abbr: "*", v: 22, h: 31}
						pn := Who{long: "Panther", abbr: "P", v: 20, h: 31, pct: 72.1, hasPct: true}
						vs := mk(OutLong, OcBoth)
						vs.ship = &Ship{v: 18, h: 29}
						check("hit long nova", fmtHit(vs, st, pn, 301.2, 'N', true),
						      "Star @22-31 +4,+2 makes 301.2 unit hit on Panther displaced to 20-31 +2,+2, -72.1%\n")
						vs.set.output = OutMed
						check("hit medium nova", fmtHit(vs, st, pn, 301.2, 'N', true),
						      "* @22-31 +4,+2  301.2 unit N  P -->20-31 +2,+2, -72.1%\n")
						check("torpedo burst TO 3 12 24 13 39", torpCounts(3, 2), []int{1, 2})
						check("torpedo burst n=3 k=3", torpCounts(3, 3), []int{1, 1, 1})
						m, _ := resolve("TORPE", gameCmdList)
						check("5-char truncation", m, "TORPEDOES")
						m, _ = resolve("M", gameCmdList)
						check("M -> MOVE", m, "MOVE")
						_, stt := resolve("D", gameCmdList)
						check("D ambiguous", stt, 1)
						for _, c := range gameCmdList {
							if n := len(capName(c, gameCmdList)); n < 0 {
								check("capname", c, "")
							}
						}
						check("split multi", splitLine("M 1 1/ST ;x/y"), []string{"M 1 1", "ST"})
						check("split tell", splitLine("TE ALL ; hi/there"), []string{"TE ALL ; hi/there"})
						p := linePath(1, 1, 7, 4)
						check("bresenham end", p[len(p)-1], [2]int{7, 4})
						// help-file examples: 300 baud, 200 phaser damage -> 6+2; 3 torpedoes, 200 tube dmg -> 3*(4+2)
						check("phaser cooling 300 baud", 3+(6-3)*1.0+200.0/100, 8.0)
						check("torpedo reload 300 baud", 3*(2+(4-2)*1.0+200.0/100), 18.0)

						// ---- combat mechanics on a scratch world
						w = newWorld()
						for i := range w.grid {
							for j := range w.grid[i] {
								w.grid[i][j] = Cell{}
							}
						}
						w.planets, w.bases, w.rom, w.romulan = nil, nil, nil, false
						ra, rb := &Rec{key: "a"}, &Rec{key: "b"}
						A, B := w.ships[0], w.ships[5]
						for _, x := range []struct {
							sh   *Ship
							r    *Rec
							v, h int
						}{{A, ra, 10, 10}, {B, rb, 10, 13}} {
							x.sh.active, x.sh.rec = true, x.r
							x.sh.energy, x.sh.shield, x.sh.radioOn = shipEnergyM, shieldM, true
							x.sh.v, x.sh.h = 0, 0
							moveShipTo(x.sh, x.v, x.h)
						}
						hitShip(B, A, whoShip(A), 100, 'P')
						check("phaser damage applied", B.damage, 100.0)
						check("damage credited", ra.sc.dmgEnemy, 100.0)
						B.shieldsUp = true
						hitShip(B, A, whoShip(A), 100, 'P')
						check("shields absorb 100%", B.damage, 100.0)
						check("shield energy reduced", B.shield, 2400.0)
						B.shieldsUp = false
						w.grid[10][11] = Cell{k: KStar}
						launchTorpedoStraight := func() { torpImpact(A, 10, 11) }
						launchTorpedoStraight()
						check("nova removes star", w.grid[10][11].k, KEmpty)
						check("star penalty", ra.sc.stars, 1)
						check("nova hurt both", A.damage > 0 && B.damage > 100, true)
						w.grid[10][12] = Cell{}
						hitShip(B, A, whoShip(A), 9000, 'P')
						check("ship destroyed", B.active, false)
						check("kill credited", ra.sc.kills, 1)
						check("score total", ra.sc.total() > 500, true)
						// black hole displacement kills
						C := w.ships[6]
						C.active, C.rec, C.energy, C.shield, C.team = true, rb, shipEnergyM, shieldM, EMP
						C.v, C.h = 0, 0
						moveShipTo(C, 30, 30)
						w.grid[30][31], w.grid[30][32] = Cell{k: KHole}, Cell{k: KHole}
						died := false
						for i := 0; i < 50 && !died; i++ {
							C.active = true
							moveShipTo(C, 30, 30)
							died = displace(C, 30, 29)
						}
						check("displacement into black hole kills", died, true)
						// base destruction and win
						bs := &Base{v: 40, h: 40, team: EMP, shield: 0, alive: true}
						w.bases = []*Base{bs}
						w.grid[40][40] = Cell{k: KBase, base: bs}
						A.dev[DShield] = 0
						baseHit(bs, A, whoShip(A), 3000, 'T')
						check("base destroyed", bs.alive, false)
						check("win detected", w.over && w.winner == FED, true)
						hasF := hasAssets(FED)
						check("fed still has assets", hasF, true)
						if fails > 0 {
							fmt.Printf("%d FAILED\n", fails)
							return 1
						}
						fmt.Println("all tests passed")
						return 0
					}

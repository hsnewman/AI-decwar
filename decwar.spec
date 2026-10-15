DECWAR Telnet Server: Programming Specification
Derived from DECWAR.HLP (DECWAR Version 2.2, December 25, 1979). Target: a multiplayer, real-time clone served over TCP port 1701 using the Telnet protocol.
1. Scope, Sources and Conventions
This document specifies a server that reproduces the DECWAR experience for 1 to 10 simultaneous players, each connected with an ordinary Telnet client. The only source material is the DECWAR help file, which describes behavior but not every numeric formula. Every requirement is therefore tagged:
• (H) stated or directly implied by the help file. Implement exactly.
• (D) a design decision made here because the help file is silent. These values are tunable constants and live in a single config structure (Appendix A).
Where the original ran as separate TOPS-10 jobs sharing a high segment, this server is one process with one authoritative game state. Where the original referred to PPN, TTY and job numbers, this server substitutes user id, connection number and session id (D).
Terminology. vpos is the vertical coordinate (row), hpos the horizontal coordinate (column). Both range 1..75. A coordinate prints as vpos-hpos, e.g. 22-31. A positive vertical delta means up (increasing vpos); a positive horizontal delta means right (increasing hpos) (H).
2. System Architecture
2.1 Process model (D)
• One process, one listening socket on 0.0.0.0:1701 (configurable), IPv4 and IPv6.
• Single-threaded event loop (or one game thread plus network threads feeding a queue). All game-state mutation happens on one thread so no locking is needed.
• A monotonic scheduler (priority queue of timed events) drives everything real-time: command completion pauses, phaser cooling, torpedo reloading, base and planet firing, base regeneration, the Romulan, life-support countdown and stardate advance.
• Game tick resolution: 100 ms or finer. Scheduler events are not tied to the tick, only checked each tick.
2.2 Components
Component
Responsibility
TelnetLayer
Socket I/O, IAC parsing and negotiation, per-session byte stream
LineEditor
Character-at-a-time input, echo control, ^H ^U ^R ^G ^O ^C ^Z ESC handling
CommandParser
Splitting, tokenizing, abbreviation resolution, argument grammar
Session
Per-connection state: mode (pre-game or game), settings, command queue, output buffer
World
Galaxy grid, objects, teams, stardate, win detection
Combat
Phaser, torpedo, nova, displacement, damage application, scoring hooks
Automata
Starbases, planets, Romulan behavior
Formatter
Long, Medium and Short message and scan rendering
HelpDb
Loads DECWAR.HLP (and DECWAR.NWS) and serves topics
Persist
Gripe file, rejoin locks, team memory
2.3 Concurrency semantics (H)
The game is real time. Each player's commands run in order from that player's own queue. A command that "takes real time" (Section 9) blocks only that player's queue until its pause elapses; other players, bases, planets and the Romulan continue meanwhile. Output generated asynchronously (hit messages, TELL messages, base distress calls) is interleaved into the player's output stream at line boundaries and must never corrupt a half-typed input line (Section 3.5).
3. Telnet Protocol Layer (port 1701)
3.1 Transport
• TCP, accept loop, max simultaneous connections configurable (default 20: up to 10 players plus pre-game observers) (D).
• Idle timeout in pre-game: 30 minutes (D). In game, no idle timeout, because a ship sitting still is a legal tactic. A disconnected socket is treated exactly like ^C at command-input wait (Section 4.4).
3.2 Negotiation (D, per RFC 854/855/857/858/1073/1079/1091)
On accept, immediately send:
IAC WILL ECHO
IAC WILL SUPPRESS-GO-AHEAD
IAC DO   SUPPRESS-GO-AHEAD
IAC DO   TERMINAL-SPEED     (RFC 1079, optional)
IAC DO   NAWS               (RFC 1073, optional)
IAC DO   TERMINAL-TYPE      (RFC 1091, optional)
The server performs all echoing itself (character-at-a-time mode), because the game needs ^G echo toggling, ^R retype and ^U kill. Reply rules:
• Refuse everything else (DO x gets WONT x, WILL x gets DONT x). Never loop on repeated requests (track option state per RFC 1143 or ignore duplicates).
• On SB TERMINAL-SPEED IS <tx>,<rx> record the speed in bps. Default when unknown: 9600 (D). Speed classes drive pause lengths (Section 9.2) and the availability of COMPUTED coordinates (Section 8.0).
• On SB NAWS store width and height; used only for scan layout warnings. On SB TERMINAL-TYPE IS store the name; SET TTYTYPE is accepted but does nothing (H).
• Telnet clients that never negotiate (raw nc) must still work: timeouts of 1 s on all optional negotiations, then proceed.
3.3 Byte-stream rules
• Input: IAC IAC yields a literal 0xFF. CR NUL and CR LF each count as one CR. A lone LF is also a terminator. Strip the high bit of data bytes (7-bit game). Discard unknown IAC commands. IAC IP (interrupt process) is treated as ^C. IAC AO is treated as ^O.
• Output: every newline is sent as CR LF. A literal 0xFF in output is doubled. The bell is 0x07.
3.4 Input line editing (H unless noted)
The prompt is COMMAND:   (two trailing spaces) in Normal prompt mode, or the Informative prompt (Section 8.SET). In pre-game the prompt is PG> . Per character received while a line is being typed:
Byte
Name
Action
0x0D, 0x0A, 0x0B, 0x0C, 0x1B, 0x1A
CR, LF, VT, FF, ESC, ^Z
Terminate the line (a CR immediately followed by LF or NUL is one terminator)
0x1B as the first character
ESC
Repeat the previous command line (see 3.6)
0x08 or 0x7F
^H, DEL
Delete previous character; echo BS SP BS (D)
0x15
^U
Discard the whole line; echo ^U and newline; echo is re-enabled
0x12
^R
Reprint the current line (newline then the line)
0x07
^G
Toggle echo off or on for the rest of this line. Echo is always back on at the start of each line and at end of line
0x0F
^O
Discard all pending (unsent) output, see 3.7
0x03
^C
See Section 4.4 and 3.7
other printable

Append to buffer, echo if echo is on
Only the first 5 characters of each word are significant; the rest are ignored (H). Words are separated by spaces, tabs or commas (H). Maximum line length 255 (D); overflow rings the bell and drops the character.
3.5 Asynchronous output vs. a half-typed line (D)
If output arrives while the player is typing, emit CR LF, print the output, then reprint the prompt and the partial line buffer. If echo is off (^G), reprint the prompt only.
3.6 Command line grammar (H)
line      := command { "/" command }        ; multiple commands
comment   := ";" text-to-end                ; ignored, except in TELL
command   := keyword { word }
• Split on /, except that if the line contains a TELL command it must be last, and the entire remainder after the first ; is the message, slashes included.
• Everything after ; is a comment and ignored for all other commands.
• Each command is pushed on the session's command stack (FIFO) and executed in order. Typing ahead during a timed command appends to the same stack.
• ^C while not at input wait aborts the entire stack and rings several bells (H): send BEL BEL BEL.
• ESC repeat: re-pushes the last executed single command text (not the whole multi-command line, D). It cannot repeat TELL ("no junk mail"); on a prior TELL it does nothing and rings the bell.
3.7 Output control
• Output is buffered per session. ^O flushes the unsent buffer and prints ^O. While output is still pending, no further commands are executed (H hint on ^O): the dispatcher checks outbuf.empty() before dequeuing the next command. This models slow terminals. For fast links (speed >= 9600) the pending drain is effectively instant.
• To mimic baud-rate pacing, an optional throttle (D) meters output at speed/10 bytes per second per session, off by default.
4. Session Lifecycle
4.1 States
CONNECT -> PREGAME (prompt "PG> ") -> SETUP (team, ship) -> PLAYING (prompt COMMAND:) -> END
                                                           \-> DEAD -> PREGAME
The help file names the programs DECWPG (pre-game), DECWTI (waiting for command input) and DECWAR (executing). A session exposes this as a program string used by USERS only in Long output (D).
4.2 Banner and pre-game (H)
On connect, print a banner with version DECWAR Version 2.2, December 25, 1979 and a one-line note about the Romulan/black-hole options currently in effect. Then start the pre-game, prompt PG> . Commands legal in pre-game: ACTIVATE, GRIPE, HELP, NEWS, POINTS, QUIT, SUMMARY, TIME, USERS (H). Their resolution of abbreviation uses only this reduced list. ACTIVATE leaves pre-game and begins SETUP. Any other command prints Command not legal in pre-game. (D, message text)
Pre-game players are not in the galaxy, cannot be attacked, and are not counted as players for Romulan rules.
4.3 SETUP: team and ship selection (H, with D details)
1. Identify the captain (D): prompt Enter your name: . Case-insensitive, max 12 chars. This is the key for rejoin memory and the initial USERS captain name.
2. Team. A returning player (same captain key who was destroyed or who quit earlier in this game) is automatically rejoined to the old team (H). A new player chooses Federation (Human) or Empire (Klingon), unless there is a large imbalance in team sizes, in which case the server forces the smaller team (H). Define imbalance as abs(fed - emp) >= 2 (D).
3. Ship. List the remaining unoccupied ships of the team and prompt for one; accept any unambiguous abbreviation (1 character is enough, H). Re-prompt on error. If all five ships of the team are occupied, tell the player and return to the team prompt (if allowed) or pre-game (D).
4. Commissioning: ship receives full supplies (Section 7), is placed at a random empty sector (D: within a start region around the team's bases, avoiding stars, planets, bases, black holes and within 2 sectors of any enemy port), condition green. Announce to all radios (D): <ship> (<captain>) has entered the game.
4.4 Leaving the game
Action
Effect (H)
QUIT
Ship released to the pool, player counts as one more casualty, cannot CONTINUE, rejoin lock of 2 minutes (D: enforced by captain key and source IP). Works even under red alert
^C at command-input wait
Game aborted, ship returned to pool. For a telnet server model this as: the ship is released but the connection stays at pre-game; if no one else takes the ship or the sector within 60 s (D), the player may re-ACTIVATE and resume the same ship and position. Otherwise the ship is gone
^C during red alert
Ignored for exit purposes. Only QUIT works (H). Bells may sound
^C while commands pending
Abort stacked commands, ring bells
Connection drop
Same as ^C at input wait. If the ship is under red alert, it stays in the galaxy as a helpless target for 30 s (D) before being released
Ship destroyed
Notify, return to pre-game. A rejoin to the same team is allowed immediately (D)
5. The Galaxy
5.1 Grid (H)
75 x 75 sectors, coordinates 1..75 each axis, no wraparound. Every sector holds at most one object. Moves that leave the grid are rejected.
5.2 Object types (H)
Kind
Scan symbol (Long)
Short symbol
Notes
Federation warship
L N S V Y + space
one char
Lexington, Nimitz, Savannah, Vulcan, Yorktown
Empire warship
C D H J W + space
one char
Cobra, Demon, Hawk, Jackal, Wolf
Romulan warship
??
?
At most 1 in the game; normally cloaked
Federation starbase
<>
<

Empire starbase
)(
(

Neutral planet
 @
@

Federation planet
@F
F
Captured
Empire planet
@E
E
Captured
Star
 *
*
Can nova
Black hole
blank (two spaces)
blank
Kills ships displaced into it, absorbs torpedoes
Empty
 .
.
! instead when WARNING and in range of an enemy base or planet
Ship names have unique first letters across both teams plus Romulan, so 1-character abbreviation is always unambiguous (H).
5.3 Initial population (D, all tunable)
• Stars: 250. Planets: 60, all neutral at start (H: "all planets are neutral"). Starbases: 5 per team = 10 total (consistent with H "only 10 starbases can be functional at any one time", read as a galaxy-wide cap). Black holes: 25% chance per game of being included (H), 8 when included (D).
• Placement: bases are placed in opposite regions of the galaxy (Federation near low hpos, Empire near high hpos), at least 6 sectors apart (D). Planets and stars are scattered uniformly with the constraint that no two objects share a sector and no star is adjacent to a base on game start (D).
• Romulan: included at game start if players <= 3 (H). Exactly 0 or 1 exists at any time. After the 4th concurrent player joins, a destroyed Romulan is not recreated (H). While players <= 3, a replacement appears after romulan_respawn seconds (D: 90 s) at a random empty sector (H: "another will eventually appear somewhere").
5.4 Game end (H)
A team wins when the opposing team has: no starbases, no captured planets, and no ships in the galaxy. Neutral planets do not count. When detected, broadcast the victory message to all sessions, show the final POINTS table, wait 60 s, then reinitialize the galaxy and send all sessions to pre-game (D). Players who were killed earlier in the game and have not rejoined are not counted as ships.
6. Core Data Structures (D, suggested)
Ship {
  name, team{FED,EMP}, captain, session*,
  v, h,                       // 1..75
  energy        = 5000,       // 0 => dead. max 5000 (H)
  shield_energy = 2500,       // separate pool (H)
  shields_up    : bool,
  damage        = 0.0,        // total ship damage. >= 2500 => dead (H)
  dev[9]        : float,      // per-device damage: WARP IMPULSE TORP PHASER SHIELD COMPUTER LIFE RADIO TRACTOR
  torpedoes     = 10,         // max 10 (D)
  life_reserve  = 5,          // stardates (H)
  condition     : GREEN|YELLOW|RED,
  docked        : bool,
  tractor_peer  : Ship*,
  radio_on      : bool, gagged : set<Ship>,
  phaser_ready_at[2], torp_ready_at,   // monotonic timestamps
  score         : Score,
  settings      : { output{LONG,MED,SHORT}, scan{LONG,SHORT},
                    prompt{NORMAL,INFORMATIVE}, ocdef{ABS,REL,BOTH}, icdef{ABS,REL},
                    ttytype }
}
Planet { v, h, owner{NEUTRAL,FED,EMP}, builds 0..4, integrity }
Base   { v, h, team, shield_energy, damage }          // starbase
Star   { v, h }
BlackHole { v, h }
Romulan { v, h, cloaked, state }
Score  { dmg_enemy, kills, dmg_base, planets_captured, bases_built,
         romulan, stars_destroyed, planets_destroyed, commissioned }
World  { grid[76][76] -> object*, stardate, start_time, romulan_enabled,
         blackholes_enabled, teams[2], bases_count }
6.1 Visibility and "known" objects (H)
• A team knows an enemy base once anyone on that team has SCANned or LISTed it (or it was seen within scan range). Store known_by[team] flags on each enemy base and planet and expose them through BASES, PLANETS and LIST.
• Shield percent of an enemy object is visible only while it is within scan range (10 sectors) (H).
• Friendly ships and bases are always visible by location and shield percent. Any ship (either side) within 10 sectors is visible.
• The Romulan is cloaked: invisible on SCAN/TARGETS unless within range 1 of the viewer or actively firing, in which case it is revealed for 5 s (D). It is still listed by name in LIST and TARGETS (H: "name of any ship currently in the game (including the Romulan)").
7. Ship Model
7.1 New or fully repaired ship (H)
• Ship energy 5000, shield energy 2500, ship damage 0, device damage 0, torpedoes 10 (D), life support reserve 5 stardates.
• A ship with ship energy <= 1000 is on automatic yellow alert and a bell sounds after every move (H).
• Ship damage >= 2500 or ship energy = 0 destroys the ship (H).
7.2 Devices (H)
Device damage is a number of units. < 300: degraded performance. >= 300: inoperative. Devices are repaired in flight by REPAIR and by the normal 30 units per turn (Section 8.REPAIR). Device damage is distinct from total ship damage; total ship damage is cleared only by docking (H).
Device
Effect when degraded (<300)
Effect when inoperative (>=300)
Warp engines
Max warp 3 (H)
MOVE refused. Use IMPULSE
Impulse engines
Reserved for emergency (H)
IMPULSE refused
Photon tubes
Torpedo accuracy impaired (H), longer reload (+dmg/100 s)
Cannot fire torpedoes (D)
Phaser banks (control)
Weaker hit (H), longer cooling (+dmg/100 s)
Cannot fire phasers (D)
Deflector shields
Reduced effectiveness (D: effective % scaled by 1 - dmg/300)
Shields cannot be raised and are forced down (D)
Computer
Phaser strength reduced (H); movement and torpedo accuracy worse (H)
COMPUTED coordinates refused (H); MOVE/IMPULSE navigation "inexact" (H): add a random error of 0-2 sectors on arrival (D)
Life support
None
Countdown: ship must repair or dock within 5 stardates or the crew dies (H). Reserve decrements 1 per stardate; docking and repair back above 300 restore reserve up to 5
Sub-space radio
Messages garbled/delayed (D)
Cannot send or receive TELL (H)
Tractor beam
None
Cannot tow (D)
Devices also take damage from overuse (H): warp 5 and 6 (Section 8.MOVE), and phaser blasts above the default (Section 8.PHASERS).
7.3 Alert condition (H)
• Green: default; set by MOVE, IMPULSE and DOCK.
• Yellow: ship energy <= 1000 and not red.
• Red: set by firing phasers, firing torpedoes, being hit by either, and being fired on by a base or planet. Cleared (to green or yellow) by the next successful MOVE, IMPULSE or DOCK.
• While red, a player cannot leave with ^C. Only QUIT works. HELP and GRIPE protect the player from attack unless red alert (H): while a non-red player is in HELP or GRIPE, the ship is flagged protected and takes no damage; the flag is cleared on exit.
7.4 Energy and shield mechanics (H, formulas D)
• Shield percent = shield_energy / 2500 * 100. Display +n% when shields are up, -n% when down (H).
• Incoming hit of size X with shields up at p%: absorbed = X * p/100. Remaining X - absorbed is applied to ship damage and subtracted from ship energy. Shield energy is reduced by absorbed (H: "If shields are up, their energy is decreased each time the ship gets hit"). Shields down give no protection (hit applies in full) but the percent is still displayed with a minus sign.
• Strong shields may deflect a torpedo with little or no damage (H): if p >= 85, a torpedo hit is deflected harmlessly with probability (p - 70)/30 (D).
• Raising shields costs 100 ship energy; lowering and transferring are free (H). Energy spent on movement is doubled while shields are up (H).
• Energy transfer between ship energy and shield energy is limited by the pool maximums (5000 and 2500) (D).
8. Command Reference
8.0 Dispatch and common argument rules
Command table. Match the first word against the legal command list using shortest unambiguous prefix (H). No command needs more than 2 characters. Minimum prefixes implied by the help file's capitalization:
Command
Min
Command
Min
Command
Min
BAses
BA
LIst
LI
SCan
SC
BUild
BU
Move
M
SEt
SE
CApture
CA
NEws
NE
SHields
SH
DAmages
DA
PHasers
PH
SRscan
SR
DOck
DO
PLanets
PL
STatus
ST
Energy
E
POints
PO
SUmmary
SU
Gripe
G
Quit
Q
TArgets
TA
Help
H
RAdio
RA
TEll
TE
Impulse
I
REpair
RE
TIme
TI




TOrpedoes
TO




TRactor
TR




TYpe
TY




Users
U
Compute minimums programmatically from the table at startup rather than hard-coding them. An ambiguous or unknown word prints Ambiguous command or Invalid command (D, text). Keywords inside commands (ABSOLUTE, RELATIVE, ENEMY, ...) use the same prefix rule against the keyword set of that command, again at most 2 characters, and truncate to 5 significant characters.
Coordinate arguments (H). Wherever a command takes a coordinate it accepts one of:
1. Absolute (default): [ABSOLUTE] <vpos> <hpos>.
2. Relative: RELATIVE <dv> <dh>. Positive = up / right. Absolute = own position + delta. The keyword is optional when SET ICDEF RELATIVE is active.
3. Computed: COMPUTED <shipname>. The coordinate is that ship's current location. Requires an operational computer and is available only to captains on slow terminals (< 1200 baud) (H). For a telnet deployment, evaluate "slow" from the negotiated TERMINAL-SPEED, with a server option computed_for_all (default false) (D).
The type keyword is given once per command; all coordinates in that command share the type (H). Out-of-grid results are rejected with Invalid coordinates. Ship names accept any unambiguous abbreviation, down to one letter (H).
Timing prefix. For each command below, PAUSE means the player's command queue is blocked for that duration after the effect is applied (or before, for movement; see Section 9).
8.1 BASES, PLANETS, TARGETS, LIST, SUMMARY (information family)
All five are views over one engine, list_engine(cmd, keywords), differing only in defaults (H):
Command
Default side
Default objects
Default range
Default mode
LIST
all
ships, bases, planets
infinity
list
SUMMARY
all
all
infinity
summary only
BASES
friendly
bases
entire galaxy
list
PLANETS
every side
planets
10 sectors
list
TARGETS
enemy
ships, bases, planets
10 sectors
list; enemy flag omitted
Keywords (H), each with the prefix rule: ship names (including Romulan); <vpos> <hpos> (the object at that location); CLosest; SHips, BAses, PLanets, POrts (bases and planets; if no side is given, only friendly ports); sides FEderation/HUman, EMpire/Klingon, FRiendly, ENemy/TArgets, NEutral, CAptured; a number n (range in sectors); ALl (all sides unless one is given, and infinite range unless a range is given); LIst; SUmmary; AND or & (separates keyword groups, each group evaluated independently and the union printed).
Range is the Chebyshev distance max(|dv|, |dh|) (H hint on OCDEF BOTH: "the magnitude of either delta v or delta h, whichever is larger").
Output rules (H):
• Enemy objects are flagged with * in column 1 (not for TARGETS).
• Friendly ships and bases: location and shield percent. Enemy ships and bases: location, plus shield percent only if within 10 sectors (H). Unseen enemy ships show name only. Enemy bases appear only when known (Section 6.1). Planets: location and number of builds (known planets only, or any within range).
• Location is rendered per the viewer's OCDEF: vv-hh, dv,dh, or both (vv-hh dv,dh). With OCDEF BOTH or ABSOLUTE the range is printed alongside direction where the help text says to always show it.
• Summary mode prints counts by category: ships, bases, planets, split into friendly/enemy/neutral, plus Romulan count. SUM EN prints Romulans, enemy ships, enemy bases and enemy planets.
• Using LIST/BASES on a coordinate reveals (marks known) an enemy base there (H: scanned or listed).
• BA CL and LI CL PO return only the nearest matching object (ties broken by lowest vpos then hpos, D).
Example output (D format):
COMMAND:  BA
Friendly bases:
  <> 12-14  100%
  <> 33-21   84%
8.2 BUILD [A|R] <vpos> <hpos>
• Target must be a planet captured by your team (H). The planet must be adjacent to your ship (D, mirrors DOCK).
• Increases builds by 1 up to 4 (H). Fifth build converts the planet into a starbase of your team if the galaxy-wide functional base count is below 10 (H); otherwise refused with Starbase limit reached.
• PAUSE 5 to 7 seconds (H), randomly chosen. Awards 1000 points per completed starbase (H). Ship condition is not changed by BUILD (D).
• Failure messages: not a planet, not yours, not adjacent, already at 4 and base limit full.
8.3 CAPTURE [A|R] <vpos> <hpos>
• Target a neutral or enemy planet (H). Must be adjacent (D).
• PAUSE 5 s + 1 s per build present on an enemy planet (H). Ship energy lost: 50 per build (H).
• On completion the planet changes owner to your team; builds on a captured enemy planet are retained (D) or reset to 0 (D, default: reset to 0). Award 100 points (H).
• A planet within range of an enemy base is still capturable, but it keeps firing during the pause (D).
8.4 DAMAGES [device names]
• With no arguments, list every device with damage > 0 and its current damage value (H). With device names (prefix rule: SH T = shields, torpedo tubes; PH RA C = phasers, radio, computer) list those regardless of damage.
• Total ship damage is not reported here (H). Device names: WArp, IMpulse, Torpedoes, PHasers, SHields, COmputer, LIfe support, RAdio, TRactor (D, resolve ambiguity by unique prefix).
8.5 DOCK [STATUS [device names]]
• Requires an adjacent friendly starbase or captured friendly planet (H). Otherwise Not adjacent to a friendly base or planet.
• Per move, restores up to (H):
Resource
Base
Planet
Ship energy
+1000
+500
Shield energy
+500
+250
Photon torpedoes
+10
+5
Life support reserve
+5
+5
Ship damage
-100
-50
Ship damage if already docked
-200
-100
• Capped at maximums (5000, 2500, 10, 5, damage floor 0). If nothing needs restoring, DOCK has no effect (H).
• Sets condition green, docked = true (cleared by any movement). "Infinite" torpedo supply while docked (H): the torpedo count is refilled to 10 on every dock.
• PAUSE 2 to 4 s (H, depends on slowest terminal; Section 9.2).
• Optional STATUS [devices] suffix prints a status report after docking, equivalent to running STATUS with those arguments.
8.6 ENERGY <ship> <units>
• Receiving ship must be in an adjacent sector and friendly (H). Transfers units of ship energy. 10% is lost in broadcast (H): receiver gains 0.9 * units.
• If the receiver would exceed 5000, the transfer is automatically reduced to the maximum possible (H); the sender loses only the reduced amount (D, scaled so the 10% loss still holds).
• Both parties receive a message. Requires sender energy >= units (leaving at least 1) (D). No pause (D).
8.7 GRIPE
• Reads lines until ^Z (send) or ^C (abort) (H). Prepend a header with: version, date, time, ship name, user name, TTY speed, PPN (user id), TTY number (connection id), job number (session id), and flags for Romulans and black holes (H). Insert the new gripe at the top of DECWAR.GRP (H). DECWAR.FXD holds answered gripes (H).
• Protects the ship from attack unless red alert (Section 7.3). Cap a gripe at 4000 characters (D).
• Available in pre-game and in game.
8.8 HELP [* | keywords]
• Reads topics from DECWAR.HLP. Parser (H): a line beginning with . followed by a name starts a topic; the next line is the one-line title; a line containing only . ends the topic. Strip form feeds (0x0C). Topic name lookup uses prefix matching with the same 5-character rule.
• HELP alone prints the Introduction. HELP * prints the command list (the numbered list in the Introduction). HELP <command> prints that topic. Several keywords print several topics in order (e.g. H HI G prints HINTS then GRIPE).
• Topics present in the file: INTRODUCTION, INPUT, OUTPUT, PREGAME, HINTS, PAUSES, CTL-C, and one per command. The help refers to an ESCAPE topic that the file lacks; supply a short built-in one (D).
• Protects the ship unless red alert (Section 7.3). Works in pre-game.
8.9 IMPULSE [A|R] <vpos> <hpos>
• Moves exactly one sector vertically, horizontally or diagonally (warp factor 1) (H). Target must be an adjacent empty sector within the grid.
• Energy cost per warp-1 move (Section 8.12). Ship condition becomes green (H). Clears docked. Breaks tractor beam unless the other ship follows (Section 8.25).
• PAUSE 2 to 4 s (H).
• Refused if impulse engines are inoperative (>= 300).
8.10 MOVE [A|R|C] <vpos> <hpos> (warp)
• Warp factor w = Euclidean distance to the destination (one warp unit = one horizontal or vertical grid step; diagonal = hypotenuse) (H). Allowed 0 < w <= max_warp, where max_warp = 6, or 3 if warp engines are damaged (< 300) (H). Inoperative warp (>= 300): refused.
• The ship travels in a straight line, stepping sector by sector (Bresenham). If an obstacle (star, planet, base, ship, black hole) lies on the path, the ship stops in the last free sector before it (D). Entering a black hole = ship destroyed (H: "if displaced into one you're dead" applies to displacement; for navigation apply the same, D).
• Energy: cost = k_warp * w^2, doubled with shields up (H). Default k_warp = 10 (D). Warp 5 and 6 cause potential warp-engine damage (H): probability 5% (w in 5..6) and 15% (w = 6), damage U(20,120) units (D).
• COMPUTED mode: MOVE C W brings the ship adjacent to the Wolf's current position along the approach line ("ram" without collision) (H). Requires computer operational and a slow terminal (Section 8.0).
• Computer damage makes arrival inexact (H): displace the arrival point by a random 0..2 sectors, never into an occupied sector (D).
• Ship condition becomes green; if energy <= 1000, yellow and ring the bell after every move (H). docked cleared.
• PAUSE 2 to 4 s (H).
8.11 NEWS
Print DECWAR.NWS if present (H); otherwise print No news. Works in pre-game.
8.12 Energy cost reference (D)
Action
Ship energy
Warp move, factor w
10*w^2 (x2 shields up)
Impulse (w=1)
10 (x2 shields up)
Tractor-assisted move (towing ship)
3 x normal shields-down cost (H)
Phaser default blast
200; specific blasts 50..500 (H)
Phaser with shields up
+200 (H)
Raise shields
100 (H)
Capture of an enemy planet
50 per build (H)
Torpedo burst
0 (H)
8.13 PHASERS [A|R|C] [energy] <vpos> <hpos>
• One target per command (H). Energy argument, if present, must be 50..500 inclusive; default 200 (H). It precedes the coordinates.
• Range 10 sectors vertically, horizontally or diagonally (Chebyshev) (H). Not line of sight: obstacles are ignored (H).
• Hit size is inversely proportional to distance (H): hit = E * q / d, with q = 0.9 (D) at distance 1 and E the energy fired, then scaled by (1 - phaser_dmg/300) * (1 - computer_dmg/600) (D, "damage to phaser control or computer reduces strength", H). Distance d = Euclidean, minimum 1.
• Consumes E ship energy, plus 200 more if shields are up (high-speed shield control cycles them, H). Refused if insufficient.
• Friendly fire is cancelled by the weapons officer (H): print Weapons officer cancels phaser blast at friendly <target>; no energy consumed (D).
• Two banks, each with its own cooldown (H). If both are cooling, command waits or is refused with Phasers still cooling (D: refuse; the player can stack commands).
• Damage risk (H): about 5% at 200 and rising to about 65% at 500; fit P = 0.05 + 0.60*((E - 200)/300) for E>=200, 0.05 below (D). Severity scales with E (D: U(0.2,1.0) * E/2 units to the PHASER device, small chance of a second device).
• Sets red alert (H). Hits on bases and planets damage installations (builds) but can never destroy a planet (H).
• Cooldown per bank (H): slow_term_pause(3..6 s) + phaser_dmg/100 seconds. See Section 9.3.
• Target may be an enemy ship, enemy base, enemy or neutral planet, or the Romulan (revealing it).
8.14 POINTS [Me|I|Federation|Human|Empire|Klingon|Romulan|All]
Prints the score breakdown (H) with these categories and weights:
Category
Points (H unless D)
Damage to enemies
1 per 10 damage units dealt (D)
Enemies destroyed
500 each
Damage to bases
1 per 10 damage units (D)
Planets captured
100 each
Bases built
1000 each
Romulans damaged/destroyed
same rates as enemies (D)
Stars destroyed
-50 each
Planets destroyed
-100 each
Total points
sum of the above
Ships commissioned
count
Total / number of players
per-player average
Total / stardates
rate
Default is your own ship. PO KLI FED prints both teams. If the Romulan is not in the game, ignore the ROMULAN keyword (H). Available in pre-game.
8.15 QUIT
Section 4.4. Always works.
8.16 RADIO ON | OFf | Gag <ship> | Ungag <ship>
• ON/OFF toggles whether the player receives messages from ships and bases (H). A ship with radio off, or radio damage >= 300, cannot be TELLed (H).
• GAG suppresses all messages from a named ship; UNGAG restores them (H). State is per receiving ship.
• Base distress calls ("Base at 12-14 under attack") and destruction notices go to friendly radios that are on (H).
8.17 REPAIR [units]
• Removes units of damage from each damaged device, in addition to the normal repair rate of 30 units per turn (H). The "turn" is each completed time-consuming command; ticks every 5 seconds of real time otherwise (D).
• Default (no argument): same as RE 100 if docked, else RE 50 (H).
• PAUSE 0.08 * units seconds, halved if docked (H). RE 50 therefore takes 4 s in flight, 2 s docked.
• Does not reduce total ship damage (H). Can restore a device above 300 so it becomes operational. Resets life-support reserve if life support recovers (H).
• Ship condition unchanged.
8.18 SCAN / SRSCAN [Up|Down|Right|Left|Corner] [range | vr hr] [W]
• SCAN default range 10; SRSCAN default 7 (H). Maximum 10; larger values are clipped (H). One number means a square range; two numbers are vertical and horizontal ranges (e.g. SC 2 8 is 5 rows x 17 columns centered on the ship) (H).
• Direction keywords select a sub-rectangle relative to the ship (H): UP upper half, DOWN lower half, RIGHT right half, LEFT left half, CORNER the rectangle bounded by the ship and the given offset (e.g. SC C -5 -5 puts the ship in the upper-right corner of the scan). Ranges given with a negative sign apply for CORNER.
• W (WARNING) shows ! instead of . for empty sectors within range of an enemy base (4) or enemy/neutral planet (2) (H).
• Draw rows with the highest vpos at the top (D, consistent with "up" = positive vpos), columns with lowest hpos at left. Print column numbers across the top and row numbers on both sides. Clip to the grid; cells outside print blank.
• Symbols per Section 5.2. SET SCAN SHORT uses one-character symbols (H). The ship running the scan is shown by its own letter (D). Each scanned sector's contents become known (Section 6.1).
• Scan costs no energy (D) and has no pause; output volume may delay the next command (Section 3.7).
        28  29  30  31  32  33  34
  34     .   .   *   .   .   .   .   34
  33     .   @E  .   .  <>   .   .   33
  32     .   .   L   .   .   C   .   32
  31     *   .   .   .   .   .   .   31
        28  29  30  31  32  33  34
8.19 SET <keyword> <value>
Keyword
Values
Effect (H)
NAme
text
Changes the captain name shown in USERS (max 12)
Output
Long / Medium / Short
Sets message length (Section 10)
Scan
Long / Short
2-char or 1-char scan symbols
Prompt
Normal / Informative
COMMAND:   or >  with flags
OCdef
Absolute / Relative / Both
Output coordinate style
Icdef
Absolute / Relative
Default input coordinate type
Ttytype
CRT, ADM-3a, ADM-2, SOROC, BEEHIVE, ACT-IV, ACT-V
Accepted, does nothing (H)
Informative prompt: >  preceded by flags, in this order (H): S if shields are down or < 10%; E if ship energy < 1000 (yellow); D if ship damage > 2000; nL if life support is critically damaged with n stardates of reserve remaining. Example: SE> .
Settings are per session. DECWAR.INI (H, "Use the SET command in DECWAR.INI"): optionally replay a per-captain list of SET lines at entry. Store as profiles/<captain>.ini (D).
8.20 SHIELDS Up | Down | Transfer <energy>
• UP: costs 100 ship energy (H); fails if insufficient energy or shields inoperative. Raising cuts any tractor beam (H).
• DOWN: free (H).
• TRANSFER n: positive moves n units to shields, negative from shields (H); clamp to the available amounts and the 2500 and 5000 maximums.
• Shield display: +n% up, -n% down (H).
8.21 STATUS [Condition|Location|Torpedoes|Energy|Damage|Shields|Radio]
• Prints the current stardate plus the requested attributes (H): condition (green/yellow/red), location, torpedoes, ship energy, ship damage (total), shield energy, shield percent and up/down, radio on/off. No argument prints all. Prefix rule on attribute names (ST E D SH is valid).
8.22 TELL <recipients> ; <message>
• Recipients (H): All, FEderation/HUman, EMpire/Klingon, ENemy, FRiendly, or a comma-separated list of ship names (the Romulan is not addressable).
• Message is the text after the first ; on the line (H), up to 120 characters (D). The sender needs an operational radio and radio ON. Recipients with radio off, radio damage >= 300, or the sender gagged, are skipped (H).
• Delivered with a header, e.g. From Vulcan: Hello!. If no recipient could be reached, tell the sender No one received your message (D). Cannot be repeated with ESC (H). No range limitation (H). Case is preserved.
8.23 TIME
Print (H): time since the game started; time since the player's ship entered the game; run (CPU) time of the player's job so far this game; total run time since login; current time of day. For a modern server, use elapsed process CPU time attributed to the session (D) formatted as H:MM:SS.s. Works in pre-game.
8.24 TORPEDOES [A|R|C] n <v1> <h1> [<v2> <h2> [<v3> <h3>]]
• n = 1..3 torpedoes, aimed at one, two or three coordinates. With one coordinate all n go there. With several coordinates, each gets one torpedo and any surplus goes to the last coordinate (H: TO 3 12 24 13 39 fires one at 12-24 and two at 13-39). When n equals the number of coordinates, each gets one. Implement: counts = [1]*len(coords); counts[-1] += n - len(coords).
• COMPUTED form: TO C 3 BUZZARD fires three at that ship's current location.
• Requires torpedoes >= n and tubes operational (D). Uses no ship energy (H). Sets red alert (H).
• Reload time (H): n * (slow_term_pause(2..4 s) + torp_dmg/100) seconds before tubes can fire again.
• Details of flight and effects in Section 9.
8.25 TRACTOR <ship> | Off
• TRactor <ship>: both ships friendly, adjacent, both shields down (H); both tractor devices operational (D). Creates a link A <-> B where the caller is the towing (leading) ship.
• Either ship can pull the other by MOVE or IMPULSE: the towed ship ends the move trailing the lead ship (on the sector it vacated or the one before it along the path) (H). Towing ship pays 3x the normal shields-down movement energy (H).
• The link is cut automatically if either ship raises shields, either ship is hit by a torpedo or damaged by a nova (H), either ship docks, or they cease to be adjacent (D).
• TR or TR OFF breaks any link (H).
8.26 TYPE OPtion | OUtput
• TY OP prints version and date, Romulan included yes/no, black holes included yes/no (H).
• TY OU prints: SHORT/MEDIUM/LONG output; NORMAL or INFORMATIVE prompt; SHORT or LONG scans; ABSOLUTE or RELATIVE input default; ABSOLUTE/RELATIVE/BOTH output; current TTYTYPE (H).
8.27 USERS
Print each ship currently in the game with (H): ship name, captain (SET NAME), TTY speed, PPN, TTY number, job number. Medium output omits TTY and job numbers; Short output omits TTY speed and PPN as well (leaving ship and captain only). Pre-game listing shows only players who have entered the game (D).
9. Real-Time Rules and Combat Mechanics
9.1 Time base
• Real seconds drive pauses (H). Stardates drive life support and scoring rates: advance 0.1 stardate per 6 real seconds (i.e. 1.0 stardate per minute) (D). Stardate printed with 1 decimal in STATUS.
• Auto-repair "turn" = each completed timed command, or every 5 seconds for an idle ship (D).
9.2 Pause length and the slowest terminal (H)
Some pauses "are based on the speed of the slowest terminal in the game". Define slow_factor = clamp((9600 - min_speed_bps)/ (9600 - 300), 0, 1) over all in-game sessions (D), so 300 baud gives 1.0 and >= 9600 gives 0. Then for a range lo..hi: pause = lo + (hi - lo) * slow_factor.
Command
Pause (H)
BUILD
5 to 7 s
CAPTURE
5 s + 1 s per enemy build
DOCK
2 to 4 s
IMPULSE
2 to 4 s
MOVE
2 to 4 s
REPAIR
0.08 x units s (x0.5 if docked)
Phaser bank cooling
3 to 6 s + phaser_dmg/100 per bank
Torpedo reload
2 to 4 s + torp_dmg/100 per torpedo
The help's worked examples (reproduce in tests): 300 baud with 200 units of phaser damage gives 6 + 2 = 8 s per bank, so one shot per 4 s or two per 8 s; 300 baud, 200 torp-tube damage, 3 torpedoes gives 3 x (4 + 2) = 18 s.
9.3 Torpedo flight (H, mechanics D)
1. For each torpedo, compute a launch vector toward the target coordinate. Deflection adds a random angular error, sigma = base + a*shield_pct + b*computer_dmg + c*tube_dmg (H: "deflected by shield strength, computer and tube damage, and misfires"). Suggested: base 1 deg, a 0.03 deg per %, b 0.05 deg per unit, c 0.05 deg per unit. Computed fire reduces base to 0.5 deg (D).
2. Range: a torpedo travels 8 to 10 sectors before self-destructing (H: "minimum range 8, some travel 10"); choose U{8,9,10} per torpedo. Max aimable range is 10 (H).
3. Misfire: chance 2% + tube_dmg/1000. A misfire aborts the rest of the burst and may damage the tubes (H).
4. Step the torpedo sector by sector along its path. The first object encountered intercepts it (H): any ship, base, planet, star, or black hole.
5. Effects by object:
    ◦ Enemy ship: hit size ~300 +/- 30 (H examples: 285.3, 301.2), shield rules of Section 7.4, then displacement (below), red alert for the victim. Emit hit message (Section 10).
    ◦ Friendly ship, base or planet: "accidental" hit neutralized (H): no damage, notify firer.
    ◦ Enemy base: damages shield percent and damage; a base is destroyed when its damage reaches its capacity (D: 2500 + shields exhausted). Award 500 on destruction (D, treated as enemy destroyed).
    ◦ Planet: reduces builds; destroyed only when no enemy installations remain (H), i.e. builds == 0 and a further torpedo hits; the planet becomes empty space, -100 points (H), applies even to neutral planets.
    ◦ Star: the star becomes a nova: removed from grid, -50 points to the firer (H), and an explosion of ~300 units (H example: 301.2) hits every object within radius 3 (D), reduced by shields ("shield from the energy released when a star goes nova", H), with displacement. A tractor beam is broken for ships hit by a nova (H).
    ◦ Black hole: the torpedo vanishes (H).
    ◦ Nothing: at end of range the torpedo self-destructs.
6. Displacement (H example: Vulcan displaced from 22-31 to 20-31): on a ship hit by torpedo or nova, move the ship U{0,1,2} sectors away from the source along the line; stop before the grid edge or an occupied sector; if the final sector is a black hole, the ship is destroyed (H).
9.4 Phaser and weapon automata (H, parameters D)
• Starbase: immobile. Fires at any enemy ship within 4 sectors (H). Every 3 s, a phaser blast of ~400/d reduced by target shields ("overwhelming phaser power"). Emits a distress call to friendly radios when first hit (H). A damaged base slowly regenerates while not destroyed: +2 shield percent every 5 s, damage -20 every 5 s (H).
• Planet: fires at enemy ships within 2 sectors (H). Captured planets fire only at the enemy side; neutral planets fire at everyone (H). Power scales with builds: 100 + 60*builds per blast every 4 s (D). Resupply is half that of a base (H, see DOCK).
• A base or planet hit puts the target ship on red alert (H). Messages follow the same hit format (Section 10). A phaser from a base or planet has the "phaser hit" label (H example Emp planet(3) ... makes 155.5 unit phaser hit).
• Romulan: starts cloaked and wanders (random walk, up to warp 3). When a Federation or Empire ship or base (a "suitable target") is within 7 sectors, it attacks (H): alternating phaser (~300/d) and torpedo volleys with infinite energy and torpedoes (H). It re-cloaks after 20 s without a target (D). When destroyed, 1000 points to the killer's team (D); respawn rules in Section 5.3.
• Death: any ship at damage >= 2500 or energy <= 0 is destroyed, removed from the grid, kills credited to the last attacker (500 points, H), message <ship> destroyed, session returned to pre-game. If destroyed, a ship's captain is chalked up as a casualty (Section 4.4). Notify all friendly radios.
10. Output Formats (H)
10.1 Hit messages
Three lengths controlled by SET OUTPUT. Format template (fields in brackets):
LONG   : [Src] @[loc], [pct]%  makes [amt] unit [torpedo|phaser] hit on
         [Tgt] [displaced to [loc], [pct]%] @[loc], [pct]%
MEDIUM : [S] @[loc], [pct]%  [amt] unit [T|P]  [t] [-->loc, pct] @[loc], [pct]%
SHORT  : [S] [loc] [pct]  [amt][T|P]  [t] [>loc] [pct]
Worked examples to implement as test vectors (H):
Length
Output
Long
Goblin @22-31, +83.6%  makes 285.3 unit torpedo hit on Vulcan displaced to 20-31, +72.1%
Medium
G @22-31, +83.6%  285.3 unit T  V -->20-31, +72.1%
Short
G 22-31 +83  285T  V >20-31 +72
Long (planet)
Emp planet(3) @15-16 makes 155.5 unit phaser hit on Buzzard @15-17, 66.8%
Medium
-@3 @15-16  155.5 unit P  B @15-17, 66.8%
Short
-@3 15-16  155P  B 15-17 +66
Long (nova)
Star @22-31 +4,+2 makes 301.2 unit hit on Panther displaced to 20-31 +2,+2, -72.1%
Medium
* @22-31 +4,+2  301.2 unit N  P -->20-31 +2,+2, -72.1%
Short
* 22-31 +4,+2  301N P >20-31 +2,+2, -72
Notes: the sign prefix on a percentage is + shields up, - shields down. Short format truncates percentages to integers. -@3 is an Empire planet with 3 builds. Relative coordinates (+4,+2) appear only with SET OCDEF BOTH or RELATIVE. The planet/base name abbreviations in Medium and Short are -@n, +@n (Federation) and -<>/)( forms (D).
10.2 Other output rules
• Prompts, banners and bells per Sections 3 and 8. A bell is sent after every move when ship energy <= 1000 (H), and a series of bells when ^C aborts stacked commands (H).
• All output is plain 7-bit ASCII. No ANSI sequences. Keep lines <= 79 columns (D) except SCAN, which may reach 80 + labels.
11. Persistence and Files (D)
File
Purpose
DECWAR.HLP
Help text (the uploaded file, unmodified)
DECWAR.NWS
NEWS text
DECWAR.GRP
Pending gripes (newest at top)
DECWAR.FXD
Answered gripes
profiles/<captain>.ini
Per-captain SET lines (stand-in for DECWAR.INI)
In-memory only
Galaxy, scores, rejoin locks (a restart starts a new game)
12. Error and Edge-Case Rules (D)
• Any command that needs an operational device prints <Device> inoperative and has no other effect.
• A syntax error prints a one-line usage taken from the command's help syntax line and discards the rest of the stacked commands on that line (so a typo mid-line cannot fire a later TORPEDO).
• Unknown ship names print No such ship; ships not in the game print <Ship> is not in the game.
• Actions on out-of-range or invisible targets print Target out of range without costing energy.
• Never crash on bad input: all numeric parses are range-checked (integers within +/-10000), and lines over 255 characters are truncated.
13. Testing and Acceptance
1. Telnet: connect with telnet host 1701, nc host 1701, and a TinTin++ or MUD client. Verify no negotiation loops, correct 0xFF escaping, CR LF output.
2. Line editing: each key in Section 3.4, including ESC repeat at line start, ^G echo toggle, ^U, ^R, line terminators CR/LF/VT/FF/ESC/^Z.
3. Parsing: 5-character truncation (TORPE), commas/tabs as separators, / multi-commands, ; comments, TELL rescan, PH C B/M R 1 0 style lines.
4. Coordinates: absolute, relative and computed give identical targets in the help examples (R 1 -1 from 36-46 gives 37-45, etc.).
5. Timing: the worked pause examples in 9.2 (8 s, 18 s), REPAIR 4 s and DOCK limits.
6. Combat: shield percent math, red alert transitions, torpedo burst allocation (TO 3 12 24 13 39), friendly-fire neutralization, nova, displacement into black hole, planet destruction only when no builds.
7. Multiplayer: 10 concurrent clients, Romulan present for <= 3 players, not respawned after the 4th joins, team-imbalance forcing, 2-minute QUIT lock, rejoin to the old team.
8. Score: per-category POINTS output and win-condition reset.
9. Soak: 10 scripted bots for 1 hour with no deadlocks, no desync of the command queues, no unbounded memory growth.
Appendix A. Tunable Constants (D)
port=1701  max_connections=20  idle_pregame_s=1800
grid=75    stars=250  planets=60  bases_per_team=5  bases_max_total=10
blackhole_chance=0.25  blackholes=8
romulan_max_players=3  romulan_respawn_s=90  romulan_sight=7
ship_energy=5000 shield_energy=2500 ship_dmg_limit=2500 torps_max=10 life_reserve=5
yellow_energy=1000  device_inop=300  warp_k=10  max_warp=6  max_warp_damaged=3
phaser_default=200 phaser_min=50 phaser_max=500 phaser_range=10
torp_range_min=8 torp_range_max=10 torp_base_dmg=300
base_range=4 planet_range=2 base_fire_s=3 planet_fire_s=4
stardate_per_s=1/60  quit_lock_s=120  ctrl_c_hold_s=60
computed_for_all=false  slow_bps_threshold=1200
Appendix B. Items the Help File Leaves Open
• Exact damage, shield-absorption and phaser-falloff constants, base and planet firing cadence, and initial object counts (replaced by the tunables above).
• The meaning of "turn" for the 30-unit auto-repair (chosen as completed timed commands or 5 s).
• Whether CAPTURE/BUILD require adjacency (assumed yes, matching DOCK).
• Whether the 10-base cap is per team or galaxy-wide (assumed galaxy-wide, 5 each at start).
• Precise Romulan AI and cloaking rules.
• Exact error message text (all invented here and safe to change).

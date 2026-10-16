// Presentation only: the coloured terminal output. The decoding in main.go does
// not depend on any of it -- run with -color=false for the bare reference log.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	color = flag.Bool("color", isTerminal(), "colour the log (default on when stderr is a terminal and NO_COLOR is unset)")
	width = flag.Int("width", 80, "wrap coloured lines at this many columns (0 = no wrapping)")
)

// stamp is the width of the "15:04:05 " prefix log adds in colour mode.
const stamp = 9

// wrap joins words with sep into lines that fit -width. Continuation lines
// carry no log timestamp, so they are padded by its width and then indent. A
// line break keeps sep's non-space part (the comma between H02 fields) at the
// end of the line.
func wrap(first, indent, sep string, words []string) string {
	if *width <= 0 {
		return first + strings.Join(words, sep)
	}
	var b strings.Builder
	b.WriteString(first)
	n := stamp + visible(first)
	for i, w := range words {
		if i > 0 {
			if n+visible(sep)+visible(w) > *width {
				b.WriteString(strings.TrimRight(sep, " ") + "\n" + strings.Repeat(" ", stamp) + indent)
				n = stamp + visible(indent)
			} else {
				b.WriteString(sep)
				n += visible(sep)
			}
		}
		b.WriteString(w)
		n += visible(w)
	}
	return b.String()
}

// visible is the on-screen width of s: ANSI escapes take no room, and the
// emoji in the headlines take two columns.
func visible(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			esc = r != 'm'
		case r == '\x1b':
			esc = true
		case r >= 0x1f000:
			n += 2
		default:
			n++
		}
	}
	return n
}

func isTerminal() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// event is one decoded message, as the coloured headline shows it. Kind is
// login, pos or status.
type event struct {
	Proto, Kind, IMEI string
	Time              time.Time
	Lat, Lon          float64
	Speed             float64 // km/h
	Course            int
	Fix, Acc, SOS     bool
	Battery           string // "5/6" (GT06 level) or "100%" (H02 V8)
	Signal            int
}

const (
	reset   = "\x1b[0m"
	bold    = "\x1b[1m"
	dim     = "\x1b[2m"
	red     = "\x1b[31m"
	green   = "\x1b[32m"
	yellow  = "\x1b[33m"
	blue    = "\x1b[34m"
	magenta = "\x1b[35m"
	cyan    = "\x1b[36m"
	white   = "\x1b[97m"
	alarm   = "\x1b[1;97;41m"
)

func paint(c, s string) string {
	if !*color {
		return s
	}
	return c + s + reset
}

// Each device gets its own colour (ANSI 256-colour codes).
var palette = []int{39, 205, 214, 120, 141, 51, 209, 227}

var devices = struct {
	sync.Mutex
	idx map[string]int
	acc map[string]bool
}{idx: map[string]int{}, acc: map[string]bool{}}

func device(imei string) int {
	devices.Lock()
	defer devices.Unlock()
	i, ok := devices.idx[imei]
	if !ok {
		i = len(devices.idx) % len(palette)
		devices.idx[imei] = i
	}
	return i
}

func tag(imei string) string {
	if imei == "" {
		imei = "?"
	}
	if !*color {
		return imei
	}
	return fmt.Sprintf("\x1b[1;38;5;%dm%s%s", palette[device(imei)], imei, reset)
}

// show prints one decoded message. line is the full reference log line:
// printed as is in plain mode, dimmed under a headline in colour mode.
func show(e event, line string) {
	if !*color {
		log.Print(line)
		return
	}
	log.Print(wrap("", "     ", "  ", headline(e)))
	log.Print(paint(dim, wrap("       ", "       ", " ", strings.Fields(line))))
	if e.Kind != "login" {
		devices.Lock()
		prev, seen := devices.acc[e.IMEI]
		devices.acc[e.IMEI] = e.Acc
		devices.Unlock()
		if seen && prev != e.Acc {
			log.Print(paint(bold+yellow, "       🔑 IGNITION "+map[bool]string{true: "ON ", false: "OFF "}[e.Acc]) + tag(e.IMEI))
		}
	}
	if e.SOS {
		log.Print(paint(alarm, fmt.Sprintf("  🚨 SOS  %s  at %.5f, %.5f  ", e.IMEI, e.Lat, e.Lon)))
	}
}

// headline returns the parts of the headline, which show joins and wraps.
// Coordinates are cut to 5 decimals (about 1 m) and the time is UTC, as in the
// reference line below it, so a position fits in 80 columns.
func headline(e event) []string {
	who := fmt.Sprintf("%s %s", paint(bold, fmt.Sprintf("%-4s", e.Proto)), tag(e.IMEI))
	switch e.Kind {
	case "login":
		return []string{who, paint(bold+green, "🔑 login, session open")}
	case "status":
		return []string{who, "🔋 " + paint(yellow, e.Battery), fmt.Sprintf("📶 %d", e.Signal), accText(e.Acc)}
	}
	// Speed is padded to 3 digits, so one space before it is enough; "ACC on "
	// is padded to the width of "ACC off", so the time stays in one column.
	s := []string{who,
		"📍 " + paint(green, fmt.Sprintf("%.5f, %.5f", e.Lat, e.Lon)) + " " +
			paint(bold+yellow, fmt.Sprintf("%3.0f km/h", e.Speed)) + " " + arrow(e.Course),
		accText(e.Acc) + " " + paint(dim, e.Time.Format("15:04:05"))}
	if !e.Fix {
		s = append(s, paint(red, "no fix"))
	}
	if e.Battery != "" {
		s = append(s, "🔋 "+paint(yellow, e.Battery))
	}
	return s
}

func accText(on bool) string {
	if on {
		return paint(green, "ACC on ")
	}
	return paint(dim, "ACC off")
}

func arrow(course int) string {
	return string([]rune("↑↗→↘↓↙←↖")[(course+22)%360/45])
}

// legend explains the colours used in the raw frames below it.
func legend() {
	log.Print(paint(bold, "GT06 frame  ") + strings.Join([]string{
		paint(dim, "start"), paint(yellow, "length"), paint(bold+magenta, "protocol"),
		paint(bold+green, "imei"), paint(blue, "date"), paint(white, "sats"),
		paint(green, "lat"), paint(cyan, "lon"), paint(yellow, "speed"), paint(magenta, "course/status"),
		paint(bold+white, "serial"), paint(red, "crc"), paint(dim, "stop")}, " "))
}

// rawGT06 prints the frame with each field in its own colour, split on field
// boundaries, so the layout described in main.go can be read off the wire.
func rawGT06(m []byte) {
	if !*color {
		log.Printf("GT06 raw     %x", m)
		return
	}
	col, grp := make([]string, len(m)), make([]int, len(m))
	g := 0
	set := func(from, to int, c string) {
		g++
		for i := max(from, 0); i < to && i < len(m); i++ {
			col[i], grp[i] = c, g
		}
	}
	set(0, len(m), dim)
	if len(m) >= 4 {
		set(2, 3, yellow)
		set(3, 4, bold+magenta)
		switch m[3] {
		case 0x01:
			set(4, 12, bold+green)
		case 0x12, 0x16, 0x22:
			set(4, 10, blue)
			set(10, 11, white)
			set(11, 15, green)
			set(15, 19, cyan)
			set(19, 20, yellow)
			set(20, 22, magenta)
			set(22, int(m[2])-1, dim)
		case 0x13:
			set(4, 5, magenta)
			set(5, 6, yellow)
			set(6, 7, white)
			set(7, int(m[2])-1, dim)
		}
		l := int(m[2])
		set(l-1, l+1, bold+white)
		set(l+1, l+3, red)
		set(l+3, len(m), dim)
	}
	// One word per field group, so a wrapped frame breaks between fields.
	var words []string
	var b strings.Builder
	for i, x := range m {
		if i > 0 && grp[i] != grp[i-1] {
			words = append(words, b.String())
			b.Reset()
		}
		fmt.Fprintf(&b, "%s%02x%s", col[i], x, reset)
	}
	words = append(words, b.String())
	log.Print(wrap(paint(bold, "GT06")+" ← ", "       ", " ", words))
}

// showAck prints the reply the server sends back to a GT06 device.
func showAck(r []byte) {
	if !*color {
		log.Printf("GT06 ack     %x", r)
		return
	}
	log.Printf("%s → %s  %s", paint(bold, "GT06"), paint(dim, fmt.Sprintf("%x", r)),
		paint(dim, fmt.Sprintf("ack proto=%02x serial=%x crc=%x", r[3], r[4:6], r[6:8])))
}

// rawH02 prints the text message with each comma-separated field coloured.
func rawH02(s string) {
	if !*color {
		log.Printf("H02 raw %s", s)
		return
	}
	cols := []string{dim, bold + green, bold + magenta, blue, white, green, green, cyan, cyan, yellow, magenta, blue, bold + white}
	f := strings.Split(s, ",")
	for i := range f {
		c := dim
		if i < len(cols) {
			c = cols[i]
		}
		f[i] = c + f[i] + reset
	}
	log.Print(wrap(paint(bold, "H02 ")+" ← ", "       ", paint(dim, ","), f))
}

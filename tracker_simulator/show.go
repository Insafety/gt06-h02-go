// Presentation only: the coloured simulator log. Run with -color=false for the
// plain one.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var color = flag.Bool("color", isTerminal(), "colour the log (default on when stderr is a terminal and NO_COLOR is unset)")

func isTerminal() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	cyan   = "\x1b[36m"
	white  = "\x1b[97m"
	alarm  = "\x1b[1;97;41m"
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
}{idx: map[string]int{}}

func tag(id string) string {
	if !*color {
		return id
	}
	devices.Lock()
	i, ok := devices.idx[id]
	if !ok {
		i = len(devices.idx) % len(palette)
		devices.idx[id] = i
	}
	devices.Unlock()
	return fmt.Sprintf("\x1b[1;38;5;%dm%s%s", palette[i], id, reset)
}

var sent, acked atomic.Int64

func connected(name, addr string) {
	if *color {
		log.Printf("%s %s", tag(name), paint(green, "● connected to "+addr))
	}
}

func ended(name string) {
	if !*color {
		log.Printf("%s: end of session", name)
		return
	}
	log.Printf("%s %s", tag(name), paint(dim, "■ end of session"))
}

func summary() {
	if *color {
		log.Print(paint(bold, fmt.Sprintf("done: %d messages sent, %d acks received", sent.Load(), acked.Load())))
	}
}

// sentGT06 logs one frame written to the listener: what it is, how far into the
// session, and the frame itself, shortened.
func sentGT06(name, imei string, i, n int, b []byte) {
	sent.Add(1)
	if !*color {
		log.Printf("%s: %x", name, b)
		return
	}
	what, c := gt06Kind(b)
	h := hex.EncodeToString(b)
	if len(h) > 34 {
		h = h[:20] + "…" + h[len(h)-12:]
	}
	log.Printf("%s %s %s  %s  %s", paint(bold, "GT06"), tag(imei), counter(i, n), paint(c, fmt.Sprintf("%-8s", what)), paint(dim, h))
}

func gt06Kind(b []byte) (string, string) {
	if len(b) < 4 {
		return "short", red
	}
	switch b[3] {
	case 0x01:
		return "login", bold + green
	case 0x12, 0x22:
		return "position", cyan
	case 0x13:
		return "status", yellow
	case 0x16:
		return "alarm", alarm
	}
	return fmt.Sprintf("proto %02x", b[3]), white
}

// sentH02 logs one text message. H02 has no ack, so this is all there is.
func sentH02(imei string, i, n int, l string) {
	sent.Add(1)
	if !*color {
		log.Printf("%s: %s", imei, l)
		return
	}
	f := strings.Split(strings.TrimRight(l, "#"), ",")
	what, c := "message", white
	if len(f) > 2 {
		what, c = f[2], cyan
	}
	// Status word is active-low: bit 1 clear means the SOS button is pressed.
	if len(f) > 12 {
		if st, err := strconv.ParseUint(f[12], 16, 32); err == nil && st&(1<<1) == 0 {
			what, c = f[2]+" SOS", alarm
		}
	}
	text := l
	if len(f) > 9 {
		text = strings.Join(f[3:10], ",") + ",…"
	}
	log.Printf("%s %s %s  %s  %s", paint(bold, "H02 "), tag(imei), counter(i, n), paint(c, fmt.Sprintf("%-8s", what)), paint(dim, text))
}

func counter(i, n int) string {
	return paint(dim, fmt.Sprintf("▶ %*d/%d", len(strconv.Itoa(n)), i, n))
}

// readAcks logs the listener's replies to a GT06 device:
// 78 78 05 | protocol | serial(2) | crc(2) | 0d 0a.
func readAcks(c net.Conn, imei string) {
	buf := make([]byte, 256)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		for m := buf[:n]; len(m) >= 10 && m[0] == 0x78 && m[1] == 0x78; m = m[10:] {
			acked.Add(1)
			if !*color {
				log.Printf("%s: ack %x", imei, m[:10])
				continue
			}
			what, _ := gt06Kind(m)
			log.Printf("%s %s %s", paint(bold, "GT06"), tag(imei),
				paint(green, fmt.Sprintf("◀ ack  %-8s serial %x", what, m[4:6])))
		}
	}
}

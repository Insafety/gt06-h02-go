package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"io"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	dir   = flag.String("dir", "tracker_simulator/samples", "capture directory")
	host  = flag.String("host", "localhost", "listener host")
	delay = flag.Duration("delay", 3*time.Second, "delay between messages")
	real  = flag.Bool("real", false, "replay the GT06 sessions on their recorded timeline instead of -delay")
	speed = flag.Float64("speed", 1, "with -real, play the recording this many times faster")
	pGT06 = flag.String("gt06", "5023", "GT06 port")
	pH02  = flag.String("h02", "5024", "H02 port")
	move  = flag.Float64("move", 0, "km/h to drive the devices instead of replaying their coordinates")
	merge = flag.Bool("merge", false, "send messages two per write, as one TCP segment")
	split = flag.Bool("split", false, "send each write in two halves, 200ms apart")
)

func main() {
	flag.Parse()
	if *color {
		log.SetFlags(log.Ltime)
	}
	var wg sync.WaitGroup

	files, _ := filepath.Glob(filepath.Join(*dir, "log_gt06", "*", "*.txt"))
	// With -real all sessions share one clock: time zero is the earliest
	// recorded frame, and each session dials when it really started. A session
	// file is one connection, so a device reconnects between its sessions,
	// after the recorded gap, instead of holding several connections at once.
	t0 := int64(0)
	if *real {
		t0 = firstStamp(files)
		if *speed <= 0 {
			*speed = 1
		}
	}
	start := time.Now()
	for _, f := range files {
		wg.Add(1)
		go func(f string) { defer wg.Done(); gt06(f, start, t0) }(f)
	}

	for imei, lines := range h02Devices(filepath.Join(*dir, "h02-log.txt")) {
		wg.Add(1)
		go func(imei string, lines []string) { defer wg.Done(); h02(imei, lines) }(imei, lines)
	}

	if len(files) == 0 {
		log.Printf("no GT06 sessions in %s", filepath.Join(*dir, "log_gt06"))
	}
	wg.Wait()
	summary()
}

// stampOf reads the unix-ms timestamp at the start of a GT06 sample line.
func stampOf(l string) (int64, bool) {
	stamp, _, ok := strings.Cut(l, " ")
	if !ok {
		return 0, false
	}
	ms, err := strconv.ParseInt(stamp, 10, 64)
	return ms, err == nil
}

// firstStamp is the earliest first-line timestamp across the session files.
func firstStamp(files []string) int64 {
	t0 := int64(0)
	for _, f := range files {
		if lines := readLines(f); len(lines) > 0 {
			if ms, ok := stampOf(lines[0]); ok && (t0 == 0 || ms < t0) {
				t0 = ms
			}
		}
	}
	return t0
}

func gt06(path string, start time.Time, t0 int64) {
	lines := readLines(path)
	if len(lines) == 0 {
		return
	}
	imei := filepath.Base(filepath.Dir(path))
	name := imei + "/" + filepath.Base(path)

	// at is when a frame recorded at ms is due, on the shared -real clock.
	at := func(ms int64) time.Time {
		return start.Add(time.Duration(float64(ms-t0) / *speed * float64(time.Millisecond)))
	}
	if *real {
		if ms, ok := stampOf(lines[0]); ok {
			time.Sleep(time.Until(at(ms)))
		}
	}

	c := dial(*pGT06, imei)
	if c == nil {
		return
	}
	defer c.Close()
	go readAcks(c, imei)
	out := &stream{c: c}

	trip := &route{}
	for i, l := range lines {
		_, raw, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		b, err := hex.DecodeString(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		if *move > 0 {
			b = trip.apply(b, *delay)
		}

		if ms, ok := stampOf(l); *real && ok {
			time.Sleep(time.Until(at(ms)))
		} else {
			time.Sleep(*delay)
		}

		if err := out.send(b, i == len(lines)-1); err != nil {
			log.Printf("%s: %v", name, err)
			return
		}
		sentGT06(name, imei, i+1, len(lines), b)
	}
	// Give the last ack time to arrive before the connection closes.
	time.Sleep(300 * time.Millisecond)
	ended(imei)
}

// stream writes messages to the listener, one per write by default. -merge
// holds every other message back and sends it glued to the next one; -split
// writes each in two halves a moment apart. Real mobile links do both, and the
// listener has to put the frames back together.
type stream struct {
	c    net.Conn
	held []byte
}

func (s *stream) send(b []byte, last bool) error {
	if *merge && s.held == nil && !last {
		s.held = append([]byte(nil), b...)
		return nil
	}
	b = append(s.held, b...)
	s.held = nil
	if *split && len(b) > 1 {
		if _, err := s.c.Write(b[:len(b)/2]); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond)
		b = b[len(b)/2:]
	}
	_, err := s.c.Write(b)
	return err
}

type route struct {
	lat, lon, course float64
	set              bool
}

func (r *route) apply(b []byte, step time.Duration) []byte {
	if len(b) < 26 || b[0] != 0x78 || (b[3] != 0x12 && b[3] != 0x16) {
		return b
	}
	if !r.set {
		r.lat = float64(binary.BigEndian.Uint32(b[11:15])) / 1800000
		r.lon = float64(binary.BigEndian.Uint32(b[15:19])) / 1800000
		r.course = 45
		r.set = true
	} else {
		d := *move * step.Hours()
		r.course = math.Mod(r.course+7, 360)
		rad := r.course * math.Pi / 180
		r.lat += d * math.Cos(rad) / 111.32
		r.lon += d * math.Sin(rad) / (111.32 * math.Cos(r.lat*math.Pi/180))
	}

	out := make([]byte, len(b))
	copy(out, b)
	binary.BigEndian.PutUint32(out[11:15], uint32(math.Abs(r.lat)*1800000))
	binary.BigEndian.PutUint32(out[15:19], uint32(math.Abs(r.lon)*1800000))
	out[19] = byte(*move)
	cs := uint16(0x1400) | uint16(r.course)
	if r.lon < 0 {
		cs |= 0x800
	}
	binary.BigEndian.PutUint16(out[20:22], cs)
	binary.BigEndian.PutUint16(out[len(out)-4:len(out)-2], crc16(out[2:len(out)-4]))
	return out
}

func crc16(b []byte) uint16 {
	crc := uint16(0xffff)
	for _, x := range b {
		crc ^= uint16(x)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0x8408
			} else {
				crc >>= 1
			}
		}
	}
	return ^crc
}

func h02(imei string, lines []string) {
	c := dial(*pH02, imei)
	if c == nil {
		return
	}
	defer c.Close()
	go io.Copy(io.Discard, c) // H02 devices get no reply
	out := &stream{c: c}

	for i, l := range lines {
		time.Sleep(*delay)
		if err := out.send([]byte(l), i == len(lines)-1); err != nil {
			log.Printf("%s: %v", imei, err)
			return
		}
		sentH02(imei, i+1, len(lines), l)
	}
	ended(imei)
}

func h02Devices(path string) map[string][]string {
	out := map[string][]string{}
	for _, l := range readLines(path) {
		f := strings.Split(l, ",")
		if len(f) < 3 || f[0] != "*HQ" {
			continue
		}
		out[f[1]] = append(out[f[1]], l)
	}
	return out
}

func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		log.Println(err)
		return nil
	}
	defer f.Close()

	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if l := strings.TrimSpace(s.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func dial(port, name string) net.Conn {
	addr := net.JoinHostPort(*host, port)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("%s: %v", name, err)
		return nil
	}
	connected(name, addr)
	return c
}

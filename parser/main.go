// Demo listener for GT06 (binary, port 5023) and H02 (text, port 5024). It
// only decodes and prints: no database, no storage, no log files. The GT06 side
// still acknowledges, because a GT06 device re-sends a packet until the serial
// it gets back matches.
//
// The comments below record what real devices do on the wire, as observed on
// real trackers, including the places where they differ from the protocol
// documents.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

func main() {
	flag.Parse()
	if *color {
		log.SetFlags(log.Ltime)
		legend()
	}
	// ":5023" binds to every IP the host has. To listen on one address only,
	// name it: "203.0.113.10:5023", or "127.0.0.1:5023" for local tests.
	go listen(":5023", gt06, splitGT06)
	listen(":5024", h02, splitH02)
}

// listen builds one decoder per accepted connection, because GT06 identity lives
// in the connection and not in the packet (see the session note below) -- the
// decoder returned by gt06 holds that session's IMEI and ACC state.
//
// TCP delivers a byte stream, not messages: one read can hold two frames (a
// tracker flushing positions it stored while out of coverage) or half of one
// (a slow mobile link). So the stream is cut into whole frames first, by the
// protocol's split function, and only those reach the decoder -- which can then
// index fixed offsets safely. bufio.Scanner keeps any unfinished bytes between
// reads.
func listen(addr string, newDecoder func(net.Conn) func([]byte), split bufio.SplitFunc) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("listening on", addr)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		go func() {
			defer c.Close()
			log.Println("connected", c.RemoteAddr())
			// New session: a reconnecting device logs in again, and its old
			// IMEI must not leak into the new connection.
			decode := newDecoder(c)
			s := bufio.NewScanner(c)
			s.Split(split)
			for s.Scan() {
				decode(s.Bytes())
			}
			if err := s.Err(); err != nil {
				log.Println("closed", c.RemoteAddr(), err)
			} else {
				log.Println("closed", c.RemoteAddr(), "EOF")
			}
		}()
	}
}

// splitGT06 cuts the stream into frames: 78 78, a length byte, then length+3
// more bytes up to and including 0d 0a -- length+5 in all. 79 79 frames carry
// a two-byte length instead (length+6). Bytes before a start marker are skipped,
// and a frame whose declared length does not land on 0d 0a is dropped from its
// marker on, so one corrupt frame costs only itself, not the rest of the
// connection.
func splitGT06(data []byte, atEOF bool) (int, []byte, error) {
	start := -1
	for i := 0; i+1 < len(data); i++ {
		if (data[i] == 0x78 || data[i] == 0x79) && data[i+1] == data[i] {
			start = i
			break
		}
	}
	switch {
	case start > 0:
		log.Printf("GT06 skipped %x", data[:start])
		return start, nil, nil
	case start < 0 && len(data) > 1:
		// No marker anywhere: drop all but the last byte, which may be the
		// first half of one.
		log.Printf("GT06 skipped %x", data[:len(data)-1])
		return len(data) - 1, nil, nil
	case start < 0:
		return 0, nil, nil
	}
	n := 0
	if data[0] == 0x78 {
		if len(data) < 3 {
			return 0, nil, nil
		}
		n = int(data[2]) + 5
	} else {
		if len(data) < 4 {
			return 0, nil, nil
		}
		n = int(binary.BigEndian.Uint16(data[2:4])) + 6
	}
	if len(data) < n {
		return 0, nil, nil // frame not complete yet
	}
	if data[n-2] != 0x0d || data[n-1] != 0x0a {
		log.Printf("GT06 bad length %x, resyncing", data[:n])
		return 2, nil, nil
	}
	return n, data[:n], nil
}

// splitH02 cuts the stream on '#'. Anything before the '*' that starts a
// message -- usually the \r\n some devices append -- is skipped. (H02 also has
// a binary form starting with '$'; this demo decodes only the text one.)
func splitH02(data []byte, atEOF bool) (int, []byte, error) {
	start := bytes.IndexByte(data, '*')
	if start < 0 {
		return len(data), nil, nil
	}
	if start > 0 {
		if junk := bytes.TrimSpace(data[:start]); len(junk) > 0 {
			log.Printf("H02 skipped %q", junk)
		}
		return start, nil, nil
	}
	end := bytes.IndexByte(data, '#')
	if end < 0 {
		return 0, nil, nil // message not complete yet
	}
	return end + 1, data[:end+1], nil
}

// GT06 frame: 78 78 | length | protocol | body | serial(2) | crc(2) | 0d 0a.
// The length byte counts everything from the protocol number to the CRC, so the
// serial sits at m[length-1:length+1] and the whole frame is length+5 bytes.
// (Frames starting 79 79 carry a two-byte length; this demo does not decode
// them, so they land in the "unknown" branch.)
//
// The session dance -- the one thing that makes GT06 harder than H02: the device
// opens the TCP connection, sends a 0x01 login carrying its IMEI, waits for the
// ack, and only then starts sending positions, alarms and status packets. NONE of
// those later packets carry the IMEI. The connection itself is the identity, so
// the IMEI from the login is kept in a per-connection variable and attached to
// everything that follows (that is also why the ACC state read from a 0x13 has
// to be remembered per session: the next position cannot repeat it).
// Consequences worth knowing:
//   - Miss the login, restart the listener mid-session, or read a position out of
//     a connection whose login was never seen, and the position is anonymous --
//     park it as unknown rather than guessing whose it is.
//   - A device that drops its GPRS link and reconnects logs in again, so the same
//     IMEI can own several connections in a row, each with its own serial run.
//   - The IMEI arrives as 8 BCD bytes, i.e. 16 hex digits for 15 digits of IMEI,
//     the first nibble being padding -- so it is the string [1:] of the hex.
//
// That dance is exactly why gt06 is written as a decoder factory: the two session
// variables below are all the state a valid position needs -- the IMEI to say whose
// it is, and the last known ACC so a position from a device that reports ignition
// only in its status packet still carries it.
func gt06(c net.Conn) func([]byte) {
	imei, acc := "", false

	return func(m []byte) {
		rawGT06(m)
		if len(m) < 10 || m[0] != 0x78 || m[1] != 0x78 {
			log.Println("GT06 unknown")
			return
		}
		switch m[3] {
		case 0x01:
			// Login: the only packet on the whole connection carrying the IMEI (8 BCD
			// bytes, leading pad nibble, hence the [1:]). It must be acked before the
			// device will send anything else -- see the session note above the function.
			imei = hex.EncodeToString(m[4:12])[1:]
			show(event{Proto: "GT06", Kind: "login", IMEI: imei},
				fmt.Sprintf("GT06 login   imei=%s", imei))
		case 0x12, 0x16, 0x22: // position / alarm / small-tracker position
			if len(m) < 22 {
				return
			}
			// Coordinates are minutes x 30000, so /30000/60 = /1800000. Both the
			// 26- and 33-byte content variants of 0x12 keep these offsets (verified
			// on the EC33), and 0x16 mirrors 0x12 up to here as well.
			lat := float64(binary.BigEndian.Uint32(m[11:15])) / 1800000
			lon := float64(binary.BigEndian.Uint32(m[15:19])) / 1800000
			// Course/status word: bits 0-9 heading, bit 10 north, bit 11 west,
			// bit 12 "GPS fixed". On the EC33, bit 14 also mirrors ACC (verified by
			// switching the ignition wire); some protocol documents name bit 15
			// instead, which always read 0 on our devices. Traccar reads bit 14 as
			// "ACC valid" and bit 15 as the value, which would show the EC33 with
			// its ignition always off. Not read here, because the status packet
			// below is the one place every model reports ACC.
			cs := binary.BigEndian.Uint16(m[20:22])
			// 0x22 sends the date as BCD and its north bit is unreliable on the
			// devices we have seen, so its latitude is left positive. Traccar reads
			// 0x22 like 0x12 (plain-byte date, north bit honoured), so models
			// differ here: check the date and sign against your own device.
			if m[3] != 0x22 && cs&0x400 == 0 {
				lat = -lat
			}
			if cs&0x800 != 0 {
				lon = -lon
			}
			// fixed=0 frames are printed like any other. Some GT06 models keep
			// sending stale-but-plausible coordinates while unfixed, which is where
			// phantom trip distance comes from; the EC33 instead stays silent and
			// sends only heartbeats (~3 min) until it has a fix.
			fix := cs&0x1000 != 0
			// The 38-byte 0x12 (length 0x26) puts the ACC byte right after the LBS
			// block (MCC 2, MNC 1, LAC 2, CellID 3). The shorter variant has no such
			// field, so it leaves acc at whatever the last 0x13 status packet set.
			if m[3] == 0x12 && m[2] == 0x26 && len(m) >= 38 {
				acc = m[30] == 1
			}
			sos := ""
			// 0x16 is one byte longer than the 0x12 it mirrors (an LBS length byte
			// precedes the cell block, and terminal info, voltage, GSM, alarm cause
			// and language follow it), so the alarm cause is found from the declared
			// length rather than by walking the variable-length LBS block:
			// length+5 frame - 2 stop - 2 crc - 2 serial - 1 language = m[2]-3.
			// Cause 0x01 is the SOS button -- the only cause treated as SOS here.
			// Cause 0x13 is "device removed" and is deliberately not treated as SOS.
			if m[3] == 0x16 && len(m) >= int(m[2])+5 {
				if a := m[int(m[2])-3]; a == 0x01 {
					sos = " SOS"
				} else {
					sos = fmt.Sprintf(" alarm=%02x", a)
				}
			}
			// The LBS block follows the course/status word: MCC 2 bytes, MNC 1, LAC 2,
			// CellID 3. Labelled for 0x12 only, where those offsets are verified (the
			// ACC byte at m[30] sits exactly one byte past them). 0x16 puts an LBS
			// length byte in front of the block and 0x22 is another model's layout,
			// so for those the same bytes are only shown raw.
			lbs := ""
			if m[3] == 0x12 && len(m) >= 30 {
				lbs = fmt.Sprintf(" mcc=%d mnc=%d lac=%d cell=%d", binary.BigEndian.Uint16(m[22:24]), m[24],
					binary.BigEndian.Uint16(m[25:27]), uint32(m[27])<<16|uint32(m[28])<<8|uint32(m[29]))
			}
			// Everything between the course/status word and the serial, raw: the LBS
			// block, and on 0x16 the terminal info, voltage, GSM level, alarm cause and
			// language that follow it. Shown by offset rather than guessed at, for the
			// same reason the H02 tail is.
			tail, serial := "", ""
			if i := int(m[2]) - 1; i > 22 && i+1 < len(m) {
				tail, serial = hex.EncodeToString(m[22:i]), hex.EncodeToString(m[i:i+2])
			}
			// m[10]: low nibble satellite count, high nibble GPS info length.
			// The IMEI comes from this session's login, not from the packet. An empty
			// one means the login was never seen.
			// The serial is what the ack must echo; it counts up per connection, so a
			// gap in it is a packet the listener never received.
			t := gt06Time(m[4:10], m[3] == 0x22)
			show(event{Proto: "GT06", Kind: "pos", IMEI: imei, Time: t, Lat: lat, Lon: lon, Speed: float64(m[19]),
				Course: int(cs & 0x3ff), Fix: fix, Acc: acc, SOS: sos == " SOS"},
				fmt.Sprintf("GT06 pos %02x imei=%q %s lat=%.6f lon=%.6f speed=%d course=%d sats=%d gpsinfo=%d fix=%t acc=%t status=%04x len=%d serial=%s%s%s | %s",
					m[3], imei, t, lat, lon, m[19], cs&0x3ff, m[10]&0x0f, m[10]>>4,
					fix, acc, cs, m[2], serial, lbs, sos, tail))
		case 0x13:
			// Status/heartbeat -- the only packet with battery, and the only place
			// every GT06 build reports ACC. It carries no timestamp of its own.
			// m[4] terminal information: bit 0 defense, 1 ACC, 2 charging, 3-5 alarm,
			// 6 GPS tracking, 7 oil/electricity cut-off. Verified live on the EC33:
			// the byte went 0x46 -> 0x44 the moment the ACC wire was pulled.
			// m[5] is a 0..6 level, NOT a percent. m[6] is GSM signal strength.
			if len(m) < 15 {
				return
			}
			acc = m[4]&0x02 != 0
			// Body is exactly five bytes: terminal info, voltage level, GSM signal,
			// alarm, language. The terminal byte is printed raw as well, because its
			// bits are what the EC33 wiring was verified against.
			show(event{Proto: "GT06", Kind: "status", IMEI: imei, Battery: fmt.Sprintf("%d/6", m[5]), Signal: int(m[6]), Acc: acc},
				fmt.Sprintf("GT06 status  imei=%q battery=%d/6 signal=%d acc=%t terminal=%02x alarm=%02x lang=%02x serial=%s",
					imei, m[5], m[6], acc, m[4], m[7], m[8],
					hex.EncodeToString(m[int(m[2])-1:int(m[2])+1])))
		default:
			// e.g. 0x8A time sync, 0x94 information transfer, 0x80 server command
			// reply -- all acked below so the device does not retry them.
			log.Printf("GT06 proto %02x, not decoded", m[3])
		}
		ack(c, m)
	}
}

// gt06Time reads the 6-byte yy mm dd hh mm ss date. 0x12/0x16 send plain bytes;
// 0x22 sends BCD, i.e. 0x25 means 2025 and reading it as a byte gives 37.
func gt06Time(b []byte, bcd bool) time.Time {
	v := func(x byte) int {
		if bcd {
			return int(x>>4)*10 + int(x&0x0f)
		}
		return int(x)
	}
	// Device clocks are UTC; nothing in the frame says otherwise.
	return time.Date(2000+v(b[0]), time.Month(v(b[1])), v(b[2]), v(b[3]), v(b[4]), v(b[5]), 0, time.UTC)
}

// ack answers with the same protocol number and the frame's own serial, which
// is the two bytes before the CRC -- so its offset follows the declared length
// and is not fixed at 12:14 (that only happens to be right for the login; any
// other packet would get a wrong serial, and the device keeps re-sending it).
func ack(c net.Conn, m []byte) {
	s := int(m[2]) - 1
	if s < 4 || s+1 >= len(m) {
		return
	}
	r := []byte{0x78, 0x78, 0x05, m[3], m[s], m[s+1], 0, 0, 0x0d, 0x0a}
	binary.BigEndian.PutUint16(r[6:8], crc16ITU(r[2:6]))
	c.Write(r)
	showAck(r)
}

// crc16ITU is CRC-16/X-25 (reflected poly 0x8408, init 0xFFFF, final complement)
// over the length byte through the serial -- start and stop bytes excluded. Done
// bitwise here; a 256-entry lookup table gives the same result faster. A device
// silently ignores an ack whose CRC is wrong, so it looks exactly like no ack.
func crc16ITU(b []byte) uint16 {
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

// H02 (*HQ) is plain text, comma separated, terminated with '#':
//
//	*HQ,imei,type,hhmmss,A|V,ddmm.mmmm,N|S,dddmm.mmmm,E|W,speed,course,ddmmyy,status[,...]#
//
// Unlike GT06 there is no login: every message carries the device ID, so a
// session holds no state. V1 is the basic position, V8 the extended one (more
// trailing fields, including satellites and battery percent); other types such
// as heartbeats and link messages share the first fields but not the rest.
// Nothing is acked -- these devices do not expect a reply, and there is no
// session to keep: the decoder is stateless, unlike the GT06 one above.
func h02(net.Conn) func([]byte) {
	return h02Decode
}

func h02Decode(m []byte) {
	rawH02(strings.TrimRight(string(m), "\r\n"))
	f := strings.Split(strings.TrimRight(string(m), "#\r\n"), ",")
	if len(f) < 13 || f[0] != "*HQ" {
		log.Println("H02 unknown")
		return
	}
	// 'A' = fix, 'V' = none. A 'V' record still carries coordinates, but stale
	// ones; a listener that stores positions should usually skip them. Here they
	// are decoded like any other so the difference is visible on the wire.
	fix := f[4] == "A"
	// Date and time are separate fields, both UTC, day-first: ddmmyy + hhmmss.
	t, _ := time.Parse("020106150405", f[11]+f[3])
	// Coordinates are NMEA-style ddmm.mmmm / dddmm.mmmm -- two degree digits for
	// latitude, three for longitude; hemisphere is a separate field.
	lat, lon := dm(f[5], 2), dm(f[7], 3)
	if f[6] == "S" {
		lat = -lat
	}
	if f[8] == "W" {
		lon = -lon
	}
	speed, _ := strconv.ParseFloat(f[9], 64) // knots, hence the 1.852 below
	course, _ := strconv.ParseFloat(f[10], 64)
	// The status word is 8 hex digits and ACTIVE-LOW: a bit reads 0 while its
	// condition holds, which is why an ordinary position is nearly all f's.
	// Bit 1 clear = SOS button pressed (verified live). Bit 18 (0x40000) is the
	// ACC line: set = ignition off. One button press can arrive twice, so
	// deduplicate before raising alerts.
	st, _ := strconv.ParseUint(f[12], 16, 32)
	sos := ""
	if st&(1<<1) == 0 {
		sos = " SOS"
	}
	bat, battery := "", ""
	// Battery percent rides on V8 only, in the last field -- so it still carries
	// the '#' (trimmed above). Reading a fixed two characters made 100% read as
	// 10. V1-only devices never report a charge.
	if f[2] == "V8" && len(f) >= 21 {
		bat, battery = fmt.Sprintf(" battery=%s%%", f[20]), f[20]+"%"
	}
	// Everything past the status word is model-specific -- LBS cell data,
	// satellites, IO -- and devices do not agree on the order. So the tail is
	// printed verbatim, by index, rather than guessed at: that is how the right
	// index gets confirmed against a real device.
	tail := ""
	for i, v := range f[13:] {
		tail += fmt.Sprintf(" f%d=%s", i+13, v)
	}
	show(event{Proto: "H02", Kind: "pos", IMEI: f[1], Time: t, Lat: lat, Lon: lon, Speed: speed * 1.852,
		Course: int(course), Fix: fix, Acc: st&(1<<18) == 0, SOS: sos != "", Battery: battery},
		fmt.Sprintf("H02 %s imei=%s %s lat=%.6f lon=%.6f speed=%.1f course=%.0f fix=%t ign=%t status=%s%s%s |%s",
			f[2], f[1], t, lat, lon, speed*1.852, course, fix, st&(1<<18) == 0, f[12], bat, sos, tail))
}

// dm converts ddmm.mmmm (deg = width of the degree part) to decimal degrees.
func dm(s string, deg int) float64 {
	if len(s) < deg {
		return 0
	}
	d, _ := strconv.ParseFloat(s[:deg], 64)
	m, _ := strconv.ParseFloat(s[deg:], 64)
	return d + m/60
}

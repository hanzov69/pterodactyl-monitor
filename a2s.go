package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// a2sInfo sends a Steam server query (A2S_INFO, the request the in-game
// server browser makes) and returns a short description of the reply. It
// needs no credentials, and a server that answers it is joinable.
func a2sInfo(addr string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	base := append([]byte{0xFF, 0xFF, 0xFF, 0xFF, 'T'}, "Source Engine Query\x00"...)
	req := base
	buf := make([]byte, 4096)
	deadline := time.Now().Add(timeout)
	var lastErr error = errors.New("timed out")
	for attempt := 0; attempt < 6 && time.Now().Before(deadline); attempt++ {
		if _, err := conn.Write(req); err != nil {
			return "", err
		}
		wait := time.Now().Add(timeout / 3) // UDP: resend a couple of times before giving up
		if wait.After(deadline) {
			wait = deadline
		}
		conn.SetReadDeadline(wait)
		n, err := conn.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				lastErr = errors.New("timed out")
				continue
			}
			return "", err
		}
		b := buf[:n]
		if n < 5 {
			lastErr = errors.New("malformed reply")
			continue
		}
		switch header := binary.LittleEndian.Uint32(b); {
		case header == 0xFFFFFFFE: // split reply; it is answering, which is all we need
			return "responding to queries", nil
		case header != 0xFFFFFFFF:
			lastErr = errors.New("malformed reply")
		case b[4] == 'A' && n >= 9: // challenge: repeat the request with the number it gave us
			req = append(append([]byte(nil), base...), b[5:9]...)
		case b[4] == 'I':
			return parseA2SInfo(b[5:]), nil
		case b[4] == 'm': // legacy GoldSource reply
			return "responding to queries", nil
		default:
			lastErr = fmt.Errorf("unexpected reply type 0x%02x", b[4])
		}
	}
	return "", lastErr
}

func parseA2SInfo(r []byte) string {
	cstr := func() string {
		i := bytes.IndexByte(r, 0)
		if i < 0 {
			s := string(r)
			r = nil
			return s
		}
		s := string(r[:i])
		r = r[i+1:]
		return s
	}
	skip := func(n int) {
		if len(r) < n {
			n = len(r)
		}
		r = r[n:]
	}
	skip(1) // protocol
	cstr()  // server name
	mapName := cstr()
	cstr() // folder
	cstr() // game
	if len(r) < 9 {
		return "responding to queries"
	}
	players, limit := int(r[2]), int(r[3])
	skip(9) // app id, players, max players, bots, type, environment, visibility, VAC
	cstr()  // version
	if len(r) > 0 {
		edf := r[0]
		skip(1)
		if edf&0x80 != 0 {
			skip(2) // game port
		}
		if edf&0x10 != 0 {
			skip(8) // steam id
		}
		if edf&0x40 != 0 {
			skip(2) // spectator port
			cstr()
		}
		if edf&0x20 != 0 {
			// Rust (and others) put the real counts in tags because the
			// fixed fields above are single bytes: "mp200,cp143,..."
			for _, tag := range strings.Split(cstr(), ",") {
				if n, ok := tagNumber(tag, "cp"); ok {
					players = n
				}
				if n, ok := tagNumber(tag, "mp"); ok {
					limit = n
				}
			}
		}
	}
	out := fmt.Sprintf("%d/%d players", players, limit)
	if m := printable(mapName, 40); m != "" {
		out += " · " + m
	}
	return out
}

func tagNumber(tag, prefix string) (int, bool) {
	if !strings.HasPrefix(tag, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(tag[len(prefix):])
	return n, err == nil && n >= 0
}

func printable(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, strings.ToValidUTF8(s, ""))
	if rs := []rune(s); len(rs) > max {
		s = string(rs[:max]) + "…"
	}
	return strings.TrimSpace(s)
}

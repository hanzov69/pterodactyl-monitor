package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// probe runs a server's service check.
func (m *Monitor) probe(ctx context.Context, p *Probe) result {
	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	timeout := m.cfg.Timeout
	start := time.Now()
	timed := func(detail string, err error, failPrefix string) result {
		ms := time.Since(start).Milliseconds()
		if err != nil {
			return failResult(ms, failPrefix+errDetail(err))
		}
		return okResult(ms, detail)
	}

	switch p.Type {
	case "http":
		code, _, ms, err := m.get(ctx, false, p.URL, nil)
		switch {
		case err != nil:
			return failResult(ms, "HTTP check failed: "+errDetail(err))
		case code >= 400:
			return failResult(ms, fmt.Sprintf("HTTP %d", code))
		}
		return okResult(ms, fmt.Sprintf("HTTP %d", code))
	case "tcp":
		return timed(tcpOpen(ctx, addr, p.Port, timeout))
	case "a2s":
		info, err := a2sInfo(addr, timeout)
		return timed(info, err, "no answer to server query: ")
	case "minecraft":
		info, err := minecraftStatus(ctx, p.Host, p.Port, timeout)
		return timed(info, err, "no answer to status ping: ")
	case "raknet":
		return timed("answering pings", raknetPing(addr, timeout), "no answer to ping: ")
	case "palworld":
		if p.Password == "" { // not known (yet): settle for the RCON port being open
			return timed(tcpOpen(ctx, addr, p.Port, timeout))
		}
		info, err := palworldRCON(ctx, addr, p.Password, timeout)
		return timed(info, err, "RCON check failed: ")
	}
	return result{kind: resSkip, detail: "unknown check type " + p.Type}
}

func tcpOpen(ctx context.Context, addr string, port int, timeout time.Duration) (string, error, string) {
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err, fmt.Sprintf("port %d not accepting connections: ", port)
	}
	conn.Close()
	return fmt.Sprintf("port %d open", port), nil, ""
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// minecraftStatus performs a Java Edition Server List Ping: the request the
// multiplayer screen makes to show a server's version and player count.
func minecraftStatus(ctx context.Context, host string, port int, timeout time.Duration) (string, error) {
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	// Handshake (next state: status), then an empty status request.
	hs := []byte{0x00}
	hs = binary.AppendUvarint(hs, 47) // protocol version; any value gets a status reply
	hs = binary.AppendUvarint(hs, uint64(len(host)))
	hs = append(hs, host...)
	hs = binary.BigEndian.AppendUint16(hs, uint16(port))
	hs = binary.AppendUvarint(hs, 1)
	pkt := append(binary.AppendUvarint(nil, uint64(len(hs))), hs...)
	pkt = append(pkt, 0x01, 0x00)
	if _, err := conn.Write(pkt); err != nil {
		return "", err
	}

	br := bufio.NewReader(conn)
	if _, err := binary.ReadUvarint(br); err != nil { // packet length
		return "", err
	}
	if id, err := binary.ReadUvarint(br); err != nil || id != 0 {
		return "", errors.New("unexpected reply")
	}
	n, err := binary.ReadUvarint(br)
	if err != nil || n == 0 || n > 4<<20 {
		return "", errors.New("unexpected reply")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", err
	}
	var st struct {
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
		Players struct {
			Online int `json:"online"`
			Max    int `json:"max"`
		} `json:"players"`
	}
	if err := json.Unmarshal(buf, &st); err != nil {
		return "", errors.New("unexpected reply")
	}
	out := fmt.Sprintf("%d/%d players", st.Players.Online, st.Players.Max)
	if v := printable(st.Version.Name, 40); v != "" {
		out += " · " + v
	}
	return out, nil
}

var raknetMagic = []byte{0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe, 0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78}

// raknetPing sends a RakNet "unconnected ping", which game servers built on
// RakNet (Don't Starve Together, among others) answer without a session.
func raknetPing(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := []byte{0x01}
	req = binary.BigEndian.AppendUint64(req, uint64(time.Now().UnixMilli()))
	req = append(req, raknetMagic...)
	req = binary.BigEndian.AppendUint64(req, 0x70746572_6f6d6f6e) // client id
	buf := make([]byte, 2048)
	lastErr := errors.New("timed out")
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := conn.Write(req); err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(timeout / 3))
		n, err := conn.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if n > 0 && buf[0] == 0x1c && bytes.Contains(buf[:n], raknetMagic) { // unconnected pong
			return nil
		}
		lastErr = errors.New("unexpected reply")
	}
	return lastErr
}

var palworldVersion = regexp.MustCompile(`\[(v[^\]\s]+)\]`)

// palworldRCON logs in to a Palworld server's RCON port and asks it for its
// version and who is online. Palworld answers neither Steam queries nor
// anything else unauthenticated, so this is the only way to see inside it.
func palworldRCON(ctx context.Context, addr, password string, timeout time.Duration) (string, error) {
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	const (
		typeAuth         = 3
		typeExec         = 2
		typeAuthResponse = 2
	)
	send := func(id, typ int32, body string) error {
		pkt := binary.LittleEndian.AppendUint32(nil, uint32(len(body)+10))
		pkt = binary.LittleEndian.AppendUint32(pkt, uint32(id))
		pkt = binary.LittleEndian.AppendUint32(pkt, uint32(typ))
		pkt = append(append(pkt, body...), 0, 0)
		_, err := conn.Write(pkt)
		return err
	}
	recv := func() (id, typ int32, body string, err error) {
		var size int32
		if err = binary.Read(conn, binary.LittleEndian, &size); err != nil {
			return
		}
		if size < 10 || size > 1<<20 {
			return 0, 0, "", errors.New("unexpected reply")
		}
		buf := make([]byte, size)
		if _, err = io.ReadFull(conn, buf); err != nil {
			return
		}
		id = int32(binary.LittleEndian.Uint32(buf))
		typ = int32(binary.LittleEndian.Uint32(buf[4:]))
		return id, typ, string(bytes.TrimRight(buf[8:], "\x00")), nil
	}

	if err := send(1, typeAuth, password); err != nil {
		return "", err
	}
	for i := 0; ; i++ { // some servers send an empty packet ahead of the auth reply
		id, typ, _, err := recv()
		if err != nil {
			return "", err
		}
		if typ == typeAuthResponse {
			if id == -1 {
				return "", errors.New("login rejected (admin password in the panel does not match the server)")
			}
			break
		}
		if i >= 2 {
			return "", errors.New("unexpected reply")
		}
	}
	exec := func(cmd string) (string, error) {
		if err := send(2, typeExec, cmd); err != nil {
			return "", err
		}
		_, _, body, err := recv() // Palworld does not echo request ids, so they aren't checked
		return body, err
	}

	var parts []string
	if list, err := exec("ShowPlayers"); err == nil { // "name,playeruid,steamid" header, then one line per player
		n := -1
		for _, line := range strings.Split(list, "\n") {
			if strings.TrimSpace(line) != "" {
				n++
			}
		}
		if n >= 0 {
			parts = append(parts, plural(n, "player"))
		}
	} else {
		return "", err
	}
	if info, err := exec("Info"); err == nil { // "Welcome to Pal Server[v0.0.0.0] name"
		if v := palworldVersion.FindStringSubmatch(info); v != nil {
			parts = append(parts, printable(v[1], 24))
		}
	}
	if len(parts) == 0 {
		return "RCON responding", nil
	}
	return strings.Join(parts, " · "), nil
}

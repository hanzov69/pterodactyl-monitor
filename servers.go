package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// clientServer is a server as returned by the panel's client API.
type clientServer struct {
	Identifier   string `json:"identifier"`
	UUID         string `json:"uuid"`
	Name         string `json:"name"`
	Node         string `json:"node"`
	Invocation   string `json:"invocation"`
	DockerImage  string `json:"docker_image"`
	Status       string `json:"status"`
	Suspended    bool   `json:"is_suspended"`
	Installing   bool   `json:"is_installing"`
	Transferring bool   `json:"is_transferring"`
	SFTP         struct {
		IP string `json:"ip"` // the node's FQDN
	} `json:"sftp_details"`
	Relationships struct {
		Allocations struct {
			Data []struct {
				Attributes struct {
					IP      string `json:"ip"`
					Alias   string `json:"ip_alias"`
					Port    int    `json:"port"`
					Default bool   `json:"is_default"`
				} `json:"attributes"`
			} `json:"data"`
		} `json:"allocations"`
		Variables struct {
			Data []struct {
				Attributes struct {
					Env   string `json:"env_variable"`
					Value string `json:"server_value"`
				} `json:"attributes"`
			} `json:"data"`
		} `json:"variables"`
		Egg struct {
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"egg"`
	} `json:"relationships"`
}

// listServers enumerates servers through the client API. An admin's account
// key sees every server on the panel; any other key sees the servers its user
// owns or is a subuser of.
func (m *Monitor) listServers(ctx context.Context) ([]Target, result) {
	var totalMs int64
	fetch := func(query string) ([]clientServer, *result) {
		var out []clientServer
		for page := 1; ; page++ {
			u := fmt.Sprintf("%s/api/client?include=egg&per_page=100&page=%d%s", m.cfg.APIURL, page, query)
			code, body, ms, err := m.get(ctx, m.cfg.LANInsecure, u, bearer(m.cfg.ClientKey))
			totalMs += ms
			if err != nil {
				r := failResult(totalMs, errDetail(err))
				return nil, &r
			}
			if code != http.StatusOK {
				hint := ""
				if code == 401 || code == 403 {
					hint = " (check PANEL_CLIENT_KEY: needs an account API key, ptlc_...)"
				}
				r := failResult(totalMs, fmt.Sprintf("HTTP %d%s", code, hint))
				return nil, &r
			}
			var resp struct {
				Data []struct {
					Attributes clientServer `json:"attributes"`
				} `json:"data"`
				Meta pageMeta `json:"meta"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				r := failResult(totalMs, "unexpected API response: "+err.Error())
				return nil, &r
			}
			for _, d := range resp.Data {
				out = append(out, d.Attributes)
			}
			if page >= resp.Meta.Pagination.TotalPages {
				return out, nil
			}
		}
	}

	servers, fail := fetch("&type=admin-all")
	if fail == nil && len(servers) == 0 { // not an admin key: fall back to what it can see
		servers, fail = fetch("")
	}
	if fail != nil {
		return nil, *fail
	}

	targets := make([]Target, 0, len(servers))
	for _, s := range servers {
		t := Target{
			ID:         "server-" + s.Identifier,
			Name:       s.Name,
			Kind:       "server",
			Identifier: s.Identifier,
			Node:       s.Node,
			Probe:      m.probeFor(s),
		}
		t.URL = t.Probe.String()
		switch {
		case s.Suspended:
			t.Idle = "suspended"
		case s.Transferring:
			t.Idle = "transferring"
		case s.Installing:
			t.Idle = "installing"
		case s.Status != "":
			t.Idle = s.Status // install_failed, restoring_backup, ...
		}
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool {
		return strings.ToLower(targets[i].Name) < strings.ToLower(targets[j].Name)
	})
	return targets, okResult(totalMs, fmt.Sprintf("%d server(s) listed", len(targets)))
}

// ProbeSpec is a per-server override from SERVER_CHECKS.
type ProbeSpec struct {
	Type string `json:"type"` // http | tcp | a2s | minecraft | raknet | palworld | none
	URL  string `json:"url"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

func parseServerChecks(raw string) (map[string]ProbeSpec, error) {
	out := map[string]ProbeSpec{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	var in map[string]ProbeSpec
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, err
	}
	for k, v := range in {
		v.Type = strings.ToLower(v.Type)
		switch v.Type {
		case "http", "tcp", "a2s", "minecraft", "raknet", "palworld", "none":
		default:
			return nil, fmt.Errorf("%q: unknown check type %q (want http, tcp, a2s, minecraft, raknet, palworld or none)", k, v.Type)
		}
		if v.URL != "" {
			if u, err := url.Parse(v.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, fmt.Errorf("%q: url must be an http(s) URL", k)
			}
		}
		out[strings.ToLower(k)] = v
	}
	return out, nil
}

// endpoint returns where a server's primary allocation can be reached.
func (s clientServer) endpoint() (string, int) {
	allocs := s.Relationships.Allocations.Data
	for i, a := range allocs {
		if !a.Attributes.Default && i < len(allocs)-1 {
			continue
		}
		host := a.Attributes.IP
		if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
			host = a.Attributes.Alias // bound to all interfaces: use the alias, else the node itself
			if host == "" {
				host = s.SFTP.IP
			}
		}
		return host, a.Attributes.Port
	}
	return s.SFTP.IP, 0
}

// probeFor picks the service check for a server: an explicit SERVER_CHECKS
// entry (matched by name, short id or UUID) wins; otherwise a check is chosen
// for server types that are recognised. Unrecognised servers are judged on
// their power state alone.
func (m *Monitor) probeFor(s clientServer) *Probe {
	host, port := s.endpoint()
	vars := map[string]string{}
	for _, v := range s.Relationships.Variables.Data {
		vars[v.Attributes.Env] = v.Attributes.Value
	}
	portVar := func(name string) int {
		n, _ := strconv.Atoi(vars[name])
		return n
	}
	build := func(typ, rawURL, h string, p int) *Probe {
		if h == "" {
			h = host
		}
		if p == 0 {
			p = port
		}
		switch {
		case typ == "http" && rawURL != "":
			return &Probe{Type: "http", URL: rawURL}
		case h == "" || p == 0:
			return nil
		case typ == "http":
			return &Probe{Type: "http", URL: "http://" + net.JoinHostPort(h, strconv.Itoa(p)) + "/"}
		case typ == "palworld":
			return &Probe{Type: typ, Host: h, Port: p, Password: vars["ADMIN_PASSWORD"]}
		}
		return &Probe{Type: typ, Host: h, Port: p}
	}

	for _, key := range []string{s.Name, s.Identifier, s.UUID} {
		if spec, ok := m.cfg.ServerChecks[strings.ToLower(key)]; ok && key != "" {
			if spec.Type == "none" {
				return nil
			}
			if spec.Type == "palworld" && spec.Port == 0 {
				spec.Port = portVar("RCON_PORT")
			}
			return build(spec.Type, spec.URL, spec.Host, spec.Port)
		}
	}

	hay := strings.ToLower(s.Invocation + " " + s.DockerImage + " " + s.Relationships.Egg.Attributes.Name)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(hay, w) {
				return true
			}
		}
		return false
	}
	_, hasJar := vars["SERVER_JARFILE"]
	switch {
	case has("rustdedicated"): // Rust answers Steam queries on its query port
		return build("a2s", "", "", portVar("QUERY_PORT"))
	case has("valheim"):
		if vars["ENABLE_CROSSPLAY"] == "1" || port == 0 {
			return nil // crossplay servers are reached through PlayFab and answer nothing locally
		}
		return build("a2s", "", "", port+1) // Steam query port is the game port + 1
	case has("enshrouded"):
		if q := portVar("QUERY_PORT"); q != 0 || port == 0 {
			return build("a2s", "", "", q)
		}
		return build("a2s", "", "", port+1)
	case has("projectzomboid", "project zomboid"):
		return build("a2s", "", "", 0)
	case has("palserver", "palworld"): // nothing unauthenticated to ask; use its RCON port
		if p := portVar("RCON_PORT"); p != 0 {
			return build("palworld", "", "", p)
		}
	case has("dontstarve", "don't starve"):
		return build("raknet", "", "", 0)
	case hasJar && has("java"): // Minecraft: Java Edition and the proxies in front of it
		return build("minecraft", "", "", 0)
	case has("srcds"): // Source engine games answer on the game port
		return build("a2s", "", "", 0)
	case has("nginx", "apache", "httpd", "caddy", "lighttpd"):
		return build("http", "", "", 0)
	}
	return nil
}

// checkServer combines the panel's view of a server (its power state) with
// the server's own service check, if it has one.
func (m *Monitor) checkServer(ctx context.Context, s Target) result {
	power, err := m.serverPower(ctx, s.Identifier)
	switch {
	case err != nil:
		// The panel couldn't reach the node. The service may still be fine, so
		// let its own check decide; without one there is nothing to go on.
		if s.Probe == nil {
			return result{kind: resSkip, detail: "state unavailable: " + err.Error()}
		}
		r := m.probe(ctx, s.Probe)
		r.detail = "state unavailable · " + r.detail
		return r
	case power == "running":
		if s.Probe == nil {
			return result{kind: resOK, detail: "running", power: power}
		}
		r := m.probe(ctx, s.Probe)
		if r.kind == resOK {
			r.detail = "running · " + r.detail
		} else {
			r.detail = "running, but " + r.detail
		}
		r.power = power
		return r
	case power == "starting" || power == "stopping":
		return result{kind: resPending, detail: power, power: power}
	default:
		return result{kind: resFail, detail: power, power: power}
	}
}

func (m *Monitor) serverPower(ctx context.Context, identifier string) (string, error) {
	u := fmt.Sprintf("%s/api/client/servers/%s/resources", m.cfg.APIURL, identifier)
	code, body, _, err := m.get(ctx, m.cfg.LANInsecure, u, bearer(m.cfg.ClientKey))
	if err != nil {
		return "", fmt.Errorf("%s", errDetail(err))
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("panel returned HTTP %d", code)
	}
	var resp struct {
		Attributes struct {
			State string `json:"current_state"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Attributes.State == "" {
		return "", fmt.Errorf("unexpected panel response")
	}
	return resp.Attributes.State, nil
}

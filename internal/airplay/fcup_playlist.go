package airplay

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var playlistURI = regexp.MustCompile(`(^|[:,])[ \t]*URI="([^"\r\n]*)"`)

// Parse HLS attribute lists without splitting commas inside quoted CODECS,
// PARAMS or URI values. Returned values exclude quotes.
func playlistAttributes(line string) (map[string]string, error) {
	_, rest, ok := strings.Cut(line, ":")
	if !ok {
		return nil, fmt.Errorf("missing HLS attributes")
	}
	attrs := make(map[string]string)
	for rest != "" {
		rest = strings.TrimSpace(rest)
		key, value, ok := strings.Cut(rest, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid HLS attribute")
		}
		if _, exists := attrs[key]; exists {
			return nil, fmt.Errorf("duplicate HLS attribute")
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			end := strings.IndexByte(value[1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("unterminated HLS attribute")
			}
			attrs[key] = value[1 : 1+end]
			rest = strings.TrimSpace(value[end+2:])
			if rest != "" {
				if rest[0] != ',' {
					return nil, fmt.Errorf("invalid HLS delimiter")
				}
				rest = rest[1:]
			}
		} else {
			v, tail, _ := strings.Cut(value, ",")
			attrs[key] = strings.TrimSpace(v)
			rest = tail
		}
	}
	return attrs, nil
}

func (s *fcupStore) rewriteResource(base *url.URL, raw string) (string, error) {
	if raw == "" || len(raw) > 16384 || strings.ContainsAny(raw, "\r\n\x00") {
		return "", fmt.Errorf("invalid HLS resource")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid HLS URI")
	}
	u = base.ResolveReference(u)
	if validFCUPURL(u) && u.Scheme == s.origin.Scheme {
		return s.localURL(u.String()), nil
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Fragment == "" {
		if s.mapURL != nil {
			return s.localURL(u.String()), nil
		}
		return u.String(), nil
	}
	return "", fmt.Errorf("unsupported HLS resource protocol")
}

func (s *fcupStore) rewritePlaylist(uri string, data []byte) ([]byte, error) {
	base, err := url.Parse(uri)
	if err != nil || len(data) > maxFCUPResource || strings.ContainsRune(string(data), '\x00') {
		return nil, fmt.Errorf("invalid FCUP playlist")
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || lines[0] != "#EXTM3U" {
		return nil, fmt.Errorf("invalid HLS header")
	}
	var out strings.Builder
	var condensed map[string]string
	streamURI := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#YT-EXT-CONDENSED-URL:") || strings.HasPrefix(line, "#YT-EXT-CONDENSED-URI:") {
			if condensed != nil {
				return nil, fmt.Errorf("duplicate condensed HLS header")
			}
			condensed, err = playlistAttributes(line)
			if err != nil {
				return nil, err
			}
			u, e := url.Parse(condensed["BASE-URI"])
			if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || condensed["PREFIX"] == "" || condensed["PARAMS"] == "" {
				return nil, fmt.Errorf("invalid condensed HLS header")
			}
			for _, param := range strings.Split(condensed["PARAMS"], ",") {
				if param == "" || strings.ContainsAny(param, "/\r\n?#") {
					return nil, fmt.Errorf("invalid condensed HLS parameter")
				}
			}
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			// Preserve every codec and rendition. This proxy transports HLS;
			// only the player can decide what its platform decoder supports.
			if streamURI {
				return nil, fmt.Errorf("missing HLS variant URI")
			}
			if _, e := playlistAttributes(line); e != nil {
				return nil, e
			}
			streamURI = true
		}
		if strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:") {
			if _, e := playlistAttributes(line); e != nil {
				return nil, e
			}
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			if streamURI {
				streamURI = false
			}
			if condensed != nil && strings.HasPrefix(line, condensed["PREFIX"]) {
				values := strings.Split(strings.TrimPrefix(line, condensed["PREFIX"]), "/")
				params := strings.Split(condensed["PARAMS"], ",")
				if len(values) != len(params) {
					return nil, fmt.Errorf("invalid condensed HLS segment")
				}
				var expanded strings.Builder
				expanded.WriteString(strings.TrimRight(condensed["BASE-URI"], "/"))
				for i, param := range params {
					if values[i] == "" {
						return nil, fmt.Errorf("empty condensed HLS value")
					}
					expanded.WriteString("/" + param + "/" + values[i])
				}
				line = expanded.String()
			}
			line, err = s.rewriteResource(base, line)
			if err != nil {
				return nil, err
			}
		} else if strings.HasPrefix(line, "#EXT-X-") && strings.Contains(line, "URI=") {
			attrs, e := playlistAttributes(line)
			if e != nil {
				return nil, e
			}
			matches := playlistURI.FindAllStringSubmatchIndex(line, -1)
			if _, hasURI := attrs["URI"]; hasURI && len(matches) != 1 {
				return nil, fmt.Errorf("invalid quoted HLS URI")
			}
			var adjusted strings.Builder
			last := 0
			for _, m := range matches {
				value, e := s.rewriteResource(base, line[m[4]:m[5]])
				if e != nil {
					return nil, e
				}
				adjusted.WriteString(line[last:m[4]])
				adjusted.WriteString(value)
				last = m[5]
			}
			adjusted.WriteString(line[last:])
			line = adjusted.String()
		}
		if out.Len()+len(line)+1 > maxFCUPResource {
			return nil, fmt.Errorf("expanded HLS playlist exceeds budget")
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if streamURI {
		return nil, fmt.Errorf("missing HLS variant URI")
	}
	return []byte(out.String()), nil
}

package app

import (
	"net/url"

	"anykvm/internal/rdp"
	"anykvm/internal/rtsp"
)

func sourceURL(raw string) *url.URL {
	if !rtsp.IsDevice(raw) && !rdp.IsSource(raw) {
		return nil
	}
	u, e := url.Parse(raw)
	if e != nil {
		return nil
	}
	return u
}
func sourceHasPassword(raw string) bool {
	u := sourceURL(raw)
	if u == nil || u.User == nil {
		return false
	}
	password, ok := u.User.Password()
	return ok && password != ""
}
func redactSourceURL(raw string) string {
	u := sourceURL(raw)
	if u == nil || u.User == nil {
		return raw
	}
	if p, ok := u.User.Password(); ok && p != "" {
		u.User = url.UserPassword(u.User.Username(), retainedSecret)
		return u.String()
	}
	return raw
}
func restoreSourceURL(raw, previous string) string {
	u, old := sourceURL(raw), sourceURL(previous)
	if u == nil || old == nil || u.User == nil || old.User == nil {
		return raw
	}
	p, _ := u.User.Password()
	if p != retainedSecret {
		return raw
	}
	if u.Scheme != old.Scheme || u.Host != old.Host || u.User.Username() != old.User.Username() {
		u.User = url.UserPassword(u.User.Username(), "")
		return u.String()
	}
	password, _ := old.User.Password()
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String()
}

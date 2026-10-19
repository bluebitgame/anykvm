package wsmedia

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStartReportsAddressConflict(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s := NewServer(Config{HTTPAddr: listener.Addr().String()})
	if err := s.Start(); err == nil {
		s.Close()
		t.Fatal("Start succeeded on an occupied address")
	}
}

func TestStartRollsBackFirstListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	// The second bind conflicts with the first, so startup must release it.
	s := NewServer(Config{HTTPAddr: address, HTTPSAddr: address})
	if err := s.Start(); err == nil {
		s.Close()
		t.Fatal("expected duplicate listener failure")
	}
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listener leaked after failed startup: %v", err)
	}
	listener.Close()
}

func TestWebSocketRejectsCrossOrigin(t *testing.T) {
	s := NewServer(Config{})
	r := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	r.Header.Set("Origin", "https://unrelated.example")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	w := httptest.NewRecorder()
	s.handleWS(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request status = %d", w.Code)
	}
}

func TestWebAccessRequiresConfiguredPassword(t *testing.T) {
	s := NewServer(Config{HTTPAddr: "127.0.0.1:0", Password: "secret"})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	request := func(password string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
		if password != "" {
			r.SetBasicAuth("user", password)
		}
		w := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(w, r)
		return w
	}
	if w := request(""); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing password was not challenged")
	}
	if w := request("wrong"); w.Code != http.StatusUnauthorized {
		t.Fatal("wrong password accepted")
	}
	if w := request("secret"); w.Code != http.StatusOK {
		t.Fatalf("correct password status = %d", w.Code)
	}
}

func TestWebAccessCanRequireConfiguredUsername(t *testing.T) {
	handler, err := NewServer(Config{User: "anykvm", Password: "secret"}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := func(user, password string) int {
		r := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
		r.SetBasicAuth(user, password)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if status := request("other", "secret"); status != http.StatusUnauthorized {
		t.Fatalf("wrong username status = %d", status)
	}
	if status := request("anykvm", "wrong"); status != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d", status)
	}
	if status := request("anykvm", "secret"); status != http.StatusOK {
		t.Fatalf("correct credentials status = %d", status)
	}
}

func TestSettingsUsesDedicatedAdminPassword(t *testing.T) {
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Fatalf("settings path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler, err := NewServer(Config{
		User:          "anykvm",
		Password:      "viewer-password",
		AdminPassword: "admin-password",
		AdminHandler:  admin,
	}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, user, password, remoteAddr string) int {
		r := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
		r.RemoteAddr = remoteAddr
		r.SetBasicAuth(user, password)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if status := request("/settings/", "admin", "viewer-password", "198.51.100.2:1234"); status != http.StatusUnauthorized {
		t.Fatalf("viewer password accessed settings: %d", status)
	}
	if status := request("/settings/", "anykvm", "admin-password", "198.51.100.2:1234"); status != http.StatusUnauthorized {
		t.Fatalf("non-admin username accessed settings: %d", status)
	}
	if status := request("/settings/", "admin", "admin-password", "198.51.100.2:1234"); status != http.StatusNoContent {
		t.Fatalf("admin password settings status = %d", status)
	}
	if status := request("/settings/", "", "", "127.0.0.1:1234"); status != http.StatusUnauthorized {
		t.Fatalf("password-free settings status = %d", status)
	}
	if status := request("/", "anykvm", "admin-password", "198.51.100.2:1234"); status != http.StatusUnauthorized {
		t.Fatalf("admin password accessed viewer: %d", status)
	}
	if status := request("/", "anykvm", "viewer-password", "198.51.100.2:1234"); status != http.StatusOK {
		t.Fatalf("viewer password viewer status = %d", status)
	}
}

func TestSettingsWithoutAdminPasswordIsLoopbackOnly(t *testing.T) {
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler, err := NewServer(Config{AdminHandler: admin, SettingsEnabled: true}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := func(remoteAddr string) int {
		r := httptest.NewRequest(http.MethodGet, "http://localhost/settings/", nil)
		r.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if status := request("127.0.0.1:1234"); status != http.StatusNoContent {
		t.Fatalf("password-free settings status = %d", status)
	}
	if status := request("198.51.100.2:1234"); status != http.StatusNotFound {
		t.Fatalf("remote password-free settings status = %d", status)
	}
}

func TestWebAccessUpdatesAtRuntime(t *testing.T) {
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	server := NewServer(Config{
		User:          "old-user",
		Password:      "old-viewer",
		AdminPassword: "old-admin",
		AdminHandler:  admin,
	})
	handler, err := server.Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, user, password string) int {
		r := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
		r.RemoteAddr = "198.51.100.2:1234"
		r.SetBasicAuth(user, password)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if status := request("/", "old-user", "old-viewer"); status != http.StatusOK {
		t.Fatalf("initial viewer status = %d", status)
	}
	if status := request("/settings/", "admin", "old-admin"); status != http.StatusNoContent {
		t.Fatalf("initial settings status = %d", status)
	}

	server.UpdateAccess("new-user", "new-viewer", "new-admin")
	if status := request("/", "old-user", "old-viewer"); status != http.StatusUnauthorized {
		t.Fatalf("old viewer credentials status = %d", status)
	}
	if status := request("/", "new-user", "new-viewer"); status != http.StatusOK {
		t.Fatalf("new viewer credentials status = %d", status)
	}
	if status := request("/settings/", "admin", "old-admin"); status != http.StatusUnauthorized {
		t.Fatalf("old settings credentials status = %d", status)
	}
	if status := request("/settings/", "admin", "new-admin"); status != http.StatusNoContent {
		t.Fatalf("new settings credentials status = %d", status)
	}
}

func TestTargetPickerDoesNotClaimUntilTargetWebSocket(t *testing.T) {
	selected := -1
	cover := []byte("png")
	s := NewServer(Config{
		Targets:         []Target{{Name: "Desktop", Platform: "darwin"}, {Name: "Phone", Platform: "ios"}},
		AdminPassword:   "admin-password",
		SettingsEnabled: true,
		TargetCover: func(platform string) []byte {
			if platform != "darwin" {
				t.Fatalf("cover platform = %q", platform)
			}
			return cover
		},
		MouseMode: func() string {
			if selected == 1 {
				return "relative"
			}
			return "absolute"
		},
		OnTargetSelected: func(index int) error {
			selected = index
			return nil
		},
	})
	handler, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("picker", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/targets", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("targets status = %d", response.Code)
		}
		var body struct {
			Targets []struct {
				Name, Cover string
			} `json:"targets"`
			Settings bool `json:"settings"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Targets) != 2 || body.Targets[0].Cover != "/api/targets/Desktop/cover" || !body.Settings {
			t.Fatalf("picker payload = %+v", body)
		}
		if selected != -1 {
			t.Fatal("loading the picker selected a target")
		}
	})

	t.Run("cover and control page", func(t *testing.T) {
		for _, path := range []string{"/api/targets/Desktop/cover", "/target/Desktop"} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("%s status = %d", path, response.Code)
			}
		}
		if selected != -1 {
			t.Fatal("loading a target page selected a target")
		}
	})

	server := httptest.NewServer(handler)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?target=Phone"
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var meta metadata
	if len(data) == 0 || data[0] != msgMetadata {
		t.Fatalf("expected metadata: %v", data)
	}
	if err := json.Unmarshal(data[1:], &meta); err != nil {
		t.Fatal(err)
	}
	if meta.MouseMode != "relative" {
		t.Fatalf("target mouse mode = %q", meta.MouseMode)
	}
	if selected != 1 {
		t.Fatalf("WebSocket target selection = %d, want 1", selected)
	}
}

func TestTargetPickerHidesPasswordFreeSettingsFromRemote(t *testing.T) {
	handler, err := NewServer(Config{SettingsEnabled: true}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := func(remoteAddr string) bool {
		r := httptest.NewRequest(http.MethodGet, "/api/targets", nil)
		r.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var body struct {
			Settings bool `json:"settings"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Settings
	}
	if !request("127.0.0.1:1234") {
		t.Fatal("loopback picker hid password-free settings")
	}
	if request("198.51.100.2:1234") {
		t.Fatal("remote picker exposed password-free settings")
	}
}

func TestVideoBackpressureCanBeCancelled(t *testing.T) {
	s := NewServer(Config{})
	c := &wsClient{sendCh: make(chan *outboundMessage, 1), done: make(chan struct{})}
	s.clients[c] = struct{}{}
	c.sendCh <- &outboundMessage{data: []byte{1}}
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() { s.broadcastH264([]byte{2}, stop); close(finished) }()
	close(stop)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("capture cancellation did not unblock video broadcast")
	}
}

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"streamgo/internal/config"
	"streamgo/internal/services"
)

func TestPartySmoke(t *testing.T) {
	cfg := &config.Config{}
	svc := services.NewPartyService(cfg)
	h := NewPartyHandler(svc, cfg)
	r := chi.NewRouter()
	h.Routes(r)
	server := httptest.NewServer(r)
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}

	createBody := `{"userId":"u1","deviceId":"d1","displayName":"Host","maxMembers":5}`
	resp, err = http.Post(server.URL+"/api/parties", "application/json", bytes.NewBufferString(createBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var created map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	code, _ := created["code"].(string)
	token, _ := created["token"].(string)
	if code == "" || token == "" {
		t.Fatalf("create response missing code/token: %#v", created)
	}

	joinBody := `{"userId":"u2","deviceId":"d2","displayName":"Listener"}`
	resp, err = http.Post(server.URL+"/api/parties/"+code+"/join", "application/json", bytes.NewBufferString(joinBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join status = %d", resp.StatusCode)
	}

	resp, err = http.Get(server.URL + "/api/parties/" + code + "/preview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var preview map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if count, _ := preview["memberCount"].(float64); count != 2 {
		t.Fatalf("preview memberCount = %v", preview["memberCount"])
	}

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/parties/"+code, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get party status = %d", resp.StatusCode)
	}

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/parties/" + code
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var welcome map[string]any
	if err := json.Unmarshal(data, &welcome); err != nil {
		t.Fatal(err)
	}
	if welcome["type"] != "welcome" {
		t.Fatalf("first frame = %#v", welcome)
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping","clientMs":123}`)); err != nil {
		t.Fatal(err)
	}
	foundPong := false
	for i := 0; i < 8; i++ {
		_, data, err = conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatal(err)
		}
		if frame["type"] == "pong" {
			foundPong = true
			break
		}
	}
	if !foundPong {
		t.Fatal("did not receive pong frame")
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"control","action":"play","positionMs":1000}`)); err != nil {
		t.Fatal(err)
	}
	foundState := false
	for i := 0; i < 8; i++ {
		_, data, err = conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatal(err)
		}
		if frame["type"] != "state" {
			continue
		}
		playback, _ := frame["playback"].(map[string]any)
		if seq, _ := playback["seq"].(float64); seq > 0 {
			foundState = true
			break
		}
	}
	if !foundState {
		t.Fatal("did not receive updated state frame")
	}
}

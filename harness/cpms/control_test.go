package cpms

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestControlMode(t *testing.T) {
	t.Parallel()

	s, _ := start(t, Options{})
	ctl := httptest.NewServer(s.ControlHandler())
	t.Cleanup(ctl.Close)

	if code, body := post(t, ctl.URL+"/mode?m=reject-boot", ""); code != http.StatusOK {
		t.Fatalf("mode: %d %s", code, body)
	}
	if s.Mode() != ModeRejectBoot {
		t.Errorf("mode = %s", s.Mode())
	}
	if code, _ := post(t, ctl.URL+"/mode?m=bogus", ""); code != http.StatusBadRequest {
		t.Errorf("bogus mode: %d, want 400", code)
	}
}

func TestControlCall(t *testing.T) {
	t.Parallel()

	s, base := start(t, Options{})
	ctl := httptest.NewServer(s.ControlHandler())
	t.Cleanup(ctl.Close)

	c := mustDial(t, base, "CP1")
	c.send(boot)
	c.mustRead()

	type reply struct {
		code int
		body string
	}
	done := make(chan reply, 1)
	go func() {
		code, body := post(t, ctl.URL+"/call?charger=CP1&action=GetConfiguration", `{"key":["X"]}`)
		done <- reply{code, body}
	}()
	got := c.mustRead()
	if got.Action != "GetConfiguration" || string(got.Payload) != `{"key":["X"]}` {
		t.Fatalf("charger got %s %s", got.Action, got.Payload)
	}
	c.send(`[3,"` + got.ID + `",{"configurationKey":[]}]`)
	r := <-done
	if r.code != http.StatusOK || strings.TrimSpace(r.body) != `{"configurationKey":[]}` {
		t.Errorf("reply %d %q", r.code, r.body)
	}

	if code, _ := post(t, ctl.URL+"/call?charger=nope&action=Reset", `{}`); code != http.StatusNotFound {
		t.Errorf("unknown charger: %d, want 404", code)
	}
}

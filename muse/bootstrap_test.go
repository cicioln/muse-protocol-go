package muse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBootstrapUnknownVMStateSkipsWake(t *testing.T) {
	for _, state := range []string{"UNKNOWN", ""} {
		var wakes atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/session":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "assigned", "vm_id": "vm-1", "vm_state": state,
					"endpoint_url": "wss://vm-1.example/"})
			case "/api/hatch/vm/wake":
				wakes.Add(1)
				_, _ = w.Write([]byte(`{"status":"wake_requested"}`))
			case "/api/hatch/token":
				_, _ = w.Write([]byte(`{"token":"tok"}`))
			case "/api/hatch/noise-notary-token":
				_, _ = w.Write([]byte(`{"notaryToken":" nt "}`))
			default:
				http.NotFound(w, r)
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, _, err := newSiteClient(srv.URL, "", map[string]string{}).bootstrap(ctx)
		cancel()
		srv.Close()
		if err != nil {
			t.Fatalf("state %q: bootstrap failed: %v", state, err)
		}
		if s.VMID != "vm-1" || s.AuthToken != "tok" || s.NotaryToken != "nt" || s.Endpoint != "wss://vm-1.example/" {
			t.Fatalf("state %q: unexpected session %+v", state, s)
		}
		if n := wakes.Load(); n != 0 {
			t.Fatalf("state %q: woke the VM %d times", state, n)
		}
	}
}

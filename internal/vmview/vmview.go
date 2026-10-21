// Package vmview serves a browser-based VNC viewer (noVNC) so users can watch a
// QEMU VM (e.g. a golden-image bake) live. QEMU exposes RFB over a websocket
// (-vnc …,websocket=PORT); this serves the embedded noVNC client that connects
// to it.
package vmview

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"
)

//go:embed novnc
var novncFS embed.FS

// Serve runs the viewer HTTP server until ctx is cancelled. wsPort is the QEMU
// VNC websocket port; the landing page auto-connects the browser to it (using
// the hostname the browser used, so it works locally and remotely).
func Serve(ctx context.Context, httpAddr string, wsPort int, logger *slog.Logger) error {
	h, err := handler(wsPort)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: httpAddr, Handler: h}
	listener, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", httpAddr, err)
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("vm viewer listening", "url", "http://"+listener.Addr().String())
		errCh <- srv.Serve(listener)
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve VM viewer: %w", err)
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		shutdownErr := srv.Shutdown(shutCtx)
		serveErr := <-errCh
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(shutdownErr, serveErr)
	}
}

func handler(wsPort int) (http.Handler, error) {
	sub, err := fs.Sub(novncFS, "novnc")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/novnc/", http.StripPrefix("/novnc/", http.FileServer(http.FS(sub))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, landingHTML, wsPort)
	})
	return mux, nil
}

// landingHTML redirects to the noVNC lite client, pointing it at the QEMU
// websocket on the same host the browser used. %d = ws port.
const landingHTML = `<!doctype html><html><head><meta charset="utf-8"><title>multirunner VM</title></head>
<body><script>
var p = %d;
location.replace('/novnc/vnc_lite.html?host=' + location.hostname +
  '&port=' + p + '&path=&autoconnect=true&resize=scale&reconnect=true');
</script>Connecting to the VM…</body></html>`

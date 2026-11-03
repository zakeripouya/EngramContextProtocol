package ecpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/server"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// ---------------------------------------------------------------- stdio

// IsECPInitialize reports whether a first message is an ECP (not MCP) initialize:
// it names an ECP protocol version or lists supportedVersions, which MCP lacks.
func IsECPInitialize(line []byte) bool {
	var req struct {
		Method string `json:"method"`
		Params struct {
			ProtocolVersion   string   `json:"protocolVersion"`
			SupportedVersions []string `json:"supportedVersions"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &req) != nil || req.Method != "initialize" {
		return false
	}
	for _, v := range SupportedVersions {
		if req.Params.ProtocolVersion == v {
			return true
		}
	}
	return len(req.Params.SupportedVersions) > 0
}

// ServeStdio serves one native ECP connection, one JSON message per line, until
// the reader ends. first, if not nil, is a line already read (by version sniffing).
func (s *Server) ServeStdio(ctx context.Context, first []byte, r io.Reader, w io.Writer, client string) error {
	sess := s.NewSession(client, false)
	defer s.Close(sess)
	var wmu sync.Mutex
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
		if f, ok := w.(interface{ Sync() error }); ok {
			_ = f.Sync()
		}
		return nil
	}
	sess.Attach(write)
	handle := func(line []byte) error {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil
		}
		if out := s.Handle(ctx, sess, line); out != nil {
			return write(out)
		}
		return nil
	}
	if first != nil {
		if err := handle(first); err != nil {
			return err
		}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		if err := handle(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// ---------------------------------------------------------------- HTTP

// SessionHeader carries the ECP session id over HTTP.
const SessionHeader = "Ecp-Session-Id"

type ctxKey struct{}

type caller struct {
	client  string
	service bool
}

// Authenticate resolves a request's bearer token to a client.
func Authenticate(st *store.Store, r *http.Request) (client string, service bool, err error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return "", false, store.ErrBadToken
	}
	return st.AuthToken(strings.TrimSpace(tok))
}

func requireToken(st *store.Store, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, service, err := Authenticate(st, r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="engram"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(response{JSONRPC: "2.0", ID: nullID, Error: &rpcError{Code: int(ecp.Unauthorized),
				Message: "missing or invalid bearer token; issue one with: engram token create CLIENT"}})
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, caller{client, service})))
	})
}

func callerOf(r *http.Request) caller { c, _ := r.Context().Value(ctxKey{}).(caller); return c }

// HTTPHandler serves native ECP at /ecp, the MCP compatibility profile at /mcp
// (Streamable HTTP), and /healthz. Every request needs a bearer token from
// "engram token create"; the token decides the client identity, never clientInfo.
//
//	POST   /ecp   a JSON-RPC message or batch; initialize returns the Ecp-Session-Id header
//	GET    /ecp   server-sent events: notifications for the session in Ecp-Session-Id
//	DELETE /ecp   end the session
func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.Handle("/ecp", requireToken(s.st, http.HandlerFunc(s.serveECP)))
	mux.Handle("/mcp", requireToken(s.st, s.mcpHandler()))
	return mux
}

func (s *Server) serveECP(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	id := r.Header.Get(SessionHeader)
	var sess *Session
	if id != "" {
		sess = s.Session(id)
		if sess == nil || sess.Client() != c.client {
			http.Error(w, "unknown session; send initialize again", http.StatusNotFound)
			return
		}
	}
	switch r.Method {
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		if sess == nil {
			var first request
			if json.Unmarshal(body, &first) != nil || first.Method != "initialize" {
				http.Error(w, "missing "+SessionHeader+"; the first request must be initialize", http.StatusBadRequest)
				return
			}
			sess = s.NewSession(c.client, c.service)
		}
		out := s.Handle(r.Context(), sess, body)
		w.Header().Set(SessionHeader, sess.ID)
		if out == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	case http.MethodGet:
		if sess == nil {
			http.Error(w, "missing "+SessionHeader, http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		var mu sync.Mutex
		done := make(chan struct{})
		var once sync.Once
		sess.Attach(func(b []byte) error {
			mu.Lock()
			defer mu.Unlock()
			select {
			case <-done:
				return errors.New("stream closed")
			default:
			}
			if _, err := io.WriteString(w, "event: message\ndata: "+string(b)+"\n\n"); err != nil {
				once.Do(func() { close(done) })
				return err
			}
			flusher.Flush()
			return nil
		})
		keepAlive := time.NewTicker(25 * time.Second)
		defer keepAlive.Stop()
		for {
			select {
			case <-r.Context().Done():
				once.Do(func() { close(done) })
				sess.Detach()
				return
			case <-done:
				sess.Detach()
				return
			case <-sess.end:
				return
			case <-keepAlive.C:
				mu.Lock()
				_, err := io.WriteString(w, ": keep-alive\n\n")
				if err == nil {
					flusher.Flush()
				}
				mu.Unlock()
				if err != nil {
					sess.Detach()
					return
				}
			}
		}
	case http.MethodDelete:
		if sess != nil {
			s.Close(sess)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// mcpHandler serves the MCP tools over Streamable HTTP. Each client (from its
// token) gets one MCP server whose push watcher runs for the process lifetime.
func (s *Server) mcpHandler() http.Handler {
	var mu sync.Mutex
	servers := map[string]*server.Server{}
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c := callerOf(r)
		mu.Lock()
		defer mu.Unlock()
		srv, ok := servers[c.client]
		if !ok {
			srv = server.New(s.st, c.client)
			servers[c.client] = srv
			go srv.Watch(context.Background(), s.o.PollEvery)
		}
		return srv.Server
	}, nil)
}

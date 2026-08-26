// Package server wires the HTTP surface of the service: the public QR
// generator, the 001-999 redirects and the /adminpage management console.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/mvtp/qrurl/internal/cleanup"
	"github.com/mvtp/qrurl/internal/config"
	"github.com/mvtp/qrurl/internal/httpx"
	"github.com/mvtp/qrurl/internal/qrfile"
	"github.com/mvtp/qrurl/internal/store"
	"github.com/mvtp/qrurl/web"
)

// Server holds the dependencies of the HTTP layer.
type Server struct {
	cfg     *config.Config
	db      *store.DB
	files   *qrfile.Store
	cleaner *cleanup.Cleaner
	log     *slog.Logger

	pages     map[string]*template.Template
	static    fs.FS
	assetTags map[string]string

	loginLimiter       *httpx.Limiter
	globalLoginLimiter *httpx.Limiter
	apiLimiter         *httpx.Limiter
	publicLimiter      *httpx.Limiter
	redirectLimiter    *httpx.Limiter

	now func() time.Time

	startedAt time.Time
}

// Options configures a Server.
type Options struct {
	Config  *config.Config
	DB      *store.DB
	Files   *qrfile.Store
	Cleaner *cleanup.Cleaner
	Logger  *slog.Logger
	Now     func() time.Time
}

// New builds the HTTP server.
func New(opts Options) (*Server, error) {
	if opts.Config == nil || opts.DB == nil || opts.Files == nil {
		return nil, fmt.Errorf("server: config, database and file store are required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}

	staticFS, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, fmt.Errorf("server: static assets: %w", err)
	}

	s := &Server{
		cfg:          opts.Config,
		db:           opts.DB,
		files:        opts.Files,
		cleaner:      opts.Cleaner,
		log:          log,
		static:       staticFS,
		loginLimiter: httpx.NewLimiter(10, 15*time.Minute),
		// A second, address independent budget so a botnet spread over many
		// source addresses cannot grind through passwords either.
		globalLoginLimiter: httpx.NewLimiter(60, 15*time.Minute),
		apiLimiter:         httpx.NewLimiter(600, time.Minute),
		publicLimiter:      httpx.NewLimiter(120, time.Minute),
		// The 001-999 space is small enough to walk, so cap how fast a single
		// address can sweep it. Normal use is a handful of clicks a minute.
		redirectLimiter: httpx.NewLimiter(opts.Config.RedirectRate, time.Minute),
		now:             nowFn,
		startedAt:       time.Now().UTC(),
	}
	if err := s.loadAssets(staticFS); err != nil {
		return nil, err
	}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadAssets computes a short content hash per static file so that the browser
// can cache them forever and still pick up a new build.
func (s *Server) loadAssets(staticFS fs.FS) error {
	s.assetTags = map[string]string{}
	return fs.WalkDir(staticFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(staticFS, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		s.assetTags[p] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
}

func (s *Server) assetURL(name string) string {
	if tag, ok := s.assetTags[name]; ok {
		return "/static/" + name + "?v=" + tag
	}
	return "/static/" + name
}

// pageNames are the standalone templates rendered by the server.
var pageNames = []string{"home", "login", "admin", "notice"}

func (s *Server) loadTemplates() error {
	funcs := template.FuncMap{
		"asset":   s.assetURL,
		"baseURL": func() string { return s.cfg.BaseURL },
		"year":    func() int { return time.Now().Year() },
	}
	s.pages = make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New("layout.html").Funcs(funcs).
			ParseFS(web.FS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("server: parse template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

// Handler builds the routing table wrapped in the global middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public surface.
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /robots.txt", s.handleRobots)
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.staticHandler()))
	mux.HandleFunc("GET /qr.png", s.handlePublicRender)
	mux.HandleFunc("GET /qr.svg", s.handlePublicRender)

	// Administrator console.
	mux.HandleFunc("GET /adminpage", s.handleAdminPage)
	mux.HandleFunc("GET /adminpage/", s.handleAdminRedirect)
	mux.HandleFunc("GET /adminpage/login", s.handleLoginForm)
	mux.HandleFunc("POST /adminpage/login", s.handleLogin)
	mux.HandleFunc("POST /adminpage/logout", s.requireAdmin(s.csrf(s.handleLogout)))
	// The literal preview routes take precedence over the {name} wildcard.
	mux.HandleFunc("GET /adminpage/qr/preview.png", s.requireAdmin(s.handleAdminQRPreview))
	mux.HandleFunc("GET /adminpage/qr/preview.svg", s.requireAdmin(s.handleAdminQRPreview))
	mux.HandleFunc("GET /adminpage/qr/{name}", s.requireAdmin(s.handleAdminQRFile))

	mux.HandleFunc("GET /adminpage/api/overview", s.requireAdmin(s.handleOverview))
	mux.HandleFunc("GET /adminpage/api/shorturls", s.requireAdmin(s.handleListShortURLs))
	mux.HandleFunc("POST /adminpage/api/shorturls", s.requireAdmin(s.csrf(s.handleCreateShortURL)))
	mux.HandleFunc("PATCH /adminpage/api/shorturls/{id}", s.requireAdmin(s.csrf(s.handleUpdateShortURL)))
	mux.HandleFunc("DELETE /adminpage/api/shorturls/{id}", s.requireAdmin(s.csrf(s.handleDeleteShortURL)))
	mux.HandleFunc("POST /adminpage/api/shorturls/{id}/extend", s.requireAdmin(s.csrf(s.handleExtendShortURL)))
	mux.HandleFunc("POST /adminpage/api/shorturls/{id}/qr", s.requireAdmin(s.csrf(s.handleCreateQRForShortURL)))

	mux.HandleFunc("GET /adminpage/api/qrcodes", s.requireAdmin(s.handleListQRCodes))
	mux.HandleFunc("POST /adminpage/api/qrcodes", s.requireAdmin(s.csrf(s.handleCreateQRCode)))
	mux.HandleFunc("PATCH /adminpage/api/qrcodes/{id}", s.requireAdmin(s.csrf(s.handleUpdateQRCode)))
	mux.HandleFunc("DELETE /adminpage/api/qrcodes/{id}", s.requireAdmin(s.csrf(s.handleDeleteQRCode)))
	mux.HandleFunc("POST /adminpage/api/qrcodes/{id}/extend", s.requireAdmin(s.csrf(s.handleExtendQRCode)))
	mux.HandleFunc("POST /adminpage/api/qrcodes/{id}/regenerate", s.requireAdmin(s.csrf(s.handleRegenerateQRCode)))
	mux.HandleFunc("POST /adminpage/api/cleanup", s.requireAdmin(s.csrf(s.handleRunCleanup)))

	mux.HandleFunc("GET /adminpage/api/account", s.requireAdmin(s.handleAccount))
	mux.HandleFunc("POST /adminpage/api/account/password", s.requireAdmin(s.csrf(s.handleChangePassword)))
	mux.HandleFunc("POST /adminpage/api/account/sessions/revoke", s.requireAdmin(s.csrf(s.handleRevokeSessions)))
	mux.HandleFunc("GET /adminpage/api/export/{kind}", s.requireAdmin(s.handleExport))

	// The short URL redirect must be registered last so that every literal
	// route above takes precedence.
	mux.HandleFunc("GET /{code}", s.handleRedirect)

	return s.withMiddleware(mux)
}

func (s *Server) staticHandler() http.Handler {
	fileServer := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if _, ok := s.assetTags[clean]; !ok {
			s.renderNotice(w, r, http.StatusNotFound, "찾을 수 없습니다", "요청하신 파일이 없습니다.")
			return
		}
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// renderPage executes a page template.
func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	t, ok := s.pages[name]
	if !ok {
		s.log.Error("server: unknown template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		s.log.Error("server: render template", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(buf.String()))
	}
}

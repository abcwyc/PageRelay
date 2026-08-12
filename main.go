package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
	_ "modernc.org/sqlite"
)

//go:embed web/*
var webFS embed.FS

const (
	maxUploadBytes = int64(2 << 30)
	sessionTTL     = 12 * time.Hour
)

type config struct {
	listen, shareListen, publicBase, dbPath, dataDir, adminUser, adminPassword, adminPasswordFile string
	cookieSecure                                                                                  bool
}

type app struct {
	db        *sql.DB
	cfg       config
	templates *template.Template
	writeMu   sync.Mutex
	shareMu   sync.Mutex
}

type user struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Quota     int64  `json:"quota"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"created_at"`
}

type principal struct {
	UserID   int64
	Username string
	Role     string
	CSRF     string
	Expires  time.Time
}

type share struct {
	ID        int64
	OwnerID   int64
	Token     string
	Name      string
	RelPath   string
	ExpiresAt sql.NullString
	Active    bool
	CreatedAt string
}

type fileItem struct {
	Name, Path, Type, Modified, URL string
	Size                            int64
}

type pageData struct {
	Title, Page, CSRF, Username, Role, CurrentPath, ParentPath, ShareURL, Error, Notice string
	Quota, Used                                                                         int64
	Users                                                                               []user
	Files                                                                               []fileItem
	Shares                                                                              []share
	Audit                                                                               []map[string]string
}

var usernameRx = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{2,31}$`)

func main() {
	cfg := config{}
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:8080", "management listener")
	flag.StringVar(&cfg.shareListen, "share-listen", "127.0.0.1:8081", "public share listener")
	flag.StringVar(&cfg.publicBase, "public-base", "", "public share base URL, for example http://10.0.0.2:8081")
	flag.StringVar(&cfg.dbPath, "db", "./filehub.db", "SQLite database")
	flag.StringVar(&cfg.dataDir, "data", "./data", "file storage root")
	flag.StringVar(&cfg.adminUser, "admin-user", "admin", "bootstrap administrator username")
	flag.StringVar(&cfg.adminPassword, "admin-password", "", "bootstrap administrator password")
	flag.StringVar(&cfg.adminPasswordFile, "admin-password-file", "", "read the bootstrap administrator password from a file")
	flag.BoolVar(&cfg.cookieSecure, "cookie-secure", false, "set Secure session cookies")
	flag.Parse()

	if err := os.MkdirAll(cfg.dataDir, 0750); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.dbPath), 0750); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+cfg.dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, cfg: cfg}
	a.templates = parseTemplates()
	if err := a.migrate(); err != nil {
		log.Fatal(err)
	}
	if err := a.bootstrapAdmin(); err != nil {
		log.Fatal(err)
	}

	errCh := make(chan error, 2)
	go func() {
		log.Printf("FileHub management listening on http://%s", cfg.listen)
		errCh <- serve(cfg.listen, a.managementHandler())
	}()
	go func() {
		log.Printf("FileHub shares listening on http://%s", cfg.shareListen)
		errCh <- serve(cfg.shareListen, a.publicHandler())
	}()
	log.Fatal(<-errCh)
}

func parseTemplates() *template.Template {
	return template.Must(template.New("layout.html").Funcs(template.FuncMap{
		"formatBytes": formatBytes,
		"divGB":       func(v int64) int64 { return v >> 30 },
		"pct": func(used, quota int64) int {
			if quota <= 0 {
				return 0
			}
			p := used * 100 / quota
			if p > 100 {
				return 100
			}
			return int(p)
		},
		"isAdmin": func(role string) bool { return role == "admin" },
	}).ParseFS(webFS, "web/*.html"))
}

func (a *app) managementHandler() http.Handler {
	manage := http.NewServeMux()
	manage.HandleFunc("/assets/", staticAsset)
	manage.HandleFunc("/health", a.health)
	manage.HandleFunc("/login", a.login)
	manage.HandleFunc("/logout", a.logout)
	manage.HandleFunc("/", a.requireAuth(a.dashboard))
	manage.HandleFunc("/files", a.requireAuth(a.files))
	manage.HandleFunc("/content", a.requireAuth(a.content))
	manage.HandleFunc("/api/list", a.requireAuth(a.apiList))
	manage.HandleFunc("/api/item", a.requireAuth(a.apiItem))
	manage.HandleFunc("/api/share", a.requireAuth(a.apiShare))
	manage.HandleFunc("/api/new", a.requireAuth(a.apiNew))
	manage.HandleFunc("/api/save", a.requireAuth(a.apiSave))
	manage.HandleFunc("/api/directories", a.requireAuth(a.apiDirectories))
	manage.HandleFunc("/api/move", a.requireAuth(a.apiMove))
	manage.HandleFunc("/api/delete-batch", a.requireAuth(a.apiDeleteBatch))
	manage.HandleFunc("/api/download", a.requireAuth(a.apiDownload))
	manage.HandleFunc("/upload", a.requireAuth(a.upload))
	manage.HandleFunc("/mkdir", a.requireAuth(a.mkdir))
	manage.HandleFunc("/rename", a.requireAuth(a.renameFile))
	manage.HandleFunc("/delete", a.requireAuth(a.deleteFile))
	manage.HandleFunc("/shares", a.requireAuth(a.shares))
	manage.HandleFunc("/shares/create", a.requireAuth(a.createShare))
	manage.HandleFunc("/shares/revoke", a.requireAuth(a.revokeShare))
	manage.HandleFunc("/admin/users", a.requireAdmin(a.adminUsers))
	manage.HandleFunc("/admin/users/create", a.requireAdmin(a.createUser))
	manage.HandleFunc("/admin/users/update", a.requireAdmin(a.updateUser))
	manage.HandleFunc("/admin/audit", a.requireAdmin(a.adminAudit))
	return securityHeaders(false, manage)
}

func (a *app) publicHandler() http.Handler {
	public := http.NewServeMux()
	public.HandleFunc("/assets/", staticAsset)
	public.HandleFunc("/health", a.health)
	public.HandleFunc("/s/", a.publicShare)
	public.HandleFunc("/p/", a.publicPreview)
	return securityHeaders(true, public)
}

func serve(addr string, handler http.Handler) error {
	s := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	return s.ListenAndServe()
}

func securityHeaders(public bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if !public {
			w.Header().Set("X-Frame-Options", "SAMEORIGIN")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self' data:; media-src 'self'; frame-src 'self' http: https:; object-src 'none'; base-uri 'none'; frame-ancestors 'self'")
		}
		next.ServeHTTP(w, r)
	})
}

func staticAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if name != "app.css" && name != "app.js" && name != "hello-rental-logo.png" && name != "mermaid.min.js" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(name, ".png") {
		w.Header().Set("Content-Type", "image/png")
	} else {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

func (a *app) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE COLLATE NOCASE, password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','user')), quota_bytes INTEGER NOT NULL DEFAULT 10737418240, disabled INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, csrf TEXT NOT NULL, expires_at TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS shares (id INTEGER PRIMARY KEY AUTOINCREMENT, owner_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, token_hash TEXT NOT NULL UNIQUE, token_value TEXT NOT NULL, name TEXT NOT NULL, rel_path TEXT NOT NULL, expires_at TEXT, active INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER REFERENCES users(id) ON DELETE SET NULL, action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS preview_tokens (token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, rel_path TEXT NOT NULL, expires_at TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user_expires ON sessions(user_id, expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_shares_owner_active ON shares(owner_id, active)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_preview_tokens_expires ON preview_tokens(expires_at)`,
	}
	for _, stmt := range stmts {
		if _, err := a.db.Exec(stmt); err != nil {
			return err
		}
	}
	_, _ = a.db.Exec(`DELETE FROM sessions WHERE expires_at < datetime('now')`)
	_, _ = a.db.Exec(`DELETE FROM preview_tokens WHERE expires_at < datetime('now')`)
	// FileHub 2.0 uses a simple enabled/disabled share state. Keep every
	// previously active link valid and leave revoked links revoked.
	_, _ = a.db.Exec(`UPDATE shares SET expires_at=NULL WHERE active=1`)
	_, _ = a.db.Exec(`PRAGMA optimize`)
	return nil
}

func (a *app) bootstrapAdmin() error {
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	password := a.cfg.adminPassword
	if password == "" && a.cfg.adminPasswordFile != "" {
		contents, err := os.ReadFile(a.cfg.adminPasswordFile)
		if err != nil {
			return fmt.Errorf("read bootstrap password file: %w", err)
		}
		password = strings.TrimRight(string(contents), "\r\n")
	}
	if password == "" {
		password = randomToken(18)
		log.Printf("BOOTSTRAP ADMIN PASSWORD (shown once): %s", password)
	}
	if len(password) < 10 {
		return errors.New("bootstrap administrator password must contain at least 10 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	res, err := a.db.Exec(`INSERT INTO users(username,password_hash,role,quota_bytes) VALUES(?,?, 'admin', ?)`, a.cfg.adminUser, hash, int64(100<<30))
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	return os.MkdirAll(a.userRoot(id), 0750)
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "ok\n")
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.render(w, "login", pageData{Title: "登录", Page: "login"})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil {
		badRequest(w, "请求无效")
		return
	}
	var u user
	var hash string
	var disabled int
	err := a.db.QueryRow(`SELECT id,username,password_hash,role,quota_bytes,disabled,created_at FROM users WHERE username=?`, strings.TrimSpace(r.FormValue("username"))).Scan(&u.ID, &u.Username, &hash, &u.Role, &u.Quota, &disabled, &u.CreatedAt)
	if err != nil || disabled != 0 || !verifyPassword(hash, r.FormValue("password")) {
		a.audit(0, "login_failed", strings.TrimSpace(r.FormValue("username")), clientIP(r))
		time.Sleep(350 * time.Millisecond)
		a.renderStatus(w, http.StatusUnauthorized, "login", pageData{Title: "登录", Page: "login", Error: "用户名或密码不正确"})
		return
	}
	token, csrf := randomToken(32), randomToken(24)
	expires := time.Now().Add(sessionTTL).UTC()
	if _, err := a.db.Exec(`INSERT INTO sessions(token_hash,user_id,csrf,expires_at) VALUES(?,?,?,?)`, tokenHash(token), u.ID, csrf, expires.Format("2006-01-02 15:04:05")); err != nil {
		serverError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "filehub_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.cfg.cookieSecure, Expires: expires})
	a.audit(u.ID, "login", "", clientIP(r))
	http.Redirect(w, r, "/files", http.StatusSeeOther)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	p, ok := a.authenticate(r)
	if !ok || !a.checkCSRF(r, p) {
		badRequest(w, "请求已过期")
		return
	}
	if c, err := r.Cookie("filehub_session"); err == nil {
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "filehub_session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteStrictMode, Secure: a.cfg.cookieSecure})
	a.audit(p.UserID, "logout", "", clientIP(r))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) authenticate(r *http.Request) (principal, bool) {
	c, err := r.Cookie("filehub_session")
	if err != nil || len(c.Value) < 32 {
		return principal{}, false
	}
	var p principal
	var expires string
	var disabled int
	err = a.db.QueryRow(`SELECT u.id,u.username,u.role,s.csrf,s.expires_at,u.disabled FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=?`, tokenHash(c.Value)).Scan(&p.UserID, &p.Username, &p.Role, &p.CSRF, &expires, &disabled)
	if err != nil || disabled != 0 {
		return principal{}, false
	}
	p.Expires, _ = time.Parse("2006-01-02 15:04:05", expires)
	if time.Now().UTC().After(p.Expires) {
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash(c.Value))
		return principal{}, false
	}
	return p, true
}

func (a *app) requireAuth(next func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.authenticate(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r, p)
	}
}

func (a *app) requireAdmin(next func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return a.requireAuth(func(w http.ResponseWriter, r *http.Request, p principal) {
		if p.Role != "admin" {
			http.Error(w, "禁止访问", http.StatusForbidden)
			return
		}
		next(w, r, p)
	})
}

func (a *app) checkCSRF(r *http.Request, p principal) bool {
	return r.FormValue("csrf") != "" && hmac.Equal([]byte(r.FormValue("csrf")), []byte(p.CSRF))
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request, p principal) {
	http.Redirect(w, r, "/files", http.StatusSeeOther)
}

func (a *app) files(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	rel, err := cleanRel(r.URL.Query().Get("path"))
	if err != nil {
		badRequest(w, "目录无效")
		return
	}
	root := a.userRoot(p.UserID)
	full, err := safeJoin(root, rel)
	if err != nil {
		badRequest(w, "目录无效")
		return
	}
	info, err := os.Stat(full)
	if err != nil || !info.IsDir() {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			serverError(w, err)
		}
		return
	}
	used := dirSize(root)
	quota := a.userQuota(p.UserID)
	parent := ""
	if rel != "" {
		parent = path.Dir(rel)
		if parent == "." {
			parent = ""
		}
	}
	a.render(w, "files", pageData{Title: "文件工作台", Page: "files", CSRF: p.CSRF, Username: p.Username, Role: p.Role, CurrentPath: rel, ParentPath: parent, Used: used, Quota: quota})
}

func (a *app) content(w http.ResponseWriter, r *http.Request, p principal) {
	rel, err := cleanRel(r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		badRequest(w, "路径无效")
		return
	}
	full, err := safeJoin(a.userRoot(p.UserID), rel)
	if err != nil {
		badRequest(w, "路径无效")
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	a.audit(p.UserID, "content_read", rel, clientIP(r))
	servePrivateFile(w, r, full, info)
}

func servePrivateFile(w http.ResponseWriter, r *http.Request, file string, info fs.FileInfo) {
	ext := strings.ToLower(filepath.Ext(file))
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	// Never execute user-authored HTML inside the authenticated management origin.
	if ext == ".html" || ext == ".htm" || ext == ".svg" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.Name()))
	}
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func (a *app) upload(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		badRequest(w, "上传内容过大或格式错误")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if !a.checkCSRF(r, p) {
		badRequest(w, "请求已过期")
		return
	}
	rel, err := cleanRel(r.FormValue("path"))
	if err != nil {
		badRequest(w, "目录无效")
		return
	}
	root := a.userRoot(p.UserID)
	destDir, err := safeJoin(root, rel)
	if err != nil {
		badRequest(w, "目录无效")
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		badRequest(w, "没有选择文件")
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	used := dirSize(root)
	quota := a.userQuota(p.UserID)
	for _, fh := range files {
		name := filepath.Base(strings.ReplaceAll(fh.Filename, "\\", "/"))
		if name == "." || name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		src, err := fh.Open()
		if err != nil {
			serverError(w, err)
			return
		}
		dest, err := safeJoin(destDir, name)
		if err != nil {
			src.Close()
			badRequest(w, "文件名无效")
			return
		}
		var oldSize int64
		if oldInfo, statErr := os.Stat(dest); statErr == nil {
			if oldInfo.IsDir() {
				src.Close()
				badRequest(w, "同名目录已存在")
				return
			}
			oldSize = oldInfo.Size()
		}
		if used-oldSize+fh.Size > quota {
			src.Close()
			http.Error(w, "存储配额不足", http.StatusRequestEntityTooLarge)
			return
		}
		tmp := dest + ".upload-" + randomToken(6)
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if err != nil {
			src.Close()
			serverError(w, err)
			return
		}
		n, copyErr := io.Copy(out, src)
		closeErr := out.Close()
		src.Close()
		if copyErr != nil || closeErr != nil {
			os.Remove(tmp)
			serverError(w, firstErr(copyErr, closeErr))
			return
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			serverError(w, err)
			return
		}
		used = used - oldSize + n
		uploadedPath := path.Join(rel, name)
		if _, err := a.ensureShare(p.UserID, uploadedPath); err != nil {
			serverError(w, err)
			return
		}
		a.audit(p.UserID, "upload", uploadedPath, clientIP(r))
	}
	a.writeSuccess(w, r, rel)
}

func (a *app) mkdir(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.ParseForm()
	if !a.checkCSRF(r, p) {
		badRequest(w, "请求已过期")
		return
	}
	rel, err := cleanRel(r.FormValue("path"))
	if err != nil {
		badRequest(w, "目录无效")
		return
	}
	name := filepath.Base(strings.TrimSpace(r.FormValue("name")))
	if name == "." || name == "" || strings.HasPrefix(name, ".") {
		badRequest(w, "目录名无效")
		return
	}
	base, _ := safeJoin(a.userRoot(p.UserID), rel)
	dest, err := safeJoin(base, name)
	if err != nil {
		badRequest(w, "目录名无效")
		return
	}
	if err := os.Mkdir(dest, 0750); err != nil {
		badRequest(w, "目录已存在或无法创建")
		return
	}
	a.audit(p.UserID, "mkdir", path.Join(rel, name), clientIP(r))
	a.writeSuccess(w, r, rel)
}

func (a *app) renameFile(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	_ = r.ParseForm()
	if !a.checkCSRF(r, p) {
		badRequest(w, "请求已过期")
		return
	}
	rel, err := cleanRel(r.FormValue("path"))
	if err != nil || rel == "" {
		badRequest(w, "路径无效")
		return
	}
	newName := filepath.Base(strings.TrimSpace(r.FormValue("name")))
	if newName == "." || newName == "" || strings.HasPrefix(newName, ".") {
		badRequest(w, "新名称无效")
		return
	}
	root := a.userRoot(p.UserID)
	source, err := safeJoin(root, rel)
	if err != nil {
		badRequest(w, "路径无效")
		return
	}
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	base, _ := safeJoin(root, parent)
	dest, err := safeJoin(base, newName)
	if err != nil {
		badRequest(w, "新名称无效")
		return
	}
	if _, err := os.Stat(dest); err == nil {
		badRequest(w, "同名文件或目录已存在")
		return
	}
	if err := os.Rename(source, dest); err != nil {
		serverError(w, err)
		return
	}
	newRel := path.Join(parent, newName)
	_, _ = a.db.Exec(`UPDATE shares SET rel_path = ? || substr(rel_path, length(?) + 1), name = CASE WHEN rel_path = ? THEN ? ELSE name END WHERE owner_id=? AND (rel_path=? OR rel_path LIKE ?)`, newRel, rel, rel, newName, p.UserID, rel, rel+"/%")
	a.audit(p.UserID, "rename", rel+" -> "+newRel, clientIP(r))
	a.writeSuccess(w, r, parent)
}

func (a *app) deleteFile(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.ParseForm()
	if !a.checkCSRF(r, p) {
		badRequest(w, "请求已过期")
		return
	}
	rel, err := cleanRel(r.FormValue("path"))
	if err != nil || rel == "" {
		badRequest(w, "路径无效")
		return
	}
	full, err := safeJoin(a.userRoot(p.UserID), rel)
	if err != nil {
		badRequest(w, "路径无效")
		return
	}
	if err := os.RemoveAll(full); err != nil {
		serverError(w, err)
		return
	}
	_, _ = a.db.Exec(`UPDATE shares SET active=0 WHERE owner_id=? AND (rel_path=? OR rel_path LIKE ?)`, p.UserID, rel, rel+"/%")
	a.audit(p.UserID, "delete", rel, clientIP(r))
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	a.writeSuccess(w, r, parent)
}

func (a *app) shares(w http.ResponseWriter, r *http.Request, p principal) {
	rows, err := a.db.Query(`SELECT id,owner_id,token_value,name,rel_path,expires_at,active,created_at FROM shares WHERE owner_id=? ORDER BY id DESC`, p.UserID)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	var list []share
	for rows.Next() {
		var s share
		rows.Scan(&s.ID, &s.OwnerID, &s.Token, &s.Name, &s.RelPath, &s.ExpiresAt, &s.Active, &s.CreatedAt)
		list = append(list, s)
	}
	a.render(w, "shares", pageData{Title: "分享链接", Page: "shares", CSRF: p.CSRF, Username: p.Username, Role: p.Role, Shares: list, ShareURL: a.shareBase(r)})
}

func (a *app) createShare(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.ParseForm()
	if !a.checkCSRF(r, p) {
		badRequest(w, "请求已过期")
		return
	}
	rel, err := cleanRel(r.FormValue("path"))
	if err != nil || rel == "" {
		badRequest(w, "请选择文件或目录")
		return
	}
	full, err := safeJoin(a.userRoot(p.UserID), rel)
	if err != nil {
		badRequest(w, "路径无效")
		return
	}
	if _, err := os.Stat(full); err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err = a.ensureShare(p.UserID, rel); err != nil {
		serverError(w, err)
		return
	}
	a.audit(p.UserID, "share_create", rel, clientIP(r))
	http.Redirect(w, r, "/shares", http.StatusSeeOther)
}

func (a *app) revokeShare(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.ParseForm()
	if !a.checkCSRF(r, p) {
		badRequest(w, "请求已过期")
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	_, err := a.db.Exec(`UPDATE shares SET active=0 WHERE id=? AND owner_id=?`, id, p.UserID)
	if err != nil {
		serverError(w, err)
		return
	}
	a.audit(p.UserID, "share_revoke", strconv.FormatInt(id, 10), clientIP(r))
	http.Redirect(w, r, "/shares", http.StatusSeeOther)
}

func (a *app) publicShare(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/s/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || len(parts[0]) < 20 {
		http.NotFound(w, r)
		return
	}
	var s share
	err := a.db.QueryRow(`SELECT id,owner_id,token_value,name,rel_path,expires_at,active,created_at FROM shares WHERE token_hash=?`, tokenHash(parts[0])).Scan(&s.ID, &s.OwnerID, &s.Token, &s.Name, &s.RelPath, &s.ExpiresAt, &s.Active, &s.CreatedAt)
	if err != nil || !s.Active {
		http.NotFound(w, r)
		return
	}
	if s.ExpiresAt.Valid {
		expires, _ := time.Parse("2006-01-02 15:04:05", s.ExpiresAt.String)
		if time.Now().UTC().After(expires) {
			http.NotFound(w, r)
			return
		}
	}
	base, err := safeJoin(a.userRoot(s.OwnerID), s.RelPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(base)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !info.IsDir() {
		if len(parts) > 1 && parts[1] != "" {
			http.NotFound(w, r)
			return
		}
		if detectPreview(s.RelPath, false) == "markdown" {
			a.serveMarkdownPreview(w, r, base, info.Name(), "/s/"+parts[0]+"/")
			return
		}
		serveSharedFile(w, r, base, info)
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	rel, err := cleanRel(sub)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target, err := safeJoin(base, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	info, err = os.Stat(target)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !info.IsDir() {
		serveSharedFile(w, r, target, info)
		return
	}
	index := filepath.Join(target, "index.html")
	if idx, err := os.Stat(index); err == nil && !idx.IsDir() {
		serveSharedFile(w, r, index, idx)
		return
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		serverError(w, err)
		return
	}
	var items []fileItem
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		typ := "file"
		if e.IsDir() {
			typ = "dir"
		}
		href := path.Join("/s", parts[0], rel, e.Name())
		if e.IsDir() {
			href += "/"
		}
		items = append(items, fileItem{Name: e.Name(), Type: typ, Size: fi.Size(), Modified: fi.ModTime().Format("2006-01-02 15:04"), URL: href})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Type != items[j].Type {
			return items[i].Type == "dir"
		}
		return items[i].Name < items[j].Name
	})
	a.render(w, "public", pageData{Title: s.Name, Page: "public", Files: items, CurrentPath: rel})
}

func serveSharedFile(w http.ResponseWriter, r *http.Request, file string, info fs.FileInfo) {
	ext := strings.ToLower(filepath.Ext(file))
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	if ext == ".html" || ext == ".htm" || ext == ".svg" {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-same-origin; default-src 'self' data: blob:; style-src 'self' 'unsafe-inline' data:; script-src 'self' 'unsafe-inline' 'unsafe-eval'; img-src 'self' data: blob:; media-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'")
	}
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func (a *app) adminUsers(w http.ResponseWriter, r *http.Request, p principal) {
	rows, err := a.db.Query(`SELECT id,username,role,quota_bytes,disabled,created_at FROM users ORDER BY id`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	var users []user
	for rows.Next() {
		var u user
		var dis int
		rows.Scan(&u.ID, &u.Username, &u.Role, &u.Quota, &dis, &u.CreatedAt)
		u.Disabled = dis != 0
		users = append(users, u)
	}
	a.render(w, "users", pageData{Title: "账号管理", Page: "users", CSRF: p.CSRF, Username: p.Username, Role: p.Role, Users: users})
}

func (a *app) createUser(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.ParseForm()
	if !a.checkCSRF(r, p) {
		badRequest(w, "请求已过期")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	role := r.FormValue("role")
	if !usernameRx.MatchString(username) || len(password) < 10 || (role != "user" && role != "admin") {
		badRequest(w, "账号格式无效，密码至少 10 位")
		return
	}
	quotaGB, _ := strconv.ParseInt(r.FormValue("quota_gb"), 10, 64)
	if quotaGB < 1 || quotaGB > 1000 {
		quotaGB = 10
	}
	hash, err := hashPassword(password)
	if err != nil {
		serverError(w, err)
		return
	}
	res, err := a.db.Exec(`INSERT INTO users(username,password_hash,role,quota_bytes) VALUES(?,?,?,?)`, username, hash, role, quotaGB<<30)
	if err != nil {
		badRequest(w, "用户名已存在")
		return
	}
	id, _ := res.LastInsertId()
	if err := os.MkdirAll(a.userRoot(id), 0750); err != nil {
		serverError(w, err)
		return
	}
	a.audit(p.UserID, "user_create", username, clientIP(r))
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (a *app) updateUser(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.ParseForm()
	if !a.checkCSRF(r, p) {
		badRequest(w, "请求已过期")
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	disabled := 0
	if r.FormValue("disabled") == "1" {
		disabled = 1
	}
	quotaGB, _ := strconv.ParseInt(r.FormValue("quota_gb"), 10, 64)
	if quotaGB < 1 || quotaGB > 1000 {
		badRequest(w, "配额无效")
		return
	}
	if id == p.UserID && disabled == 1 {
		badRequest(w, "不能禁用当前管理员")
		return
	}
	if pass := r.FormValue("password"); pass != "" {
		if len(pass) < 10 {
			badRequest(w, "密码至少 10 位")
			return
		}
		hash, _ := hashPassword(pass)
		_, _ = a.db.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hash, id)
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	}
	_, err := a.db.Exec(`UPDATE users SET quota_bytes=?,disabled=? WHERE id=?`, quotaGB<<30, disabled, id)
	if err != nil {
		serverError(w, err)
		return
	}
	if disabled == 1 {
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
		_, _ = a.db.Exec(`UPDATE shares SET active=0 WHERE owner_id=?`, id)
	}
	a.audit(p.UserID, "user_update", strconv.FormatInt(id, 10), clientIP(r))
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (a *app) adminAudit(w http.ResponseWriter, r *http.Request, p principal) {
	rows, err := a.db.Query(`SELECT COALESCE(u.username,'-'),a.action,a.target,a.ip,a.created_at FROM audit_logs a LEFT JOIN users u ON u.id=a.user_id ORDER BY a.id DESC LIMIT 200`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	var logs []map[string]string
	for rows.Next() {
		var u, act, target, ip, t string
		rows.Scan(&u, &act, &target, &ip, &t)
		logs = append(logs, map[string]string{"user": u, "action": act, "target": target, "ip": ip, "time": t})
	}
	a.render(w, "audit", pageData{Title: "审计日志", Page: "audit", CSRF: p.CSRF, Username: p.Username, Role: p.Role, Audit: logs})
}

func (a *app) render(w http.ResponseWriter, name string, data pageData) {
	a.renderStatus(w, http.StatusOK, name, data)
}
func (a *app) renderStatus(w http.ResponseWriter, status int, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.templates.ExecuteTemplate(w, "layout.html", map[string]any{"View": name, "Data": data}); err != nil {
		log.Printf("template: %v", err)
	}
}
func (a *app) audit(userID int64, action, target, ip string) {
	var uid any = nil
	if userID > 0 {
		uid = userID
	}
	_, _ = a.db.Exec(`INSERT INTO audit_logs(user_id,action,target,ip) VALUES(?,?,?,?)`, uid, action, target, ip)
}
func (a *app) userRoot(id int64) string {
	return filepath.Join(a.cfg.dataDir, "users", strconv.FormatInt(id, 10))
}
func (a *app) userQuota(id int64) int64 {
	var q int64
	_ = a.db.QueryRow(`SELECT quota_bytes FROM users WHERE id=?`, id).Scan(&q)
	return q
}

func hashPassword(password string) (string, error) {
	salt := mustRandom(16)
	sum := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("argon2id$v=19$m=65536,t=3,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum)), nil
}
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[3])
	expected, err2 := base64.RawStdEncoding.DecodeString(parts[4])
	if err1 != nil || err2 != nil {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, uint32(len(expected)))
	return hmac.Equal(actual, expected)
}
func mustRandom(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func randomToken(n int) string  { return base64.RawURLEncoding.EncodeToString(mustRandom(n)) }
func tokenHash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func cleanRel(s string) (string, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\\", "/")
	s = strings.TrimPrefix(s, "/")
	c := path.Clean(s)
	if c == "." {
		return "", nil
	}
	if c == ".." || strings.HasPrefix(c, "../") || strings.Contains(c, "\x00") {
		return "", errors.New("invalid path")
	}
	return c, nil
}
func safeJoin(root, rel string) (string, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	r, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	f, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if f != r && !strings.HasPrefix(f, r+string(os.PathSeparator)) {
		return "", errors.New("path escape")
	}
	cur := f
	for {
		info, err := os.Lstat(cur)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink forbidden")
		}
		if cur == r || filepath.Dir(cur) == cur {
			break
		}
		cur = filepath.Dir(cur)
	}
	return f, nil
}
func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, e := d.Info(); e == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
func formatBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
func (a *app) shareBase(r *http.Request) string {
	if a.cfg.publicBase != "" {
		return strings.TrimRight(a.cfg.publicBase, "/") + "/s/"
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = strings.Split(v, ",")[0]
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		_, port, _ := net.SplitHostPort(a.cfg.shareListen)
		host = net.JoinHostPort(h, port)
	}
	return scheme + "://" + host + "/s/"
}
func clientIP(r *http.Request) string {
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		return strings.TrimSpace(strings.Split(x, ",")[0])
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return h
	}
	return r.RemoteAddr
}
func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
func badRequest(w http.ResponseWriter, msg string) { http.Error(w, msg, http.StatusBadRequest) }
func serverError(w http.ResponseWriter, err error) {
	log.Printf("server error: %v", err)
	http.Error(w, "服务器内部错误", http.StatusInternalServerError)
}

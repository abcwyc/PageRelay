package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const testAdminPassword = "correct-horse-battery-staple"

func newTestApp(t *testing.T) *app {
	t.Helper()
	root := t.TempDir()
	cfg := config{
		dbPath:        filepath.Join(root, "filehub.db"),
		dataDir:       filepath.Join(root, "data"),
		adminUser:     "admin",
		adminPassword: testAdminPassword,
		publicBase:    "http://share.test",
	}
	if err := os.MkdirAll(cfg.dataDir, 0750); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+cfg.dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := &app{db: db, cfg: cfg, templates: parseTemplates()}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	if err := a.bootstrapAdmin(); err != nil {
		t.Fatal(err)
	}
	return a
}

func loginAs(t *testing.T, h http.Handler, username, password string) *http.Cookie {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, body = %s", w.Code, w.Body.String())
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "filehub_session" {
			return cookie
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func requestWithCookie(method, target string, body io.Reader, cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(method, target, body)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

func uploadFile(t *testing.T, h http.Handler, cookie *http.Cookie, csrf, name, contents string) *httptest.ResponseRecorder {
	return uploadFileWithName(t, h, cookie, csrf, name, contents)
}

// uploadFileWithName sends a multipart upload where relPath is provided
// via the parallel paths[] field to simulate a folder upload that
// preserves structure. The on-wire filename is the base name.
func uploadFileWithName(t *testing.T, h http.Handler, cookie *http.Cookie, csrf, relPath, contents string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("path", ""); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("paths", relPath); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("files", filepath.Base(relPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, contents); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := requestWithCookie(http.MethodPost, "/upload", &body, cookie)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func postForm(t *testing.T, h http.Handler, target string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := requestWithCookie(http.MethodPost, target, strings.NewReader(values.Encode()), cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestPasswordHashAndPaths(t *testing.T) {
	hash, err := hashPassword(testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(hash, testAdminPassword) || verifyPassword(hash, "wrong-password") {
		t.Fatal("password verification mismatch")
	}
	if _, err := cleanRel("../../etc/passwd"); err == nil {
		t.Fatal("path traversal was accepted")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeJoin(root, "escape/secret"); err == nil {
		t.Fatal("symlink escape was accepted")
	}
}

func TestAccountIsolationUploadAndPublicShare(t *testing.T) {
	a := newTestApp(t)
	manage := a.managementHandler()
	public := a.publicHandler()

	loginPage := httptest.NewRecorder()
	manage.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, "/login", nil))
	if loginPage.Code != http.StatusOK || !strings.Contains(loginPage.Body.String(), "登录") {
		t.Fatalf("login page status=%d", loginPage.Code)
	}
	if !strings.Contains(loginPage.Body.String(), `id="login-form"`) || !strings.Contains(loginPage.Body.String(), `type="submit"`) {
		t.Fatal("login form is missing keyboard-submit semantics")
	}
	if !strings.Contains(loginPage.Body.String(), "PageRelay") || !strings.Contains(loginPage.Body.String(), "将本地文件快速变成可分享的网页。") {
		t.Fatal("Chinese login page is missing the PageRelay name or slogan")
	}

	englishRequest := httptest.NewRequest(http.MethodGet, "/login", nil)
	englishRequest.Header.Set("Accept-Language", "en-US,en;q=0.9")
	englishPage := httptest.NewRecorder()
	manage.ServeHTTP(englishPage, englishRequest)
	if englishPage.Code != http.StatusOK || !strings.Contains(englishPage.Body.String(), `lang="en"`) ||
		!strings.Contains(englishPage.Body.String(), "Turn local files into shareable web pages.") ||
		!strings.Contains(englishPage.Body.String(), ">Sign in</button>") {
		t.Fatalf("English login page is not localized: status=%d", englishPage.Code)
	}

	adminCookie := loginAs(t, manage, "admin", testAdminPassword)
	adminPrincipal, ok := a.authenticate(requestWithCookie(http.MethodGet, "/files", nil, adminCookie))
	if !ok || adminPrincipal.Role != "admin" {
		t.Fatal("admin session was not authenticated")
	}

	// Overwriting a file with the same size must account for the replaced bytes.
	if _, err := a.db.Exec(`UPDATE users SET quota_bytes=5 WHERE id=?`, adminPrincipal.UserID); err != nil {
		t.Fatal(err)
	}
	if got := uploadFile(t, manage, adminCookie, adminPrincipal.CSRF, "note.txt", "12345"); got.Code != http.StatusSeeOther {
		t.Fatalf("first upload status=%d body=%s", got.Code, got.Body.String())
	}
	if got := uploadFile(t, manage, adminCookie, adminPrincipal.CSRF, "note.txt", "abcde"); got.Code != http.StatusSeeOther {
		t.Fatalf("overwrite upload status=%d body=%s", got.Code, got.Body.String())
	}

	if _, err := a.db.Exec(`UPDATE users SET quota_bytes=? WHERE id=?`, int64(1<<30), adminPrincipal.UserID); err != nil {
		t.Fatal(err)
	}
	htmlBody := "<!doctype html><title>Shared page</title><h1>Hello</h1>"
	if got := uploadFile(t, manage, adminCookie, adminPrincipal.CSRF, "page.html", htmlBody); got.Code != http.StatusSeeOther {
		t.Fatalf("HTML upload status=%d body=%s", got.Code, got.Body.String())
	}

	privateReq := requestWithCookie(http.MethodGet, "/content?path=page.html", nil, adminCookie)
	privateRec := httptest.NewRecorder()
	manage.ServeHTTP(privateRec, privateReq)
	if privateRec.Code != http.StatusOK || !strings.HasPrefix(privateRec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("private HTML must download, status=%d disposition=%q", privateRec.Code, privateRec.Header().Get("Content-Disposition"))
	}

	createShare := postForm(t, manage, "/shares/create", url.Values{
		"csrf": {adminPrincipal.CSRF}, "path": {"page.html"}, "days": {"7"},
	}, adminCookie)
	if createShare.Code != http.StatusSeeOther {
		t.Fatalf("create share status=%d body=%s", createShare.Code, createShare.Body.String())
	}
	var shareID int64
	var token string
	if err := a.db.QueryRow(`SELECT id,token_value FROM shares WHERE owner_id=? AND rel_path='page.html'`, adminPrincipal.UserID).Scan(&shareID, &token); err != nil {
		t.Fatal(err)
	}
	publicRec := httptest.NewRecorder()
	public.ServeHTTP(publicRec, httptest.NewRequest(http.MethodGet, "/s/"+token+"/", nil))
	if publicRec.Code != http.StatusOK || !strings.Contains(publicRec.Body.String(), "Shared page") {
		t.Fatalf("public share status=%d body=%s", publicRec.Code, publicRec.Body.String())
	}
	if !strings.Contains(publicRec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("shared HTML was not sandboxed")
	}

	createUser := postForm(t, manage, "/admin/users/create", url.Values{
		"csrf": {adminPrincipal.CSRF}, "username": {"alice"}, "password": {"alice-password-2026"}, "role": {"user"}, "quota_gb": {"1"},
	}, adminCookie)
	if createUser.Code != http.StatusSeeOther {
		t.Fatalf("create user status=%d body=%s", createUser.Code, createUser.Body.String())
	}
	aliceCookie := loginAs(t, manage, "alice", "alice-password-2026")
	aliceRead := httptest.NewRecorder()
	manage.ServeHTTP(aliceRead, requestWithCookie(http.MethodGet, "/content?path=page.html", nil, aliceCookie))
	if aliceRead.Code != http.StatusNotFound {
		t.Fatalf("another account read private file, status=%d", aliceRead.Code)
	}
	var aliceID int64
	if err := a.db.QueryRow(`SELECT id FROM users WHERE username='alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO shares(owner_id,token_hash,token_value,name,rel_path) VALUES(?,?,?,?,?)`, aliceID, tokenHash("alice-share-token"), "alice-share-token", "example", "example"); err != nil {
		t.Fatal(err)
	}
	disableAlice := postForm(t, manage, "/admin/users/update", url.Values{
		"csrf": {adminPrincipal.CSRF}, "id": {"2"}, "quota_gb": {"1"}, "disabled": {"1"},
	}, adminCookie)
	if disableAlice.Code != http.StatusSeeOther {
		t.Fatalf("disable user status=%d body=%s", disableAlice.Code, disableAlice.Body.String())
	}
	var aliceShareActive int
	if err := a.db.QueryRow(`SELECT active FROM shares WHERE owner_id=?`, aliceID).Scan(&aliceShareActive); err != nil {
		t.Fatal(err)
	}
	if aliceShareActive != 0 {
		t.Fatal("disabling an account did not revoke its shares")
	}
	if _, ok := a.authenticate(requestWithCookie(http.MethodGet, "/files", nil, aliceCookie)); ok {
		t.Fatal("disabled account retained an active session")
	}

	revoke := postForm(t, manage, "/shares/revoke", url.Values{
		"csrf": {adminPrincipal.CSRF}, "id": {strconv.FormatInt(shareID, 10)},
	}, adminCookie)
	if revoke.Code != http.StatusSeeOther {
		t.Fatalf("revoke share status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	var active int
	if err := a.db.QueryRow(`SELECT active FROM shares WHERE id=?`, shareID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatal("share was not revoked")
	}
	revokedRec := httptest.NewRecorder()
	public.ServeHTTP(revokedRec, httptest.NewRequest(http.MethodGet, "/s/"+token, nil))
	if revokedRec.Code != http.StatusNotFound {
		t.Fatalf("revoked link status=%d", revokedRec.Code)
	}
}

func TestWriteRequiresCSRF(t *testing.T) {
	a := newTestApp(t)
	manage := a.managementHandler()
	cookie := loginAs(t, manage, "admin", testAdminPassword)
	w := postForm(t, manage, "/mkdir", url.Values{"name": {"should-not-exist"}}, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("write without CSRF status=%d", w.Code)
	}
}

func TestWorkspaceAPIsPermanentShareAndPreview(t *testing.T) {
	a := newTestApp(t)
	manage := a.managementHandler()
	public := a.publicHandler()
	cookie := loginAs(t, manage, "admin", testAdminPassword)
	p, ok := a.authenticate(requestWithCookie(http.MethodGet, "/files", nil, cookie))
	if !ok {
		t.Fatal("admin session was not authenticated")
	}
	workspacePage := httptest.NewRecorder()
	manage.ServeHTTP(workspacePage, requestWithCookie(http.MethodGet, "/files", nil, cookie))
	if workspacePage.Code != http.StatusOK || !strings.Contains(workspacePage.Body.String(), "<span>操作</span>") ||
		!strings.Contains(workspacePage.Body.String(), `id="row-action-menu"`) ||
		!strings.Contains(workspacePage.Body.String(), `id="row-rename-file"`) ||
		!strings.Contains(workspacePage.Body.String(), `id="row-delete-file"`) ||
		!strings.Contains(workspacePage.Body.String(), `id="main-upload-overlay"`) ||
		!strings.Contains(workspacePage.Body.String(), `/assets/pagerelay-icon.svg?v=2`) {
		t.Fatal("workspace file list is missing per-row file actions")
	}

	markdown := "# Preview title\n\nHello **PageRelay**.\n\n```mermaid\nflowchart LR\n  A[上传] --> B[分享]\n```\n\n<script>alert('xss')</script>\n"
	if got := uploadFile(t, manage, cookie, p.CSRF, "readme.md", markdown); got.Code != http.StatusSeeOther {
		t.Fatalf("Markdown upload status=%d body=%s", got.Code, got.Body.String())
	}

	listRec := httptest.NewRecorder()
	manage.ServeHTTP(listRec, requestWithCookie(http.MethodGet, "/api/list", nil, cookie))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list API status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var list apiListResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.LogicalPath != "/admin" || len(list.Items) != 1 || !list.Items[0].Shared {
		t.Fatalf("unexpected list response: %#v", list)
	}
	oldShareURL := list.Items[0].ShareURL

	itemRec := httptest.NewRecorder()
	manage.ServeHTTP(itemRec, requestWithCookie(http.MethodGet, "/api/item?path=readme.md", nil, cookie))
	if itemRec.Code != http.StatusOK {
		t.Fatalf("item API status=%d body=%s", itemRec.Code, itemRec.Body.String())
	}
	var item apiFileItem
	if err := json.Unmarshal(itemRec.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if item.Preview != "markdown" || item.PreviewURL == "" || item.LogicalPath != "/admin/readme.md" {
		t.Fatalf("unexpected item response: %#v", item)
	}
	previewURL, err := url.Parse(item.PreviewURL)
	if err != nil {
		t.Fatal(err)
	}
	previewRec := httptest.NewRecorder()
	public.ServeHTTP(previewRec, httptest.NewRequest(http.MethodGet, previewURL.RequestURI(), nil))
	if previewRec.Code != http.StatusOK || !strings.Contains(previewRec.Body.String(), "<h1>Preview title</h1>") {
		t.Fatalf("Markdown preview status=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	if strings.Contains(previewRec.Body.String(), "<script>alert('xss')</script>") {
		t.Fatal("Markdown preview emitted raw script content")
	}
	if !strings.Contains(previewRec.Body.String(), "table{display:block;width:100%;max-width:100%;overflow-x:auto") ||
		!strings.Contains(previewRec.Body.String(), "td code,th code{white-space:normal;overflow-wrap:anywhere") {
		t.Fatal("Markdown preview is missing responsive table styles")
	}
	if !strings.Contains(previewRec.Body.String(), `/assets/mermaid.min.js?v=11.16.1`) ||
		!strings.Contains(previewRec.Body.String(), `securityLevel:'strict'`) ||
		!strings.Contains(previewRec.Header().Get("Content-Security-Policy"), "script-src 'self' 'unsafe-inline'") {
		t.Fatal("Markdown preview is missing safe Mermaid rendering")
	}
	sharedMarkdownURL, err := url.Parse(oldShareURL)
	if err != nil {
		t.Fatal(err)
	}
	sharedMarkdownRec := httptest.NewRecorder()
	public.ServeHTTP(sharedMarkdownRec, httptest.NewRequest(http.MethodGet, sharedMarkdownURL.RequestURI(), nil))
	if sharedMarkdownRec.Code != http.StatusOK || !strings.HasPrefix(sharedMarkdownRec.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(sharedMarkdownRec.Body.String(), "<h1>Preview title</h1>") ||
		!strings.Contains(sharedMarkdownRec.Body.String(), `/assets/mermaid.min.js?v=11.16.1`) {
		t.Fatalf("shared Markdown did not open as a rendered preview: status=%d type=%q", sharedMarkdownRec.Code, sharedMarkdownRec.Header().Get("Content-Type"))
	}

	disable := postForm(t, manage, "/api/share", url.Values{
		"csrf": {p.CSRF}, "path": {"readme.md"}, "enabled": {"false"},
	}, cookie)
	if disable.Code != http.StatusOK {
		t.Fatalf("disable share status=%d body=%s", disable.Code, disable.Body.String())
	}
	oldURL, _ := url.Parse(oldShareURL)
	oldRec := httptest.NewRecorder()
	public.ServeHTTP(oldRec, httptest.NewRequest(http.MethodGet, oldURL.RequestURI(), nil))
	if oldRec.Code != http.StatusNotFound {
		t.Fatalf("disabled share status=%d", oldRec.Code)
	}

	enable := postForm(t, manage, "/api/share", url.Values{
		"csrf": {p.CSRF}, "path": {"readme.md"}, "enabled": {"true"},
	}, cookie)
	if enable.Code != http.StatusOK {
		t.Fatalf("enable share status=%d body=%s", enable.Code, enable.Body.String())
	}
	var enabledResult map[string]any
	if err := json.Unmarshal(enable.Body.Bytes(), &enabledResult); err != nil {
		t.Fatal(err)
	}
	newShareURL, _ := enabledResult["share_url"].(string)
	if newShareURL == "" || newShareURL == oldShareURL {
		t.Fatalf("re-enabled share did not receive a new URL: old=%q new=%q", oldShareURL, newShareURL)
	}
	newURL, _ := url.Parse(newShareURL)
	newRec := httptest.NewRecorder()
	public.ServeHTTP(newRec, httptest.NewRequest(http.MethodGet, newURL.RequestURI(), nil))
	if newRec.Code != http.StatusOK {
		t.Fatalf("re-enabled share status=%d body=%s", newRec.Code, newRec.Body.String())
	}

	create := postForm(t, manage, "/api/new", url.Values{
		"csrf": {p.CSRF}, "path": {""}, "kind": {"html"}, "name": {"index"},
	}, cookie)
	if create.Code != http.StatusCreated {
		t.Fatalf("create HTML status=%d body=%s", create.Code, create.Body.String())
	}
	var active int
	if err := a.db.QueryRow(`SELECT active FROM shares WHERE owner_id=? AND rel_path='index.html' ORDER BY id DESC LIMIT 1`, p.UserID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("new HTML did not default to shared: active=%d err=%v", active, err)
	}
	htmlItemRec := httptest.NewRecorder()
	manage.ServeHTTP(htmlItemRec, requestWithCookie(http.MethodGet, "/api/item?path=index.html", nil, cookie))
	var htmlItem apiFileItem
	if err := json.Unmarshal(htmlItemRec.Body.Bytes(), &htmlItem); err != nil {
		t.Fatal(err)
	}
	htmlPreviewURL, _ := url.Parse(htmlItem.PreviewURL)
	htmlPreviewRec := httptest.NewRecorder()
	public.ServeHTTP(htmlPreviewRec, httptest.NewRequest(http.MethodGet, htmlPreviewURL.RequestURI(), nil))
	if htmlPreviewRec.Code != http.StatusOK || !strings.Contains(htmlPreviewRec.Header().Get("Content-Security-Policy"), "sandbox") || !strings.Contains(htmlPreviewRec.Header().Get("Content-Security-Policy"), "allow-same-origin") {
		t.Fatalf("HTML preview status=%d CSP=%q", htmlPreviewRec.Code, htmlPreviewRec.Header().Get("Content-Security-Policy"))
	}

	logoRec := httptest.NewRecorder()
	manage.ServeHTTP(logoRec, httptest.NewRequest(http.MethodGet, "/assets/pagerelay-logo.svg", nil))
	if logoRec.Code != http.StatusOK || logoRec.Header().Get("Content-Type") != "image/svg+xml; charset=utf-8" || logoRec.Body.Len() < 200 {
		t.Fatalf("logo asset status=%d type=%q bytes=%d", logoRec.Code, logoRec.Header().Get("Content-Type"), logoRec.Body.Len())
	}
	iconRec := httptest.NewRecorder()
	manage.ServeHTTP(iconRec, httptest.NewRequest(http.MethodGet, "/assets/pagerelay-icon.svg", nil))
	if iconRec.Code != http.StatusOK || iconRec.Header().Get("Content-Type") != "image/svg+xml; charset=utf-8" || iconRec.Body.Len() < 200 {
		t.Fatalf("icon asset status=%d type=%q bytes=%d", iconRec.Code, iconRec.Header().Get("Content-Type"), iconRec.Body.Len())
	}
}

func TestMigrationKeepsActiveLegacySharesPermanent(t *testing.T) {
	a := newTestApp(t)
	var ownerID int64
	if err := a.db.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO shares(owner_id,token_hash,token_value,name,rel_path,expires_at,active) VALUES(?,?,?,?,?,datetime('now','+1 day'),1)`, ownerID, tokenHash("legacy-token"), "legacy-token", "legacy", "legacy.html"); err != nil {
		t.Fatal(err)
	}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	var expires sql.NullString
	if err := a.db.QueryRow(`SELECT expires_at FROM shares WHERE token_value='legacy-token'`).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires.Valid {
		t.Fatalf("active legacy share still expires at %q", expires.String)
	}
}

func TestBatchMoveDownloadAndDelete(t *testing.T) {
	a := newTestApp(t)
	manage := a.managementHandler()
	cookie := loginAs(t, manage, "admin", testAdminPassword)
	p, ok := a.authenticate(requestWithCookie(http.MethodGet, "/files", nil, cookie))
	if !ok {
		t.Fatal("admin session was not authenticated")
	}
	for name, body := range map[string]string{"alpha.txt": "alpha", "beta.txt": "beta"} {
		if got := uploadFile(t, manage, cookie, p.CSRF, name, body); got.Code != http.StatusSeeOther {
			t.Fatalf("upload %s status=%d body=%s", name, got.Code, got.Body.String())
		}
	}
	createDir := postForm(t, manage, "/api/new", url.Values{
		"csrf": {p.CSRF}, "path": {""}, "kind": {"folder"}, "name": {"archive"},
	}, cookie)
	if createDir.Code != http.StatusCreated {
		t.Fatalf("create directory status=%d body=%s", createDir.Code, createDir.Body.String())
	}

	pathsJSON := `["alpha.txt","beta.txt"]`
	move := postForm(t, manage, "/api/move", url.Values{
		"csrf": {p.CSRF}, "paths": {pathsJSON}, "destination": {"archive"},
	}, cookie)
	if move.Code != http.StatusOK {
		t.Fatalf("batch move status=%d body=%s", move.Code, move.Body.String())
	}
	for _, rel := range []string{"archive/alpha.txt", "archive/beta.txt"} {
		if _, err := os.Stat(filepath.Join(a.userRoot(p.UserID), filepath.FromSlash(rel))); err != nil {
			t.Fatalf("moved file %s: %v", rel, err)
		}
		var active int
		if err := a.db.QueryRow(`SELECT active FROM shares WHERE owner_id=? AND rel_path=? ORDER BY id DESC LIMIT 1`, p.UserID, rel).Scan(&active); err != nil || active != 1 {
			t.Fatalf("share path was not moved for %s: active=%d err=%v", rel, active, err)
		}
	}

	downloadReq := requestWithCookie(http.MethodGet, "/api/download?path=archive%2Falpha.txt&path=archive%2Fbeta.txt", nil, cookie)
	downloadRec := httptest.NewRecorder()
	manage.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusOK || downloadRec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("batch download status=%d type=%q", downloadRec.Code, downloadRec.Header().Get("Content-Type"))
	}
	reader, err := zip.NewReader(bytes.NewReader(downloadRec.Body.Bytes()), int64(downloadRec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 2 || reader.File[0].Name != "alpha.txt" || reader.File[1].Name != "beta.txt" {
		t.Fatalf("unexpected ZIP entries: %#v", reader.File)
	}

	deletePaths := `["archive/alpha.txt","archive/beta.txt"]`
	remove := postForm(t, manage, "/api/delete-batch", url.Values{
		"csrf": {p.CSRF}, "paths": {deletePaths},
	}, cookie)
	if remove.Code != http.StatusOK {
		t.Fatalf("batch delete status=%d body=%s", remove.Code, remove.Body.String())
	}
	for _, rel := range []string{"archive/alpha.txt", "archive/beta.txt"} {
		if _, err := os.Stat(filepath.Join(a.userRoot(p.UserID), filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("deleted file still exists: %s", rel)
		}
	}
}

// newTestAppWithDefaultAdmin bootstraps an admin with the fixed default
// credentials (no explicit password), so the forced-password-change flow
// can be exercised.
func newTestAppWithDefaultAdmin(t *testing.T) *app {
	t.Helper()
	root := t.TempDir()
	cfg := config{
		dbPath:    filepath.Join(root, "filehub.db"),
		dataDir:   filepath.Join(root, "data"),
		adminUser: "admin",
		publicBase: "http://share.test",
	}
	if err := os.MkdirAll(cfg.dataDir, 0750); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+cfg.dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := &app{db: db, cfg: cfg, templates: parseTemplates()}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	if err := a.bootstrapAdmin(); err != nil {
		t.Fatal(err)
	}
	return a
}

// loginAsRaw posts credentials and returns the recorder + session cookie so
// callers can inspect the redirect target.
func loginAsRaw(t *testing.T, h http.Handler, username, password string) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, body = %s", w.Code, w.Body.String())
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "filehub_session" {
			return w, cookie
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil, nil
}

func TestBootstrapDefaultAdminForcesPasswordChange(t *testing.T) {
	a := newTestAppWithDefaultAdmin(t)
	manage := a.managementHandler()

	// Default credentials must be admin/admin.
	rec, cookie := loginAsRaw(t, manage, "admin", "admin")
	if loc := rec.Result().Header.Get("Location"); loc != "/change-password" {
		t.Fatalf("login redirect = %q, want /change-password", loc)
	}

	// Every protected route must bounce back to /change-password until changed.
	filesRec := httptest.NewRecorder()
	manage.ServeHTTP(filesRec, requestWithCookie(http.MethodGet, "/files", nil, cookie))
	if loc := filesRec.Result().Header.Get("Location"); loc != "/change-password" {
		t.Fatalf("/files redirect = %q, want /change-password", loc)
	}

	// Wrong current password must be rejected.
	p, ok := a.authenticate(requestWithCookie(http.MethodGet, "/change-password", nil, cookie))
	if !ok || !p.MustChangePassword {
		t.Fatal("session must carry must_change_password before changing")
	}
	bad := postForm(t, manage, "/change-password", url.Values{
		"csrf": {p.CSRF}, "current": {"wrong"}, "new": {"new-strong-password"}, "confirm": {"new-strong-password"},
	}, cookie)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password status=%d", bad.Code)
	}

	// Mismatched confirmation must be rejected.
	mismatch := postForm(t, manage, "/change-password", url.Values{
		"csrf": {p.CSRF}, "current": {"admin"}, "new": {"new-strong-password"}, "confirm": {"different-strong-pw"},
	}, cookie)
	if mismatch.Code != http.StatusBadRequest {
		t.Fatalf("mismatched confirm status=%d", mismatch.Code)
	}

	// Too-short new password must be rejected.
	short := postForm(t, manage, "/change-password", url.Values{
		"csrf": {p.CSRF}, "current": {"admin"}, "new": {"short"}, "confirm": {"short"},
	}, cookie)
	if short.Code != http.StatusBadRequest {
		t.Fatalf("short new password status=%d", short.Code)
	}

	// Successful change.
	ok2 := postForm(t, manage, "/change-password", url.Values{
		"csrf": {p.CSRF}, "current": {"admin"}, "new": {"new-strong-password"}, "confirm": {"new-strong-password"},
	}, cookie)
	if loc := ok2.Result().Header.Get("Location"); loc != "/files" {
		t.Fatalf("change-password redirect = %q, want /files", loc)
	}

	// must_change_password must be cleared afterward.
	p2, ok := a.authenticate(requestWithCookie(http.MethodGet, "/files", nil, cookie))
	if !ok || p2.MustChangePassword {
		t.Fatal("must_change_password must be cleared after changing")
	}

	// /files is now reachable and the old default password no longer works.
	filesRec2 := httptest.NewRecorder()
	manage.ServeHTTP(filesRec2, requestWithCookie(http.MethodGet, "/files", nil, cookie))
	if filesRec2.Code != http.StatusOK {
		t.Fatalf("/files status after change=%d", filesRec2.Code)
	}
	oldForm := url.Values{"username": {"admin"}, "password": {"admin"}}
	oldReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(oldForm.Encode()))
	oldReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	oldRec := httptest.NewRecorder()
	manage.ServeHTTP(oldRec, oldReq)
	if oldRec.Code != http.StatusUnauthorized {
		t.Fatalf("old default password status=%d, want 401", oldRec.Code)
	}
	// Login with the new password reaches /files.
	rec2, _ := loginAsRaw(t, manage, "admin", "new-strong-password")
	if loc := rec2.Result().Header.Get("Location"); loc != "/files" {
		t.Fatalf("post-change login redirect = %q, want /files", loc)
	}
}

func TestFolderUploadPreservesStructure(t *testing.T) {
	a := newTestApp(t)
	manage := a.managementHandler()
	public := a.publicHandler()
	cookie := loginAs(t, manage, "admin", testAdminPassword)
	p, ok := a.authenticate(requestWithCookie(http.MethodGet, "/files", nil, cookie))
	if !ok {
		t.Fatal("not authenticated")
	}

	// Simulate a folder upload: the relPath carries the structure.
	deep := uploadFileWithName(t, manage, cookie, p.CSRF, "projects/2024/report.md", "# Report\n")
	if deep.Code != http.StatusSeeOther {
		t.Fatalf("folder upload status=%d body=%s", deep.Code, deep.Body.String())
	}
	root := a.userRoot(p.UserID)
	if _, err := os.Stat(filepath.Join(root, "projects", "2024", "report.md")); err != nil {
		t.Fatalf("nested file not created: %v", err)
	}

	// The file must be reachable as a private download through the management UI.
	priv := httptest.NewRecorder()
	manage.ServeHTTP(priv, requestWithCookie(http.MethodGet, "/content?path=projects/2024/report.md", nil, cookie))
	if priv.Code != http.StatusOK {
		t.Fatalf("nested content status=%d", priv.Code)
	}

	// A share must exist and the public share endpoint must serve it.
	var shareURL string
	rows, err := a.db.Query(`SELECT rel_path, token_value FROM shares WHERE owner_id=?`, p.UserID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var lp, tok string
		if err := rows.Scan(&lp, &tok); err != nil {
			t.Fatal(err)
		}
		if lp == "projects/2024/report.md" {
			shareURL = "/s/" + tok
		}
	}
	if shareURL == "" {
		t.Fatal("no share created for nested folder file")
	}
	shareRec := httptest.NewRecorder()
	public.ServeHTTP(shareRec, httptest.NewRequest(http.MethodGet, shareURL+"/", nil))
	if shareRec.Code != http.StatusOK {
		t.Fatalf("public share status=%d", shareRec.Code)
	}
}

func TestSharedHTMLServesRelativeAsset(t *testing.T) {
	a := newTestApp(t)
	manage := a.managementHandler()
	public := a.publicHandler()
	cookie := loginAs(t, manage, "admin", testAdminPassword)
	p, ok := a.authenticate(requestWithCookie(http.MethodGet, "/files", nil, cookie))
	if !ok {
		t.Fatal("not authenticated")
	}

	// Upload an HTML page and a sibling image referenced by relative URL.
	htmlBody := `<!doctype html><h1>Site</h1><img src="logo.png">`
	if got := uploadFileWithName(t, manage, cookie, p.CSRF, "site/index.html", htmlBody); got.Code != http.StatusSeeOther {
		t.Fatalf("html upload status=%d body=%s", got.Code, got.Body.String())
	}
	if got := uploadFileWithName(t, manage, cookie, p.CSRF, "site/logo.png", "\x89PNG\r\n\x1a\n"); got.Code != http.StatusSeeOther {
		t.Fatalf("image upload status=%d body=%s", got.Code, got.Body.String())
	}

	// The HTML share auto-created on upload must serve its relative asset.
	var token string
	if err := a.db.QueryRow(`SELECT token_value FROM shares WHERE owner_id=? AND rel_path='site/index.html'`, p.UserID).Scan(&token); err != nil {
		t.Fatalf("no share for site/index.html: %v", err)
	}
	htmlRec := httptest.NewRecorder()
	public.ServeHTTP(htmlRec, httptest.NewRequest(http.MethodGet, "/s/"+token+"/", nil))
	if htmlRec.Code != http.StatusOK || !strings.Contains(htmlRec.Body.String(), "Site") {
		t.Fatalf("shared html status=%d body=%s", htmlRec.Code, htmlRec.Body.String())
	}
	// Without the trailing slash the browser would resolve relative asset URLs
	// against /s/ (losing the token), so the HTML share must redirect.
	redir := httptest.NewRecorder()
	public.ServeHTTP(redir, httptest.NewRequest(http.MethodGet, "/s/"+token, nil))
	if redir.Code != http.StatusSeeOther || redir.Header().Get("Location") != "/s/"+token+"/" {
		t.Fatalf("html share must redirect to trailing slash: %d %q", redir.Code, redir.Header().Get("Location"))
	}
	imgRec := httptest.NewRecorder()
	public.ServeHTTP(imgRec, httptest.NewRequest(http.MethodGet, "/s/"+token+"/logo.png", nil))
	if imgRec.Code != http.StatusOK || !strings.HasPrefix(imgRec.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("relative asset not served: status=%d type=%q", imgRec.Code, imgRec.Header().Get("Content-Type"))
	}

	// A non-html single-file share must not expose sibling assets.
	var imgToken string
	if err := a.db.QueryRow(`SELECT token_value FROM shares WHERE owner_id=? AND rel_path='site/logo.png'`, p.UserID).Scan(&imgToken); err != nil {
		t.Fatalf("no share for site/logo.png: %v", err)
	}
	leak := httptest.NewRecorder()
	public.ServeHTTP(leak, httptest.NewRequest(http.MethodGet, "/s/"+imgToken+"/index.html", nil))
	if leak.Code != http.StatusNotFound {
		t.Fatalf("non-html share must not expose siblings, status=%d", leak.Code)
	}
}

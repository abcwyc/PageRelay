package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	htmltpl "html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

const (
	maxEditableBytes = int64(5 << 20)
	previewTTL       = 10 * time.Minute
)

type apiFileItem struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	LogicalPath string `json:"logical_path"`
	Type        string `json:"type"`
	Preview     string `json:"preview"`
	Size        int64  `json:"size"`
	SizeLabel   string `json:"size_label"`
	Modified    string `json:"modified"`
	ModifiedTS  int64  `json:"modified_ts"`
	Shared      bool   `json:"shared"`
	ShareURL    string `json:"share_url,omitempty"`
	PreviewURL  string `json:"preview_url,omitempty"`
	ContentURL  string `json:"content_url,omitempty"`
	Editable    bool   `json:"editable"`
}

type apiListResponse struct {
	Path        string        `json:"path"`
	LogicalPath string        `json:"logical_path"`
	Items       []apiFileItem `json:"items"`
	Used        int64         `json:"used"`
	Quota       int64         `json:"quota"`
}

type activeShare struct {
	ID    int64
	Token string
}

func (a *app) apiList(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	rel, err := cleanRel(r.URL.Query().Get("path"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "目录无效")
		return
	}
	root := a.userRoot(p.UserID)
	full, err := safeJoin(root, rel)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "目录无效")
		return
	}
	info, err := os.Stat(full)
	if err != nil || !info.IsDir() {
		jsonError(w, http.StatusNotFound, "目录不存在")
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "无法读取目录")
		return
	}
	treeOnly := r.URL.Query().Get("tree") == "1"
	shares := map[string]activeShare{}
	if !treeOnly {
		shares, err = a.activeShares(p.UserID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "无法读取分享状态")
			return
		}
	}
	items := make([]apiFileItem, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		entryInfo, err := entry.Info()
		if err != nil {
			continue
		}
		child := path.Join(rel, entry.Name())
		items = append(items, a.makeAPIItem(r, p, child, entryInfo, shares[child], false))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Type != items[j].Type {
			return items[i].Type == "dir"
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	result := apiListResponse{Path: rel, LogicalPath: logicalPath(p.Username, rel), Items: items}
	if !treeOnly {
		result.Used = dirSize(root)
		result.Quota = a.userQuota(p.UserID)
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) apiItem(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	rel, err := cleanRel(r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		jsonError(w, http.StatusBadRequest, "路径无效")
		return
	}
	full, err := safeJoin(a.userRoot(p.UserID), rel)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "路径无效")
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		jsonError(w, http.StatusNotFound, "文件不存在")
		return
	}
	share, _ := a.findActiveShare(p.UserID, rel)
	item := a.makeAPIItem(r, p, rel, info, share, true)
	if !info.IsDir() && (item.Preview == "html" || item.Preview == "markdown" || item.Preview == "svg") {
		token, err := a.createPreviewToken(p.UserID, rel)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "无法创建安全预览")
			return
		}
		item.PreviewURL = a.previewBase(r) + token + "/"
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *app) makeAPIItem(r *http.Request, p principal, rel string, info os.FileInfo, share activeShare, detailed bool) apiFileItem {
	typ := "file"
	if info.IsDir() {
		typ = "dir"
	}
	preview := detectPreview(rel, info.IsDir())
	item := apiFileItem{
		Name: info.Name(), Path: rel, LogicalPath: logicalPath(p.Username, rel),
		Type: typ, Preview: preview, Size: info.Size(), SizeLabel: formatBytes(info.Size()),
		Modified: info.ModTime().Format("2006-01-02 15:04"), ModifiedTS: info.ModTime().Unix(),
		Shared: share.Token != "", Editable: isEditable(rel) && info.Size() <= maxEditableBytes,
	}
	if share.Token != "" {
		item.ShareURL = a.shareBase(r) + share.Token
	}
	if !info.IsDir() {
		item.ContentURL = "/content?path=" + url.QueryEscape(rel)
		if detailed && preview != "html" && preview != "markdown" && preview != "svg" {
			item.PreviewURL = item.ContentURL
		}
	}
	return item
}

func (a *app) apiShare(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil || !a.checkCSRF(r, p) {
		jsonError(w, http.StatusBadRequest, "请求已过期")
		return
	}
	rel, err := cleanRel(r.FormValue("path"))
	if err != nil || rel == "" {
		jsonError(w, http.StatusBadRequest, "路径无效")
		return
	}
	full, err := safeJoin(a.userRoot(p.UserID), rel)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "路径无效")
		return
	}
	if _, err := os.Stat(full); err != nil {
		jsonError(w, http.StatusNotFound, "文件不存在")
		return
	}
	enabled := r.FormValue("enabled") == "true" || r.FormValue("enabled") == "1"
	var current activeShare
	if enabled {
		current, err = a.ensureShare(p.UserID, rel)
	} else {
		a.shareMu.Lock()
		_, err = a.db.Exec(`UPDATE shares SET active=0 WHERE owner_id=? AND rel_path=? AND active=1`, p.UserID, rel)
		a.shareMu.Unlock()
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "无法更新分享状态")
		return
	}
	action := "share_disable"
	if enabled {
		action = "share_enable"
	}
	a.audit(p.UserID, action, rel, clientIP(r))
	result := map[string]any{"shared": enabled, "share_url": ""}
	if enabled {
		result["share_url"] = a.shareBase(r) + current.Token
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) apiNew(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil || !a.checkCSRF(r, p) {
		jsonError(w, http.StatusBadRequest, "请求已过期")
		return
	}
	dir, err := cleanRel(r.FormValue("path"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "目录无效")
		return
	}
	name := cleanName(r.FormValue("name"))
	kind := r.FormValue("kind")
	if name == "" {
		jsonError(w, http.StatusBadRequest, "名称无效")
		return
	}
	base, err := safeJoin(a.userRoot(p.UserID), dir)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "目录无效")
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if kind == "folder" {
		dest, err := safeJoin(base, name)
		if err != nil || os.Mkdir(dest, 0750) != nil {
			jsonError(w, http.StatusBadRequest, "同名目录已存在")
			return
		}
		a.audit(p.UserID, "mkdir", path.Join(dir, name), clientIP(r))
		writeJSON(w, http.StatusCreated, map[string]any{"path": path.Join(dir, name), "type": "dir"})
		return
	}
	templates := map[string]struct {
		ext, body string
	}{
		"markdown": {".md", "# " + strings.TrimSuffix(name, filepath.Ext(name)) + "\n\n"},
		"html":     {".html", "<!doctype html>\n<html lang=\"zh-CN\">\n<head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>" + htmltpl.HTMLEscapeString(name) + "</title></head>\n<body>\n  <h1>" + htmltpl.HTMLEscapeString(name) + "</h1>\n</body>\n</html>\n"},
		"text":     {".txt", ""},
	}
	tpl, ok := templates[kind]
	if !ok {
		jsonError(w, http.StatusBadRequest, "文件类型无效")
		return
	}
	if filepath.Ext(name) == "" {
		name += tpl.ext
	}
	if dirSize(a.userRoot(p.UserID))+int64(len(tpl.body)) > a.userQuota(p.UserID) {
		jsonError(w, http.StatusRequestEntityTooLarge, "存储配额不足")
		return
	}
	dest, err := safeJoin(base, name)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "文件名无效")
		return
	}
	file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err == nil {
		_, err = file.WriteString(tpl.body)
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		_ = os.Remove(dest)
		if os.IsExist(err) {
			jsonError(w, http.StatusConflict, "同名文件已存在")
		} else {
			jsonError(w, http.StatusInternalServerError, "无法创建文件")
		}
		return
	}
	rel := path.Join(dir, name)
	share, err := a.ensureShare(p.UserID, rel)
	if err != nil {
		_ = os.Remove(dest)
		jsonError(w, http.StatusInternalServerError, "无法初始化分享")
		return
	}
	a.audit(p.UserID, "file_create", rel, clientIP(r))
	writeJSON(w, http.StatusCreated, map[string]any{"path": rel, "type": "file", "share_url": a.shareBase(r) + share.Token})
}

func (a *app) apiSave(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxEditableBytes+1<<20)
	if err := r.ParseForm(); err != nil || !a.checkCSRF(r, p) {
		jsonError(w, http.StatusBadRequest, "请求无效")
		return
	}
	rel, err := cleanRel(r.FormValue("path"))
	content := []byte(r.FormValue("content"))
	if err != nil || rel == "" || !isEditable(rel) || int64(len(content)) > maxEditableBytes {
		jsonError(w, http.StatusBadRequest, "文件不可编辑或内容过大")
		return
	}
	root := a.userRoot(p.UserID)
	full, err := safeJoin(root, rel)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "路径无效")
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		jsonError(w, http.StatusNotFound, "文件不存在")
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if dirSize(root)-info.Size()+int64(len(content)) > a.userQuota(p.UserID) {
		jsonError(w, http.StatusRequestEntityTooLarge, "存储配额不足")
		return
	}
	tmp := full + ".save-" + randomToken(6)
	if err := os.WriteFile(tmp, content, 0640); err != nil {
		jsonError(w, http.StatusInternalServerError, "保存失败")
		return
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		jsonError(w, http.StatusInternalServerError, "保存失败")
		return
	}
	a.audit(p.UserID, "file_save", rel, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

func (a *app) apiDirectories(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	root := a.userRoot(p.UserID)
	type directory struct {
		Path        string `json:"path"`
		LogicalPath string `json:"logical_path"`
	}
	directories := []directory{{Path: "", LogicalPath: logicalPath(p.Username, "")}}
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		directories = append(directories, directory{Path: rel, LogicalPath: logicalPath(p.Username, rel)})
		if len(directories) >= 2000 {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "无法读取目录")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"directories": directories})
}

func (a *app) apiMove(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil || !a.checkCSRF(r, p) {
		jsonError(w, http.StatusBadRequest, "请求已过期")
		return
	}
	paths, err := parsePathList(r.FormValue("paths"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "待移动文件无效")
		return
	}
	destination, err := cleanRel(r.FormValue("destination"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "目标目录无效")
		return
	}
	root := a.userRoot(p.UserID)
	destinationDir, err := safeJoin(root, destination)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "目标目录无效")
		return
	}
	if info, err := os.Stat(destinationDir); err != nil || !info.IsDir() {
		jsonError(w, http.StatusBadRequest, "目标目录不存在")
		return
	}
	type move struct{ sourceRel, targetRel, source, target string }
	moves := make([]move, 0, len(paths))
	for _, sourceRel := range paths {
		if sourceRel == "" || destination == path.Dir(sourceRel) || (path.Dir(sourceRel) == "." && destination == "") {
			jsonError(w, http.StatusConflict, "文件已经位于目标目录")
			return
		}
		source, err := safeJoin(root, sourceRel)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "源路径无效")
			return
		}
		info, err := os.Stat(source)
		if err != nil {
			jsonError(w, http.StatusNotFound, "文件不存在")
			return
		}
		if info.IsDir() && (destination == sourceRel || strings.HasPrefix(destination, sourceRel+"/")) {
			jsonError(w, http.StatusBadRequest, "不能把目录移动到自身内部")
			return
		}
		targetRel := path.Join(destination, path.Base(sourceRel))
		target, err := safeJoin(root, targetRel)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "目标路径无效")
			return
		}
		if _, err := os.Stat(target); err == nil {
			jsonError(w, http.StatusConflict, "目标目录存在同名文件")
			return
		}
		moves = append(moves, move{sourceRel: sourceRel, targetRel: targetRel, source: source, target: target})
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	moved := 0
	for index, item := range moves {
		if err := os.Rename(item.source, item.target); err != nil {
			for rollback := moved - 1; rollback >= 0; rollback-- {
				_ = os.Rename(moves[rollback].target, moves[rollback].source)
			}
			jsonError(w, http.StatusInternalServerError, "移动失败")
			return
		}
		moved = index + 1
	}
	for _, item := range moves {
		_, _ = a.db.Exec(`UPDATE shares SET rel_path = ? || substr(rel_path, length(?) + 1), name = CASE WHEN rel_path = ? THEN ? ELSE name END WHERE owner_id=? AND (rel_path=? OR rel_path LIKE ?)`, item.targetRel, item.sourceRel, item.sourceRel, path.Base(item.targetRel), p.UserID, item.sourceRel, item.sourceRel+"/%")
		a.audit(p.UserID, "move", item.sourceRel+" -> "+item.targetRel, clientIP(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"moved": len(moves), "destination": destination})
}

func (a *app) apiDeleteBatch(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil || !a.checkCSRF(r, p) {
		jsonError(w, http.StatusBadRequest, "请求已过期")
		return
	}
	paths, err := parsePathList(r.FormValue("paths"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "待删除文件无效")
		return
	}
	root := a.userRoot(p.UserID)
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	for _, rel := range paths {
		if rel == "" {
			jsonError(w, http.StatusBadRequest, "不能删除根目录")
			return
		}
		full, err := safeJoin(root, rel)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "路径无效")
			return
		}
		if err := os.RemoveAll(full); err != nil {
			jsonError(w, http.StatusInternalServerError, "删除失败")
			return
		}
		_, _ = a.db.Exec(`UPDATE shares SET active=0 WHERE owner_id=? AND (rel_path=? OR rel_path LIKE ?)`, p.UserID, rel, rel+"/%")
		a.audit(p.UserID, "delete", rel, clientIP(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": len(paths)})
}

func (a *app) apiDownload(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	rawPaths := r.URL.Query()["path"]
	if len(rawPaths) == 0 || len(rawPaths) > 100 {
		badRequest(w, "请选择 1–100 个文件")
		return
	}
	root := a.userRoot(p.UserID)
	type download struct{ rel, full string }
	downloads := make([]download, 0, len(rawPaths))
	for _, raw := range rawPaths {
		rel, err := cleanRel(raw)
		if err != nil || rel == "" {
			badRequest(w, "路径无效")
			return
		}
		full, err := safeJoin(root, rel)
		if err != nil {
			badRequest(w, "路径无效")
			return
		}
		if _, err := os.Stat(full); err != nil {
			http.NotFound(w, r)
			return
		}
		downloads = append(downloads, download{rel: rel, full: full})
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="filehub-%s.zip"`, time.Now().Format("20060102-1504")))
	archive := zip.NewWriter(w)
	defer archive.Close()
	for _, selected := range downloads {
		archiveRoot := path.Base(selected.rel)
		err := filepath.WalkDir(selected.full, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 || (current != selected.full && strings.HasPrefix(entry.Name(), ".")) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(selected.full, current)
			if err != nil {
				return err
			}
			name := archiveRoot
			if rel != "." {
				name = path.Join(archiveRoot, filepath.ToSlash(rel))
			}
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			header.Name = name
			if info.IsDir() {
				header.Name += "/"
				_, err = archive.CreateHeader(header)
				return err
			}
			header.Method = zip.Deflate
			writer, err := archive.CreateHeader(header)
			if err != nil {
				return err
			}
			file, err := os.Open(current)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(writer, file)
			closeErr := file.Close()
			return firstErr(copyErr, closeErr)
		})
		if err != nil {
			log.Printf("batch download: %v", err)
			return
		}
	}
	a.audit(p.UserID, "download_batch", fmt.Sprintf("%d items", len(downloads)), clientIP(r))
}

func parsePathList(value string) ([]string, error) {
	var raw []string
	if err := json.Unmarshal([]byte(value), &raw); err != nil || len(raw) == 0 || len(raw) > 100 {
		return nil, fmt.Errorf("invalid path list")
	}
	result := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, value := range raw {
		rel, err := cleanRel(value)
		if err != nil || rel == "" || seen[rel] {
			return nil, fmt.Errorf("invalid path")
		}
		seen[rel] = true
		result = append(result, rel)
	}
	return result, nil
}

func (a *app) ensureShare(ownerID int64, rel string) (activeShare, error) {
	a.shareMu.Lock()
	defer a.shareMu.Unlock()
	if current, err := a.findActiveShare(ownerID, rel); err == nil {
		return current, nil
	} else if err != sql.ErrNoRows {
		return activeShare{}, err
	}
	token := randomToken(24)
	res, err := a.db.Exec(`INSERT INTO shares(owner_id,token_hash,token_value,name,rel_path,expires_at,active) VALUES(?,?,?,?,?,NULL,1)`, ownerID, tokenHash(token), token, path.Base(rel), rel)
	if err != nil {
		return activeShare{}, err
	}
	id, err := res.LastInsertId()
	return activeShare{ID: id, Token: token}, err
}

func (a *app) findActiveShare(ownerID int64, rel string) (activeShare, error) {
	var current activeShare
	err := a.db.QueryRow(`SELECT id,token_value FROM shares WHERE owner_id=? AND rel_path=? AND active=1 AND (expires_at IS NULL OR expires_at > datetime('now')) ORDER BY id DESC LIMIT 1`, ownerID, rel).Scan(&current.ID, &current.Token)
	return current, err
}

func (a *app) activeShares(ownerID int64) (map[string]activeShare, error) {
	rows, err := a.db.Query(`SELECT id,rel_path,token_value FROM shares WHERE owner_id=? AND active=1 AND (expires_at IS NULL OR expires_at > datetime('now')) ORDER BY id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]activeShare{}
	for rows.Next() {
		var id int64
		var rel, token string
		if err := rows.Scan(&id, &rel, &token); err != nil {
			return nil, err
		}
		result[rel] = activeShare{ID: id, Token: token}
	}
	return result, rows.Err()
}

func (a *app) createPreviewToken(userID int64, rel string) (string, error) {
	token := randomToken(24)
	expires := time.Now().UTC().Add(previewTTL).Format("2006-01-02 15:04:05")
	_, err := a.db.Exec(`INSERT INTO preview_tokens(token_hash,user_id,rel_path,expires_at) VALUES(?,?,?,?)`, tokenHash(token), userID, rel, expires)
	if err == nil {
		_, _ = a.db.Exec(`DELETE FROM preview_tokens WHERE expires_at < datetime('now')`)
	}
	return token, err
}

func (a *app) publicPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/p/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || len(parts[0]) < 20 {
		http.NotFound(w, r)
		return
	}
	var userID int64
	var rel string
	err := a.db.QueryRow(`SELECT p.user_id,p.rel_path FROM preview_tokens p JOIN users u ON u.id=p.user_id WHERE p.token_hash=? AND p.expires_at > datetime('now') AND u.disabled=0`, tokenHash(parts[0])).Scan(&userID, &rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	source, err := safeJoin(a.userRoot(userID), rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub, err = cleanRel(parts[1])
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	if !sourceInfo.IsDir() && sub == "" {
		if detectPreview(rel, false) == "markdown" {
			a.serveMarkdownPreview(w, r, source, sourceInfo.Name(), "/p/"+parts[0]+"/")
			return
		}
		servePreviewFile(w, r, source, sourceInfo, a.managementOrigin())
		return
	}
	base := source
	if !sourceInfo.IsDir() {
		base = filepath.Dir(source)
	}
	target, err := safeJoin(base, sub)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(target)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if info.IsDir() {
		target = filepath.Join(target, "index.html")
		info, err = os.Stat(target)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
	}
	servePreviewFile(w, r, target, info, a.managementOrigin())
}

func (a *app) serveMarkdownPreview(w http.ResponseWriter, r *http.Request, file, title, baseURL string) {
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	source, err := io.ReadAll(io.LimitReader(f, maxEditableBytes+1))
	if err != nil || int64(len(source)) > maxEditableBytes {
		http.Error(w, "Markdown 文件过大，无法预览", http.StatusRequestEntityTooLarge)
		return
	}
	var rendered bytes.Buffer
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	if err := md.Convert(source, &rendered); err != nil {
		http.Error(w, "Markdown 渲染失败", http.StatusInternalServerError)
		return
	}
	mermaidScripts := ""
	if bytes.Contains(rendered.Bytes(), []byte(`class="language-mermaid"`)) {
		mermaidScripts = `<script src="/assets/mermaid.min.js?v=11.16.1"></script><script>(()=>{const blocks=[...document.querySelectorAll('pre>code.language-mermaid')];for(const block of blocks){const diagram=document.createElement('div');diagram.className='mermaid';diagram.textContent=block.textContent;block.parentElement.replaceWith(diagram)}mermaid.initialize({startOnLoad:false,securityLevel:'strict',theme:'neutral',fontFamily:'-apple-system,BlinkMacSystemFont,Segoe UI,sans-serif'});mermaid.run({querySelector:'.mermaid'}).catch(()=>{document.querySelectorAll('.mermaid').forEach(node=>node.classList.add('mermaid-error'))})})()</script>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self'; base-uri 'self'; frame-ancestors "+a.managementOrigin())
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><base href="%s"><title>%s</title><style>%s</style></head><body><article>%s</article>%s</body></html>`, htmltpl.HTMLEscapeString(baseURL), htmltpl.HTMLEscapeString(title), markdownCSS, rendered.String(), mermaidScripts)
}

func servePreviewFile(w http.ResponseWriter, r *http.Request, file string, info os.FileInfo, frameAncestor string) {
	ext := strings.ToLower(filepath.Ext(file))
	if ext == ".html" || ext == ".htm" || ext == ".svg" {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-modals allow-same-origin; default-src 'self' data: blob:; style-src 'self' 'unsafe-inline' data:; script-src 'self' 'unsafe-inline' 'unsafe-eval'; img-src 'self' data: blob:; media-src 'self' data: blob:; connect-src 'self'; frame-ancestors "+frameAncestor)
	}
	ctype := mimeForPath(file)
	w.Header().Set("Content-Type", ctype)
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func (a *app) previewBase(r *http.Request) string {
	base := strings.TrimSuffix(a.shareBase(r), "/s/")
	return base + "/p/"
}

func (a *app) managementOrigin() string {
	return "http://" + a.cfg.listen
}

func (a *app) writeSuccess(w http.ResponseWriter, r *http.Request, dir string) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
		return
	}
	http.Redirect(w, r, "/files?path="+url.QueryEscape(dir), http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func jsonError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func cleanName(value string) string {
	value = strings.TrimSpace(value)
	name := filepath.Base(strings.ReplaceAll(value, "\\", "/"))
	if name == "." || name == "" || strings.HasPrefix(name, ".") || strings.ContainsRune(name, '\x00') {
		return ""
	}
	return name
}

func logicalPath(username, rel string) string {
	if rel == "" {
		return "/" + username
	}
	return "/" + username + "/" + rel
}

func detectPreview(rel string, isDir bool) string {
	if isDir {
		return "directory"
	}
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".html", ".htm":
		return "html"
	case ".md", ".markdown":
		return "markdown"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".bmp":
		return "image"
	case ".svg":
		return "svg"
	case ".pdf":
		return "pdf"
	case ".mp4", ".webm", ".mov":
		return "video"
	case ".mp3", ".wav", ".ogg", ".m4a", ".aac":
		return "audio"
	case ".txt", ".log", ".json", ".csv", ".xml", ".yaml", ".yml", ".toml", ".ini", ".css", ".js", ".ts", ".go", ".py", ".sh", ".sql":
		return "text"
	default:
		return "none"
	}
}

func isEditable(rel string) bool {
	switch detectPreview(rel, false) {
	case "html", "markdown", "text":
		return true
	default:
		return false
	}
}

func mimeForPath(file string) string {
	ext := strings.ToLower(filepath.Ext(file))
	switch ext {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	default:
		if detected := mime.TypeByExtension(ext); detected != "" {
			return detected
		}
		return "application/octet-stream"
	}
}

const markdownCSS = `:root{color-scheme:light}*{box-sizing:border-box}html,body{max-width:100%;overflow-x:hidden}body{margin:0;background:#fff;color:#202124;font:15px/1.72 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}article{width:100%;max-width:850px;min-width:0;margin:0 auto;padding:48px 56px 80px;overflow-wrap:anywhere}h1,h2,h3,h4{line-height:1.3;letter-spacing:-.02em;margin:1.6em 0 .6em}h1{font-size:2em;border-bottom:1px solid #e7e8eb;padding-bottom:.35em}h2{font-size:1.45em;border-bottom:1px solid #eceef1;padding-bottom:.3em}p{margin:.8em 0}a{color:#0877e8}code{background:#f2f3f5;border-radius:4px;padding:.15em .35em;font:13px ui-monospace,SFMono-Regular,Menlo,monospace}pre{max-width:100%;overflow:auto;background:#f6f7f8;border:1px solid #e5e7eb;border-radius:8px;padding:16px}pre code{background:none;padding:0}blockquote{border-left:3px solid #c9ccd2;color:#666;margin:1em 0;padding:.2em 1em}table{display:block;width:100%;max-width:100%;overflow-x:auto;border-collapse:collapse;margin:1em 0}th,td{border:1px solid #dde0e5;padding:8px 10px;text-align:left;overflow-wrap:anywhere;word-break:break-word}td code,th code{white-space:normal;overflow-wrap:anywhere;word-break:break-word}.mermaid{width:100%;max-width:100%;margin:1.4em 0;padding:18px;overflow-x:auto;border:1px solid #e5e7eb;border-radius:10px;background:#fbfbfc;text-align:center}.mermaid svg{display:block;max-width:100%;height:auto;margin:0 auto}.mermaid-error{white-space:pre-wrap;text-align:left;color:#8e2635}img{max-width:100%;height:auto;border-radius:6px}hr{border:0;border-top:1px solid #e5e7eb;margin:2em 0}@media(max-width:700px){article{padding:28px 22px 60px}.mermaid{padding:10px}}`

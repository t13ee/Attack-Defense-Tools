// SnapBack.go — hỗ trợ include vừa DIR vừa FILE, seed đúng kiểu; tối ưu chống spam; self-protect.
package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

/************ cấu hình ************/
var (
	baseURL string // backup server, vd: http://127.0.0.1:1412
	logURL  string // log server (nếu trống sẽ dùng baseURL)

	// khoảng poll snapshot
	snapshotPollInterval = 5 * time.Second

	// micro-sweep quét nhanh các thư mục đang watch để dọn path lạ
	microSweepInterval = 1 * time.Millisecond
)

/************ utils ************/
func splitAndTrim(inp string) []string {
	parts := strings.Split(inp, ",")
	out := make([]string, 0, len(parts))
	for _, t := range parts {
		if s := strings.TrimSpace(t); s != "" {
			out = append(out, s)
		}
	}
	return out
}
func resolvePaths(paths []string) []string {
	var resolved []string
	for _, p := range paths {
		if abs, err := filepath.Abs(p); err == nil {
			resolved = append(resolved, abs)
		}
	}
	return resolved
}
func getFileMode(path string) os.FileMode {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	return info.Mode().Perm()
}
func checkSymlink(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0
}
func toMD5(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
func permOf(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return fmt.Sprintf("%#o", info.Mode().Perm())
}

/************ logging ************/
var httpLogClient = &http.Client{Timeout: 800 * time.Millisecond}

type LogEvent struct {
	ID        string `json:"id,omitempty"`
	Timestamp int64  `json:"ts_unix,omitempty"`
	TimeISO   string `json:"ts_iso,omitempty"`

	Host      string `json:"host,omitempty"`
	EventType string `json:"event_type,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Path      string `json:"path,omitempty"`
	FileType  string `json:"file_type,omitempty"`
	OldMD5    string `json:"old_md5,omitempty"`
	NewMD5    string `json:"new_md5,omitempty"`
	OldPerm   string `json:"old_perm,omitempty"`
	NewPerm   string `json:"new_perm,omitempty"`
	Note      string `json:"note,omitempty"`
	Restored  bool   `json:"restored,omitempty"`
}

var hostName string

func sevFor(evt string) string {
	switch strings.ToUpper(evt) {
	case "REMOVE":
		return "MEDIUM"
	case "WRITE":
		return "CRITICAL"
	case "CREATE":
		return "HIGH"
	case "SYMLINK":
		return "LOW"
	case "CHMOD":
		return "LOW"
	case "BACKUP_UPDATE":
		return "SAFE"
	default:
		return "LOW"
	}
}
func postLog(ev LogEvent) {
	if logURL == "" {
		return
	}
	ev.Timestamp = time.Now().Unix()
	ev.TimeISO = time.Now().Format(time.RFC3339)
	ev.Host = hostName
	body, _ := json.Marshal(ev)
	go func(b []byte) {
		req, err := http.NewRequest("POST", strings.TrimRight(logURL, "/")+"/log", bytes.NewReader(b))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		_, _ = httpLogClient.Do(req)
	}(body)
}

/************ self-protect ************/
var selfExePath string

func isSelfProtected(path string) bool {
	abs, _ := filepath.Abs(path)
	if selfExePath != "" && abs == selfExePath {
		return true
	}
	base := filepath.Base(abs)
	if base == "SnapBack" || base == "SnapBack.go" {
		return true
	}
	if strings.Contains(abs, "/go-build/") || strings.Contains(abs, "\\go-build\\") {
		return true
	}
	return false
}

/************ snapshot store ************/
type SnapBack struct {
	pathIncludeList []string
	pathExcludeList []string

	muSnap   sync.RWMutex
	snapshot map[string]map[string]string // path -> meta

	// watcher
	muWatch sync.Mutex
	watcher *fsnotify.Watcher
	watched map[string]struct{} // set[dir] we added

	// seed tránh post DIR cha trùng
	muPostedDirs sync.Mutex
	postedDirs   map[string]struct{}
}

func NewSnapBack(includeList, excludeList []string) *SnapBack {
	return &SnapBack{
		pathIncludeList: resolvePaths(includeList),
		pathExcludeList: resolvePaths(excludeList),
		snapshot:        make(map[string]map[string]string),
		watched:         make(map[string]struct{}),
		postedDirs:      make(map[string]struct{}),
	}
}
func (sb *SnapBack) isExcluded(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if isSelfProtected(abs) {
		return true
	}
	for _, ex := range sb.pathExcludeList {
		if abs == ex || strings.HasPrefix(abs, ex+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}
func (sb *SnapBack) fetchSnapshot() map[string]map[string]string {
	resp, err := http.Get(strings.TrimRight(baseURL, "/") + "/backup_file")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	var m map[string]map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	return m
}
func (sb *SnapBack) hasMeta(abs string) (map[string]string, bool) {
	sb.muSnap.RLock()
	defer sb.muSnap.RUnlock()
	m, ok := sb.snapshot[abs]
	return m, ok
}
func (sb *SnapBack) isAllowedDir(abs string) bool {
	if meta, ok := sb.hasMeta(abs); ok {
		return strings.ToUpper(meta["Type-File"]) == "DIR"
	}
	return false
}
func (sb *SnapBack) isAllowedFile(abs string) bool {
	if meta, ok := sb.hasMeta(abs); ok {
		return strings.ToUpper(meta["Type-File"]) == "DATA"
	}
	return false
}
func (sb *SnapBack) isAllowedSymlink(abs string) bool {
	if meta, ok := sb.hasMeta(abs); ok {
		return strings.ToUpper(meta["Type-File"]) == "SYMLINK"
	}
	return false
}

/************ blob cache (RAM) ************/
type cacheEntry struct {
	key  string
	data []byte
	size int
	ts   int64
}

var (
	blobMu    sync.Mutex
	blobMap   = make(map[string]*cacheEntry)
	blobBytes int
	blobLimit = 64 * 1024 * 1024 // 64MB mặc định
)

func blobKey(fn, md5 string) string { return fn + "|" + md5 }
func getBlob(fn, md5sum string) ([]byte, bool) {
	k := blobKey(fn, md5sum)
	blobMu.Lock()
	if e, ok := blobMap[k]; ok {
		e.ts = time.Now().UnixNano()
		body := e.data
		blobMu.Unlock()
		return body, true
	}
	blobMu.Unlock()

	resp, err := http.Get(strings.TrimRight(baseURL, "/") + "/?filename=" + fn)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	blobMu.Lock()
	defer blobMu.Unlock()
	e := &cacheEntry{key: k, data: body, size: len(body), ts: time.Now().UnixNano()}
	blobMap[k] = e
	blobBytes += e.size
	// LRU prune
	if blobBytes > blobLimit {
		type kv struct {
			k string
			e *cacheEntry
		}
		list := make([]kv, 0, len(blobMap))
		for k2, v2 := range blobMap {
			list = append(list, kv{k2, v2})
		}
		for i := 0; i < len(list)-1; i++ {
			for j := i + 1; j < len(list); j++ {
				if list[i].e.ts > list[j].e.ts {
					list[i], list[j] = list[j], list[i]
				}
			}
		}
		for _, it := range list {
			if blobBytes <= blobLimit {
				break
			}
			delete(blobMap, it.k)
			blobBytes -= it.e.size
		}
	}
	return body, true
}

/************ chống symlink & ghi an toàn ************/
func (sb *SnapBack) cleanParents(absPath string) {
	dir := filepath.Dir(absPath)
	for {
		if dir == "/" || dir == "." {
			break
		}
		fi, err := os.Lstat(dir)
		if err != nil {
			_ = os.Mkdir(dir, 0755)
		} else if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			_ = os.Remove(dir)
			_ = os.Mkdir(dir, 0755)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
}
func safeWriteNoFollow(absPath string, data []byte, perm os.FileMode) error {
	if checkSymlink(absPath) {
		_ = os.Remove(absPath)
	}
	fd, err := syscall.Open(absPath, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC|syscall.O_NOFOLLOW, uint32(perm))
	if err != nil {
		if err == syscall.ELOOP {
			_ = os.Remove(absPath)
			fd, err = syscall.Open(absPath, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC|syscall.O_NOFOLLOW, uint32(perm))
		}
		if err != nil {
			return err
		}
	}
	f := os.NewFile(uintptr(fd), absPath)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	_ = f.Chmod(perm)
	_ = f.Sync()
	return nil
}

/************ enforce theo snapshot ************/
func (sb *SnapBack) ensureData(absPath string, meta map[string]string) (changed bool) {
	fn := meta["Backup-File-Path"]
	if fn == "" {
		return false
	}
	expMD5 := meta["Md5"]
	permStr := meta["Permission-File"]
	pv, _ := strconv.ParseUint(permStr, 8, 32)
	if pv == 0 {
		pv = 0644
	}
	perm := os.FileMode(pv)

	if st, err := os.Lstat(absPath); err == nil && st.Mode().IsRegular() && !checkSymlink(absPath) {
		if expMD5 == "" || toMD5(absPath) == expMD5 {
			return false
		}
	}
	body, ok := getBlob(fn, expMD5)
	if !ok {
		return false
	}
	sb.cleanParents(absPath)
	_ = os.MkdirAll(filepath.Dir(absPath), 0755)
	if err := safeWriteNoFollow(absPath, body, perm); err != nil {
		return false
	}
	if expMD5 == "" || toMD5(absPath) == expMD5 {
		return true
	}
	_ = safeWriteNoFollow(absPath, body, perm)
	return true
}
func (sb *SnapBack) ensureDir(absPath string, permStr string) (changed bool) {
	pv, _ := strconv.ParseUint(permStr, 8, 32)
	if pv == 0 {
		pv = 0755
	}
	perm := os.FileMode(pv)
	if checkSymlink(absPath) {
		_ = os.Remove(absPath)
		changed = true
	} else if st, err := os.Lstat(absPath); err == nil && !st.IsDir() {
		_ = os.Remove(absPath)
		changed = true
	}
	if err := os.MkdirAll(absPath, perm); err == nil {
		_ = os.Chmod(absPath, perm)
	}
	return changed
}
func (sb *SnapBack) ensureSymlink(absPath, targetFN string) (changed bool) {
	body, ok := getBlob(targetFN, "")
	if !ok {
		return false
	}
	target := string(body)
	if checkSymlink(absPath) {
		t, _ := os.Readlink(absPath)
		if t == target {
			return false
		}
		_ = os.Remove(absPath)
		changed = true
	} else {
		_ = os.Remove(absPath)
		changed = true
	}
	return os.Symlink(target, absPath) == nil || changed
}
func (sb *SnapBack) ensurePath(absPath string) {
	if sb.isExcluded(absPath) || isSelfProtected(absPath) {
		return
	}
	info, _ := os.Lstat(absPath)
	meta, has := sb.hasMeta(absPath)

	if !has {
		if _, err := os.Lstat(absPath); err == nil {
			if !isSelfProtected(absPath) {
				_ = os.RemoveAll(absPath)
			}
			postLog(LogEvent{
				EventType: "CREATE",
				Severity:  sevFor("CREATE"),
				Path:      absPath,
				FileType:  "DATA",
				Note:      "Unauthorized path -> removed by sweep/enforce",
			})
		}
		return
	}

	switch strings.ToUpper(meta["Type-File"]) {
	case "DATA":
		ch := sb.ensureData(absPath, meta)
		postLog(LogEvent{
			EventType: "WRITE", Severity: sevFor("WRITE"),
			Path: absPath, FileType: "DATA",
			OldMD5: meta["Md5"], NewMD5: toMD5(absPath),
			OldPerm: meta["Permission-File"], NewPerm: permOf(info),
			Restored: ch,
		})
	case "DIR":
		ch := sb.ensureDir(absPath, meta["Permission-File"])
		postLog(LogEvent{
			EventType: "WRITE", Severity: sevFor("WRITE"),
			Path: absPath, FileType: "DIR",
			OldPerm: meta["Permission-File"], NewPerm: permOf(info),
			Restored: ch,
		})
	case "SYMLINK":
		ch := sb.ensureSymlink(absPath, meta["Backup-File-Path"])
		postLog(LogEvent{
			EventType: "SYMLINK", Severity: sevFor("SYMLINK"),
			Path: absPath, FileType: "SYMLINK",
			Restored: ch, Note: "Symlink enforced from backup",
		})
	}
}

/************ seed (sửa để hỗ trợ include DIR + FILE) ************/
func (sb *SnapBack) postDIR(path string, mode os.FileMode) {
	sb.muPostedDirs.Lock()
	if _, ok := sb.postedDirs[path]; ok {
		sb.muPostedDirs.Unlock()
		return
	}
	sb.postedDirs[path] = struct{}{}
	sb.muPostedDirs.Unlock()

	req, _ := http.NewRequest("POST", strings.TrimRight(baseURL, "/"), nil)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Path", path)
	req.Header.Set("Permission-File", fmt.Sprintf("%#o", mode))
	req.Header.Set("Type-File", "DIR")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
func (sb *SnapBack) postDATA(path string, data []byte, md5sum string, mode os.FileMode) {
	req, _ := http.NewRequest("POST", strings.TrimRight(baseURL, "/"), bytes.NewReader(data))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Path", path)
	req.Header.Set("Permission-File", fmt.Sprintf("%#o", mode))
	req.Header.Set("Type-File", "DATA")
	req.Header.Set("Md5", md5sum)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
func (sb *SnapBack) postSYMLINK(path string, target string, mode os.FileMode) {
	req, _ := http.NewRequest("POST", strings.TrimRight(baseURL, "/"), bytes.NewBufferString(target))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Path", path)
	req.Header.Set("Permission-File", fmt.Sprintf("%#o", mode))
	req.Header.Set("Type-File", "SYMLINK")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
func (sb *SnapBack) seedAllIncludes() {
	for _, inc := range sb.pathIncludeList {
		if sb.isExcluded(inc) {
			continue
		}
		absInc, _ := filepath.Abs(inc)
		info, err := os.Lstat(absInc)
		if err != nil {
			// không tồn tại → bỏ qua
			continue
		}
		// Post chain cha: nếu inc là DIR → bao gồm chính nó.
		// Nếu inc là FILE/SYMLINK → chỉ post các DIR cha (không post file như DIR).
		var startDir string
		if info.IsDir() {
			startDir = absInc
		} else {
			startDir = filepath.Dir(absInc)
		}
		cur := startDir
		for {
			sb.postDIR(cur, getFileMode(cur))
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}

		// Xử lý theo loại của include path
		if info.IsDir() {
			// walk bên trong
			filepath.WalkDir(absInc, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if sb.isExcluded(p) {
					if d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				mode := getFileMode(p)
				if d.IsDir() {
					sb.postDIR(p, mode)
					return nil
				}
				if checkSymlink(p) {
					tgt, _ := os.Readlink(p)
					if !filepath.IsAbs(tgt) {
						tgt = filepath.Join(filepath.Dir(p), tgt)
					}
					sb.postSYMLINK(p, tgt, mode)
					return nil
				}
				md5sum := toMD5(p)
				if md5sum != "" {
					if data, err := os.ReadFile(p); err == nil {
						sb.postDATA(p, data, md5sum, mode)
					}
				}
				return nil
			})
			continue
		}

		// include là FILE hoặc SYMLINK
		mode := getFileMode(absInc)
		if checkSymlink(absInc) {
			tgt, _ := os.Readlink(absInc)
			if !filepath.IsAbs(tgt) {
				tgt = filepath.Join(filepath.Dir(absInc), tgt)
			}
			sb.postSYMLINK(absInc, tgt, mode)
		} else {
			md5sum := toMD5(absInc)
			if md5sum != "" {
				if data, err := os.ReadFile(absInc); err == nil {
					sb.postDATA(absInc, data, md5sum, mode)
				}
			}
		}
	}
}

/************ build & refresh watch set ************/
func (sb *SnapBack) buildWatchSet() map[string]struct{} {
	set := make(map[string]struct{})
	sb.muSnap.RLock()
	defer sb.muSnap.RUnlock()
	for p, meta := range sb.snapshot {
		if sb.isExcluded(p) {
			continue
		}
		under := false
		for _, root := range sb.pathIncludeList {
			if p == root || strings.HasPrefix(p, root+string(os.PathSeparator)) {
				under = true
				break
			}
			// nếu root là FILE, vẫn cần watch thư mục chứa p (p == root trong snapshot)
			if root == p {
				under = true
				break
			}
		}
		if !under {
			continue
		}
		dir := filepath.Dir(p)
		set[dir] = struct{}{}
		if strings.ToUpper(meta["Type-File"]) == "DIR" {
			set[p] = struct{}{}
		}
	}
	return set
}
func (sb *SnapBack) refreshWatches() {
	if sb.watcher == nil {
		return
	}
	want := sb.buildWatchSet()
	sb.muWatch.Lock()
	for d := range want {
		if sb.isExcluded(d) || checkSymlink(d) {
			continue
		}
		if _, ok := sb.watched[d]; ok {
			continue
		}
		_ = sb.watcher.Add(d)
		sb.watched[d] = struct{}{}
	}
	sb.muWatch.Unlock()
}

/************ sweep ************/
func (sb *SnapBack) fullSweep() {
	for _, root := range sb.pathIncludeList {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if sb.isExcluded(p) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			sb.ensurePath(p)
			return nil
		})
		// khôi phục path thiếu
		sb.muSnap.RLock()
		for path := range sb.snapshot {
			if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) {
				if _, err := os.Lstat(path); os.IsNotExist(err) {
					sb.ensurePath(path)
				}
			}
		}
		sb.muSnap.RUnlock()
	}
}
func (sb *SnapBack) microSweep() {
	sb.muWatch.Lock()
	for dir := range sb.watched {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, de := range ents {
			p := filepath.Join(dir, de.Name())
			if sb.isExcluded(p) || isSelfProtected(p) {
				continue
			}
			if _, ok := sb.hasMeta(p); !ok {
				_ = os.RemoveAll(p)
			}
		}
	}
	sb.muWatch.Unlock()
}

/************ watcher ************/
func (sb *SnapBack) watchLoop() {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Println("watcher error:", err)
		return
	}
	sb.watcher = w
	sb.refreshWatches()

	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				abs, _ := filepath.Abs(ev.Name)
				if isSelfProtected(abs) {
					continue
				}

				// CREATE
				if ev.Has(fsnotify.Create) {
					if fi, err := os.Lstat(abs); err == nil {
						if fi.IsDir() {
							if !sb.isAllowedDir(abs) {
								_ = os.RemoveAll(abs)
								postLog(LogEvent{EventType: "CREATE", Severity: sevFor("CREATE"), Path: abs, FileType: "DIR", Note: "Unauthorized DIR -> removed"})
								continue
							}
							sb.muWatch.Lock()
							if _, seen := sb.watched[abs]; !seen && !checkSymlink(abs) {
								_ = sb.watcher.Add(abs)
								sb.watched[abs] = struct{}{}
							}
							sb.muWatch.Unlock()
						} else if fi.Mode()&os.ModeSymlink != 0 {
							if !sb.isAllowedSymlink(abs) {
								_ = os.Remove(abs)
								postLog(LogEvent{EventType: "CREATE", Severity: sevFor("CREATE"), Path: abs, FileType: "SYMLINK", Note: "Unauthorized SYMLINK -> removed"})
								continue
							}
							sb.ensurePath(abs)
						} else {
							if !sb.isAllowedFile(abs) {
								_ = os.Remove(abs)
								postLog(LogEvent{EventType: "CREATE", Severity: sevFor("CREATE"), Path: abs, FileType: "DATA", Note: "Unauthorized FILE -> removed"})
								continue
							}
							sb.ensurePath(abs)
						}
					}
				}

				// WRITE
				if ev.Has(fsnotify.Write) {
					if _, ok := sb.hasMeta(abs); !ok {
						_ = os.RemoveAll(abs)
						postLog(LogEvent{EventType: "WRITE", Severity: sevFor("WRITE"), Path: abs, FileType: "DATA", Note: "Write to unknown path -> removed"})
						continue
					}
					sb.ensurePath(abs)
				}

				// RENAME
				if ev.Has(fsnotify.Rename) {
					if _, ok := sb.hasMeta(abs); !ok {
						_ = os.RemoveAll(abs)
						postLog(LogEvent{EventType: "RENAME", Severity: sevFor("WRITE"), Path: abs, FileType: "DATA", Note: "Rename to unknown path -> removed"})
						continue
					}
					sb.ensurePath(abs)
				}

				// REMOVE
				if ev.Has(fsnotify.Remove) {
					if _, ok := sb.hasMeta(abs); ok {
						sb.ensurePath(abs)
					}
				}

				// CHMOD
				if ev.Has(fsnotify.Chmod) {
					if meta, ok := sb.hasMeta(abs); ok {
						info, _ := os.Lstat(abs)
						nowPerm := permOf(info)
						expPerm := meta["Permission-File"]
						if expPerm != "" && nowPerm != "" && nowPerm != expPerm {
							if p, err := strconv.ParseUint(expPerm, 8, 32); err == nil {
								_ = os.Chmod(abs, os.FileMode(p))
								postLog(LogEvent{EventType: "CHMOD", Severity: sevFor("CHMOD"), Path: abs, FileType: strings.ToUpper(meta["Type-File"]), OldPerm: nowPerm, NewPerm: expPerm, Restored: true, Note: "Permission restored"})
							}
						}
					}
				}

			case <-w.Errors:
				// Overflow hoặc lỗi watcher: full-sweep để đồng bộ cứng
				sb.fullSweep()
			}
		}
	}()
}

/************ poll snapshot ************/
func (sb *SnapBack) pollSnapshot() {
	if m := sb.fetchSnapshot(); m != nil {
		sb.muSnap.Lock()
		sb.snapshot = m
		sb.muSnap.Unlock()
		sb.refreshWatches()
	}
	t := time.NewTicker(snapshotPollInterval)
	defer t.Stop()
	for range t.C {
		if m := sb.fetchSnapshot(); m != nil {
			sb.muSnap.Lock()
			sb.snapshot = m
			sb.muSnap.Unlock()
			sb.refreshWatches()
		}
	}
}

/************ Run ************/
func (sb *SnapBack) Run() {
	hn, _ := os.Hostname()
	if hn == "" {
		hn = guessHostname()
	}
	hostName = hn

	// Seed include: xử lý đúng DIR/FILE/SYMLINK
	sb.seedAllIncludes()

	// Load snapshot ban đầu
	if m := sb.fetchSnapshot(); m != nil {
		sb.muSnap.Lock()
		sb.snapshot = m
		sb.muSnap.Unlock()
	}

	// Watcher + poll + micro-sweep
	go sb.pollSnapshot()
	sb.watchLoop()

	go func() {
		tk := time.NewTicker(microSweepInterval)
		defer tk.Stop()
		for range tk.C {
			sb.microSweep()
		}
	}()

	select {}
}

/************ main ************/
func main() {
	host := flag.String("host", "localhost", "Server host")
	port := flag.String("port", "1412", "Server port")
	scheme := flag.String("scheme", "http", "Server scheme")
	urlFlag := flag.String("url", "", "Backup server URL (override host/port/scheme)")
	logURLFlag := flag.String("log_url", "", "Log server URL (optional)")
	include := flag.String("pil", "", "Path include list, comma-separated")
	exclude := flag.String("pel", "", "Path exclude list, comma-separated")
	cacheMB := flag.Int("cache_mb", 64, "RAM cache MB for backup blobs")
	flag.Parse()

	if p, err := os.Executable(); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			selfExePath = abs
		}
	}

	if *urlFlag != "" {
		baseURL = *urlFlag
	} else {
		baseURL = fmt.Sprintf("%s://%s:%s", *scheme, *host, *port)
	}
	if *logURLFlag != "" {
		logURL = *logURLFlag
	} else {
		logURL = baseURL
	}
	if *cacheMB <= 0 {
		*cacheMB = 64
	}
	blobLimit = *cacheMB * 1024 * 1024

	if *include == "" {
		fmt.Println("Include list is required. Use -pil \"path1,path2,...\"")
		os.Exit(1)
	}
	incl := splitAndTrim(*include)
	excl := []string{}
	if *exclude != "" {
		excl = splitAndTrim(*exclude)
	}

	sb := NewSnapBack(incl, excl)
	sb.Run()
}

/************ helpers ************/
func guessHostname() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "unknown"
}

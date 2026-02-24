// Server.go
package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	uploadDir      = "./backups"
	backupFilePath = "./backup_file.json"
)

type BackupFile struct {
	TypeFile       string `json:"Type-File"`
	Md5            string `json:"Md5"`
	PermissionFile string `json:"Permission-File"`
	BackupFilePath string `json:"Backup-File-Path"`
}

/************ In-memory log store ************/
type LogItem struct {
	ID        string `json:"id,omitempty"`
	Timestamp int64  `json:"ts_unix,omitempty"`
	TimeISO   string `json:"ts_iso,omitempty"`
	Host      string `json:"host"`
	EventType string `json:"event_type"`
	Severity  string `json:"severity"`
	Path      string `json:"path"`
	FileType  string `json:"file_type"`
	OldMD5    string `json:"old_md5,omitempty"`
	NewMD5    string `json:"new_md5,omitempty"`
	OldPerm   string `json:"old_perm,omitempty"`
	NewPerm   string `json:"new_perm,omitempty"`
	Note      string `json:"note,omitempty"`
	Restored  bool   `json:"restored,omitempty"`
}

var (
	muBackup     sync.Mutex
	muLogs       sync.RWMutex
	logBuf       = make([]LogItem, 0, 100000)
	maxLogBuffer = 100000
)

func init() {
	_ = os.MkdirAll(uploadDir, os.ModePerm)
	rand.Seed(time.Now().UnixNano())
}

func ensureStorage() {
	if _, err := os.Stat(uploadDir); os.IsNotExist(err) {
		_ = os.MkdirAll(uploadDir, os.ModePerm)
	}
	if _, err := os.Stat(backupFilePath); os.IsNotExist(err) {
		empty := make(map[string]BackupFile)
		_ = saveJSON(empty)
	}
}

func generateFileName(original string) string {
	randomBytes := make([]byte, 16)
	_, _ = rand.Read(randomBytes)
	randomHash := md5.Sum(randomBytes)
	return hex.EncodeToString(randomHash[:]) + "-" + filepath.Base(original)
}

func loadJSON() (map[string]BackupFile, error) {
	file, err := os.Open(backupFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]BackupFile), nil
		}
		return nil, err
	}
	defer file.Close()

	var data map[string]BackupFile
	dec := json.NewDecoder(file)
	if err := dec.Decode(&data); err != nil {
		return make(map[string]BackupFile), nil
	}
	return data, nil
}

func saveJSON(data map[string]BackupFile) error {
	tmp := backupFilePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, backupFilePath)
}

/************ perm normalize ************/
func normalizePerm(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%#o", os.FileMode(v)), true
}

/************ Backup server ************/
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	ensureStorage()
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST", http.StatusMethodNotAllowed)
		return
	}
	path := r.Header.Get("Path")
	md5sum := r.Header.Get("Md5")
	permissionFile := r.Header.Get("Permission-File")
	typeFile := r.Header.Get("Type-File")
	if path == "" || permissionFile == "" || typeFile == "" {
		http.Error(w, "Missing headers", http.StatusBadRequest)
		return
	}
	if norm, ok := normalizePerm(permissionFile); ok {
		if norm != "" {
			permissionFile = norm
		}
	} else {
		http.Error(w, "Invalid Permission-File", http.StatusBadRequest)
		return
	}

	var fileName string
	if typeFile != "DIR" {
		fileName = generateFileName(path)
		fullPath := filepath.Join(uploadDir, fileName)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body error", http.StatusInternalServerError)
			return
		}
		if typeFile == "DATA" && md5sum == "" {
			sum := md5.Sum(body)
			md5sum = hex.EncodeToString(sum[:])
		}
		if err := os.WriteFile(fullPath, body, 0644); err != nil {
			http.Error(w, "save error", http.StatusInternalServerError)
			return
		}
	}

	muBackup.Lock()
	defer muBackup.Unlock()
	data, _ := loadJSON()
	data[path] = BackupFile{
		TypeFile:       typeFile,
		Md5:            md5sum,
		PermissionFile: permissionFile,
		BackupFilePath: fileName,
	}
	_ = saveJSON(data)
	w.WriteHeader(http.StatusOK)
}

func updateBackupHandler(w http.ResponseWriter, r *http.Request) {
	ensureStorage()
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST", http.StatusMethodNotAllowed)
		return
	}
	path := r.Header.Get("Path")
	permNewRaw := strings.TrimSpace(r.Header.Get("Permission-File"))
	if path == "" {
		http.Error(w, "Missing Path", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body error", http.StatusInternalServerError)
		return
	}
	if len(body) == 0 {
		http.Error(w, "no file uploaded", http.StatusBadRequest)
		return
	}

	permNew := ""
	if permNewRaw != "" {
		if norm, ok := normalizePerm(permNewRaw); ok {
			permNew = norm
		} else {
			http.Error(w, "Invalid Permission-File", http.StatusBadRequest)
			return
		}
	}

	muBackup.Lock()
	defer muBackup.Unlock()
	data, _ := loadJSON()

	cur, ok := data[path]
	if !ok {
		http.Error(w, "Path not found in backup_file.json", http.StatusNotFound)
		return
	}
	if strings.ToUpper(cur.TypeFile) != "DATA" {
		http.Error(w, "Only DATA entries can be updated", http.StatusBadRequest)
		return
	}

	fileName := cur.BackupFilePath
	if fileName == "" {
		fileName = generateFileName(path)
	}
	fullPath := filepath.Join(uploadDir, fileName)
	if err := os.WriteFile(fullPath, body, 0644); err != nil {
		http.Error(w, "save error", http.StatusInternalServerError)
		return
	}

	sum := md5.Sum(body)
	cur.Md5 = hex.EncodeToString(sum[:])
	if permNew != "" {
		cur.PermissionFile = permNew
	}
	cur.BackupFilePath = fileName
	data[path] = cur

	if err := saveJSON(data); err != nil {
		http.Error(w, "save json error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data[path])
}

func fileHandler(w http.ResponseWriter, r *http.Request) {
	ensureStorage()
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		http.Error(w, "Missing filename", http.StatusBadRequest)
		return
	}
	fullPath := filepath.Join(uploadDir, filename)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	_, _ = w.Write(data)
}

func infoHandler(w http.ResponseWriter, r *http.Request) {
	ensureStorage()
	muBackup.Lock()
	defer muBackup.Unlock()
	data, _ := loadJSON()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

/************ log server ************/
func postLogHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST", http.StatusMethodNotAllowed)
		return
	}
	var item LogItem
	if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if item.Timestamp == 0 {
		item.Timestamp = time.Now().Unix()
		item.TimeISO = time.Now().Format(time.RFC3339)
	}
	item.ID = strconv.FormatInt(time.Now().UnixNano(), 10)

	muLogs.Lock()
	if len(logBuf) >= maxLogBuffer {
		copy(logBuf, logBuf[1:])
		logBuf = logBuf[:len(logBuf)-1]
	}
	logBuf = append(logBuf, item)
	muLogs.Unlock()

	w.WriteHeader(http.StatusOK)
}

func getLogsHandler(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	typ := strings.TrimSpace(r.URL.Query().Get("type"))
	pathExact := strings.TrimSpace(r.URL.Query().Get("path"))
	pathLike := strings.TrimSpace(r.URL.Query().Get("path_like"))
	sev := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("severity")))

	muLogs.RLock()
	cp := make([]LogItem, len(logBuf))
	copy(cp, logBuf)
	muLogs.RUnlock()

	sort.Slice(cp, func(i, j int) bool { return cp[i].Timestamp > cp[j].Timestamp })

	out := make([]LogItem, 0, limit)
	for _, it := range cp {
		match := true
		if host != "" && !strings.EqualFold(it.Host, host) {
			match = false
		}
		if typ != "" && !strings.EqualFold(it.EventType, typ) {
			match = false
		}
		if sev != "" && strings.ToUpper(it.Severity) != sev {
			match = false
		}
		if pathExact != "" && it.Path != pathExact {
			match = false
		}
		if pathLike != "" && !strings.Contains(strings.ToLower(it.Path), strings.ToLower(pathLike)) {
			match = false
		}
		if !match {
			continue
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

/************ UI (có Severity SAFE) ************/
const uiHTML = `
<!doctype html>
<html lang="vi">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>SnapBack</title>
<style>
:root{
  --bg:#0b1020; --card:#0e162b; --fg:#eaf1ff; --muted:#9fb0d9;
  --grid:#223055; --grid-strong:#2c3d66; --zebra:#0a1226; --zebra2:#0c1831;
  --low:#1b5e20; --low-t:#c8f7d1;
  --med:#6d4c00; --med-t:#ffe6a8;
  --high:#8b1e1e; --high-t:#ffd1d1;
  --crit:#8a1451; --crit-t:#ffd2ea;
  --safe:#135e75; --safe-t:#c9f4ff;
  --t-data:#163774; --t-data-t:#d7e6ff; --t-dir:#1d3a18; --t-dir-t:#ddffd2; --t-sym:#3b2348; --t-sym-t:#ffd9ff;
  --sel:#1a2a55; --disabled:#2a2f3f;
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 system-ui,Segoe UI,Roboto,Inter,Arial}
header{padding:12px 16px;border-bottom:1px solid var(--grid-strong);font-weight:700}
.layout{display:grid;grid-template-columns:1fr 720px;gap:14px;padding:14px}
@media(max-width:1500px){.layout{grid-template-columns:1fr}}
.card{background:var(--card);border:1px solid var(--grid-strong);border-radius:12px;overflow:hidden;display:flex;flex-direction:column}
.hd{padding:10px 12px;border-bottom:1px solid var(--grid-strong);display:flex;gap:10px;align-items:center;justify-content:space-between}
.hd .right{display:flex;gap:10px;align-items:center;flex-wrap:wrap}
.bd{flex:1;min-height:0}
input,select,button,textarea{background:#0b142b;color:var(--fg);border:1px solid var(--grid);border-radius:8px;padding:6px 8px}
button{cursor:pointer}
.kv{color:var(--muted);font-size:12px}
.table-wrap{height:calc(56vh - 24px);overflow:auto}
.table{width:100%;border-collapse:collapse;min-width:1200px}
thead th{position:sticky;top:0;background:#101b36;z-index:1;color:var(--muted);font-weight:700;font-size:12px;letter-spacing:.02em;border:1px solid var(--grid-strong);padding:10px 10px;text-align:left;white-space:nowrap}
tbody td{border:1px solid var(--grid);padding:10px 10px;white-space:nowrap}
tbody tr:nth-child(odd){background:var(--zebra)}
tbody tr:nth-child(even){background:var(--zebra2)}
tbody tr.sel{background:var(--sel) !important}
tbody tr.disabled{opacity:.55}
td.path{max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
td.details{white-space:normal;overflow-wrap:anywhere;line-height:1.35}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.badge{padding:2px 10px;border-radius:999px;font-weight:700;font-size:12px;border:1px solid #0004;display:inline-block}
.low{background:var(--low);color:var(--low-t)}
.medium{background:var(--med);color:var(--med-t)}
.high{background:var(--high);color:var(--high-t)}
.critical{background:var(--crit);color:var(--crit-t)}
.safe{background:var(--safe);color:var(--safe-t)}
.t-data{background:var(--t-data);color:var(--t-data-t)}
.t-dir{background:var(--t-dir);color:var(--t-dir-t)}
.t-sym{background:var(--t-sym);color:var(--t-sym-t)}
.form{padding:12px;border-top:1px solid var(--grid-strong);display:grid;gap:8px}
.note{font-size:12px;color:var(--muted)}
.msg{font-size:13px;padding:6px 10px;border-radius:8px}
.msg.ok{background:#1b5e20;color:#d8ffd8}
.msg.err{background:#8b1e1e;color:#ffd8d8}
</style>
</head>
<body>
<header>SnapBack — Logs (trái) & Backup (phải)</header>

<div class="layout">

  <!-- LEFT: LOGS -->
  <main class="card">
    <div class="hd">
      <b>REALTIME LOGS</b>
      <div class="right">
        <label>Refresh(s) <input id="refreshSec" type="number" min="1" max="300" value="5" style="width:70px"></label>
        <label>Limit <input id="limit" type="number" min="10" max="2000" value="500" style="width:80px"></label>
        <label>Host <input id="host" placeholder="(all)" style="width:130px"></label>
        <label>Type
          <select id="type">
            <option value="">(all)</option>
            <option>CREATE</option><option>WRITE</option><option>REMOVE</option><option>CHMOD</option><option>SYMLINK</option><option>BACKUP_UPDATE</option>
          </select>
        </label>
        <label>Severity
          <select id="severity">
            <option value="">(all)</option>
            <option>SAFE</option>
            <option>LOW</option>
            <option>MEDIUM</option>
            <option>HIGH</option>
            <option>CRITICAL</option>
          </select>
        </label>
        <label>Path <input id="path" placeholder="exact" style="width:200px"></label>
        <label>Like <input id="pathLike" placeholder="contains" style="width:200px"></label>
        <button id="apply">Apply</button>
        <button id="pause">Pause</button>
        <span class="kv">Last: <b id="last"></b></span>
        <span id="status" class="kv"></span>
      </div>
    </div>
    <div class="bd">
      <div class="table-wrap">
        <table class="table">
          <colgroup>
            <col style="width:16%"><col style="width:12%"><col style="width:10%"><col style="width:12%"><col style="width:14%"><col style="width:36%">
          </colgroup>
          <thead><tr><th>Time</th><th>Host</th><th>Type</th><th>Severity</th><th>Path</th><th>Details</th></tr></thead>
          <tbody id="tbody"></tbody>
        </table>
      </div>
    </div>
  </main>

  <!-- RIGHT: BACKUP -->
  <aside class="card">
    <div class="hd">
      <b>BACKUP STATUS</b>
      <div class="right">
        <input id="bkSearch" placeholder="Tìm path (contains…)" style="width:260px"/>
        <button id="bkReload">Reload</button>
        <span class="kv">Tổng số: <b id="bkCount">0</b></span>
      </div>
    </div>
    <div class="bd">
      <div class="table-wrap">
        <table class="table" id="bkTable">
          <colgroup>
            <col style="width:46%"><col style="width:12%"><col style="width:24%"><col style="width:10%"><col style="width:8%">
          </colgroup>
          <thead><tr><th>Path</th><th>Type</th><th>MD5</th><th>Permission</th><th>Backup File</th></tr></thead>
          <tbody id="bkBody"></tbody>
        </table>
      </div>
    </div>

    <div class="form">
      <div class="note">
        Chọn dòng <b>DATA</b> → Path & Permission tự điền. Update ghi đè file backup cũ, cập nhật MD5 và Permission (nếu đổi).
      </div>
      <div>
        <label>Path
          <input id="upPath" placeholder="/absolute/path/to/file" />
        </label>
      </div>
      <div>
        <label>Permission
          <input id="upPerm" value="0644" />
        </label>
      </div>
      <div>
        <label>File (DATA)
          <input type="file" id="upFile"/>
        </label>
      </div>
      <div>
        <button id="btnUpdate">Update Backup (DATA)</button>
        <span id="upMsg" class="msg" style="display:none"></span>
      </div>
    </div>

  </aside>

</div>

<script>
let timer=null, paused=false;
const tbody=document.getElementById('tbody');
const lastEl=document.getElementById('last');
const status=document.getElementById('status');
const bkBody=document.getElementById('bkBody');
const bkCount=document.getElementById('bkCount');
const bkSearch=document.getElementById('bkSearch');
const upPath=document.getElementById('upPath');
const upPerm=document.getElementById('upPerm');
const upFile=document.getElementById('upFile');
const upMsg=document.getElementById('upMsg');

let backupRaw={};
let selectedPath="";

function esc(s){return (s==null?'':String(s)).replace(/[&<>"]/g,(c)=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]||c));}
function sevBadge(s){const m={low:'low',medium:'medium',high:'high',critical:'critical',safe:'safe'};return '<span class="badge '+(m[(s||'').toLowerCase()]||'')+'">'+esc(s||'-')+'</span>';}
function typeBadge(t){const m={DATA:'t-data',DIR:'t-dir',SYMLINK:'t-sym'};const T=(t||'').toUpperCase();return '<span class="badge '+(m[T]||'')+'">'+esc(T||'-')+'</span>';}

function renderLogs(rows){
  tbody.innerHTML='';
  for(const r of rows){
    const detHTML=[], detPlain=[];
    if(r.file_type){ detHTML.push('type: <b>'+esc(r.file_type)+'</b>'); detPlain.push('type: '+(r.file_type||'-')); }
    if(r.old_md5||r.new_md5){ detHTML.push('md5: <span class="mono"><b>'+esc(r.old_md5||'-')+'</b> → <b>'+esc(r.new_md5||'-')+'</b></span>'); detPlain.push('md5: '+(r.old_md5||'-')+' → '+(r.new_md5||'-')); }
    if(r.old_perm||r.new_perm){ detHTML.push('perm: <b>'+esc(r.old_perm||'-')+'</b> → <b>'+esc(r.new_perm||'-')+'</b>'); detPlain.push('perm: '+(r.old_perm||'-')+' → '+(r.new_perm||'-')); }
    if(r.restored){ detHTML.push('restored: <b>'+r.restored+'</b>'); detPlain.push('restored: '+r.restored); }
    if(r.note){ detHTML.push('note: <b>'+esc(r.note)+'</b>'); detPlain.push('note: '+r.note); }
    const detailsHTML = detHTML.join(' | ');
    const detailsTitle = detPlain.join(' | ');
    const timeStr = new Date((r.ts_unix||0)*1000).toLocaleString();

    const tr=document.createElement('tr');
    tr.innerHTML =
      '<td title="'+esc(timeStr)+'">'+esc(timeStr)+'</td>'+
      '<td title="'+esc(r.host||"-")+'">'+esc(r.host||"-")+'</td>'+
      '<td title="'+esc(r.event_type||"-")+'"><b>'+esc(r.event_type||"-")+'</b></td>'+
      '<td title="'+esc(r.severity||"-")+'">'+sevBadge(r.severity)+'</td>'+
      '<td class="path" title="'+esc(r.path||"-")+'">'+esc(r.path||"-")+'</td>'+
      '<td class="details" title="'+esc(detailsTitle)+'">'+detailsHTML+'</td>';
    tbody.appendChild(tr);
  }
}

async function fetchLogs(){
  if (paused) return;
  const qs=new URLSearchParams({ limit:String(document.getElementById('limit').value||500) });
  const host=document.getElementById('host').value.trim();
  const type=document.getElementById('type').value;
  const path=document.getElementById('path').value.trim();
  const like=document.getElementById('pathLike').value.trim();
  const severity=document.getElementById('severity').value;
  if(host) qs.set('host',host);
  if(type) qs.set('type',type);
  if(path) qs.set('path',path);
  if(like) qs.set('path_like',like);
  if(severity) qs.set('severity',severity);
  try{
    const res=await fetch('/logs?'+qs.toString(),{cache:'no-store'});
    renderLogs(await res.json());
    lastEl.textContent=new Date().toLocaleString();
    status.textContent='';
  }catch{ status.textContent='(fetch logs error)'; }
}
function apply(){
  if (timer) clearInterval(timer);
  let sec=parseInt(document.getElementById('refreshSec').value||'5',10);
  if(!(sec>0)) sec=5; if(sec>300) sec=300;
  timer=setInterval(fetchLogs, sec*1000);
  fetchLogs();
}
document.getElementById('apply').addEventListener('click', apply);
document.getElementById('pause').addEventListener('click', function(){ paused=!paused; this.textContent=paused?'Resume':'Pause'; if(!paused) fetchLogs(); });

/* BACKUP TABLE */
function clearSelection(){ for(const tr of bkBody.querySelectorAll('tr.sel')) tr.classList.remove('sel'); }
function pickRow(tr, path, meta){
  clearSelection();
  tr.classList.add('sel');
  selectedPath = path;
  upPath.value = path;
  upPerm.value = (meta["Permission-File"]||"0644");
  upFile.value = "";
  upFile.focus();
}
function renderBackup(){
  const term=bkSearch.value.trim().toLowerCase();
  const entries=Object.entries(backupRaw).sort((a,b)=>a[0]<b[0]?-1:1);
  bkBody.innerHTML=''; let cnt=0;
  for(const [path,meta] of entries){
    if(term && path.toLowerCase().indexOf(term)===-1) continue;
    cnt++;
    const t=(meta["Type-File"]||"-"), md5=(meta["Md5"]||"-"), perm=(meta["Permission-File"]||"-"), bfile=(meta["Backup-File-Path"]||"-");
    const tr=document.createElement('tr');
    if((t||"").toUpperCase()!=="DATA"){ tr.classList.add('disabled'); tr.title="Chỉ hỗ trợ update DATA"; }
    tr.innerHTML =
      '<td class="path" title="'+esc(path)+'">'+esc(path)+'</td>'+
      '<td title="'+esc(t)+'">'+typeBadge(t)+'</td>'+
      '<td class="mono" title="'+esc(md5)+'">'+esc(md5)+'</td>'+
      '<td class="mono" title="'+esc(perm)+'">'+esc(perm)+'</td>'+
      '<td class="mono" title="'+esc(bfile)+'">'+esc(bfile)+'</td>';
    if((t||"").toUpperCase()==="DATA"){
      tr.style.cursor='pointer';
      tr.addEventListener('click', ()=>pickRow(tr, path, meta));
    }
    bkBody.appendChild(tr);
  }
  bkCount.textContent=String(cnt);
  if(selectedPath && backupRaw[selectedPath]){
    for(const tr of bkBody.querySelectorAll('tr')){
      if(tr.firstChild && tr.firstChild.textContent===selectedPath){ tr.classList.add('sel'); break; }
    }
  }
}
async function fetchBackup(){
  try{ const res=await fetch('/backup_file',{cache:'no-store'}); backupRaw=await res.json(); }catch{ backupRaw={}; }
  renderBackup();
}
document.getElementById('bkReload').addEventListener('click', fetchBackup);

/* UPDATE BACKUP */
function showMsg(text, isErr){
  upMsg.textContent=text;
  upMsg.className='msg ' + (isErr?'err':'ok');
  upMsg.style.display='inline-block';
  setTimeout(()=>{ upMsg.style.display='none'; }, 3500);
}
async function doUpdate(){
  upMsg.style.display='none';
  const path = (upPath.value||selectedPath||"").trim();
  const perm = (upPerm.value||"").trim();
  if(!path){ showMsg('Chọn một dòng DATA hoặc nhập Path.', true); return; }
  const f = upFile.files[0];
  if(!f){ showMsg('Chọn file để upload (DATA).', true); return; }
  const body = new Uint8Array(await f.arrayBuffer());
  const headers = { 'Path': path };
  if (perm) headers['Permission-File'] = perm;
  try{
    const res = await fetch('/update_backup', { method:'POST', headers, body });
    if(!res.ok){
      const t = await res.text();
      showMsg('Update lỗi: '+t, true);
      return;
    }
    await fetchBackup();
    showMsg('Cập nhật backup thành công!', false);
  }catch(e){
    showMsg('Lỗi kết nối: '+(e&&e.message?e.message:e), true);
  }
}
document.getElementById('btnUpdate').addEventListener('click', doUpdate);

/* init */
fetchBackup();
apply();
</script>
</body>
</html>
`

func uiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(uiHTML))
}

/************ bootstrap ************/
func main() {
	backupHost := flag.String("backup_host", "0.0.0.0", "Backup host")
	backupPort := flag.String("backup_port", "1412", "Backup port")
	logHost := flag.String("log_host", "0.0.0.0", "Log/UI host")
	logPort := flag.String("log_port", "1413", "Log/UI port")
	flag.Parse()

	ensureStorage()

	// backup server
	bmux := http.NewServeMux()
	bmux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			uploadHandler(w, r)
		} else if r.Method == http.MethodGet && r.URL.Query().Has("filename") {
			fileHandler(w, r)
		} else {
			http.Error(w, "Invalid method or parameters", http.StatusBadRequest)
		}
	})
	bmux.HandleFunc("/backup_file", infoHandler)
	bmux.HandleFunc("/update_backup", updateBackupHandler)

	// log/ui server
	lmux := http.NewServeMux()
	lmux.HandleFunc("/log", postLogHandler)
	lmux.HandleFunc("/logs", getLogsHandler)
	lmux.HandleFunc("/backup_file", infoHandler)
	lmux.HandleFunc("/update_backup", updateBackupHandler)
	lmux.HandleFunc("/ui", uiHandler)
	lmux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})

	backupAddr := net.JoinHostPort(*backupHost, *backupPort)
	logAddr := net.JoinHostPort(*logHost, *logPort)

	go func() {
		log.Printf("[backup] http://%s\n", backupAddr)
		if err := http.ListenAndServe(backupAddr, bmux); err != nil {
			log.Fatalf("backup server error: %v", err)
		}
	}()
	log.Printf("[log/ui] http://%s/ui\n", logAddr)
	if err := http.ListenAndServe(logAddr, lmux); err != nil {
		log.Fatalf("log server error: %v", err)
	}
}

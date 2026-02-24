package main


import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)


const (
	uploadDir       = "./backups"
	backupFilePath  = "./backup_file.json"
)

var mu sync.Mutex


type BackupFile struct {
	TypeFile       string `json:"Type-File"`
	Md5        string `json:"Md5"`
	PermissionFile string `json:"Permission-File"`
	BackupFilePath string `json:"Backup-File-Path"`
}



func init() {
	os.MkdirAll(uploadDir, os.ModePerm)
	rand.Seed(time.Now().UnixNano())
}



func ensureStorage() {
	// Tạo lại thư mục backups nếu bị xóa
	if _, err := os.Stat(uploadDir); os.IsNotExist(err) {
		_ = os.MkdirAll(uploadDir, os.ModePerm)
	}

	// Tạo lại file backup_file.json nếu bị xóa
	if _, err := os.Stat(backupFilePath); os.IsNotExist(err) {
		emptyData := make(map[string]BackupFile)
		_ = saveJSON(emptyData)
	}
}


func generateFileName(original string) string {
	randomBytes := make([]byte, 16)
	rand.Read(randomBytes)
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
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&data)
	if err != nil {
		return make(map[string]BackupFile), nil
	}
	return data, nil
}




func saveJSON(data map[string]BackupFile) error {
	file, err := os.Create(backupFilePath)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ") // Để JSON dễ đọc
	return encoder.Encode(data)
}




func uploadHandler(w http.ResponseWriter, r *http.Request) {
	ensureStorage()

	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method allowed", http.StatusMethodNotAllowed)
		return
	}


	if err := r.ParseForm(); err != nil {
		http.Error(w, "ParseForm error", http.StatusBadRequest)
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

	if typeFile == "DATA" && md5sum == "" {
		http.Error(w, "Missing Md5 for DATA type", http.StatusBadRequest)
		return
	}

	var fileName = ""
	if typeFile != "DIR" {
		fileName = generateFileName(path)
		fullPath := filepath.Join(uploadDir, fileName)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Error reading file", http.StatusInternalServerError)
			return
		}

		err = os.WriteFile(fullPath, body, 0644)
		if err != nil {
			http.Error(w, "Error saving file", http.StatusInternalServerError)
			return
		}
	}

	mu.Lock()
	defer mu.Unlock()
	data, _ := loadJSON()
	data[path] = BackupFile{
		TypeFile: 				typeFile,
		Md5:        			md5sum,
		PermissionFile: 		permissionFile,
		BackupFilePath: 		fileName,
	}

	saveJSON(data)
	w.WriteHeader(http.StatusOK)
}



func infoHandler(w http.ResponseWriter, r *http.Request) {
	ensureStorage()

	mu.Lock()
	defer mu.Unlock()
	data, _ := loadJSON()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
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
	w.Write(data)
}



func main() {
	ensureStorage()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			uploadHandler(w, r)
		} else if r.Method == http.MethodGet && r.URL.Query().Has("filename") {
			fileHandler(w, r)
		} else {
			http.Error(w, "Invalid method or parameters", http.StatusBadRequest)
		}
	})
	http.HandleFunc("/backup_file", infoHandler)

	fmt.Println("Server running at http://0.0.0.0:1412")
	http.ListenAndServe(":1412", nil)
}
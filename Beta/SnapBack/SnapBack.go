package main

import (
	"flag"
	"fmt"
	"os"
	"io"
	"io/fs"
	"path/filepath"
	"github.com/charlievieth/fastwalk"
	"crypto/md5"
	"encoding/hex"
	"strings"
	"github.com/fsnotify/fsnotify"
	"syscall"
	"bytes"
	"net/http"
	"strconv"
	"encoding/json"
	"io/ioutil"
	"os/exec"
)


//const URL = "http://3r0th3rcc.ddns.net:1412"
const URL = "http://localhost:1412"


var conf = fastwalk.Config{
		Follow: false,
	}

func splitAndTrim(inp string) []string {
	rawList := strings.Split(inp, ",")
    result := make([]string, 0, len(rawList))
    for _, item := range rawList {
        trimmed := strings.TrimSpace(item)
        if trimmed != "" {
            result = append(result, trimmed)
        }
    }
    return result
}


func resolvePaths(paths []string) []string {
	var resolved []string
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err == nil {
			resolved = append(resolved, abs)
		}
	}
	return resolved
}


func getFileMode(path string) (os.FileMode) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}

	return info.Mode().Perm()
}



func checkSymlink(path string) bool {
	fd, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fd.Mode()&os.ModeSymlink != 0
}



func checkHardLink(path string) bool {
	fd, err := os.Lstat(path)

	if fd.IsDir() {return false}

	if err != nil {return false}

	stat, ok := fd.Sys().(*syscall.Stat_t)
	if !ok {return false}

	return stat.Nlink > 1
}




func getTargetSymlinkPath(symlinkPath string) string {
	absSymlink, err := filepath.Abs(symlinkPath)
	if err != nil {return ""}
	
	fi, err := os.Lstat(absSymlink)
	if err != nil {return ""}

	if fi.Mode()&os.ModeSymlink == 0 {return ""}

	linkTarget, err := os.Readlink(absSymlink)
	if err != nil {return ""}

	if !filepath.IsAbs(linkTarget) {
		linkTarget = filepath.Join(filepath.Dir(absSymlink), linkTarget)
	}

	linkTargetAbs, err := filepath.Abs(linkTarget)
	if err != nil {return ""}

	return linkTargetAbs
}



/*
func toMD5(path string) (string) {
	currentPerm :=  getFileMode(path)
	newPerm := currentPerm | 0400		// 0400 -> +w

	if newPerm != currentPerm {
		os.Chmod(path, newPerm)
	}

	data, err := os.ReadFile(path)
	if err != nil {return ""}
	
	hash := md5.Sum(data)
	return hex.EncodeToString(hash[:])
}
*/

func toMD5(path string) string {
	currentPerm := getFileMode(path)
	newPerm := currentPerm | 0400
	if newPerm != currentPerm {
		os.Chmod(path, newPerm)
	}

	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	hash := md5.New()
	if _, err := io.Copy(hash, f); err != nil {
		return ""
	}

	return hex.EncodeToString(hash.Sum(nil))
}


type SnapBack struct {
	pathIncludeList 		[]string
	pathExcludeList 		[]string
}



func NewSnapBack(includeList, excludeList []string) * SnapBack {
	return & SnapBack{
		pathIncludeList: 		resolvePaths(includeList),
		pathExcludeList: 		resolvePaths(excludeList),
	}
}



func (sb *SnapBack) isExcluded(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	for _, ex := range sb.pathExcludeList {
		if abs == ex || strings.HasPrefix(abs, ex + string(os.PathSeparator)) {
			return true
		}
	}
	return false
}




func (sb *SnapBack) getAllFilesInDir(path string) {
	err := fastwalk.Walk(&conf, path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {return nil}

		if sb.isExcluded(path) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.Type().IsRegular() || checkSymlink(path)  {	
		//if d.Type().IsRegular() || checkHardLink(path) || checkSymlink(path)  {	// Only path file
			// fmt.Println(path)
			sb.sendFile(path)
		}

		return nil
	})

	if err != nil {}
}



func (sb *SnapBack) checkAndBackup() {
	resp, err := http.Get(URL + "/backup_file")
	if err != nil {return}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	var serverBackupMap map[string]map[string]string
	err = json.Unmarshal(body, &serverBackupMap)
	if err != nil {return}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {return}

	defer watcher.Close()

	for _, path := range sb.pathIncludeList {
		if sb.isExcluded(path) {
			continue
		}

		fd, err := os.Lstat(path)
		if err != nil {continue}

		if fd.IsDir() {
			err = fastwalk.Walk(&conf, path, func(p string, d fs.DirEntry, err error) error {
				if err != nil {return nil}

				if d.IsDir() && !sb.isExcluded(p) {
					_ = watcher.Add(p)
				}

				return nil
			})

			if err != nil {}
		} else {
			parentDir := filepath.Dir(path)
			if !sb.isExcluded(parentDir) {
				_ = watcher.Add(parentDir)
			}
		}
	}
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {return}
				if sb.isExcluded(event.Name) {continue}
				
				absPath, _ := filepath.Abs(event.Name)
				/*
				if err == nil {
					fmt.Println("FS event", event , "on:", absPath)
				} else {
					fmt.Println("FS event", event , "on (raw):", event.Name)
				}*/

				if event.Has(fsnotify.Create) {
					if _, ok := serverBackupMap[absPath]; !ok {
						//fmt.Println("REMOVE:", absPath)
						os.RemoveAll(absPath)
					} /*else {
						fd, err := os.Lstat(event.Name)
						if err == nil && fd.IsDir() && !sb.isExcluded(event.Name) {
							_ = watcher.Add(event.Name)
							
							newAbs, err := filepath.Abs(event.Name)
							if err == nil {
								fmt.Println("Add new folder:", newAbs)
							} else {
								fmt.Println("Add new folder (raw):", event.Name)
							}
						}
					}*/
	
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Remove) {
					if backupData, ok := serverBackupMap[absPath]; ok {
						expectedMD5 := backupData["Md5"]
						if expectedMD5 != "" {
							currentMD5 := toMD5(absPath)
							if currentMD5 == expectedMD5 {
								continue
							}
						}

						fileType := backupData["Type-File"]
						switch fileType {
						case "DATA":
							resp, err := http.Get(URL + "/?filename=" + backupData["Backup-File-Path"])
							if err != nil {
								break
							}
							content, _ := io.ReadAll(resp.Body)
							resp.Body.Close()
							permStr := backupData["Permission-File"]
							permInt, _ := strconv.ParseUint(permStr, 8, 32)
							os.MkdirAll(filepath.Dir(absPath), 0755)
							os.WriteFile(absPath, content, os.FileMode(permInt))

						case "SYMLINK":
							resp, err := http.Get(URL + "/?filename=" + backupData["Backup-File-Path"])
							if err != nil {
								break
							}
							target, _ := io.ReadAll(resp.Body)
							resp.Body.Close()
							exec.Command("rm", "-rf", absPath).Run()
							exec.Command("ln", "-s", string(target), absPath).Run()

						case "DIR":
							permStr := backupData["Permission-File"]
							permInt, _ := strconv.ParseUint(permStr, 8, 32)
							os.MkdirAll(absPath, os.FileMode(permInt))
							watcher.Add(absPath)

						 }
					}
				}
				if event.Has(fsnotify.Chmod) {
					if info, err := os.Stat(absPath); err == nil {
						expectedPermStr := serverBackupMap[absPath]["Permission-File"]
						expectedPerm, _ := strconv.ParseUint(expectedPermStr, 8, 32)
						actualPerm := info.Mode().Perm()

						if actualPerm != os.FileMode(expectedPerm) {
							//fmt.Println("BACKUP CHMOD:", absPath)
							os.Chmod(absPath, os.FileMode(expectedPerm))
						}
					}
				}
			/*
			case err, ok := <-watcher.Errors:
				if !ok {return}
				// fmt.Println("Watcher error:", err)
			*/
			}
		}
	}()
	
	// Denied main goroutine
	select {}
}


func (sb *SnapBack) sendParentPaths(path string) {
	absPath, err := filepath.Abs(path)
	if err != nil {return}

	currentDir := filepath.Dir(absPath)

	for {
		req, err := http.NewRequest("POST", URL, nil)
		if err != nil {return}

		mode := getFileMode(currentDir)

		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Path", currentDir)
		req.Header.Set("Permission-File", fmt.Sprintf("%#o", mode))
		req.Header.Set("Type-File", "DIR")
		req.Header.Set("Md5", "")
		req.Header.Set("Backup-File-Path", "")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {return}
		resp.Body.Close()

		parentDir := filepath.Dir(currentDir)
		if parentDir == currentDir {break}
		currentDir = parentDir
	}
}




func (sb *SnapBack) sendFile(path string) {
	sb.sendParentPaths(path)

	/*
	if checkHardLink(path) {
		fmt.Println("FILE HARDLINK ->", path)
	}
	*/
	mode := getFileMode(path)

	var data []byte
	var md5sum string
	var fileType = "DATA"


	if checkSymlink(path) {
		fileType = "SYMLINK"
		target := getTargetSymlinkPath(path)
		if target != "" {
			data = []byte(target)
			//fmt.Println(fmt.Sprintf("Permission -> %#o -> ", mode), "FILE SYMLINK -> ln -s", path, target)
		}
	} else {
		md5sum = toMD5(path)
		if md5sum != "" {
			//fmt.Println(fmt.Sprintf("Permission -> %#o -> ", mode), "Send -> ", md5sum, " -> ", path)
			var err error
			data, err = os.ReadFile(path)
			if err != nil {return}
		} else {
			//fmt.Println("Cannot access -> ", md5sum, " -> ", path)
		}
	}

	req, err := http.NewRequest("POST", URL, bytes.NewReader(data))
	if err != nil {return}

	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Path", path)
	req.Header.Set("Permission-File", fmt.Sprintf("%#o", mode))
	req.Header.Set("Type-File", fileType)
	
	if fileType == "DATA" {
		req.Header.Set("Md5", md5sum)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {return}
	defer resp.Body.Close()

}


func (sb *SnapBack) Run() {
	// Save backup file to server
	for _, path := range sb.pathIncludeList {
		if sb.isExcluded(path) {continue}

		// Check file or dir exists
		fd, err := os.Lstat(path)

		if err != nil {continue}

		if fd.IsDir() {
			sb.getAllFilesInDir(path)
		} else {
			// fmt.Println(md5sum, path)
			sb.sendFile(path)
		}
	}

	sb.checkAndBackup()

}




// --------------------- MAIN ----------------------------
func main() {
	include := flag.String("pil", "", "")
	exclude := flag.String("pel", "", "")

	flag.Parse()


	if *include == "" {
		fmt.Println("Include list is required.")
		os.Exit(1)
	}
	includeList := splitAndTrim(*include)


	excludeList := []string{}
	if *exclude != "" {
		excludeList = splitAndTrim(*exclude)
	}

	snapback := NewSnapBack(includeList, excludeList)
	snapback.Run()
}
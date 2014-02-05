package main

import (
	"fmt"
	simplejson "github.com/bitly/go-simplejson"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"time"
	"gsyncd/index"
	"encoding/json"
	"io"
	"hash/crc32"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	fmt.Println("CPUs: ", runtime.NumCPU())
	input := args()
	done := make(chan bool)
	if len(input) >= 1 {
		start(input[0], done)
	}
	<-done
}

func start(configFile string, done chan bool) {
	b, err := ioutil.ReadFile(configFile)
	if err != nil {
		fmt.Println(configFile, " not found")
		go func() {
			done <- false
		}()
		return
	}
	json, _ := simplejson.NewJson(b)
	ip := json.Get("ip").MustString("127.0.0.1")
	port := json.Get("port").MustInt(6776)

	monitors := json.Get("monitors").MustMap()

	for k, v := range monitors {
		monitored, _ := v.(string)
		go startWork(ip, port, k, monitored, time.Second*10)
	}
}
func args() []string {
	ret := []string{}
	if len(os.Args) <= 1 {
		ret = append(ret, "gsync.json")
	} else {
		for i := 1; i < len(os.Args); i++ {
			ret = append(ret, os.Args[i])
		}
	}
	return ret
}

func startWork(ip string, port int, key string, monitored string, maxInterval time.Duration) {
	var lastIndexed int64 = 0
	sleepTime := time.Second
	for {
		serverIndexed := timeFromServer(ip, port, key)
		dirs := dirsFromServer(ip, port, key, lastIndexed)
		if len(dirs) == 0 {
			sleepTime *= 2
			if sleepTime >= maxInterval {
				sleepTime = maxInterval
			}
		} else {
			sleepTime = time.Second
			for _, dir := range dirs {
				dirMap, _ := dir.(map[string]interface{})
				dirPath, _ := dirMap["FilePath"].(string)
				dirStatus := dirMap["Status"].(string)
				dir := index.PathSafe(index.SlashSuffix(monitored) + dirPath)
				if dirStatus == "deleted" {
					os.RemoveAll(dir)
					continue
				}
				mode, _ := dirMap["FileMode"].(json.Number)
				dirMode, _ := mode.Int64()
				err := os.MkdirAll(dir, os.FileMode(dirMode))
				if err != nil {
					fmt.Println(err)
				}
				files := filesFromServer(ip, port, key, dirPath, lastIndexed)
				if len(files) == 0 {
					continue
				}
				for _, file := range files {
					fileMap, _ := file.(map[string] interface{})
					filePath, _ := fileMap["FilePath"].(string)
					fileStatus := fileMap["Status"].(string)

					file := index.PathSafe(index.SlashSuffix(monitored) + filePath)
					if fileStatus == "deleted" {
						os.RemoveAll(file)
						continue
					}
					size, _ := fileMap["FileSize"].(json.Number)
					fileSize, _ := size.Int64()
					if info, err := os.Stat(file); os.IsNotExist(err) {
						// file does not exists, downloaded
						out, _ := os.Create(file)
						defer out.Close()
						downloadFromServer(ip, port, key, filePath, 0, fileSize, out)
					} else {
						// file exists, analyze it
						modified, _ := fileMap["LastModified"].(json.Number)
						lastModified, _ := modified.Int64()
						if fileSize == info.Size() && lastModified == info.ModTime().Unix() {
							// this file is probably not changed
							continue
						}
						// file change, analyse it block by block
						fileParts := filePartsFromServer(ip, port, key, filePath)
						out, _ := os.OpenFile(file, os.O_RDWR, os.FileMode(0666))
						defer out.Close()
						out.Truncate(fileSize)
						if len(fileParts) == 0 {
							continue
						}
						h := crc32.NewIEEE()
						for _, filePart := range fileParts {
							filePartMap, _ := filePart.(map[string] interface{})
							idx, _ := filePartMap["StartIndex"].(json.Number)
							startIndex, _ := idx.Int64()
							ost, _ := filePartMap["Offset"].(json.Number)
							offset, _ := ost.Int64()
							checksum := filePartMap["Checksum"].(string)

							buf := make([]byte, offset)
							n, _ := out.ReadAt(buf, startIndex)

							h.Reset()
							h.Write(buf[:n])
							v := fmt.Sprint(h.Sum32())
							if checksum == v {
								// block unchanged
								continue
							}
							// block changed
							downloadFromServer(ip, port, key, filePath, startIndex, offset, out)
						}
					}
				}
			}
		}
		lastIndexed = serverIndexed
		time.Sleep(sleepTime)
	}
}

func downloadFromServer(ip string, port int, key string, filePath string, start int64, length int64, file *os.File) int64 {
	client := &http.Client{}
	req, _ := http.NewRequest("GET", fmt.Sprint("http://", ip, ":", port,
			"/download?&file_path=", url.QueryEscape(filePath), "&start=", start, "&length=", length), nil)
	req.Header.Add("AUTH_KEY", key)
	resp, _ := client.Do(req)
	defer resp.Body.Close()
	file.Seek(start, os.SEEK_SET)
	n, _ := io.CopyN(file, resp.Body, length)
	return n
}

func filePartsFromServer(ip string, port int, key string, filePath string) []interface{} {
	client := &http.Client{}
	req, _ := http.NewRequest("GET", fmt.Sprint("http://", ip, ":", port,
			"/file_parts?file_path=", url.QueryEscape(filePath)), nil)
	req.Header.Add("AUTH_KEY", key)
	resp, _ := client.Do(req)
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	json, _ := simplejson.NewJson(body)
	fileParts := json.MustArray()
	return fileParts
}

func filesFromServer(ip string, port int, key string, filePath string, lastIndexed int64) []interface{} {
	client := &http.Client{}
	req, _ := http.NewRequest("GET", fmt.Sprint("http://", ip, ":", port,
			"/files?last_indexed=", lastIndexed, "&file_path=", url.QueryEscape(filePath)), nil)
	req.Header.Add("AUTH_KEY", key)
	resp, _ := client.Do(req)
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	json, _ := simplejson.NewJson(body)
	files := json.MustArray()
	return files
}

func dirsFromServer(ip string, port int, key string, lastIndexed int64) []interface{} {
	client := &http.Client{}
	req, _ := http.NewRequest("GET", fmt.Sprint("http://", ip, ":", port, "/dirs?last_indexed=", lastIndexed), nil)
	req.Header.Add("AUTH_KEY", key)
	resp, _ := client.Do(req)
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	json, _ := simplejson.NewJson(body)
	dirs := json.MustArray()
	return dirs
}

func timeFromServer(ip string, port int, key string) int64 {
	client := &http.Client{}
	req, _ := http.NewRequest("GET", fmt.Sprint("http://", ip, ":", port, "/time"), nil)
	req.Header.Add("AUTH_KEY", key)
	resp, _ := client.Do(req)
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	json, _ := simplejson.NewJson(body)
	currentTime := json.Get("current_time").MustInt64(0)
	return currentTime
}

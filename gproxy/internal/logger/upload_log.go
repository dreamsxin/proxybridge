package logger

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"
)

func UploadLogs(logPath, key, remoteAddr string) {
	slog.Info("日志上传程序开始运行")
	for {
		slog.Info("to upload")
		if err := uploadLogsV2(logPath, key, remoteAddr); err != nil {
			slog.Error("上传失败", "err", err)
		}
		time.Sleep(time.Hour * 12)
	}
}

func uploadLogsV2(logPath, key, remoteAddr string) error {
	dir := filepath.Dir(logPath)
	files, err := ListDir(dir, "gz")
	if err != nil {
		return err
	}

	cur := time.Now().Format("2006-01-02")

	data := []byte(fmt.Sprintf("cur=%s&key=%s", cur, key))
	signature := md5.Sum(data)
	signatureStr := fmt.Sprintf("%x", signature)

	params := map[string]string{
		"t":    cur,
		"sign": signatureStr,
	}

	for fn := range files {
		filePath := path.Join(dir, fn)
		if err := uploadFileAndParams(remoteAddr, filePath, params); err != nil {
			slog.Error("uploadFileAndParams", "err", err)
		}
	}
	return nil
}

func uploadFileAndParams(url, filePath string, params map[string]string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("file", filePath)
	if err != nil {
		return err
	}

	_, err = io.Copy(part, file)
	if err != nil {
		return err
	}

	for key, value := range params {
		err := writer.WriteField(key, value)
		if err != nil {
			return err
		}
	}

	err = writer.Close()
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var res Res
	json.Unmarshal(bodyBytes, &res)
	if res.Code == 200 {
		os.Remove(filePath)
	} else {
		return fmt.Errorf("上传失败，err：%s", res.Message)
	}
	return nil
}

type Res struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func ListDir(dirPth string, Type string) (file map[string]int, err error) {
	dir, err1 := os.ReadDir(dirPth)
	if err1 != nil {
		err = err1
		return
	}
	file = make(map[string]int)
	typeLen := len(Type)
	for _, fi := range dir {
		name := fi.Name()
		nameLen := len(name)
		if nameLen < typeLen {
			continue
		}
		if name[nameLen-typeLen:] == Type {
			if fi.IsDir() { // 忽略目录
				file[name] = 0
			} else {
				file[name] = 1
			}
		}
	}
	err = nil
	return
}

package upx

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/upyun/go-sdk/v3/upyun"
	"github.com/upyun/upx/fsutil"
	"github.com/upyun/upx/partial"
	"github.com/upyun/upx/processbar"
	"github.com/vbauerster/mpb/v8"
)

const (
	SYNC_EXISTS = iota
	SYNC_OK
	SYNC_FAIL
	SYNC_NOT_FOUND
	DELETE_OK
	DELETE_FAIL

	MinResumePutFileSize = 100 * 1024 * 1024
	DefaultBlockSize     = 10 * 1024 * 1024
	DefaultResumeRetry   = 10
)

type Session struct {
	Bucket   string `json:"bucket"`
	Operator string `json:"username"`
	Password string `json:"password"`
	CWD      string `json:"cwd"`

	updriver *upyun.UpYun
	color    bool

	scores    map[int]int
	smu       sync.RWMutex
	multipart bool

	taskChan chan interface{}
}

type syncTask struct {
	src, dest string
	isdir     bool
}

type delTask struct {
	src, dest string
	isdir     bool
}

type UploadedFile struct {
	barId     int
	LocalPath string
	UpPath    string
	LocalInfo os.FileInfo
	Mode      int
}

var (
	session *Session
)

func (sess *Session) update(key int) {
	sess.smu.Lock()
	sess.scores[key]++
	sess.smu.Unlock()
}

func (sess *Session) dump() string {
	s := make(map[string]string)
	titles := []string{"SYNC_EXISTS", "SYNC_OK", "SYNC_FAIL", "SYNC_NOT_FOUND", "DELETE_OK", "DELETE_FAIL"}
	for i, title := range titles {
		v := fmt.Sprint(sess.scores[i])
		if len(v) > len(title) {
			title = strings.Repeat(" ", len(v)-len(title)) + title
		} else {
			v = strings.Repeat(" ", len(title)-len(v)) + v
		}
		s[title] = v
	}
	header := "+"
	for _, title := range titles {
		header += strings.Repeat("=", len(s[title])+2) + "+"
	}
	header += "\n"
	footer := strings.Replace(header, "=", "-", -1)

	ret := "\n\n" + header
	ret += "|"
	for _, title := range titles {
		ret += " " + title + " |"
	}
	ret += "\n" + footer

	ret += "|"
	for _, title := range titles {
		ret += " " + s[title] + " |"
	}
	return ret + "\n" + footer
}

func (sess *Session) AbsPath(upPath string) (ret string) {
	if strings.HasPrefix(upPath, "/") {
		ret = path.Join(upPath)
	} else {
		ret = path.Join(sess.CWD, upPath)
	}

	if strings.HasSuffix(upPath, "/") && ret != "/" {
		ret += "/"
	}
	return
}

func (sess *Session) IsUpYunDir(upPath string) (isDir bool, exist bool) {
	upInfo, err := sess.updriver.GetInfo(sess.AbsPath(upPath))
	if err != nil {
		return false, false
	}
	return upInfo.IsDir, true
}

func (sess *Session) IsLocalDir(localPath string) (isDir bool, exist bool) {
	fInfo, err := os.Stat(localPath)
	if err != nil {
		return false, false
	}
	return fInfo.IsDir(), true
}

func (sess *Session) FormatUpInfo(upInfo *upyun.FileInfo) string {
	s := "drwxrwxrwx"
	if !upInfo.IsDir {
		s = "-rw-rw-rw-"
	}
	s += fmt.Sprintf(" 1 %s %s %12d", sess.Operator, sess.Bucket, upInfo.Size)
	if upInfo.Time.Year() != time.Now().Year() {
		s += " " + upInfo.Time.Format("Jan 02  2006")
	} else {
		s += " " + upInfo.Time.Format("Jan 02 03:04")
	}
	if upInfo.IsDir && sess.color {
		s += " " + color.BlueString(upInfo.Name)
	} else {
		s += " " + upInfo.Name
	}
	return s
}

func (sess *Session) Init() error {
	sess.scores = make(map[int]int)
	sess.updriver = upyun.NewUpYun(&upyun.UpYunConfig{
		Bucket:    sess.Bucket,
		Operator:  sess.Operator,
		Password:  sess.Password,
		UserAgent: fmt.Sprintf("upx/%s", VERSION),
	})
	_, err := sess.updriver.Usage()
	return err
}

func (sess *Session) Info() {
	n, err := sess.updriver.Usage()
	if err != nil {
		PrintErrorAndExit("usage: %v", err)
	}

	tmp := []string{
		fmt.Sprintf("ServiceName:   %s", sess.Bucket),
		fmt.Sprintf("Operator:      %s", sess.Operator),
		fmt.Sprintf("CurrentDir:    %s", sess.CWD),
		fmt.Sprintf("Usage:         %s", humanizeSize(n)),
	}

	Print(strings.Join(tmp, "\n"))
}

func (sess *Session) Pwd() {
	Print("%s", sess.CWD)
}

func (sess *Session) Mkdir(upPaths ...string) {
	for _, upPath := range upPaths {
		fpath := sess.AbsPath(upPath)
		for fpath != "/" {
			if err := sess.updriver.Mkdir(fpath); err != nil {
				PrintErrorAndExit("mkdir %s: %v", fpath, err)
			}
			fpath = path.Dir(fpath)
		}
	}
}

func (sess *Session) Cd(upPath string) {
	fpath := sess.AbsPath(upPath)
	if isDir, _ := sess.IsUpYunDir(fpath); isDir {
		sess.CWD = fpath
		Print(sess.CWD)
	} else {
		PrintErrorAndExit("cd: %s: Not a directory", fpath)
	}
}

func (sess *Session) Ls(upPath string, match *MatchConfig, maxItems int, isDesc bool) {
	fpath := sess.AbsPath(upPath)
	isDir, exist := sess.IsUpYunDir(fpath)
	if !exist {
		PrintErrorAndExit("ls: cannot access %s: No such file or directory", fpath)
	}

	if !isDir {
		fInfo, err := sess.updriver.GetInfo(fpath)
		if err != nil {
			PrintErrorAndExit("ls %s: %v", fpath, err)
		}
		if IsMatched(fInfo, match) {
			Print(sess.FormatUpInfo(fInfo))
		} else {
			PrintErrorAndExit("ls: cannot access %s: No such file or directory", fpath)
		}
		return
	}

	fInfoChan := make(chan *upyun.FileInfo, 50)
	go func() {
		err := sess.updriver.List(&upyun.GetObjectsConfig{
			Path:        fpath,
			ObjectsChan: fInfoChan,
			DescOrder:   isDesc,
		})
		if err != nil {
			PrintErrorAndExit("ls %s: %v", fpath, err)
		}
	}()

	objs := 0
	for fInfo := range fInfoChan {
		if IsMatched(fInfo, match) {
			Print(sess.FormatUpInfo(fInfo))
			objs++
		}
		if maxItems > 0 && objs >= maxItems {
			break
		}
	}
	if objs == 0 && (match.Wildcard != "" || match.TimeType != TIME_NOT_SET) {
		msg := fpath
		if match.Wildcard != "" {
			msg = fpath + "/" + match.Wildcard
		}
		if match.TimeType != TIME_NOT_SET {
			msg += " timestamp@"
			if match.TimeType == TIME_AFTER || match.TimeType == TIME_INTERVAL {
				msg += "[" + match.After.Format("2006-01-02 15:04:05") + ","
			} else {
				msg += "[-oo,"
			}
			if match.TimeType == TIME_BEFORE || match.TimeType == TIME_INTERVAL {
				msg += match.Before.Format("2006-01-02 15:04:05") + "]"
			} else {
				msg += "+oo]"
			}
		}
		PrintErrorAndExit("ls: cannot access %s: No such file or directory", msg)
	}
}

func (sess *Session) getDir(upPath, localPath string, match *MatchConfig, workers int, resume bool) error {
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return err
	}

	var wg sync.WaitGroup

	fInfoChan := make(chan *upyun.FileInfo, workers*2)
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			var e error
			for fInfo := range fInfoChan {
				if IsMatched(fInfo, match) {
					fpath := path.Join(upPath, fInfo.Name)
					lpath := filepath.Join(localPath, filepath.FromSlash(cleanFilename(fInfo.Name)))
					if fInfo.IsDir {
						os.MkdirAll(lpath, 0755)
					} else {
						isContinue := resume

						// 判断本地文件是否存在
						// 如果存在，大小一致 并且本地文件的最后修改时间大于云端文件的最后修改时间 则跳过该下载
						// 如果云端文件最后的修改时间大于本地文件的创建时间，则强制重新下载
						stat, err := os.Stat(lpath)
						if err == nil {
							if stat.Size() == fInfo.Size && stat.ModTime().After(fInfo.Time) {
								continue
							}
							if stat.Size() > fInfo.Size {
								isContinue = false
							}
							if fInfo.Time.After(stat.ModTime()) {
								isContinue = false
							}
						}

						for i := 1; i <= MaxRetry; i++ {
							e = sess.getFileWithProgress(fpath, lpath, fInfo, 1, isContinue, false)
							if e == nil {
								break
							}
							if upyun.IsNotExist(e) {
								e = nil
								break
							}

							time.Sleep(time.Duration(i*(rand.Intn(MaxJitter-MinJitter)+MinJitter)) * time.Second)
						}
					}
					if e != nil {
						return
					}
				}
			}
		}()
	}

	err := sess.updriver.List(&upyun.GetObjectsConfig{
		Path:         upPath,
		ObjectsChan:  fInfoChan,
		MaxListTries: 3,
		MaxListLevel: -1,
	})
	wg.Wait()
	return err
}

func (sess *Session) getFileWithProgress(upPath, localPath string, upInfo *upyun.FileInfo, works int, resume, inprogress bool) error {
	var err error

	var bar *mpb.Bar
	if upInfo.Size > 0 {
		bar = processbar.ProcessBar.AddBar(localPath, upInfo.Size)
	}

	dir := filepath.Dir(localPath)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	w, err := NewFileWrappedWriter(localPath, bar, resume)
	if err != nil {
		return err
	}
	defer w.Close()

	downloader := partial.NewMultiPartialDownloader(
		localPath,
		upInfo.Size,
		partial.DefaultChunkSize,
		w,
		works,
		func(start, end int64) ([]byte, error) {
			var buffer bytes.Buffer
			headers := map[string]string{
				"Range": fmt.Sprintf("bytes=%d-%d", start, end),
			}
			if inprogress {
				headers["X-Upyun-Multi-In-Progress"] = "true"
			}
			_, err = sess.updriver.Get(&upyun.GetObjectConfig{
				Path:    sess.AbsPath(upPath),
				Writer:  &buffer,
				Headers: headers,
			})
			return buffer.Bytes(), err
		},
	)
	err = downloader.Download()
	if bar != nil {
		bar.EnableTriggerComplete()
		if err != nil {
			bar.Abort(false)
		}
	}
	return err
}

func (sess *Session) Get(upPath, localPath string, match *MatchConfig, workers int, resume, inprogress bool) {
	upPath = sess.AbsPath(upPath)
	headers := map[string]string{}
	if inprogress {
		headers["X-Upyun-Multi-In-Progress"] = "true"
		resume = true
	}
	upInfo, err := sess.updriver.GetInfoWithHeaders(upPath, headers)
	if err != nil {
		PrintErrorAndExit("getinfo %s: %v", upPath, err)
	}

	exist, isDir := false, false
	if localInfo, _ := os.Stat(localPath); localInfo != nil {
		exist = true
		isDir = localInfo.IsDir()
	} else {
		if strings.HasSuffix(localPath, "/") {
			isDir = true
		}
	}

	if upInfo.IsDir {
		if inprogress {
			PrintErrorAndExit("get: %s is a directory", localPath)
		}
		if exist {
			if !isDir {
				PrintErrorAndExit("get: %s Not a directory", localPath)
			} else {
				if match.Wildcard == "" {
					localPath = filepath.Join(localPath, path.Base(upPath))
				}
			}
		}
		if err := sess.getDir(upPath, localPath, match, workers, resume); err != nil {
			PrintErrorAndExit(err.Error())
		}
	} else {
		if isDir {
			localPath = filepath.Join(localPath, cleanFilename(path.Base(upPath)))
		}

		// 小于 100M 不开启多线程
		if upInfo.Size < 1024*1024*100 || inprogress {
			workers = 1
		}
		err := sess.getFileWithProgress(upPath, localPath, upInfo, workers, resume, inprogress)
		if err != nil {
			PrintErrorAndExit(err.Error())
		}
	}
}

func (sess *Session) GetStartBetweenEndFiles(upPath, localPath string, match *MatchConfig, workers int) {
	fpath := sess.AbsPath(upPath)
	isDir, exist := sess.IsUpYunDir(fpath)
	if !exist {
		if match.ItemType == DIR {
			isDir = true
		} else {
			PrintErrorAndExit("get: cannot down %s:No such file or directory", fpath)
		}
	}
	if isDir && match != nil && match.Wildcard == "" {
		if match.ItemType == FILE {
			PrintErrorAndExit("get: cannot down %s: Is a directory", fpath)
		}
	}

	fInfoChan := make(chan *upyun.FileInfo, 1)
	objectsConfig := &upyun.GetObjectsConfig{
		Path:        fpath,
		ObjectsChan: fInfoChan,
		QuitChan:    make(chan bool, 1),
	}
	go func() {
		err := sess.updriver.List(objectsConfig)
		if err != nil {
			PrintErrorAndExit("ls %s: %v", fpath, err)
		}
	}()

	startList := match.Start
	if startList != "" && startList[0] != '/' {
		startList = filepath.Join(fpath, startList)
	}
	endList := match.End
	if endList != "" && endList[0] != '/' {
		endList = filepath.Join(fpath, endList)
	}

	for fInfo := range fInfoChan {
		fp := filepath.Join(fpath, fInfo.Name)
		if (fp >= startList || startList == "") && (fp < endList || endList == "") {
			sess.Get(fp, localPath, match, workers, false, false)
		} else if strings.HasPrefix(startList, fp) {
			//前缀相同进入下一级文件夹，继续递归判断
			if fInfo.IsDir {
				sess.GetStartBetweenEndFiles(fp, localPath+fInfo.Name+"/", match, workers)
			}
		}
		if fp >= endList && endList != "" && fInfo.IsDir {
			close(objectsConfig.QuitChan)
			break
		}
	}
}

func (sess *Session) putFileWithProgress(localPath, upPath string, localInfo os.FileInfo) error {
	var err error
	fd, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer fd.Close()
	cfg := &upyun.PutObjectConfig{
		Path: upPath,
		Headers: map[string]string{
			"Content-Length": fmt.Sprint(localInfo.Size()),
		},
		Reader: fd,
	}

	var bar *mpb.Bar
	if IsVerbose {
		if localInfo.Size() > 0 {
			bar = processbar.ProcessBar.AddBar(upPath, localInfo.Size())
			cfg.ProxyReader = func(offset int64, r io.Reader) io.Reader {
				if offset > 0 {
					bar.SetCurrent(offset)
				}
				return bar.ProxyReader(r)
			}
		}
	} else {
		log.Printf("file: %s, Start\n", upPath)
	}
	if localInfo.Size() >= MinResumePutFileSize || sess.multipart {
		cfg.UseResumeUpload = true
		cfg.ResumePartSize = ResumePartSize(localInfo.Size())
		cfg.MaxResumePutTries = DefaultResumeRetry
	}

	err = sess.updriver.Put(cfg)
	if bar != nil {
		bar.EnableTriggerComplete()
		if err != nil {
			bar.Abort(false)
		}
	}
	if !IsVerbose {
		log.Printf("file: %s, Done\n", upPath)
	}
	return err
}

func (sess *Session) putRemoteFileWithProgress(rawURL, upPath string) error {
	var size int64

	// 如果可以的话，先从 Head 请求中获取文件长度
	resp, err := http.Head(rawURL)
	if err == nil && resp.ContentLength > 0 {
		size = resp.ContentLength
	}
	resp.Body.Close()

	// 通过get方法获取文件，如果get头中包含Content-Length，则使用get头中的Content-Length
	resp, err = http.Get(rawURL)
	if err != nil {
		return fmt.Errorf("http Get %s error: %v", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.ContentLength > 0 {
		size = resp.ContentLength
	}

	// 如果无法获取 Content-Length 则报错
	if size == 0 {
		return fmt.Errorf("get http file Content-Length error: response headers not has Content-Length")
	}

	// 创建进度条
	bar := processbar.ProcessBar.AddBar(upPath, size)
	reader := NewFileWrappedReader(bar, resp.Body)

	// 上传文件
	err = sess.updriver.Put(&upyun.PutObjectConfig{
		Path:   upPath,
		Reader: reader,
		UseMD5: false,
		Headers: map[string]string{
			"Content-Length": fmt.Sprint(size),
		},
	})
	if bar != nil {
		bar.EnableTriggerComplete()
		if err != nil {
			bar.Abort(false)
		}
	}
	if err != nil {
		PrintErrorAndExit("put file error: %v", err)
	}

	return nil
}

func (sess *Session) putFilesWitchProgress(localFiles []*UploadedFile, workers int) {
	var wg sync.WaitGroup

	tasks := make(chan *UploadedFile, workers*2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range tasks {
				err := sess.putFileWithProgressAndMode(
					task.LocalPath,
					task.UpPath,
					task.LocalInfo,
					task.Mode,
				)
				if err != nil {
					fmt.Println("putFileWithProgress error: ", err.Error())
					return
				}
			}
		}()
	}

	for _, f := range localFiles {
		tasks <- f
	}

	close(tasks)
	wg.Wait()
}

func (sess *Session) putDir(localPath, upPath string, workers int, withIgnore bool, mode int) {
	localAbsPath, err := filepath.Abs(localPath)
	if err != nil {
		PrintErrorAndExit(err.Error())
	}
	// 如果上传的是目录，并且是隐藏的目录，则触发提示
	rootDirInfo, err := os.Stat(localAbsPath)
	if err != nil {
		PrintErrorAndExit(err.Error())
	}
	if !withIgnore && fsutil.IsIgnoreFile(localAbsPath, rootDirInfo) {
		PrintErrorAndExit("%s is a ignore dir, use `-all` to force put all files", localAbsPath)
	}

	type FileInfo struct {
		fpath string
		fInfo os.FileInfo
	}
	localFiles := make(chan *FileInfo, workers*2)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for info := range localFiles {
				rel, _ := filepath.Rel(localAbsPath, info.fpath)
				desPath := path.Join(upPath, filepath.ToSlash(rel))
				fInfo, err := os.Stat(info.fpath)
				if err == nil && fInfo.IsDir() {
					err = sess.updriver.Mkdir(desPath)
				} else {
					err = sess.putFileWithProgressAndMode(info.fpath, desPath, info.fInfo, mode)
				}
				if err != nil {
					log.Printf("put %s to %s error: %s", info.fpath, desPath, err)
					if upyun.IsTooManyRequests(err) {
						time.Sleep(time.Second)
						continue
					}
					return
				}
			}
		}()
	}

	filepath.Walk(localAbsPath, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !withIgnore && fsutil.IsIgnoreFile(path, info) {
			if info.IsDir() {
				return filepath.SkipDir
			}
		} else {
			localFiles <- &FileInfo{
				fpath: path,
				fInfo: info,
			}
		}
		return nil
	})

	close(localFiles)
	wg.Wait()
}

// / Put 上传单文件或单目录
func (sess *Session) Put(localPath, upPath string, workers int, withIgnore, inprogress bool, mode int) {
	upPath = sess.AbsPath(upPath)
	if inprogress {
		sess.multipart = true
	}
	exist, isDir := false, false
	if upInfo, _ := sess.updriver.GetInfo(upPath); upInfo != nil {
		exist = true
		isDir = upInfo.IsDir
	}
	// 如果指定了是远程的目录 但是实际在远程的目录是文件类型则报错
	if exist && !isDir && strings.HasSuffix(upPath, "/") {
		PrintErrorAndExit("cant put to %s: path is not a directory, maybe a file", upPath)
	}
	if !exist && strings.HasSuffix(upPath, "/") {
		isDir = true
	}

	// 如果需要上传的文件是URL链接
	fileURL, _ := url.ParseRequestURI(localPath)
	if fileURL != nil && fileURL.Scheme != "" && fileURL.Host != "" {
		if !contains([]string{"http", "https"}, fileURL.Scheme) {
			PrintErrorAndExit("Invalid URL %s", localPath)
		}

		// 如果指定的远程路径 upPath 是目录
		//     则从 url 中获取文件名，获取文件名失败则报错
		if isDir {
			if spaces := strings.Split(fileURL.Path, "/"); len(spaces) > 0 {
				upPath = path.Join(upPath, spaces[len(spaces)-1])
			} else {
				PrintErrorAndExit("missing file name in the url, must has remote path name")
			}
		}
		err := sess.putRemoteFileWithProgress(localPath, upPath)
		if err != nil {
			PrintErrorAndExit(err.Error())
		}
		return
	}

	localInfo, err := os.Stat(localPath)
	if err != nil {
		PrintErrorAndExit("stat %s: %v", localPath, err)
	}

	if localInfo.IsDir() {
		if exist {
			if !isDir {
				PrintErrorAndExit("put: %s: Not a directory", upPath)
			} else {
				upPath = path.Join(upPath, filepath.Base(localPath))
			}
		}
		sess.putDir(localPath, upPath, workers, withIgnore, mode)
	} else {
		if isDir {
			upPath = path.Join(upPath, filepath.Base(localPath))
		}

		sess.putFileWithProgressAndMode(localPath, upPath, localInfo, mode)
	}
}

// put 的升级版命令, 支持多文件上传
func (sess *Session) Upload(filenames []string, upPath string, workers int, withIgnore bool, mode int) {
	upPath = sess.AbsPath(upPath)

	// 检测云端的目的地目录
	upPathExist, upPathIsDir := false, false
	if upInfo, _ := sess.updriver.GetInfo(upPath); upInfo != nil {
		upPathExist = true
		upPathIsDir = upInfo.IsDir
	}
	// 多文件上传 upPath 如果存在则只能是目录
	if upPathExist && !upPathIsDir {
		PrintErrorAndExit("upload: %s: Not a directory", upPath)
	}

	var (
		dirs         []string
		uploadedFile []*UploadedFile
	)
	for _, filename := range filenames {
		localInfo, err := os.Stat(filename)
		if err != nil {
			PrintErrorAndExit(err.Error())
		}

		if localInfo.IsDir() {
			dirs = append(dirs, filename)
		} else {
			uploadedFile = append(uploadedFile, &UploadedFile{
				barId:     -1,
				LocalPath: filename,
				UpPath:    path.Join(upPath, filepath.Base(filename)),
				LocalInfo: localInfo,
				Mode:      mode,
			})
		}
	}

	// 上传目录
	for _, localPath := range dirs {
		sess.putDir(
			localPath,
			path.Join(upPath, filepath.Base(localPath)),
			workers,
			withIgnore,
			mode,
		)
	}

	// 上传文件
	sess.putFilesWitchProgress(uploadedFile, workers)
}

func (sess *Session) rm(fpath string, isAsync bool, isFolder bool) {
	err := sess.updriver.Delete(&upyun.DeleteObjectConfig{
		Path:   fpath,
		Async:  isAsync,
		Folder: isFolder,
	})
	if err == nil || upyun.IsNotExist(err) {
		sess.update(DELETE_OK)
		PrintOnlyVerbose("DELETE %s OK", fpath)
	} else {
		sess.update(DELETE_FAIL)
		PrintError("DELETE %s FAIL %v", fpath, err)
	}
}
func (sess *Session) rmFile(fpath string, isAsync bool) {
	sess.rm(fpath, isAsync, false)
}

func (sess *Session) rmEmptyDir(fpath string, isAsync bool) {
	sess.rm(fpath, isAsync, true)
}

func (sess *Session) rmDir(fpath string, isAsync bool) {
	fInfoChan := make(chan *upyun.FileInfo, 50)
	go func() {
		err := sess.updriver.List(&upyun.GetObjectsConfig{
			Path:        fpath,
			ObjectsChan: fInfoChan,
		})
		if err != nil {
			if upyun.IsNotExist(err) {
				return
			} else {
				PrintErrorAndExit("ls %s: %v", fpath, err)
			}
		}
	}()

	for fInfo := range fInfoChan {
		fp := path.Join(fpath, fInfo.Name)
		if fInfo.IsDir {
			sess.rmDir(fp, isAsync)
		} else {
			sess.rmFile(fp, isAsync)
		}
	}
	sess.rmEmptyDir(fpath, isAsync)
}

func (sess *Session) Rm(upPath string, match *MatchConfig, isAsync bool) {
	fpath := sess.AbsPath(upPath)
	isDir, exist := sess.IsUpYunDir(fpath)
	if !exist {
		if match.ItemType == DIR {
			isDir = true
		} else {
			PrintErrorAndExit("rm: cannot remove %s: No such file or directory", fpath)
		}
	}

	if isDir && match != nil && match.Wildcard == "" {
		if match.ItemType == FILE {
			PrintErrorAndExit("rm: cannot remove %s: Is a directory, add -d/-a flag", fpath)
		}
		sess.rmDir(fpath, isAsync)
		return
	}

	if !isDir {
		fInfo, err := sess.updriver.GetInfo(fpath)
		if err != nil {
			PrintErrorAndExit("getinfo %s: %v", fpath, err)
		}
		if IsMatched(fInfo, match) {
			sess.rmFile(fpath, isAsync)
		}
		return
	}

	fInfoChan := make(chan *upyun.FileInfo, 50)
	go func() {
		err := sess.updriver.List(&upyun.GetObjectsConfig{
			Path:        fpath,
			ObjectsChan: fInfoChan,
		})
		if err != nil {
			PrintErrorAndExit("ls %s: %v", fpath, err)
		}
	}()

	for fInfo := range fInfoChan {
		fp := path.Join(fpath, fInfo.Name)
		if IsMatched(fInfo, match) {
			if fInfo.IsDir {
				sess.rmDir(fp, isAsync)
			} else {
				sess.rmFile(fp, isAsync)
			}
		}
	}
}

func (sess *Session) tree(upPath, prefix string, output chan string) (folders, files int, err error) {
	upInfos := make(chan *upyun.FileInfo, 50)
	fpath := sess.AbsPath(upPath)
	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()
		prevInfo := <-upInfos
		for fInfo := range upInfos {
			p := prefix + "|-- "
			if prevInfo.IsDir {
				if sess.color {
					output <- p + color.BlueString("%s", prevInfo.Name)
				} else {
					output <- p + prevInfo.Name
				}
				folders++
				d, f, _ := sess.tree(path.Join(fpath, prevInfo.Name), prefix+"!   ", output)
				folders += d
				files += f
			} else {
				output <- p + prevInfo.Name
				files++
			}
			prevInfo = fInfo
		}
		if prevInfo == nil {
			return
		}
		p := prefix + "`-- "
		if prevInfo.IsDir {
			if sess.color {
				output <- p + color.BlueString("%s", prevInfo.Name)
			} else {
				output <- p + prevInfo.Name
			}
			folders++
			d, f, _ := sess.tree(path.Join(fpath, prevInfo.Name), prefix+"    ", output)
			folders += d
			files += f
		} else {
			output <- p + prevInfo.Name
			files++
		}
	}()

	err = sess.updriver.List(&upyun.GetObjectsConfig{
		Path:        fpath,
		ObjectsChan: upInfos,
	})
	wg.Wait()
	return
}

func (sess *Session) Tree(upPath string) {
	fpath := sess.AbsPath(upPath)
	files, folders := 0, 0
	defer func() {
		Print("\n%d directories, %d files", folders, files)
	}()

	if isDir, _ := sess.IsUpYunDir(fpath); !isDir {
		PrintErrorAndExit("%s [error opening dir]", fpath)
	}
	Print("%s", fpath)

	output := make(chan string, 50)
	go func() {
		folders, files, _ = sess.tree(fpath, "", output)
		close(output)
	}()

	for s := range output {
		Print(s)
	}
	return
}

func (sess *Session) syncFile(localPath, upPath string, strongCheck bool) (status int, err error) {
	curMeta, err := makeDBValue(localPath, false)
	if err != nil {
		if os.IsNotExist(err) {
			return SYNC_NOT_FOUND, err
		}
		return SYNC_FAIL, err
	}
	if curMeta.IsDir == "true" {
		return SYNC_FAIL, fmt.Errorf("file type changed")
	}

	if strongCheck {
		upInfo, _ := sess.updriver.GetInfo(upPath)
		if upInfo != nil {
			curMeta.Md5, _ = md5File(localPath)
			if curMeta.Md5 == upInfo.MD5 {
				setDBValue(localPath, upPath, curMeta)
				return SYNC_EXISTS, nil
			}
		}
	} else {
		prevMeta, err := getDBValue(localPath, upPath)
		if err != nil {
			return SYNC_FAIL, err
		}

		if prevMeta != nil {
			if curMeta.ModifyTime == prevMeta.ModifyTime {
				return SYNC_EXISTS, nil
			}
			curMeta.Md5, _ = md5File(localPath)
			if curMeta.Md5 == prevMeta.Md5 {
				setDBValue(localPath, upPath, curMeta)
				return SYNC_EXISTS, nil
			}
		}
	}
	for i := 1; i <= MaxRetry; i++ {
		err = sess.updriver.Put(&upyun.PutObjectConfig{Path: upPath, LocalPath: localPath})
		if err == nil {
			break
		}
		time.Sleep(time.Duration(i*(rand.Intn(MaxJitter-MinJitter)+MinJitter)) * time.Second)
	}
	if err != nil {
		return SYNC_FAIL, err
	}
	setDBValue(localPath, upPath, curMeta)
	return SYNC_OK, nil
}

func (sess *Session) syncObject(localPath, upPath string, isDir bool) {
	if isDir {
		status, err := sess.syncDirectory(localPath, upPath)
		switch status {
		case SYNC_OK:
			PrintOnlyVerbose("sync %s to %s OK", localPath, upPath)
		case SYNC_EXISTS:
			PrintOnlyVerbose("sync %s to %s EXISTS", localPath, upPath)
		case SYNC_FAIL, SYNC_NOT_FOUND:
			PrintError("sync %s to %s FAIL %v", localPath, upPath, err)
		}
		sess.update(status)
	} else {
		sess.taskChan <- &syncTask{src: localPath, dest: upPath}
	}
}

func (sess *Session) syncDirectory(localPath, upPath string) (int, error) {
	delFunc := func(prevMeta *fileMeta) {
		sess.taskChan <- &delTask{
			src:   filepath.Join(localPath, prevMeta.Name),
			dest:  path.Join(upPath, prevMeta.Name),
			isdir: prevMeta.IsDir,
		}
	}
	syncFunc := func(curMeta *fileMeta) {
		src := filepath.Join(localPath, curMeta.Name)
		dest := path.Join(upPath, curMeta.Name)
		sess.syncObject(src, dest, curMeta.IsDir)
	}

	dbVal, err := getDBValue(localPath, upPath)
	if err != nil {
		return SYNC_FAIL, err
	}

	curMetas, err := makeFileMetas(localPath)
	if err != nil {
		// if not exist, should sync next time
		if os.IsNotExist(err) {
			return SYNC_NOT_FOUND, err
		}
		return SYNC_FAIL, err
	}

	status := SYNC_EXISTS
	var prevMetas []*fileMeta
	if dbVal != nil && dbVal.IsDir == "true" {
		prevMetas = dbVal.Items
	} else {
		if err = sess.updriver.Mkdir(upPath); err != nil {
			return SYNC_FAIL, err
		}
		status = SYNC_OK
	}

	cur, curSize, prev, prevSize := 0, len(curMetas), 0, len(prevMetas)
	for cur < curSize && prev < prevSize {
		curMeta, prevMeta := curMetas[cur], prevMetas[prev]
		if curMeta.Name == prevMeta.Name {
			if curMeta.IsDir != prevMeta.IsDir {
				delFunc(prevMeta)
			}
			syncFunc(curMeta)
			prev++
			cur++
		} else if curMeta.Name > prevMeta.Name {
			delFunc(prevMeta)
			prev++
		} else {
			syncFunc(curMeta)
			cur++
		}
	}
	for ; cur < curSize; cur++ {
		syncFunc(curMetas[cur])
	}
	for ; prev < prevSize; prev++ {
		delFunc(prevMetas[prev])
	}

	setDBValue(localPath, upPath, &dbValue{IsDir: "true", Items: curMetas})
	return status, nil
}

func (sess *Session) Sync(localPath, upPath string, workers int, delete, strong bool) {
	var wg sync.WaitGroup
	sess.taskChan = make(chan interface{}, workers*2)
	stopChan := make(chan bool, 1)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	upPath = sess.AbsPath(upPath)
	localPath, _ = filepath.Abs(localPath)

	if err := initDB(); err != nil {
		PrintErrorAndExit("sync: init database: %v", err)
	}

	var delLock sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range sess.taskChan {
				switch v := task.(type) {
				case *syncTask:
					stat, err := sess.syncFile(v.src, v.dest, strong)
					switch stat {
					case SYNC_OK:
						PrintOnlyVerbose("sync %s to %s OK", v.src, v.dest)
					case SYNC_EXISTS:
						PrintOnlyVerbose("sync %s to %s EXISTS", v.src, v.dest)
					case SYNC_FAIL, SYNC_NOT_FOUND:
						PrintError("sync %s to %s FAIL %v", v.src, v.dest, err)
					}
					sess.update(stat)
				case *delTask:
					if delete {
						delDBValue(v.src, v.dest)
						delLock.Lock()
						if v.isdir {
							sess.rmDir(v.dest, false)
						} else {
							sess.rmFile(v.dest, false)
						}
						delLock.Unlock()
					}
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(stopChan)
	}()

	go func() {
		isDir, _ := sess.IsLocalDir(localPath)
		sess.syncObject(localPath, upPath, isDir)
		close(sess.taskChan)
	}()

	select {
	case <-sigChan:
		PrintErrorAndExit("%s", sess.dump())
	case <-stopChan:
		if sess.scores[SYNC_FAIL] > 0 || sess.scores[DELETE_FAIL] > 0 {
			PrintErrorAndExit("%s", sess.dump())
		} else {
			Print("%s", sess.dump())
		}
	}
}
func (sess *Session) PostTask(app, notify, taskFile string) {
	fd, err := os.Open(taskFile)
	if err != nil {
		PrintErrorAndExit("open %s: %v", taskFile, err)
	}

	body, err := io.ReadAll(fd)
	fd.Close()
	if err != nil {
		PrintErrorAndExit("read %s: %v", taskFile, err)
	}

	var tasks []interface{}
	if err = json.Unmarshal(body, &tasks); err != nil {
		PrintErrorAndExit("json Unmarshal: %v", err)
	}

	if notify == "" {
		notify = "https://httpbin.org/post"
	}
	ids, err := sess.updriver.CommitTasks(&upyun.CommitTasksConfig{
		AppName:   app,
		NotifyUrl: notify,
		Tasks:     tasks,
	})
	if err != nil {
		PrintErrorAndExit("commit tasks: %v", err)
	}
	Print("%v", ids)
}

func (sess *Session) Purge(urls []string, file string) {
	if urls == nil {
		urls = make([]string, 0)
	}
	if file != "" {
		fd, err := os.Open(file)
		if err != nil {
			PrintErrorAndExit("open %s: %v", file, err)
		}
		body, err := io.ReadAll(fd)
		fd.Close()
		if err != nil {
			PrintErrorAndExit("read %s: %v", file, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if line == "" {
				continue
			}
			urls = append(urls, line)
		}
	}
	for idx := range urls {
		if !strings.HasPrefix(urls[idx], "http") {
			urls[idx] = "http://" + urls[idx]
		}
	}
	if len(urls) == 0 {
		return
	}

	fails, err := sess.updriver.Purge(urls)
	if fails != nil && len(fails) != 0 {
		PrintError("Purge failed urls:")
		for _, url := range fails {
			PrintError("%s", url)
		}
		PrintErrorAndExit("too many fails")
	}
	if err != nil {
		PrintErrorAndExit("purge error: %v", err)
	}
}

func (sess *Session) Copy(srcPath, destPath string, force bool) error {
	return sess.copyMove(srcPath, destPath, "copy", force)
}

func (sess *Session) Move(srcPath, destPath string, force bool) error {
	return sess.copyMove(srcPath, destPath, "move", force)
}

// 移动或者复制
// method: "move" | "copy"
// force: 是否覆盖目标文件
func (sess *Session) copyMove(srcPath, destPath, method string, force bool) error {
	// 将源文件路径转化为绝对路径
	srcPath = sess.AbsPath(srcPath)

	// 检测源文件
	sourceFileInfo, err := sess.updriver.GetInfo(srcPath)
	if err != nil {
		if upyun.IsNotExist(err) {
			return fmt.Errorf("source file %s is not exist", srcPath)
		}
		return err
	}
	if sourceFileInfo.IsDir {
		return fmt.Errorf("not support dir, %s is dir", srcPath)
	}

	// 将目标路径转化为绝对路径
	destPath = sess.AbsPath(destPath)

	destFileInfo, err := sess.updriver.GetInfo(destPath)
	// 如果返回的错误不是文件不存在错误，则返回错误
	if err != nil && !upyun.IsNotExist(err) {
		return err
	}
	// 如果没有错误，表示文件存在，则检测文件类型，并判断是否允许覆盖
	if err == nil {
		if !destFileInfo.IsDir {
			// 如果目标文件是文件类型，则需要使用强制覆盖
			if !force {
				return fmt.Errorf(
					"target path %s already exists use -f to force overwrite",
					destPath,
				)
			}
		} else {
			// 补全文件名后，再次检测文件存不存在
			destPath = path.Join(destPath, path.Base(srcPath))
			destFileInfo, err := sess.updriver.GetInfo(destPath)
			if err == nil {
				if destFileInfo.IsDir {
					return fmt.Errorf(
						"target file %s already exists and is dir",
						destPath,
					)
				}
				if !force {
					return fmt.Errorf(
						"target file %s already exists use -f to force overwrite",
						destPath,
					)
				}
			}
		}
	}

	if srcPath == destPath {
		return fmt.Errorf(
			"source and target are the same %s => %s",
			srcPath,
			destPath,
		)
	}

	switch method {
	case "copy":
		return sess.updriver.Copy(&upyun.CopyObjectConfig{
			SrcPath:  srcPath,
			DestPath: destPath,
		})
	case "move":
		return sess.updriver.Move(&upyun.MoveObjectConfig{
			SrcPath:  srcPath,
			DestPath: destPath,
		})
	default:
		return fmt.Errorf("not support method")
	}
}

func (sess *Session) putFileWithProgressAndMode(localPath, upPath string, localInfo os.FileInfo, mode int) error {
	// 检查文件是否存在于远程
	upInfo, err := sess.updriver.GetInfo(upPath)

	// 根据不同模式处理上传逻辑
	if err == nil { // 文件存在于远程
		switch mode {
		case 1: // 覆盖上传
			// 继续上传，覆盖远程文件
		case 2: // 跳过重复
			if !IsVerbose {
				log.Printf("file: %s, Skipped (already exists)\n", upPath)
			} else {
				fmt.Printf("file: %s, Skipped (already exists)\n", upPath)
			}
			return nil
		case 3: // 检查文件大小
			// 如果远程文件大小大于等于本地文件，则跳过
			if upInfo.Size >= localInfo.Size() {
				if !IsVerbose {
					log.Printf("file: %s, Skipped (remote size >= local size)\n", upPath)
				} else {
					fmt.Printf("file: %s, Skipped (remote size >= local size)\n", upPath)
				}
				return nil
			}
			// 否则继续上传，覆盖远程文件
		default:
			// 默认为模式3
			if upInfo.Size >= localInfo.Size() {
				if !IsVerbose {
					log.Printf("file: %s, Skipped (remote size >= local size)\n", upPath)
				} else {
					fmt.Printf("file: %s, Skipped (remote size >= local size)\n", upPath)
				}
				return nil
			}
		}
	}

	// 执行上传
	return sess.putFileWithProgress(localPath, upPath, localInfo)
}

func (sess *Session) PutByMap(mapFile string, workers int, mode int) {
	fd, err := os.Open(mapFile)
	if err != nil {
		PrintErrorAndExit("open %s: %v", mapFile, err)
	}
	defer fd.Close()

	scanner := bufio.NewScanner(fd)
	var (
		uploadedFile []*UploadedFile
	)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		localPath := fields[0]
		upPath := fields[1]

		localInfo, err := os.Stat(localPath)
		if err != nil {
			PrintErrorAndExit(err.Error())
		}

		uploadedFile = append(uploadedFile, &UploadedFile{
			barId:     -1,
			LocalPath: localPath,
			UpPath:    upPath,
			LocalInfo: localInfo,
			Mode:      mode,
		})

	}

	if err := scanner.Err(); err != nil {
		PrintErrorAndExit("read %s: %v", mapFile, err)
	}

	// 上传文件
	sess.putFilesWitchProgress(uploadedFile, workers)
}

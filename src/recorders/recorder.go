//go:generate mockgen -package recorders -destination mock_test.go github.com/hr3lxphr6j/bililive-go/src/recorders Recorder,Manager
package recorders

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/bluele/gcache"
	"github.com/sirupsen/logrus"

	"github.com/hr3lxphr6j/bililive-go/src/configs"
	"github.com/hr3lxphr6j/bililive-go/src/instance"
	"github.com/hr3lxphr6j/bililive-go/src/interfaces"
	"github.com/hr3lxphr6j/bililive-go/src/live"
	"github.com/hr3lxphr6j/bililive-go/src/pkg/events"
	"github.com/hr3lxphr6j/bililive-go/src/pkg/parser"
	"github.com/hr3lxphr6j/bililive-go/src/pkg/parser/ffmpeg"
	"github.com/hr3lxphr6j/bililive-go/src/pkg/parser/native/flv"
	"github.com/hr3lxphr6j/bililive-go/src/pkg/utils"
)

const (
	begin uint32 = iota
	pending
	running
	stopped
)

// for test
var (
	newParser = func(u *url.URL, useNativeFlvParser bool, cfg map[string]string) (parser.Parser, error) {
		parserName := ffmpeg.Name
		if strings.Contains(u.Path, ".flv") && useNativeFlvParser {
			parserName = flv.Name
		}
		return parser.New(parserName, cfg)
	}

	mkdir = func(path string) error {
		return os.MkdirAll(path, os.ModePerm)
	}

	removeEmptyFile = func(file string) {
		if stat, err := os.Stat(file); err == nil && stat.Size() == 0 {
			os.Remove(file)
		}
	}
)

func getDefaultFileNameTmpl(config *configs.Config) *template.Template {
	return template.Must(template.New("filename").Funcs(utils.GetFuncMap(config)).
		Parse(`{{ .Live.GetPlatformCNName }}/{{ .HostName | filenameFilter }}/[{{ now | date "2006-01-02 15-04-05"}}][{{ .HostName | filenameFilter }}][{{ .RoomName | filenameFilter }}].flv`))
}

type Recorder interface {
	Start(ctx context.Context) error
	StartTime() time.Time
	GetStatus() (map[string]string, error)
	Close()
}

type recorder struct {
	Live       live.Live
	OutPutPath string

	config     *configs.Config
	ed         events.Dispatcher
	logger     *interfaces.Logger
	cache      gcache.Cache
	startTime  time.Time
	parser     parser.Parser
	parserLock *sync.RWMutex

	stop  chan struct{}
	state uint32
}

func NewRecorder(ctx context.Context, live live.Live) (Recorder, error) {
	inst := instance.GetInstance(ctx)
	return &recorder{
		Live:       live,
		OutPutPath: instance.GetInstance(ctx).Config.OutPutPath,
		config:     inst.Config,
		cache:      inst.Cache,
		startTime:  time.Now(),
		ed:         inst.EventDispatcher.(events.Dispatcher),
		logger:     inst.Logger,
		state:      begin,
		stop:       make(chan struct{}),
		parserLock: new(sync.RWMutex),
	}, nil
}

func (r *recorder) tryRecord(ctx context.Context) {
	var streamInfos []*live.StreamUrlInfo
	var err error
	if streamInfos, err = r.Live.GetStreamInfos(); err == live.ErrNotImplemented {
		var urls []*url.URL
		if urls, err = r.Live.GetStreamUrls(); err == live.ErrNotImplemented {
			panic("GetStreamInfos and GetStreamUrls are not implemented for " + r.Live.GetPlatformCNName())
		} else if err == nil {
			streamInfos = utils.GenUrlInfos(urls, make(map[string]string))
		}
	}
	if err != nil || len(streamInfos) == 0 {
		r.getLogger().WithError(err).Warn("failed to get stream url, will retry after 5s...")
		time.Sleep(5 * time.Second)
		return
	}

	obj, _ := r.cache.Get(r.Live)
	info := obj.(*live.Info)

	tmpl := getDefaultFileNameTmpl(r.config)
	if r.config.OutputTmpl != "" {
		_tmpl, err := template.New("user_filename").Funcs(utils.GetFuncMap(r.config)).Parse(r.config.OutputTmpl)
		if err == nil {
			tmpl = _tmpl
		}
	}

	buf := new(bytes.Buffer)
	if err = tmpl.Execute(buf, info); err != nil {
		panic(fmt.Sprintf("failed to render filename, err: %v", err))
	}
	fileName := filepath.Join(r.OutPutPath, buf.String())
	outputPath, _ := filepath.Split(fileName)
	streamInfo := streamInfos[0]
	url := streamInfo.Url

	if strings.Contains(url.Path, "m3u8") {
		fileName = fileName[:len(fileName)-4] + ".ts"
	}

	if info.AudioOnly {
		fileName = fileName[:strings.LastIndex(fileName, ".")] + ".aac"
	}

	if err = mkdir(outputPath); err != nil {
		r.getLogger().WithError(err).Errorf("failed to create output path[%s]", outputPath)
		return
	}
	parserCfg := map[string]string{
		"timeout_in_us": strconv.Itoa(r.config.TimeoutInUs),
	}
	if r.config.Debug {
		parserCfg["debug"] = "true"
	}
	p, err := newParser(url, r.config.Feature.UseNativeFlvParser, parserCfg)
	if err != nil {
		r.getLogger().WithError(err).Error("failed to init parse")
		return
	}
	r.setAndCloseParser(p)
	r.startTime = time.Now()
	r.getLogger().Debugln("Start ParseLiveStream(" + url.String() + ", " + fileName + ")")
	r.getLogger().Println(r.parser.ParseLiveStream(ctx, streamInfo, r.Live, fileName))
	r.getLogger().Debugln("End ParseLiveStream(" + url.String() + ", " + fileName + ")")
	removeEmptyFile(fileName)
	ffmpegPath, err := utils.GetFFmpegPath(ctx)
	if err != nil {
		r.getLogger().WithError(err).Error("failed to find ffmpeg")
		return
	}

//格式转换时去除原本后缀名
	newFileName := fileName[0:strings.LastIndex(fileName, ".")]
	convertCmd := exec.Command(
		ffmpegPath,
		"-hide_banner",
		"-i",
		fileName,
		"-c",
		"copy",
		newFileName+".mp4",
		)
	if err = convertCmd.Run(); err != nil {
		convertCmd.Process.Kill()
		r.getLogger().Debugln(err)
	} else if r.config.OnRecordFinished.DeleteFlvAfterConvert {
		os.Remove(fileName)
	}

	cmdStr := strings.Trim(r.config.OnRecordFinished.CustomCommandline, "")
	if len(cmdStr) > 0 {
		bash := ""
		args := ""
		switch runtime.GOOS {
		case "linux":
			bash = "bash"
			args = "-c"
		case "windows":
			bash = "cmd"
			args = "/C"
		default:
			r.getLogger().Warnln("Unsupport system ", runtime.GOOS)
		}
		r.getLogger().Debugf("start executing custom_commandline: %s %s %s", bash, args, cmdStr)
		cmd := exec.Command(bash, args, cmdStr)
		if r.config.Debug {
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
		}
		if err = cmd.Run(); err != nil {
			r.getLogger().WithError(err).Debugf("custom commandline execute failure (%s %s)\n", bash, cmdStr)
			return
		}

		r.getLogger().Debugf("end executing custom_commandline: %s", args[1])
	}
}

func (r *recorder) run(ctx context.Context) {
	for {
		select {
		case <-r.stop:
			return
		default:
			r.tryRecord(ctx)
		}
	}
}

func (r *recorder) getParser() parser.Parser {
	r.parserLock.RLock()
	defer r.parserLock.RUnlock()
	return r.parser
}

func (r *recorder) setAndCloseParser(p parser.Parser) {
	r.parserLock.Lock()
	defer r.parserLock.Unlock()
	if r.parser != nil {
		if err := r.parser.Stop(); err != nil {
			r.getLogger().WithError(err).Warn("failed to end recorder")
		}
	}
	r.parser = p
}

func (r *recorder) Start(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&r.state, begin, pending) {
		return nil
	}
	go r.run(ctx)
	r.getLogger().Info("Record Start")
	r.ed.DispatchEvent(events.NewEvent(RecorderStart, r.Live))
	atomic.CompareAndSwapUint32(&r.state, pending, running)
	return nil
}

func (r *recorder) StartTime() time.Time {
	return r.startTime
}

func (r *recorder) Close() {
	if !atomic.CompareAndSwapUint32(&r.state, running, stopped) {
		return
	}
	close(r.stop)
	if p := r.getParser(); p != nil {
		if err := p.Stop(); err != nil {
			r.getLogger().WithError(err).Warn("failed to end recorder")
		}
	}
	r.getLogger().Info("Record End")
	r.ed.DispatchEvent(events.NewEvent(RecorderStop, r.Live))
}

func (r *recorder) getLogger() *logrus.Entry {
	return r.logger.WithFields(r.getFields())
}

func (r *recorder) getFields() map[string]interface{} {
	obj, err := r.cache.Get(r.Live)
	if err != nil {
		return nil
	}
	info := obj.(*live.Info)
	return map[string]interface{}{
		"host": info.HostName,
		"room": info.RoomName,
	}
}

func (r *recorder) GetStatus() (map[string]string, error) {
	statusP, ok := r.getParser().(parser.StatusParser)
	if !ok {
		return nil, ErrParserNotSupportStatus
	}
	return statusP.Status()
}

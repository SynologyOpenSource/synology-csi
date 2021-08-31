// Copyright 2021 Synology Inc.

package logger

import (
	"os"
	"runtime"
	"strings"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	nested "github.com/antonfisher/nested-logrus-formatter"
)

var WebapiDebug = false

const (
	DefaultLogLevel = logrus.InfoLevel
	DefaultTimestampFormat = time.RFC3339
)

type CallerHook struct {
	FileNameAndLine string
	Skip            int
}

func (hook *CallerHook) Fire(entry *logrus.Entry) error {
	fileName, line := findCaller(hook.Skip)
	entry.Data[hook.FileNameAndLine] = fileName + ":" + strconv.Itoa(line) // <filename>:<line>
	return nil
}

func (hook *CallerHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func NewCallerHook() logrus.Hook {
	hook := CallerHook{
		FileNameAndLine: "filePath",
		Skip: 5,
	}
	return &hook
}

func getCaller(skip int) (string, int) {
	_, file, line, ok := runtime.Caller(skip)
	if !ok{
		return "", 0
	}

	n := 0
	for i := len(file) - 1; i > 0; i-- {
		if string(file[i]) == "/" {
			n++
			if n >= 2 {
				file = file[i+1:]
				break
			}
		}
	}
	return file, line
}

func findCaller(skip int) (string, int)  {
	var file string
	var line int
	for i := 0; i < 10; i++ {
		file, line = getCaller(skip+i)
		// if called by logrus functions, continue finding the upper caller
		if !strings.HasPrefix(file, "logrus"){
			break
		}
	}
	return file, line
}

func setLogLevel(logLevel string) {	// debug, info, warn, error, fatal
	level, err := logrus.ParseLevel(logLevel)
	if err == nil {
		logrus.SetLevel(level)
	} else {
		logrus.SetLevel(DefaultLogLevel)
	}
	return
}

func Init(logLevel string) {
	logrus.AddHook(NewCallerHook())
	logrus.SetOutput(os.Stdout)
	setLogLevel(logLevel)
	logrus.SetFormatter(&nested.Formatter{
		HideKeys: true,
		TimestampFormat: DefaultTimestampFormat,
		ShowFullLevel: true,
		NoColors: true,
	})
}